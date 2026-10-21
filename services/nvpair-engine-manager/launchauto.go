// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	gib = int64(1) << 30
	// vllmOverhead covers activations, CUDA graphs and the CUDA context on top of
	// weights and KV cache. Measured on Qwen3-8B it is well under this.
	vllmOverhead = 3 * gib
	// weightsSlack is headroom over the snapshot size for runtime copies.
	weightsSlack = 1.08
	// freeMargin is left unclaimed on the GPU for the desktop and other engines.
	freeMargin = gib
	// minAutoContext is the smallest window worth shrinking to; below it the
	// model is left to vLLM, whose own error then names the real shortfall.
	minAutoContext = 4096
	maxAutoUtil    = 0.95
	minAutoUtil    = 0.15
)

// gpuMemory reports total and free bytes on GPU 0. It is a variable so tests
// can supply a machine.
var gpuMemory = nvidiaSMIMemory

func nvidiaSMIMemory() (total, free int64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=memory.total,memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	parts := strings.Split(line, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	t, errT := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	f, errF := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if errT != nil || errF != nil || t <= 0 {
		return 0, 0, false
	}
	return t * 1024 * 1024, f * 1024 * 1024, true
}

// applyAutoLaunch appends the arguments the manifest's auto_launch asks for.
// It runs after the user's launch has been validated and never changes an
// argument the user supplied, so anything set in the engine settings wins.
func applyAutoLaunch(engine string, rt Runtime, command launchCommand, modelsDir string) launchCommand {
	if rt.AutoLaunch == nil || rt.AutoLaunch.Kind != autoLaunchHFContext {
		return command
	}
	hub := filepath.Join(expandPath(modelsDir), "hub")
	extra, note := planHFContext(command.Args, hub, int64(rt.AutoLaunch.ContextTokens))
	if tools := planToolCalling(command.Args, hub); len(tools) > 0 {
		extra = append(extra, tools...)
		note += " tool calling enabled"
	}
	if len(extra) == 0 {
		return command
	}
	slog.Info("engine launch adjusted for the model and GPU", "engine", engine, "added_args", extra, "reason", note)
	command.Args = append(append([]string{}, command.Args...), extra...)
	return command
}

// launchFlagValue returns the value of a flag in either "--flag value" or
// "--flag=value" form, and whether the flag is present at all.
func launchFlagValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if value, ok := strings.CutPrefix(arg, flag+"="); ok {
			return value, true
		}
	}
	return "", false
}

type hfModelConfig struct {
	MaxPositionEmbeddings int64           `json:"max_position_embeddings"`
	NumHiddenLayers       int64           `json:"num_hidden_layers"`
	NumAttentionHeads     int64           `json:"num_attention_heads"`
	NumKeyValueHeads      int64           `json:"num_key_value_heads"`
	HiddenSize            int64           `json:"hidden_size"`
	HeadDim               int64           `json:"head_dim"`
	ModelType             string          `json:"model_type"`
	TorchDtype            string          `json:"torch_dtype"`
	Dtype                 string          `json:"dtype"`
	RopeScaling           json.RawMessage `json:"rope_scaling"`
	RopeParameters        json.RawMessage `json:"rope_parameters"`
	TextConfig            *hfModelConfig  `json:"text_config"`
}

// ropeExtended reports whether the model already carries a rope-scaling
// definition other than plain defaults. Overriding one would change the model's
// behaviour in ways this code cannot judge, so such models are only capped.
func (c hfModelConfig) ropeExtended() bool {
	for _, raw := range []json.RawMessage{c.RopeScaling, c.RopeParameters} {
		var fields map[string]any
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		if kind, ok := fields["rope_type"].(string); ok && kind != "" && kind != "default" {
			return true
		}
		if kind, ok := fields["type"].(string); ok && kind != "" && kind != "default" {
			return true
		}
		if _, ok := fields["factor"]; ok {
			return true
		}
	}
	return false
}

