// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// hfQuantConfigFile is where NVIDIA ModelOpt checkpoints declare their
// quantization; vLLM reads it beside config.json.
const hfQuantConfigFile = "hf_quant_config.json"

// checkpointQuantization is the part of config.json and hf_quant_config.json
// that says how a checkpoint's weights and KV cache are stored.
type checkpointQuantization struct {
	ModelType          string `json:"model_type"`
	QuantizationConfig *struct {
		QuantMethod  string `json:"quant_method"`
		QuantAlgo    string `json:"quant_algo"`
		ConfigGroups map[string]struct {
			Weights *floatScheme `json:"weights"`
		} `json:"config_groups"`
		KVCacheScheme *floatScheme `json:"kv_cache_scheme"`
	} `json:"quantization_config"`
	// Quantization is ModelOpt's hf_quant_config.json body.
	Quantization *struct {
		QuantAlgo        string `json:"quant_algo"`
		KVCacheQuantAlgo string `json:"kv_cache_quant_algo"`
	} `json:"quantization"`
}

// floatScheme is a compressed-tensors scheme: FP8 is type float, 8 bits.
type floatScheme struct {
	Type    string `json:"type"`
	NumBits int    `json:"num_bits"`
}

func (s *floatScheme) fp8() bool { return s != nil && s.Type == "float" && s.NumBits == 8 }

// weightFormat returns "fp8" for a checkpoint whose weights or KV cache vLLM
// would load as FP8, and its model type. It reads the three ways checkpoints
// say so: quant_method fp8 (or fbgemm_fp8) in config.json, a compressed-tensors
// float 8-bit scheme, and ModelOpt's FP8 quant_algo or kv_cache_quant_algo,
// in config.json or hf_quant_config.json. hfQuant is nil when there is none.
func weightFormat(config, hfQuant []byte) (format, modelType string, err error) {
	var cfg checkpointQuantization
	if err := json.Unmarshal(config, &cfg); err != nil {
		return "", "", fmt.Errorf("read config.json: %w", err)
	}
	if len(hfQuant) > 0 {
		var modelopt checkpointQuantization
		if err := json.Unmarshal(hfQuant, &modelopt); err != nil {
			return "", "", fmt.Errorf("read %s: %w", hfQuantConfigFile, err)
		}
		cfg.Quantization = modelopt.Quantization
	}
	isFP8 := func(algo string) bool { return strings.Contains(strings.ToUpper(algo), "FP8") }
	if q := cfg.Quantization; q != nil && (isFP8(q.QuantAlgo) || isFP8(q.KVCacheQuantAlgo)) {
		return weightFormatFP8, cfg.ModelType, nil
	}
	if q := cfg.QuantizationConfig; q != nil {
		switch strings.ToLower(q.QuantMethod) {
		case "fp8", "fbgemm_fp8":
			return weightFormatFP8, cfg.ModelType, nil
		case "modelopt":
			if isFP8(q.QuantAlgo) {
				return weightFormatFP8, cfg.ModelType, nil
			}
		case "compressed-tensors":
			if q.KVCacheScheme.fp8() {
				return weightFormatFP8, cfg.ModelType, nil
			}
			for _, group := range q.ConfigGroups {
				if group.Weights.fp8() {
					return weightFormatFP8, cfg.ModelType, nil
				}
			}
		}
	}
	return "", cfg.ModelType, nil
}

// refuses reports whether the rule refuses a checkpoint of this format and
// model type.
func (u *UnsupportedWeights) refuses(format, modelType string) bool {
	return u != nil && format != "" && slices.Contains(u.Formats, format) && !slices.Contains(u.ExceptModelTypes, modelType)
}

