// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const qwen8BConfig = `{"max_position_embeddings":40960,"num_hidden_layers":36,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":128,"torch_dtype":"bfloat16"}`

// cachedModel writes a repository whose weights file reports the given size
// without using disk (a sparse file).
func cachedModel(t *testing.T, id, config string, weightBytes int64) string {
	t.Helper()
	cache := t.TempDir()
	cachedModelAt(t, cache, id, config, weightBytes)
	return cache
}

func cachedModelAt(t *testing.T, cache, id, config string, weightBytes int64) {
	t.Helper()
	repo := writeHFRepo(t, cache, id, llmFiles(map[string]string{"config.json": config, "model.safetensors": "x"}))
	if err := os.Truncate(filepath.Join(repo, "blobs", "blob-model.safetensors"), weightBytes); err != nil {
		t.Fatal(err)
	}
}

func withGPU(t *testing.T, totalGiB, freeGiB int64) {
	t.Helper()
	old := gpuMemory
	gpuMemory = func() (int64, int64, bool) { return totalGiB * gib, freeGiB * gib, true }
	t.Cleanup(func() { gpuMemory = old })
}

func withoutGPU(t *testing.T) {
	t.Helper()
	old := gpuMemory
	gpuMemory = func() (int64, int64, bool) { return 0, 0, false }
	t.Cleanup(func() { gpuMemory = old })
}

func flagOf(t *testing.T, args []string, flag string) string {
	t.Helper()
	v, _ := launchFlagValue(args, flag)
	return v
}

func utilOf(t *testing.T, args []string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(flagOf(t, args, "--gpu-memory-utilization"), 64)
	if err != nil {
		t.Fatalf("no usable --gpu-memory-utilization in %v", args)
	}
	return v
}

func TestAutoContextExtendsAShortModelToTheTarget(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	extra, _ := planHFContext([]string{"serve", "--model", "Qwen/Qwen3-8B"}, cache, 65536)

	if got := flagOf(t, extra, "--max-model-len"); got != "65536" {
		t.Fatalf("max-model-len = %q, want 65536 (the model alone stops at 40960): %v", got, extra)
	}
	var overrides struct {
		RopeScaling struct {
			Type     string  `json:"rope_type"`
			Factor   float64 `json:"factor"`
			Original int64   `json:"original_max_position_embeddings"`
		} `json:"rope_scaling"`
		MaxPositions int64 `json:"max_position_embeddings"`
	}
	if err := json.Unmarshal([]byte(flagOf(t, extra, "--hf-overrides")), &overrides); err != nil {
		t.Fatalf("hf-overrides is not valid JSON: %v (%v)", err, extra)
	}
	if overrides.RopeScaling.Type != "yarn" || overrides.RopeScaling.Original != 40960 || overrides.RopeScaling.Factor < 1.59 || overrides.MaxPositions < 65536 {
		t.Fatalf("overrides = %+v, want yarn over the 40960 native window reaching 65536", overrides)
	}
	// 16.4 GB of weights plus about 9.7 GB of KV cache for 64K plus overhead is
	// about 60% of a 48 GiB card; vLLM's own default would take 92%.
	if util := utilOf(t, extra); util < 0.55 || util > 0.7 {
		t.Fatalf("gpu-memory-utilization = %.2f, want about 0.6", util)
	}
}

func TestAutoContextCapsALongModelAtTheTarget(t *testing.T) {
	withGPU(t, 48, 48)
	cfg := strings.Replace(qwen8BConfig, "40960", "262144", 1)
	cache := cachedModel(t, "Qwen/Long", cfg, 16_400_000_000)
	extra, _ := planHFContext([]string{"--model", "Qwen/Long"}, cache, 65536)
	if got := flagOf(t, extra, "--max-model-len"); got != "65536" {
		t.Fatalf("max-model-len = %q, want the 65536 target, not the 262144 native window", got)
	}
	if slices.Contains(extra, "--hf-overrides") {
		t.Fatalf("a model that already supports the target needs no rope override: %v", extra)
	}
}