func (c hfModelConfig) dtypeBytes() int64 {
	d := c.Dtype
	if d == "" {
		d = c.TorchDtype
	}
	if d == "float32" {
		return 4
	}
	return 2
}

// readHFModel finds the model's complete snapshot in the Hugging Face hub cache
// and returns its parsed config and size on disk.
func readHFModel(hubDir, id string) (cfg hfModelConfig, size int64, ok bool) {
	repo := filepath.Join(hubDir, "models--"+strings.ReplaceAll(id, "/", "--"))
	snapshots, err := os.ReadDir(filepath.Join(repo, "snapshots"))
	if err != nil {
		return cfg, 0, false
	}
	for _, snapshot := range snapshots {
		dir := filepath.Join(repo, "snapshots", snapshot.Name())
		bytes, complete := completeHFSnapshot(dir)
		if !complete {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil || json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		return cfg, bytes, true
	}
	return cfg, 0, false
}

// planHFContext decides the extra vLLM arguments for the model named in args:
//
//   - --max-model-len: the target window, or the model's own limit when that is
//     lower and cannot be extended, or what fits in GPU memory when less.
//   - --hf-overrides: YaRN rope scaling when the target exceeds the model's
//     native window, so a 40K model can serve 64K.
//   - --gpu-memory-utilization: just enough for weights, KV cache and overhead,
//     not vLLM's 92% default that would starve other engines.
//
// A flag the user already set is never touched. Tensor or pipeline parallel
// launches are left alone entirely: sizing them needs more than GPU 0.
func planHFContext(args []string, hubDir string, target int64) (extra []string, note string) {
	if _, ok := launchFlagValue(args, "--tensor-parallel-size"); ok {
		return nil, ""
	}
	if _, ok := launchFlagValue(args, "--pipeline-parallel-size"); ok {
		return nil, ""
	}
	model, ok := launchFlagValue(args, "--model")
	if !ok || model == "" {
		return nil, ""
	}
	_, userLen := launchFlagValue(args, "--max-model-len")
	_, userOverrides := launchFlagValue(args, "--hf-overrides")
	userUtilText, userUtil := launchFlagValue(args, "--gpu-memory-utilization")
	_, userKV := launchFlagValue(args, "--kv-cache-memory-bytes")
	if userLen && userUtil {
		return nil, ""
	}

	total, free, haveGPU := gpuMemory()
	cfg, weights, cached := readHFModel(hubDir, model)
	if !cached {
		// Not downloaded yet, so its size and limits are unknown. Only the memory
		// share can be bounded sensibly.
		if haveGPU && !userUtil && !userKV {
			util := math.Floor(math.Min(0.5, float64(free-freeMargin)/float64(total))*100) / 100
			if util >= minAutoUtil {
				return []string{"--gpu-memory-utilization", fmt.Sprintf("%.2f", util)}, "model not in cache"
			}
		}
		return nil, ""
	}
	text := cfg
	if cfg.TextConfig != nil {
		text = *cfg.TextConfig
	}
	native := text.MaxPositionEmbeddings
	layers := text.NumHiddenLayers
	kvHeads := text.NumKeyValueHeads
	if kvHeads == 0 {
		kvHeads = text.NumAttentionHeads
	}
	headDim := text.HeadDim
	if headDim == 0 && text.NumAttentionHeads > 0 {
		headDim = text.HiddenSize / text.NumAttentionHeads
	}
	if native == 0 || layers == 0 || kvHeads == 0 || headDim == 0 {
		return nil, ""
	}
	perToken := 2 * layers * kvHeads * headDim * text.dtypeBytes()
	fixed := int64(float64(weights)*weightsSlack) + vllmOverhead

	ctx := target
	var overrides string
	extend := native < target && !text.ropeExtended()
	if native < target && !extend {
		ctx = native
	}

	// The GPU share and window must fit together.
	var budget int64
	switch {
	case userUtil:
		util, err := strconv.ParseFloat(userUtilText, 64)
		if err != nil || !haveGPU {
			return nil, ""
		}
		budget = int64(util * float64(total))
	case haveGPU:
		budget = free - freeMargin
	}
	if haveGPU && budget > 0 && fixed+perToken*ctx > budget {
		fit := (budget - fixed) / perToken
		fit -= fit % 1024
		if fit >= minAutoContext {
			ctx = fit
		}
	}
	if !userLen {
		if ctx > native {
			if !extend {
				ctx = native
			} else {
				factor := math.Ceil(float64(ctx)/float64(native)*100) / 100
				scaling := map[string]any{
					"rope_type": "yarn", "factor": factor,
					"original_max_position_embeddings": native,
				}
				fields := map[string]any{"rope_scaling": scaling, "max_position_embeddings": int64(math.Ceil(float64(native) * factor))}
				if cfg.TextConfig != nil {
					fields = map[string]any{"text_config": fields}
				}
				if raw, err := json.Marshal(fields); err == nil {
					overrides = string(raw)
				}
			}
		}
		extra = append(extra, "--max-model-len", strconv.FormatInt(ctx, 10))
		if overrides != "" && !userOverrides {
			extra = append(extra, "--hf-overrides", overrides)
		}
	}
	if !userUtil && !userKV && haveGPU {
		utilCtx := ctx
		if userLen {
			if v, _ := launchFlagValue(args, "--max-model-len"); v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
					utilCtx = n
				}
			}
		}
		need := fixed + perToken*utilCtx
		util := math.Ceil(float64(need)/float64(total)*100) / 100
		// The cap rounds down so the request never exceeds what is free.
		capUtil := math.Floor(math.Min(maxAutoUtil, float64(free-freeMargin)/float64(total))*100) / 100
		util = math.Max(minAutoUtil, math.Min(util, capUtil))
		if util >= minAutoUtil {
			extra = append(extra, "--gpu-memory-utilization", fmt.Sprintf("%.2f", util))
		}
	}
	if len(extra) == 0 {
		return nil, ""
	}
	return extra, fmt.Sprintf("native window %d, target %d, weights %.1f GiB, GPU free %.1f GiB", native, target, float64(weights)/float64(gib), float64(free)/float64(gib))
}

