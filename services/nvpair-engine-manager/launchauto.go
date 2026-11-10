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
	"slices"
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

// gpuMemory reports memory only when the launch has one unambiguous GPU.
// It is a variable so tests can supply a machine.
var gpuMemory = nvidiaSMIMemory

func nvidiaSMIMemory(env map[string]string) (total, free int64, ok bool) {
	// CUDA ordinals can differ from nvidia-smi's physical-device order. Leave
	// memory sizing to the engine when it has an explicit device selection.
	if _, selected := os.LookupEnv("CUDA_VISIBLE_DEVICES"); selected {
		return 0, 0, false
	}
	for name := range env {
		if environmentKey(name) == "CUDA_VISIBLE_DEVICES" {
			return 0, 0, false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=memory.total,memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, false
	}
	return parseGPUMemory(string(out), systemMemory)
}

// parseGPUMemory reads a single GPU's memory.total,memory.free query, in MiB.
// Multiple devices are ambiguous without reproducing CUDA's enumeration, so
// the engine handles their memory sizing. On a unified-memory machine such as
// DGX Spark the GPU shares system RAM and nvidia-smi reports [N/A]; system
// memory then stands in, as it does in nvpair-node-info and vLLM itself.
func parseGPUMemory(out string, system func() (total, available int64, ok bool)) (total, free int64, ok bool) {
	line := strings.TrimSpace(out)
	if strings.ContainsAny(line, "\r\n") {
		return 0, 0, false
	}
	parts := strings.Split(line, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	if nvidiaSMINotApplicable(parts[0]) {
		return system()
	}
	t, errT := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	f, errF := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if errT != nil || errF != nil || t <= 0 {
		return 0, 0, false
	}
	return t * 1024 * 1024, f * 1024 * 1024, true
}

// nvidiaSMINotApplicable reports an nvidia-smi field that holds no number, such
// as the [N/A] or [Not Supported] a unified-memory GPU gives for its memory.
func nvidiaSMINotApplicable(field string) bool {
	value := strings.ToLower(strings.Trim(strings.TrimSpace(field), "[]"))
	return value == "n/a" || value == "not supported"
}

// systemMemory reports total and available system memory from /proc/meminfo.
func systemMemory() (total, available int64, ok bool) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	return parseMeminfo(string(raw))
}

// parseMeminfo reads MemTotal and MemAvailable, which /proc/meminfo gives in kB.
// Available rather than free counts the page cache the kernel can reclaim.
func parseMeminfo(text string) (total, available int64, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			available = kb * 1024
		}
	}
	return total, available, total > 0 && available > 0
}

// applyAutoLaunch appends the arguments the manifest's auto_launch asks for.
// It runs after the user's launch has been validated and never changes an
// argument the user supplied, so anything set in the engine settings wins.
//
// hub is the Hugging Face hub cache the engine will use. fetch, when not nil,
// prepares the model the launch serves when it is not in the cache yet, so a
// first start that downloads it is configured for it too. Sizing and the
// tool-call parser both read it, so it must return the same result on every
// call; a start builds it with sync.OnceValues.
func applyAutoLaunch(ctx context.Context, engine string, rt Runtime, command launchCommand, hub string, fetch hfModelFetcher) launchCommand {
	if rt.AutoLaunch == nil || rt.AutoLaunch.Kind != autoLaunchHFContext {
		return command
	}
	extra, note := planHFContext(command.Args, hub, int64(rt.AutoLaunch.ContextTokens), fetch, command.Env)
	if tools, parser := planToolCalling(ctx, command.Args, hub, command.Bin, fetch); len(tools) > 0 {
		extra = append(extra, tools...)
		note = strings.TrimPrefix(note+"; tool calls parsed with "+parser, "; ")
	}
	if seqs := planMaxNumSeqs(command.Args, rt.AutoLaunch.MaxNumSeqs); len(seqs) > 0 {
		extra = append(extra, seqs...)
		note = strings.TrimPrefix(note+"; at most "+seqs[1]+" requests at a time", "; ")
	}
	if len(extra) == 0 {
		return command
	}
	slog.Info("engine launch adjusted for the model and GPU", "engine", engine, "added_args", extra, "reason", note)
	command.Args = slices.Concat(command.Args, extra)
	return command
}