func TestAutoContextLeavesModelsWithRopeScalingAlone(t *testing.T) {
	withGPU(t, 48, 48)
	cfg := strings.Replace(qwen8BConfig, `"torch_dtype"`, `"rope_scaling":{"rope_type":"yarn","factor":4.0},"torch_dtype"`, 1)
	cache := cachedModel(t, "Qwen/Scaled", cfg, 16_400_000_000)
	extra, _ := planHFContext([]string{"--model", "Qwen/Scaled"}, cache, 131072)
	if slices.Contains(extra, "--hf-overrides") {
		t.Fatalf("must not override a model's own rope scaling: %v", extra)
	}
	if got := flagOf(t, extra, "--max-model-len"); got != "40960" {
		t.Fatalf("max-model-len = %q, want the model's own 40960", got)
	}
}

func TestAutoContextShrinksToWhatFitsInFreeMemory(t *testing.T) {
	withGPU(t, 48, 22)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, cache, 65536)

	n, err := strconv.ParseInt(flagOf(t, extra, "--max-model-len"), 10, 64)
	if err != nil || n < 4096 || n >= 65536 || n%1024 != 0 {
		t.Fatalf("max-model-len = %v, want a multiple of 1024 below 64K that fits 22 GiB free: %v", n, extra)
	}
	if slices.Contains(extra, "--hf-overrides") {
		t.Fatalf("a window inside the native limit needs no rope override: %v", extra)
	}
	if util := utilOf(t, extra); util > 22.0/48.0 {
		t.Fatalf("gpu-memory-utilization %.2f exceeds the free share %.2f", util, 22.0/48.0)
	}
}

func TestAutoContextNeverOverridesWhatTheUserSet(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)

	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--max-model-len=8192"}, cache, 65536)
	if slices.Contains(extra, "--max-model-len") || slices.Contains(extra, "--hf-overrides") {
		t.Fatalf("the user's context must stand untouched: %v", extra)
	}
	if util := utilOf(t, extra); util > 0.45 {
		t.Fatalf("an 8K window needs far less than %.2f of the GPU", util)
	}

	extra, _ = planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--gpu-memory-utilization", "0.5"}, cache, 65536)
	if slices.Contains(extra, "--gpu-memory-utilization") {
		t.Fatalf("the user's GPU share must stand untouched: %v", extra)
	}
	n, _ := strconv.ParseInt(flagOf(t, extra, "--max-model-len"), 10, 64)
	if n >= 65536 || n < 4096 {
		t.Fatalf("a 0.5 share (24 GiB) cannot hold 64K of Qwen3-8B; max-model-len = %d", n)
	}

	extra, _ = planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--max-model-len", "4096", "--gpu-memory-utilization", "0.3"}, cache, 65536)
	if len(extra) != 0 {
		t.Fatalf("with both set there is nothing to add: %v", extra)
	}
}

func TestAutoContextSkipsShardedLaunches(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	for _, flag := range []string{"--tensor-parallel-size", "--pipeline-parallel-size"} {
		if extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", flag, "2"}, cache, 65536); len(extra) != 0 {
			t.Fatalf("%s: sizing a sharded launch needs more than GPU 0; got %v", flag, extra)
		}
	}
}

func TestAutoContextForAModelNotInTheCache(t *testing.T) {
	withGPU(t, 48, 20)
	extra, _ := planHFContext([]string{"--model", "org/not-downloaded"}, t.TempDir(), 65536)
	if slices.Contains(extra, "--max-model-len") {
		t.Fatalf("an unknown model's limits are unknown: %v", extra)
	}
	if util := utilOf(t, extra); util > 19.0/48.0+0.001 {
		t.Fatalf("gpu-memory-utilization %.2f must leave a margin of the 20 GiB free", util)
	}
}

func TestAutoContextWithoutAGPUReading(t *testing.T) {
	withoutGPU(t)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, cache, 65536)
	if flagOf(t, extra, "--max-model-len") != "65536" {
		t.Fatalf("the window does not depend on the GPU reading: %v", extra)
	}
	if slices.Contains(extra, "--gpu-memory-utilization") {
		t.Fatalf("without a reading the GPU share is left to vLLM: %v", extra)
	}
}

