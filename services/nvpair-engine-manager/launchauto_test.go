// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestLaunchFlagValue_ReadsTheValueVLLMWillUse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantValue string
		wantFound bool
	}{
		{"separate value", []string{"--model", "org/a", "--port", "8001"}, "org/a", true},
		{"equals form", []string{"--model=org/a"}, "org/a", true},
		{"repeated: the last one wins", []string{"--model", "org/a", "--port", "8001", "--model", "org/b"}, "org/b", true},
		{"repeated in both forms", []string{"--model=org/a", "--model", "org/b", "--model=org/c"}, "org/c", true},
		{"after the terminator is not an option", []string{"--model", "org/a", "--", "--model", "org/b"}, "org/a", true},
		{"no value at the end", []string{"--port", "8001", "--model"}, "", true},
		{"a longer flag with the same prefix", []string{"--model-loader-extra-config", "{}"}, "", false},
		{"absent", []string{"--port", "8001"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, found := launchFlagValue(tc.args, "--model")
			if value != tc.wantValue || found != tc.wantFound {
				t.Fatalf("launchFlagValue(%q) = %q, %v; want %q, %v", tc.args, value, found, tc.wantValue, tc.wantFound)
			}
		})
	}
}

func TestAutoContext_SizesTheModelNamedLast(t *testing.T) {
	withGPU(t, 48, 48)
	// Only the second --model is cached. vLLM serves the second, so sizing must
	// use it rather than fall back to the uncached first one.
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	extra, _ := planHFContext([]string{"serve", "--model", "org/not-cached", "--model", "Qwen/Qwen3-8B"}, cache, 65536, nil)

	if got := flagOf(t, extra, "--max-model-len"); got != "65536" {
		t.Fatalf("max-model-len = %q, want 65536 from Qwen3-8B's config: %v", got, extra)
	}
}

func TestAutoContextExtendsAShortModelToTheTarget(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	extra, _ := planHFContext([]string{"serve", "--model", "Qwen/Qwen3-8B"}, cache, 65536, nil)

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
	extra, _ := planHFContext([]string{"--model", "Qwen/Long"}, cache, 65536, nil)
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
	extra, _ := planHFContext([]string{"--model", "Qwen/Scaled"}, cache, 131072, nil)
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
	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, cache, 65536, nil)

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

	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--max-model-len=8192"}, cache, 65536, nil)
	if slices.Contains(extra, "--max-model-len") || slices.Contains(extra, "--hf-overrides") {
		t.Fatalf("the user's context must stand untouched: %v", extra)
	}
	if util := utilOf(t, extra); util > 0.45 {
		t.Fatalf("an 8K window needs far less than %.2f of the GPU", util)
	}

	extra, _ = planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--gpu-memory-utilization", "0.5"}, cache, 65536, nil)
	if slices.Contains(extra, "--gpu-memory-utilization") {
		t.Fatalf("the user's GPU share must stand untouched: %v", extra)
	}
	n, _ := strconv.ParseInt(flagOf(t, extra, "--max-model-len"), 10, 64)
	if n >= 65536 || n < 4096 {
		t.Fatalf("a 0.5 share (24 GiB) cannot hold 64K of Qwen3-8B; max-model-len = %d", n)
	}

	extra, _ = planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--max-model-len", "4096", "--gpu-memory-utilization", "0.3"}, cache, 65536, nil)
	if len(extra) != 0 {
		t.Fatalf("with both set there is nothing to add: %v", extra)
	}
}

func TestAutoContextSkipsShardedLaunches(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	for _, flag := range []string{"--tensor-parallel-size", "--pipeline-parallel-size"} {
		if extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", flag, "2"}, cache, 65536, nil); len(extra) != 0 {
			t.Fatalf("%s: sizing a sharded launch needs more than GPU 0; got %v", flag, extra)
		}
	}
}