// planMaxNumSeqs returns --max-num-seqs with the manifest's limit, unless the
// limit is 0 or the user already set the flag. It does not depend on the model,
// so it applies to every launch, sharded ones included.
func planMaxNumSeqs(args []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	if _, set := launchFlagValue(args, "--max-num-seqs"); set {
		return nil
	}
	return []string{"--max-num-seqs", strconv.Itoa(limit)}
}

// launchFlagValue returns the value of a flag in either "--flag value" or
// "--flag=value" form, and whether the flag is present at all. vLLM accepts
// underscores and dashes in option names; values are always left literal.
// When the flag is repeated, the last value wins, as it does for vLLM's parser.
// Arguments after a "--" terminator are not options, so they are not read.
func launchFlagValue(args []string, flag string) (value string, found bool) {
	flag = strings.ReplaceAll(flag, "_", "-")
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, inlineValue, hasInlineValue := strings.Cut(arg, "=")
		if strings.ReplaceAll(name, "_", "-") != flag {
			continue
		}
		value, found = inlineValue, true
		if !hasInlineValue && i+1 < len(args) {
			value = args[i+1]
			i++ // its value
		}
	}
	return value, found
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

// text is the language model's part of the config: text_config for a
// vision-language model, the config itself otherwise.
func (c hfModelConfig) text() hfModelConfig {
	if c.TextConfig != nil {
		return *c.TextConfig
	}
	return c
}