func TestAutoContextWrapsOverridesForAMultimodalConfig(t *testing.T) {
	withGPU(t, 48, 48)
	cfg := `{"architectures":["Qwen3VLForConditionalGeneration"],"text_config":` + qwen8BConfig + `}`
	cache := cachedModel(t, "Qwen/VL", cfg, 16_400_000_000)
	extra, _ := planHFContext([]string{"--model", "Qwen/VL"}, cache, 65536)
	var overrides map[string]json.RawMessage
	if err := json.Unmarshal([]byte(flagOf(t, extra, "--hf-overrides")), &overrides); err != nil {
		t.Fatal(err)
	}
	if _, ok := overrides["text_config"]; !ok {
		t.Fatalf("a multimodal model's rope settings live under text_config: %v", extra)
	}
}

func TestApplyAutoLaunchAppendsAfterTheUsersArguments(t *testing.T) {
	withGPU(t, 48, 48)
	// models_dir/hub is the cache.
	parent := t.TempDir()
	cachedModelAt(t, filepath.Join(parent, "hub"), "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	rt := Runtime{AutoLaunch: &AutoLaunch{Kind: autoLaunchHFContext, ContextTokens: 65536}}
	in := launchCommand{Bin: "vllm", Args: []string{"serve", "--model", "Qwen/Qwen3-8B"}}
	out := applyAutoLaunch("vllm", rt, in, parent)
	if !slices.Equal(out.Args[:3], in.Args) || flagOf(t, out.Args, "--max-model-len") != "65536" {
		t.Fatalf("args = %v", out.Args)
	}
	if len(in.Args) != 3 {
		t.Fatalf("the caller's arguments were modified: %v", in.Args)
	}
	if got := applyAutoLaunch("vllm", Runtime{}, in, parent); !slices.Equal(got.Args, in.Args) {
		t.Fatalf("an engine without auto_launch must be untouched: %v", got.Args)
	}
}

func TestPlanToolCalling_UsesTheParserThatReadsTheModelsToolCalls(t *testing.T) {
	cache := cachedModel(t, "org/m", qwen8BConfig, 1_000_000)
	var probedSnapshot string
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		probedSnapshot = snapshot
		return toolParserProbe{Matches: []string{"ernie45", "granite4", "hermes", "qwen3_json"}}, nil
	})

	extra, parser := planToolCalling([]string{"--model", "org/m"}, cache, "/opt/vllm/venv/bin/vllm")

	if parser != "hermes" || flagOf(t, extra, "--tool-call-parser") != "hermes" || !slices.Contains(extra, "--enable-auto-tool-choice") {
		t.Fatalf("got %v (%q), want the hermes parser and auto tool choice", extra, parser)
	}
	if !strings.Contains(probedSnapshot, filepath.Join("models--org--m", "snapshots")) {
		t.Fatalf("probed %q, want the model's snapshot directory", probedSnapshot)
	}
}

func TestPlanToolCalling_PrefersTheModelsOwnResponseTemplate(t *testing.T) {
	cache := cachedModel(t, "org/m", qwen8BConfig, 1_000_000)
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		return toolParserProbe{HF: true, Matches: []string{"hermes"}}, nil
	})

	extra, _ := planToolCalling([]string{"--model", "org/m"}, cache, "vllm")

	if flagOf(t, extra, "--tool-call-parser") != "hf" {
		t.Fatalf("got %v, want the hf parser for a model that ships response_template", extra)
	}
}