func TestAutoContextForAModelNotInTheCache(t *testing.T) {
	withGPU(t, 48, 20)
	extra, _ := planHFContext([]string{"--model", "org/not-downloaded"}, t.TempDir(), 65536, nil)
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
	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, cache, 65536, nil)
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
	extra, _ := planHFContext([]string{"--model", "Qwen/VL"}, cache, 65536, nil)
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
	out := applyAutoLaunch("vllm", rt, in, parent, nil)
	if !slices.Equal(out.Args[:3], in.Args) || flagOf(t, out.Args, "--max-model-len") != "65536" {
		t.Fatalf("args = %v", out.Args)
	}
	if len(in.Args) != 3 {
		t.Fatalf("the caller's arguments were modified: %v", in.Args)
	}
	if got := applyAutoLaunch("vllm", Runtime{}, in, parent, nil); !slices.Equal(got.Args, in.Args) {
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

// qwen35_4BConfig is the part of Qwen/Qwen3.5-4B's config.json that sizing
// reads: a vision-language model whose language model sits in text_config.
const qwen35_4BConfig = `{"model_type":"qwen3_5","text_config":{"max_position_embeddings":262144,"num_hidden_layers":32,"num_attention_heads":16,"num_key_value_heads":4,"head_dim":256,"hidden_size":2560,"dtype":"bfloat16"}}`

// qwen35_4BBytes is about what a download of Qwen/Qwen3.5-4B fetches.
const qwen35_4BBytes = 9_450_000_000

func TestAutoContext_FirstStartOfAnUncachedModelGetsRoomForItsWeights(t *testing.T) {
	// The default model on a 16 GiB GPU, before vLLM has
	// downloaded it. A blind half share (8 GiB) cannot even hold the weights.
	withGPU(t, 16, 15)
	var cfg hfModelConfig
	if err := json.Unmarshal([]byte(qwen35_4BConfig), &cfg); err != nil {
		t.Fatal(err)
	}
	lookup := func(id string) (hfModelConfig, int64, bool) {
		if id != "Qwen/Qwen3.5-4B" {
			t.Fatalf("looked up %q, want the model being started", id)
		}
		return cfg, qwen35_4BBytes, true
	}

	extra, note := planHFContext([]string{"--model", "Qwen/Qwen3.5-4B"}, t.TempDir(), 65536, lookup)

	weightsAndOverhead := float64(qwen35_4BBytes)*weightsSlack + float64(vllmOverhead)
	if util := utilOf(t, extra); util*16*float64(gib) < weightsAndOverhead {
		t.Fatalf("gpu-memory-utilization %.2f is %.1f GiB, less than the %.1f GiB of weights and overhead: %v",
			util, util*16, weightsAndOverhead/float64(gib), extra)
	}
	if util := utilOf(t, extra); util > 14.0/16.0+0.001 {
		t.Fatalf("gpu-memory-utilization %.2f must leave a margin of the 15 GiB free", util)
	}
	if got := flagOf(t, extra, "--max-model-len"); got == "" {
		t.Fatalf("no --max-model-len, so vLLM would try the model's full 262144-token window: %v", extra)
	}
	if !strings.Contains(note, "sized from Hugging Face") {
		t.Fatalf("note %q should say where the sizing came from", note)
	}
}

func TestAutoContext_CachedModelIsNotLookedUp(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	lookup := func(id string) (hfModelConfig, int64, bool) {
		t.Fatalf("looked up %q although it is cached", id)
		return hfModelConfig{}, 0, false
	}
	if extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, cache, 65536, lookup); flagOf(t, extra, "--max-model-len") != "65536" {
		t.Fatalf("cached sizing changed: %v", extra)
	}
}

// fakeHuggingFace serves one model's file list and config.json the way the
// Hugging Face API and file server do, and points the lookups at it.
func fakeHuggingFace(t *testing.T, id string, siblings []hfSibling, config string, status int) {
	t.Helper()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "x", status)
			return
		}
		switch r.URL.Path {
		case "/api/models/" + id:
			_ = json.NewEncoder(w).Encode(map[string]any{"siblings": siblings})
		case "/" + id + "/resolve/main/config.json":
			_, _ = w.Write([]byte(config))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	oldAPI, oldFiles := hfAPIBase, hfFileBase
	hfAPIBase, hfFileBase = hub.URL+"/api/models", hub.URL
	t.Cleanup(func() { hfAPIBase, hfFileBase = oldAPI, oldFiles })
}

func TestLookupHFModel_ReadsTheConfigAndDownloadSize(t *testing.T) {
	fakeHuggingFace(t, "Qwen/Qwen3.5-4B", []hfSibling{
		{RFilename: "config.json", Size: 2_000},
		{RFilename: "model.safetensors-00001-of-00002.safetensors", Size: 5_000_000_000},
		{RFilename: "model.safetensors-00002-of-00002.safetensors", Size: 4_440_000_000},
		{RFilename: "tokenizer.json", Size: 8_000_000},
		{RFilename: "model.gguf", Size: 3_000_000_000}, // not downloaded, so not counted
	}, qwen35_4BConfig, http.StatusOK)
	t.Setenv("HF_HUB_OFFLINE", "")
	e := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())

	cfg, size, ok := e.lookupHFModel(t.Context(), "Qwen/Qwen3.5-4B")

	if !ok {
		t.Fatal("lookup failed")
	}
	if want := int64(2_000 + 5_000_000_000 + 4_440_000_000 + 8_000_000); size != want {
		t.Fatalf("size = %d, want %d (the files a download fetches)", size, want)
	}
	if native, layers, _, _, _ := cfg.text().kvShape(); native != 262144 || layers != 32 {
		t.Fatalf("config read as native %d, layers %d; want 262144, 32 from text_config", native, layers)
	}
}

