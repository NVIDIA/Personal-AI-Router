// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	modelOptFP8KVOnly      = `{"producer":{"name":"modelopt"},"quantization":{"kv_cache_quant_algo":"FP8"}}`
	modelOptFP8WeightsJSON = `{"producer":{"name":"modelopt"},"quantization":{"quant_algo":"FP8","kv_cache_quant_algo":"FP8"}}`
)

func cachedModelWithFile(t *testing.T, id, config, name, contents string) string {
	t.Helper()
	cache := cachedModel(t, id, config, 1_000_000)
	snapshot, _, _, ok := findHFSnapshot(cache, id)
	if !ok {
		t.Fatal("cached model has no complete snapshot")
	}
	if err := os.WriteFile(filepath.Join(snapshot, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return cache
}

func TestPlanMetalKVCache_UsesBFloat16ForModelOptFP8KVCache(t *testing.T) {
	cache := cachedModelWithFile(t, "org/Llama-KV-FP8", llama8BFP8Config, "hf_quant_config.json", modelOptFP8KVOnly)

	extra, note := planMetalKVCache([]string{"serve", "--model", "org/Llama-KV-FP8"}, cache, "darwin")

	if got := flagOf(t, extra, "--kv-cache-dtype"); got != "bfloat16" {
		t.Fatalf("kv-cache-dtype = %q, want bfloat16: %v", got, extra)
	}
	if !strings.Contains(note, "model dtype bfloat16") {
		t.Fatalf("note = %q", note)
	}
}

func TestPlanMetalKVCache_UsesFloat16ForCompressedKVCache(t *testing.T) {
	config := `{"max_position_embeddings":40960,"num_hidden_layers":32,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":128,"torch_dtype":"float16","quantization_config":{"quant_method":"compressed-tensors","kv_cache_scheme":{"type":"float","num_bits":8}}}`
	cache := cachedModel(t, "org/compressed", config, 1_000_000)

	extra, _ := planMetalKVCache([]string{"--model", "org/compressed"}, cache, "darwin")

	if got := flagOf(t, extra, "--kv-cache-dtype"); got != "float16" {
		t.Fatalf("kv-cache-dtype = %q, want float16: %v", got, extra)
	}
}

func TestPlanMetalKVCache_PreservesAnExplicitUserChoice(t *testing.T) {
	cache := cachedModelWithFile(t, "org/Llama-KV-FP8", llama8BFP8Config, "hf_quant_config.json", modelOptFP8KVOnly)
	args := []string{"--model", "org/Llama-KV-FP8", "--kv-cache-dtype=auto"}

	extra, _ := planMetalKVCache(args, cache, "darwin")

	if len(extra) != 0 {
		t.Fatalf("the user's KV cache choice must stand untouched: %v", extra)
	}
}

func TestPlanMetalKVCache_AddsNothingWithoutAPlatformConflict(t *testing.T) {
	quantized := cachedModelWithFile(t, "org/Llama-KV-FP8", llama8BFP8Config, "hf_quant_config.json", modelOptFP8KVOnly)
	plain := cachedModel(t, "org/plain", llama8BFP8Config, 1_000_000)
	unsupportedDtype := cachedModelWithFile(t, "org/fp32-kv", strings.Replace(llama8BFP8Config, "bfloat16", "float32", 1), "hf_quant_config.json", modelOptFP8KVOnly)

	cases := []struct {
		name  string
		args  []string
		cache string
		goos  string
	}{
		{"non-Metal platform", []string{"--model", "org/Llama-KV-FP8"}, quantized, "linux"},
		{"dense checkpoint", []string{"--model", "org/plain"}, plain, "darwin"},
		{"dtype unsupported by Metal", []string{"--model", "org/fp32-kv"}, unsupportedDtype, "darwin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra, _ := planMetalKVCache(tc.args, tc.cache, tc.goos)
			if len(extra) != 0 {
				t.Fatalf("got %v, want no automatic KV cache override", extra)
			}
		})
	}
}

func TestPlanMetalKVCache_SkipsModelOptFP8Weights(t *testing.T) {
	cache := cachedModelWithFile(t, "nvidia/Llama-FP8", llama8BFP8Config, "hf_quant_config.json", modelOptFP8WeightsJSON)

	extra, _ := planMetalKVCache([]string{"--model", "nvidia/Llama-FP8"}, cache, "darwin")

	if len(extra) != 0 {
		t.Fatalf("an unsupported checkpoint must not receive a misleading KV-cache fallback: %v", extra)
	}
}

func TestValidateMetalModel_RejectsModelOptFP8WeightsOnMacOS(t *testing.T) {
	cache := cachedModelWithFile(t, "nvidia/Llama-FP8", llama8BFP8Config, "hf_quant_config.json", modelOptFP8WeightsJSON)

	err := validateMetalModel([]string{"--model", "nvidia/Llama-FP8"}, cache, "darwin")

	if err == nil {
		t.Fatal("ModelOpt FP8 weights must be rejected before vLLM Metal starts")
	}
	for _, want := range []string{"nvidia/Llama-FP8", "ModelOpt FP8", "BF16", "MLX-compatible"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestValidateMetalModel_AllowsModelOptFP8WeightsOutsideMacOS(t *testing.T) {
	cache := cachedModelWithFile(t, "nvidia/Llama-FP8", llama8BFP8Config, "hf_quant_config.json", modelOptFP8WeightsJSON)

	err := validateMetalModel([]string{"--model", "nvidia/Llama-FP8"}, cache, "linux")

	if err != nil {
		t.Fatalf("non-Metal platforms keep their native FP8 support: %v", err)
	}
}