// toolCallParsers maps a model's config.json model_type to the vLLM tool-call
// parser that understands its tool-call format. Only families whose parser has
// been run against a real model here are listed; others keep vLLM's default,
// where a request that sets tool_choice "auto" is rejected, and can name a
// parser in the launch arguments.
var toolCallParsers = map[string]string{
	"qwen2":        "hermes",
	"qwen2_moe":    "hermes",
	"qwen3":        "hermes",
	"qwen3_moe":    "hermes",
	"qwen2_vl":     "hermes",
	"qwen2_5_vl":   "hermes",
	"qwen3_vl":     "hermes",
	"qwen3_vl_moe": "hermes",
}

// planToolCalling returns the arguments that let clients send tool_choice
// "auto", which agents such as Hermes do on every request. vLLM answers HTTP 400
// unless both --enable-auto-tool-choice and --tool-call-parser are set. A flag
// the user already supplied is left as given.
func planToolCalling(args []string, hubDir string) []string {
	model, ok := launchFlagValue(args, "--model")
	if !ok || model == "" {
		return nil
	}
	_, hasParser := launchFlagValue(args, "--tool-call-parser")
	_, hasEnable := launchFlagValue(args, "--enable-auto-tool-choice")
	if hasParser && hasEnable {
		return nil
	}
	var extra []string
	if !hasParser {
		cfg, _, cached := readHFModel(hubDir, model)
		parser := toolCallParsers[cfg.ModelType]
		if !cached || parser == "" {
			return nil
		}
		extra = append(extra, "--tool-call-parser", parser)
	}
	if !hasEnable {
		extra = append(extra, "--enable-auto-tool-choice")
	}
	return extra
}