// kvShape returns what KV cache sizing needs from a text config, and whether
// all of it is there.
func (c hfModelConfig) kvShape() (native, layers, kvHeads, headDim int64, ok bool) {
	native, layers = c.MaxPositionEmbeddings, c.NumHiddenLayers
	kvHeads = c.NumKeyValueHeads
	if kvHeads == 0 {
		kvHeads = c.NumAttentionHeads
	}
	headDim = c.HeadDim
	if headDim == 0 && c.NumAttentionHeads > 0 {
		headDim = c.HiddenSize / c.NumAttentionHeads
	}
	return native, layers, kvHeads, headDim, native > 0 && layers > 0 && kvHeads > 0 && headDim > 0
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

// hfRevision is the revision vLLM loads for the model in args: its --revision,
// or main.
func hfRevision(args []string) string {
	if revision, ok := launchFlagValue(args, "--revision"); ok && revision != "" {
		return revision
	}
	return "main"
}

// hfSnapshotDir returns the snapshot directory the hub cache holds for a model
// at a revision, resolved the way huggingface_hub resolves it: a branch or tag
// is read from refs/, a commit hash names its snapshot directly, and no other
// snapshot is taken in its place.
func hfSnapshotDir(hubDir, id, revision string) (string, bool) {
	repo := filepath.Join(hubDir, "models--"+strings.ReplaceAll(id, "/", "--"))
	commit := revision
	if ref, err := os.ReadFile(filepath.Join(repo, "refs", revision)); err == nil {
		commit = strings.TrimSpace(string(ref))
	}
	snapshots := filepath.Join(repo, "snapshots")
	dir := filepath.Join(snapshots, commit)
	if rel, err := filepath.Rel(snapshots, dir); err != nil || rel != commit || strings.ContainsAny(commit, `/\`) {
		return "", false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", false
	}
	return dir, true
}

// hfLaunchModel is the model a launch serves, as auto-configuration reads it.
type hfLaunchModel struct {
	snapshot string
	cfg      hfModelConfig
	// weights is the bytes of the weights vLLM loads.
	weights int64
	// cached is false for a model this start downloads.
	cached bool
}

// findLaunchModel reads the model args name at the revision vLLM will load:
// from its complete snapshot in the cache, or, when that revision is not fully
// cached, from fetch, which prepares a model this start is about to download.
// It reports false when neither has the model's config, and auto-configuration
// then leaves the model alone rather than read another revision's.
func findLaunchModel(args []string, hubDir string, fetch hfModelFetcher) (hfLaunchModel, bool) {
	model, ok := launchFlagValue(args, "--model")
	if !ok || model == "" {
		return hfLaunchModel{}, false
	}
	revision := hfRevision(args)
	if dir, ok := hfSnapshotDir(hubDir, model, revision); ok {
		if _, weights, complete := completeHFSnapshot(dir); complete {
			cfg, ok := readHFConfig(dir)
			return hfLaunchModel{snapshot: dir, cfg: cfg, weights: weights, cached: true}, ok
		}
	}
	if fetch == nil {
		return hfLaunchModel{}, false
	}
	pending, ok := fetch()
	if !ok {
		return hfLaunchModel{}, false
	}
	cfg, ok := readHFConfig(pending.snapshot)
	return hfLaunchModel{snapshot: pending.snapshot, cfg: cfg, weights: pending.weights}, ok
}

func readHFConfig(snapshot string) (hfModelConfig, bool) {
	var cfg hfModelConfig
	raw, err := os.ReadFile(filepath.Join(snapshot, "config.json"))
	if err != nil || json.Unmarshal(raw, &cfg) != nil {
		return cfg, false
	}
	return cfg, true
}

// autoGPUShare returns the --gpu-memory-utilization for a launch that needs
// need bytes on a GPU with total and free bytes, and whether to set one. The
// share is never below minAutoUtil, so a small model still gets room to work,
// but what is free, less freeMargin for the desktop and other engines, caps it
// before that floor: claiming more than is free fails the launch. The cap
// rounds down so the request never exceeds what is free. Nothing is set when
// nothing is free beyond the margin; vLLM then reports the shortfall itself.
func autoGPUShare(need, total, free int64) (float64, bool) {
	if total <= 0 {
		return 0, false
	}
	capShare := math.Floor(math.Min(maxAutoUtil, float64(free-freeMargin)/float64(total))*100) / 100
	if capShare <= 0 {
		return 0, false
	}
	share := math.Max(minAutoUtil, math.Ceil(float64(need)/float64(total)*100)/100)
	return math.Min(share, capShare), true
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
//
// The model is read at the revision vLLM loads (see findLaunchModel). One that
// is not in the cache yet is read through fetch, when given, since vLLM
// downloads it on this start. Without its size and config, only the memory
// share is bounded.
func planHFContext(args []string, hubDir string, target int64, fetch hfModelFetcher, env map[string]string) (extra []string, note string) {
	if _, ok := launchFlagValue(args, "--tensor-parallel-size"); ok {
		return nil, ""
	}
	if _, ok := launchFlagValue(args, "--pipeline-parallel-size"); ok {
		return nil, ""
	}
	if model, ok := launchFlagValue(args, "--model"); !ok || model == "" {
		return nil, ""
	}
	_, userLen := launchFlagValue(args, "--max-model-len")
	_, userOverrides := launchFlagValue(args, "--hf-overrides")
	userUtilText, userUtil := launchFlagValue(args, "--gpu-memory-utilization")
	_, userKV := launchFlagValue(args, "--kv-cache-memory-bytes")
	_, userDtype := launchFlagValue(args, "--dtype")
	_, userCacheDtype := launchFlagValue(args, "--kv-cache-dtype")
	if userLen && userUtil {
		return nil, ""
	}

	// A dtype override changes how checkpoint bytes become runtime weights
	// and cache storage. Let vLLM size those combinations instead of estimating
	// them from the repository's dtype and on-disk size.
	var total, free int64
	var haveGPU bool
	if !userDtype && !userCacheDtype {
		total, free, haveGPU = gpuMemory(env)
	}
	found, known := findLaunchModel(args, hubDir, fetch)
	if !known {
		// Not downloaded yet and Hugging Face could not answer, so its size and
		// limits are unknown. Give it what is free, less the margin left for the
		// desktop and other engines, under the same ceiling as a sized launch.
		// vLLM's own default would claim memory that is already in use.
		if haveGPU && !userUtil && !userKV {
			if util, ok := autoGPUShare(total, total, free); ok {
				return []string{"--gpu-memory-utilization", fmt.Sprintf("%.2f", util)}, "model not in cache and not looked up"
			}
		}
		return nil, ""
	}
	source := ""
	if !found.cached {
		source = "model not in cache, sized from Hugging Face"
	}
	cfg, weights := found.cfg, found.weights
	text := cfg.text()
	native, layers, kvHeads, headDim, ok := text.kvShape()
	if !ok {
		return nil, ""
	}
	perToken := 2 * layers * kvHeads * headDim * text.dtypeBytes()
	fixed := int64(float64(weights)*weightsSlack) + vllmOverhead

	ctx := target
	var overrides string
	extend := native < target && !text.ropeExtended() && !userOverrides
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
		if overrides != "" {
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
		if util, ok := autoGPUShare(fixed+perToken*utilCtx, total, free); ok {
			extra = append(extra, "--gpu-memory-utilization", fmt.Sprintf("%.2f", util))
		}
	}
	if len(extra) == 0 {
		return nil, ""
	}
	note = fmt.Sprintf("native window %d, target %d, weights %.1f GiB, GPU free %.1f GiB", native, target, float64(weights)/float64(gib), float64(free)/float64(gib))
	if source != "" {
		note = source + "; " + note
	}
	return extra, note
}

// launchChatFormat returns where vLLM will read this launch's chat format: the
// directory of the tokenizer it loads, and the --chat-template it is given, if
// any. By default that is the model's own snapshot. --tokenizer names another
// repository or an absolute directory, and --tokenizer-revision a revision of
// it, which defaults to the model's revision as in vLLM. ok is false when what
// the launch names cannot be read here: a tokenizer repository or revision that
// is not cached, or a chat template given as a relative path, which vLLM would
// resolve against its own working directory. A wrong guess picks a parser that
// cannot read the model's tool calls, so none is chosen instead.
func launchChatFormat(args []string, hubDir, modelSnapshot string) (tokenizer, template string, ok bool) {
	tokenizer = modelSnapshot
	name, setName := launchFlagValue(args, "--tokenizer")
	revision, setRevision := launchFlagValue(args, "--tokenizer-revision")
	if (setName && name != "") || (setRevision && revision != "") {
		model, _ := launchFlagValue(args, "--model")
		if name == "" {
			name = model
		}
		if revision == "" {
			revision = hfRevision(args)
		}
		switch {
		case filepath.IsAbs(name):
			tokenizer = name
		case name == model && revision == hfRevision(args):
			tokenizer = modelSnapshot
		case hfModelIDRe.MatchString(name):
			if tokenizer, ok = hfSnapshotDir(hubDir, name, revision); !ok {
				return "", "", false
			}
		default:
			return "", "", false
		}
	}
	if value, set := launchFlagValue(args, "--chat-template"); set && value != "" {
		literal := strings.ContainsAny(value, "{}\n")
		if !literal && !filepath.IsAbs(value) {
			return "", "", false
		}
		template = value
	}
	return tokenizer, template, true
}

// planToolCalling returns the arguments that let clients send tool_choice
// "auto", which agent clients do on every request. vLLM answers HTTP 400
// unless both --enable-auto-tool-choice and --tool-call-parser are set. The
// parser is the one the installed vLLM provides for the model's own tool-call
// format (see detectToolParser), so no list of model families is kept here. A
// flag the user already supplied is left as given. When no parser fits, nothing
// is added, and vLLM answers requests that ask for tool calls with its own error.
//
// The model's tokenizer and chat template are read at the revision vLLM loads.
// For a model this start downloads, fetch puts them in the cache first, so the
// first start is configured the same way as every later one.
func planToolCalling(ctx context.Context, args []string, hubDir, bin string, fetch hfModelFetcher) (extra []string, parser string) {
	model, ok := launchFlagValue(args, "--model")
	if !ok || model == "" {
		return nil, ""
	}
	userParser, hasParser := launchFlagValue(args, "--tool-call-parser")
	_, hasEnable := launchFlagValue(args, "--enable-auto-tool-choice")
	if hasParser && hasEnable {
		return nil, userParser
	}
	if hasParser {
		return []string{"--enable-auto-tool-choice"}, userParser
	}
	found, ok := findLaunchModel(args, hubDir, fetch)
	if !ok {
		return nil, ""
	}
	tokenizer, template, ok := launchChatFormat(args, hubDir, found.snapshot)
	if !ok {
		slog.Info("the tokenizer or chat template this launch names cannot be read here, so no tool-call parser is chosen; vLLM will refuse requests that ask for tool calls", "model", model)
		return nil, ""
	}
	parser, matched, err := detectToolParser(ctx, bin, tokenizer, template)
	if err != nil {
		slog.Warn("could not work out the model's tool-call format; vLLM will refuse requests that ask for tool calls", "model", model, "err", err)
		return nil, ""
	}
	if parser == "" {
		slog.Info("no vLLM tool-call parser reads this model's tool calls; vLLM will refuse requests that ask for tool calls", "model", model)
		return nil, ""
	}
	slog.Info("vLLM tool-call parser chosen from the model's chat template", "model", model, "parser", parser, "matched", matched)
	extra = []string{"--tool-call-parser", parser}
	if !hasEnable {
		extra = append(extra, "--enable-auto-tool-choice")
	}
	return extra, parser
}
