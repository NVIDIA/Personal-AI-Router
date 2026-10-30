// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type modelOptConfig struct {
	Producer struct {
		Name string `json:"name"`
	} `json:"producer"`
	Quantization struct {
		QuantAlgo        string `json:"quant_algo"`
		KVCacheQuantAlgo string `json:"kv_cache_quant_algo"`
	} `json:"quantization"`
}

func planMetalKVCache(args []string, hubDir, goos string) (extra []string, note string) {
	if goos != "darwin" {
		return nil, ""
	}
	if _, set := launchFlagValue(args, "--kv-cache-dtype"); set {
		return nil, ""
	}
	model, ok := launchFlagValue(args, "--model")
	if !ok || model == "" {
		return nil, ""
	}
	snapshot, cfg, _, cached := findHFSnapshot(hubDir, model)
	if !cached || modelOptFP8Weights(snapshot) || !quantizedKVCache(snapshot) {
		return nil, ""
	}
	dtype := metalDenseKVCacheDtype(cfg)
	if dtype == "" {
		return nil, ""
	}
	return []string{"--kv-cache-dtype", dtype}, "Metal requires the KV cache to use the model dtype " + dtype
}

func validateMetalModel(args []string, hubDir, goos string) error {
	if goos != "darwin" {
		return nil
	}
	model, ok := launchFlagValue(args, "--model")
	if !ok || model == "" {
		return nil
	}
	snapshot, _, _, cached := findHFSnapshot(hubDir, model)
	if !cached || !modelOptFP8Weights(snapshot) {
		return nil
	}
	return fmt.Errorf("model %q uses NVIDIA ModelOpt FP8 weights, which vLLM Metal cannot load on macOS; use a BF16, FP16, or MLX-compatible quantized checkpoint", model)
}

func metalDenseKVCacheDtype(cfg hfModelConfig) string {
	text := cfg
	if cfg.TextConfig != nil {
		text = *cfg.TextConfig
	}
	for _, value := range []string{text.Dtype, text.TorchDtype, cfg.Dtype, cfg.TorchDtype} {
		switch strings.TrimPrefix(strings.ToLower(value), "torch.") {
		case "bfloat16", "bf16":
			return "bfloat16"
		case "float16", "fp16", "half":
			return "float16"
		}
	}
	return ""
}

func quantizedKVCache(snapshot string) bool {
	if modelOpt, ok := readModelOptConfig(snapshot); ok && kvAlgorithmSet(modelOpt.Quantization.KVCacheQuantAlgo) {
		return true
	}
	var config struct {
		Quantization struct {
			KVCacheQuantAlgo string          `json:"kv_cache_quant_algo"`
			KVCacheScheme    json.RawMessage `json:"kv_cache_scheme"`
		} `json:"quantization_config"`
	}
	if !readJSONFile(filepath.Join(snapshot, "config.json"), &config) {
		return false
	}
	scheme := strings.TrimSpace(string(config.Quantization.KVCacheScheme))
	return kvAlgorithmSet(config.Quantization.KVCacheQuantAlgo) || (scheme != "" && scheme != "null")
}

func modelOptFP8Weights(snapshot string) bool {
	modelOpt, ok := readModelOptConfig(snapshot)
	return ok && strings.EqualFold(modelOpt.Producer.Name, "modelopt") && strings.EqualFold(modelOpt.Quantization.QuantAlgo, "fp8")
}

func readModelOptConfig(snapshot string) (modelOptConfig, bool) {
	var config modelOptConfig
	return config, readJSONFile(filepath.Join(snapshot, "hf_quant_config.json"), &config)
}

func kvAlgorithmSet(algorithm string) bool {
	return algorithm != "" && !strings.EqualFold(algorithm, "none")
}

func readJSONFile(path string, into any) bool {
	raw, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(raw, into) == nil
}