// checkSnapshotWeights refuses the model in snapshot when rule names its
// format. A snapshot whose config cannot be read is let through: the engine
// then reports its own error.
func checkSnapshotWeights(rule *UnsupportedWeights, model, snapshot string) error {
	if rule == nil {
		return nil
	}
	config, err := os.ReadFile(filepath.Join(snapshot, "config.json"))
	if err != nil {
		return nil
	}
	hfQuant, err := os.ReadFile(filepath.Join(snapshot, hfQuantConfigFile))
	if err != nil {
		hfQuant = nil
	}
	return checkWeights(rule, model, config, hfQuant)
}

func checkWeights(rule *UnsupportedWeights, model string, config, hfQuant []byte) error {
	format, modelType, err := weightFormat(config, hfQuant)
	if err != nil || !rule.refuses(format, modelType) {
		return nil
	}
	return fmt.Errorf("%s is an FP8 model: %s", model, rule.Reason)
}

// checkLaunchWeights refuses a start whose model the platform's engine cannot
// load, reading the model at the revision vLLM loads, from the cache or, for a
// model this start would download, through fetch. A model whose files cannot
// be read is let through.
func checkLaunchWeights(rule *UnsupportedWeights, args []string, hubDir string, fetch hfModelFetcher) error {
	if rule == nil {
		return nil
	}
	model, ok := launchFlagValue(args, "--model")
	if !ok || model == "" {
		return nil
	}
	revision := hfRevision(args)
	snapshot, ok := hfSnapshotDir(hubDir, model, revision)
	if !ok {
		pending, fetched := fetch()
		if !fetched {
			return nil
		}
		snapshot = pending.snapshot
	}
	return checkSnapshotWeights(rule, model, snapshot)
}

// checkRepoWeights refuses a download the platform's engine could not load,
// from the repository's config.json and hf_quant_config.json at main. A
// repository whose config cannot be read is let through.
func (e *Executor) checkRepoWeights(ctx context.Context, rule *UnsupportedWeights, id string, files []string, env hfEnv) error {
	if rule == nil {
		return nil
	}
	config, err := e.hfGet(ctx, env.endpoint()+"/"+id+"/resolve/main/config.json", maxHFConfigBody, env)
	if err != nil {
		return nil
	}
	var hfQuant []byte
	if slices.Contains(files, hfQuantConfigFile) {
		if hfQuant, err = e.hfGet(ctx, env.endpoint()+"/"+id+"/resolve/main/"+hfQuantConfigFile, maxHFConfigBody, env); err != nil {
			hfQuant = nil
		}
	}
	return checkWeights(rule, id, config, hfQuant)
}

// unsupportedWeightsFor is the weight rule an engine's manifest declares for a
// target platform, or nil.
func (e *Executor) unsupportedWeightsFor(engine, platform, arch string) *UnsupportedWeights {
	m, ok := e.reg.Get(engine)
	if !ok {
		return nil
	}
	p, ok := m.Platforms[platform+"/"+arch]
	if !ok {
		return nil
	}
	return p.Runtime.UnsupportedWeights
}

// fp8NameRe matches FP8 as a token of a repository id, as in
// nvidia/Llama-3.1-8B-Instruct-FP8.
var fp8NameRe = regexp.MustCompile(`(?i)(^|[-_./])fp8($|[-_.])`)

// withoutUnsupportedWeights drops catalog entries a rule refuses. A listing
// carries no config, so FP8 is read from the repository's tags and name; a
// model type the rule excepts cannot be told from a listing, so those entries
// are dropped too and stay available to a pull by id.
func withoutUnsupportedWeights(models []CatalogModel, rule *UnsupportedWeights) []CatalogModel {
	if rule == nil || !slices.Contains(rule.Formats, weightFormatFP8) {
		return models
	}
	out := make([]CatalogModel, 0, len(models))
	for _, m := range models {
		if fp8NameRe.MatchString(m.ID) || slices.ContainsFunc(m.Tags, func(tag string) bool { return strings.EqualFold(tag, weightFormatFP8) }) {
			continue
		}
		out = append(out, m)
	}
	return out
}