func TestLookupHFModel_FallsBackWhenHuggingFaceCannotAnswer(t *testing.T) {
	weights := []hfSibling{{RFilename: "model.safetensors", Size: 1_000_000_000}}
	for _, tc := range []struct {
		name    string
		status  int
		config  string
		offline string
	}{
		{"model not found", http.StatusNotFound, qwen8BConfig, ""},
		{"gated model without a token", http.StatusUnauthorized, qwen8BConfig, ""},
		{"server error", http.StatusInternalServerError, qwen8BConfig, ""},
		{"config sizing cannot use", http.StatusOK, `{"model_type":"unknown"}`, ""},
		{"HF_HUB_OFFLINE set", http.StatusOK, qwen8BConfig, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHuggingFace(t, "org/model", weights, tc.config, tc.status)
			t.Setenv("HF_HUB_OFFLINE", tc.offline)
			e := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
			if _, _, ok := e.lookupHFModel(t.Context(), "org/model"); ok {
				t.Fatal("lookup succeeded; want the capped fallback")
			}

			// The start then falls back to the capped share, as before.
			withGPU(t, 16, 15)
			extra, _ := planHFContext([]string{"--model", "org/model"}, t.TempDir(), 65536, func(id string) (hfModelConfig, int64, bool) {
				return e.lookupHFModel(t.Context(), id)
			})
			// 15 GiB free less the 1 GiB margin is 0.87 of 16 GiB.
			if util := utilOf(t, extra); util != 0.87 || slices.Contains(extra, "--max-model-len") {
				t.Fatalf("fallback = %v, want only --gpu-memory-utilization 0.87 (free memory less the margin)", extra)
			}
		})
	}
}

func TestApplyAutoLaunch_LimitsConcurrentRequestsUnlessTheUserSetsALimit(t *testing.T) {
	withoutGPU(t)
	for _, tc := range []struct {
		name  string
		args  []string
		limit int
		want  string
	}{
		{"limit added", []string{"serve", "--model", "org/m"}, 32, "32"},
		{"the user's own limit wins", []string{"serve", "--model", "org/m", "--max-num-seqs", "4"}, 32, "4"},
		{"the user's own limit in = form wins", []string{"serve", "--model", "org/m", "--max-num-seqs=64"}, 32, "64"},
		{"a sharded launch gets it too", []string{"serve", "--model", "org/m", "--tensor-parallel-size", "2"}, 32, "32"},
		{"no limit in the manifest leaves vLLM's default", []string{"serve", "--model", "org/m"}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := Runtime{AutoLaunch: &AutoLaunch{Kind: autoLaunchHFContext, ContextTokens: 65536, MaxNumSeqs: tc.limit}}
			out := applyAutoLaunch("vllm", rt, launchCommand{Bin: "vllm", Args: tc.args}, t.TempDir(), nil)
			if got := flagOf(t, out.Args, "--max-num-seqs"); got != tc.want {
				t.Fatalf("--max-num-seqs = %q, want %q: %v", got, tc.want, out.Args)
			}
			if n := strings.Count(strings.Join(out.Args, " "), "--max-num-seqs"); n > 1 {
				t.Fatalf("--max-num-seqs appears %d times: %v", n, out.Args)
			}
		})
	}
}

func TestBundledVLLMLaunch_FirstStartOfTheDefaultModelFitsA16GiBGPU(t *testing.T) {
	// The default model on a 16 GiB GPU with an empty cache, through the bundled
	// manifest. Each added argument is one vLLM needs to start there: the share
	// must hold the weights, the context must be bounded, and the concurrent
	// requests must leave room for Qwen3.5's per-request state.
	withGPU(t, 16, 15)
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	if !ok {
		t.Fatal("vllm manifest not loaded")
	}
	rt := manifest.Platforms["linux/amd64"].Runtime
	var cfg hfModelConfig
	if err := json.Unmarshal([]byte(qwen35_4BConfig), &cfg); err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (hfModelConfig, int64, bool) { return cfg, qwen35_4BBytes, true }
	in := launchCommand{Bin: "vllm", Args: []string{"serve", "--model", "Qwen/Qwen3.5-4B"}}

	out := applyAutoLaunch("vllm", rt, in, t.TempDir(), lookup)

	weightsAndOverhead := float64(qwen35_4BBytes)*weightsSlack + float64(vllmOverhead)
	if util := utilOf(t, out.Args); util*16*float64(gib) < weightsAndOverhead {
		t.Fatalf("gpu-memory-utilization %.2f cannot hold the weights and overhead: %v", util, out.Args)
	}
	if flagOf(t, out.Args, "--max-model-len") == "" {
		t.Fatalf("no --max-model-len: %v", out.Args)
	}
	if got := flagOf(t, out.Args, "--max-num-seqs"); got != "32" {
		t.Fatalf("--max-num-seqs = %q, want 32: vLLM's default of 256 does not start on this GPU: %v", got, out.Args)
	}
}