func TestPlanToolCalling_AddsNothingWhenNoParserCanBeChosen(t *testing.T) {
	cache := cachedModel(t, "org/m", qwen8BConfig, 1_000_000)
	cases := map[string]func(bin, snapshot string) (toolParserProbe, error){
		"no parser reads the format": func(bin, snapshot string) (toolParserProbe, error) { return toolParserProbe{}, nil },
		"the probe fails":            func(bin, snapshot string) (toolParserProbe, error) { return toolParserProbe{}, errors.New("no python") },
	}
	for name, probe := range cases {
		withToolParserProbe(t, probe)
		if extra, parser := planToolCalling([]string{"--model", "org/m"}, cache, "vllm"); len(extra) != 0 || parser != "" {
			t.Errorf("%s: got %v (%q), want vLLM's defaults", name, extra, parser)
		}
	}

	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		t.Fatal("a model that is not downloaded has no template to probe")
		return toolParserProbe{}, nil
	})
	if extra, _ := planToolCalling([]string{"--model", "org/not-cached"}, t.TempDir(), "vllm"); len(extra) != 0 {
		t.Fatalf("an uncached model got %v", extra)
	}
}

func TestPlanToolCalling_KeepsTheParserTheUserChose(t *testing.T) {
	cache := cachedModel(t, "org/q", qwen8BConfig, 1_000_000)
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		t.Fatal("a parser the user named needs no probe")
		return toolParserProbe{}, nil
	})

	if extra, _ := planToolCalling([]string{"--model", "org/q", "--tool-call-parser", "qwen3_xml", "--enable-auto-tool-choice"}, cache, "vllm"); len(extra) != 0 {
		t.Fatalf("the user set both: %v", extra)
	}
	extra, parser := planToolCalling([]string{"--model", "org/q", "--tool-call-parser", "qwen3_xml"}, cache, "vllm")
	if parser != "qwen3_xml" || !slices.Equal(extra, []string{"--enable-auto-tool-choice"}) {
		t.Fatalf("got %v (%q), want only the enable flag beside the user's parser", extra, parser)
	}
}

func TestParsesToolCalls_NeedsBothFlags(t *testing.T) {
	cases := map[string]struct {
		args []string
		want bool
	}{
		"both flags":       {[]string{"--model", "m", "--tool-call-parser", "hermes", "--enable-auto-tool-choice"}, true},
		"equals form":      {[]string{"--tool-call-parser=hermes", "--enable-auto-tool-choice"}, true},
		"parser only":      {[]string{"--tool-call-parser", "hermes"}, false},
		"enable flag only": {[]string{"--enable-auto-tool-choice"}, false},
		"neither":          {[]string{"--model", "m"}, false},
	}
	for name, c := range cases {
		if got := parsesToolCalls(c.args); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

func TestParseGPUMemory_ReadsADiscreteGPUAndFallsBackOnUnifiedMemory(t *testing.T) {
	system := func() (int64, int64, bool) { return 128 * gib, 100 * gib, true }
	for _, tc := range []struct {
		name        string
		out         string
		total, free int64
		ok          bool
	}{
		{"discrete GPU, in MiB", "49140, 48600\n", 49140 << 20, 48600 << 20, true},
		{"first GPU of several", "24576, 20000\n49140, 48600\n", 24576 << 20, 20000 << 20, true},
		{"DGX Spark reports N/A", "[N/A], [N/A]\n", 128 * gib, 100 * gib, true},
		{"not supported", "[Not Supported], [Not Supported]\n", 128 * gib, 100 * gib, true},
		{"malformed", "error\n", 0, 0, false},
		{"empty", "", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total, free, ok := parseGPUMemory(tc.out, system)
			if total != tc.total || free != tc.free || ok != tc.ok {
				t.Fatalf("parseGPUMemory(%q) = %d, %d, %v; want %d, %d, %v", tc.out, total, free, ok, tc.total, tc.free, tc.ok)
			}
		})
	}
}

func TestParseMeminfo_ReadsTotalAndAvailable(t *testing.T) {
	text := "MemTotal:       130567000 kB\nMemFree:         2000000 kB\nMemAvailable:   104000000 kB\nBuffers:          100 kB\n"
	total, available, ok := parseMeminfo(text)
	if !ok || total != 130567000*1024 || available != 104000000*1024 {
		t.Fatalf("parseMeminfo = %d, %d, %v", total, available, ok)
	}
	if _, _, ok := parseMeminfo("MemTotal: 100 kB\n"); ok {
		t.Fatal("a reading without MemAvailable must not count as known")
	}
}
