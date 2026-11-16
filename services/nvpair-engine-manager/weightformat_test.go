// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The checkpoints below are trimmed to the fields that decide the format, as
// published on Hugging Face.
const (
	// nvidia/Llama-3.1-8B-Instruct-FP8: ModelOpt declares FP8 weights and an FP8
	// KV cache in hf_quant_config.json; config.json says nothing.
	modelOptFP8Config  = `{"model_type":"llama","torch_dtype":"bfloat16"}`
	modelOptFP8HFQuant = `{"producer":{"name":"modelopt"},"quantization":{"quant_algo":"FP8","kv_cache_quant_algo":"FP8","exclude_modules":["lm_head"]}}`
	// Qwen/Qwen3-8B-FP8.
	qwenFP8Config = `{"model_type":"qwen3","quantization_config":{"quant_method":"fp8","weight_block_size":[128,128]}}`
	// Qwen/Qwen3.5-4B-FP8 style: vLLM-Metal dequantizes this family itself.
	qwen35FP8Config = `{"model_type":"qwen3_5","quantization_config":{"quant_method":"fp8","weight_block_size":[128,128]}}`
	// RedHatAI/Meta-Llama-3.1-8B-Instruct-FP8.
	compressedFP8Config = `{"model_type":"llama","quantization_config":{"quant_method":"compressed-tensors","config_groups":{"group_0":{"weights":{"type":"float","num_bits":8}}}}}`
	// A compressed-tensors INT4 checkpoint.
	compressedInt4Config = `{"model_type":"llama","quantization_config":{"quant_method":"compressed-tensors","config_groups":{"group_0":{"weights":{"type":"int","num_bits":4}}}}}`
	bf16Config           = `{"model_type":"llama","torch_dtype":"bfloat16"}`
)

var macFP8Rule = &UnsupportedWeights{
	Formats:          []string{"fp8"},
	ExceptModelTypes: []string{"qwen3_5", "qwen3_5_moe"},
	Reason:           "choose the model's BF16 version instead",
}

func TestWeightFormat_ReadsFP8TheWayVLLMDoes(t *testing.T) {
	test := func(name, config, hfQuant, wantFormat string) {
		t.Run(name, func(t *testing.T) {
			var quant []byte
			if hfQuant != "" {
				quant = []byte(hfQuant)
			}

			format, _, err := weightFormat([]byte(config), quant)

			require.NoError(t, err)
			assert.Equal(t, wantFormat, format)
		})
	}
	test("ModelOpt FP8 in hf_quant_config.json", modelOptFP8Config, modelOptFP8HFQuant, "fp8")
	test("ModelOpt FP8 KV cache only", bf16Config, `{"quantization":{"quant_algo":"","kv_cache_quant_algo":"FP8"}}`, "fp8")
	test("ModelOpt FP8 in config.json", `{"quantization_config":{"quant_method":"modelopt","quant_algo":"FP8"}}`, "", "fp8")
	test("quant_method fp8", qwenFP8Config, "", "fp8")
	test("compressed-tensors float 8-bit weights", compressedFP8Config, "", "fp8")
	test("compressed-tensors FP8 KV cache", `{"quantization_config":{"quant_method":"compressed-tensors","kv_cache_scheme":{"type":"float","num_bits":8}}}`, "", "fp8")
	test("compressed-tensors INT4", compressedInt4Config, "", "")
	test("ModelOpt NVFP4", bf16Config, `{"quantization":{"quant_algo":"NVFP4"}}`, "")
	test("unquantized", bf16Config, "", "")
}

func TestUnsupportedWeights_RefusesOnlyTheFormatsAndModelTypesItNames(t *testing.T) {
	refused := func(name, config, hfQuant string) {
		t.Run(name, func(t *testing.T) {
			err := checkWeights(macFP8Rule, "org/m", []byte(config), []byte(hfQuant))

			require.ErrorContains(t, err, "org/m is an FP8 model: choose the model's BF16 version instead")
		})
	}
	allowed := func(name, config string, rule *UnsupportedWeights) {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, checkWeights(rule, "org/m", []byte(config), nil))
		})
	}
	refused("the ModelOpt model QA loaded", modelOptFP8Config, modelOptFP8HFQuant)
	refused("Qwen3 FP8", qwenFP8Config, "")
	refused("compressed-tensors FP8", compressedFP8Config, "")
	allowed("a model type the engine loads in FP8 anyway", qwen35FP8Config, macFP8Rule)
	allowed("an unquantized model", bf16Config, macFP8Rule)
	allowed("a platform without the rule", qwenFP8Config, nil)
}

// vLLM-Metal refuses an FP8 KV cache and its mlx-lm loader has no FP8 path
// for most families, so only the Mac declares the rule. Linux, DGX Spark
// (linux/arm64) included, loads FP8 as before.
func TestBundledVLLMManifest_RefusesFP8OnlyOnTheMac(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	require.True(t, ok)

	mac := manifest.Platforms["darwin/arm64"].Runtime.UnsupportedWeights
	require.NotNil(t, mac)
	assert.Equal(t, []string{"fp8"}, mac.Formats)
	assert.Nil(t, manifest.Platforms["linux/arm64"].Runtime.UnsupportedWeights, "DGX Spark loads FP8")
	assert.Nil(t, manifest.Platforms["linux/amd64"].Runtime.UnsupportedWeights)
}

func TestUnsupportedWeightsValidation(t *testing.T) {
	test := func(name string, rule UnsupportedWeights, valid bool) {
		t.Run(name, func(t *testing.T) {
			p := Platform{Runtime: Runtime{Bin: "vllm", UnsupportedWeights: &rule}}

			err := p.validate("darwin/arm64")

			assert.Equal(t, valid, err == nil, "validate = %v", err)
		})
	}
	test("fp8 with a reason", UnsupportedWeights{Formats: []string{"fp8"}, Reason: "use BF16"}, true)
	test("no reason", UnsupportedWeights{Formats: []string{"fp8"}}, false)
	test("no formats", UnsupportedWeights{Reason: "use BF16"}, false)
	test("a format it cannot read", UnsupportedWeights{Formats: []string{"int4"}, Reason: "use BF16"}, false)
}

func TestWithoutUnsupportedWeights_HidesFP8RepositoriesFromATargetThatCannotLoadThem(t *testing.T) {
	models := []CatalogModel{
		{ID: "nvidia/Llama-3.1-8B-Instruct-FP8"},
		{ID: "Qwen/Qwen3-8B", Tags: []string{"safetensors"}},
		{ID: "org/tagged-only", Tags: []string{"fp8"}},
		{ID: "org/fp8-in-the-middle-v2"},
		{ID: "org/notfp8model"},
	}

	assert.Equal(t, models, withoutUnsupportedWeights(models, nil), "a target without the rule sees everything")
	var ids []string
	for _, m := range withoutUnsupportedWeights(models, macFP8Rule) {
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []string{"Qwen/Qwen3-8B", "org/notfp8model"}, ids)
}

// A Load of a model the platform cannot run is refused at the start, before
// vLLM loads it, and the engine is put back on the model it was serving.
func TestLaunchModel_KeepsThePreviousModelWhenThePlatformCannotLoadTheNewOne(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	st, err := e.state("fake")
	require.NoError(t, err)
	modelsDir := t.TempDir()
	repo := writeHFRepo(t, filepath.Join(modelsDir, "hub"), "nvidia/Llama-3.1-8B-Instruct-FP8", llmFiles(map[string]string{
		"config.json": modelOptFP8Config, hfQuantConfigFile: modelOptFP8HFQuant, "model.safetensors": "w",
	}))
	require.DirExists(t, repo)
	st.mu.Lock()
	st.modelsDir = modelsDir
	st.plat.Runtime.UnsupportedWeights = macFP8Rule
	// As the vLLM manifest does, so the engine's hub cache is modelsDir/hub.
	st.plat.Runtime.Env = map[string]string{"HF_HOME": modelsDir}
	st.mu.Unlock()
	before := launchTextOf(t, e)

	_, err = loadModel(t, e, "nvidia/Llama-3.1-8B-Instruct-FP8")

	require.ErrorContains(t, err, "nvidia/Llama-3.1-8B-Instruct-FP8 is an FP8 model: choose the model's BF16 version instead")
	require.ErrorContains(t, err, "the previous model was restored")
	assert.Equal(t, before, launchTextOf(t, e))
	assert.True(t, mustStatus(t, e).Running, "the engine must be serving the previous model again")
}

// A download the platform could never load is refused before it starts.
func TestPullHFModel_RefusesAModelThePlatformCannotLoad(t *testing.T) {
	siblings := []hfSibling{
		{RFilename: "config.json", Size: 1},
		{RFilename: hfQuantConfigFile, Size: 1},
		{RFilename: "tokenizer.json", Size: 1},
		{RFilename: "model.safetensors", Size: 8_000_000_000},
	}
	e, _, _ := hfPullFixture(t, siblings, http.StatusOK, "echo the download must not start >&2; exit 1")
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"siblings": siblings}))
			return
		}
		switch filepath.Base(r.URL.Path) {
		case "config.json":
			_, err := w.Write([]byte(modelOptFP8Config))
			assert.NoError(t, err)
		case hfQuantConfigFile:
			_, err := w.Write([]byte(modelOptFP8HFQuant))
			assert.NoError(t, err)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	old := hfDefaultEndpoint
	hfDefaultEndpoint = hub.URL
	t.Cleanup(func() { hfDefaultEndpoint = old })
	st, err := e.state("vllm")
	require.NoError(t, err)
	st.mu.Lock()
	st.plat.Runtime.UnsupportedWeights = macFP8Rule
	st.mu.Unlock()
	t.Setenv("HF_HUB_OFFLINE", "")

	_, err = e.PullModelStream(t.Context(), "vllm", "nvidia/Llama-3.1-8B-Instruct-FP8", nil)

	require.ErrorContains(t, err, "is an FP8 model: choose the model's BF16 version instead")
	assert.NotContains(t, err.Error(), "the download must not start")
}

// checkLaunchWeights reads the model a start serves at its revision, and for a
// model not in the cache, what the metadata fetch put there.
func TestCheckLaunchWeights_ReadsTheModelThisStartServes(t *testing.T) {
	hub := t.TempDir()
	writeHFRepo(t, hub, "org/cached-fp8", llmFiles(map[string]string{"config.json": qwenFP8Config, "model.safetensors": "w"}))
	noFetch := func() (hfPendingModel, bool) { return hfPendingModel{}, false }

	require.ErrorContains(t, checkLaunchWeights(macFP8Rule, []string{"serve", "--model", "org/cached-fp8"}, hub, noFetch), "org/cached-fp8 is an FP8 model")
	assert.NoError(t, checkLaunchWeights(macFP8Rule, []string{"serve", "--model", "org/unknown"}, hub, noFetch), "a model that cannot be read is left to vLLM")
	assert.NoError(t, checkLaunchWeights(nil, []string{"serve", "--model", "org/cached-fp8"}, hub, noFetch))

	pending := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(pending, "config.json"), []byte(compressedFP8Config), 0o644))
	fetch := func() (hfPendingModel, bool) { return hfPendingModel{snapshot: pending}, true }
	assert.ErrorContains(t, checkLaunchWeights(macFP8Rule, []string{"serve", "--model", "org/not-yet-downloaded"}, hub, fetch), "org/not-yet-downloaded is an FP8 model")
}

// engine:catalog applies the target's rule: a Mac is not offered FP8 vLLM
// models, and DGX Spark (linux/arm64) is offered them as before.
func TestRunCatalog_HidesFP8VLLMModelsOnlyFromAMac(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode([]any{
			vllmRow("Qwen/Chat-8B", nil),
			vllmRow("nvidia/Llama-3.1-8B-Instruct-FP8", nil),
		}))
	}))
	t.Cleanup(listing.Close)
	ex := NewExecutor(loadWithOverrides(t, t.TempDir()), NewReporter(nil), func(string, any) {}, t.TempDir())
	offered := func(name, platform, arch string, want []string) {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			m := NewManager(NewCodec(&out), ex, nil)
			m.catalog.baseURL = listing.URL
			id := json.RawMessage("1")
			params := fmt.Sprintf(`{"engine":"vllm","platform":%q,"arch":%q}`, platform, arch)

			m.runCatalog(t.Context(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:catalog", Params: json.RawMessage(params)})

			var reply struct {
				Result CatalogResult `json:"result"`
			}
			require.NoError(t, json.Unmarshal(out.Bytes(), &reply), out.String())
			var ids []string
			for _, model := range reply.Result.Models {
				ids = append(ids, model.ID)
			}
			assert.ElementsMatch(t, want, ids)
		})
	}
	offered("a Mac", "darwin", "arm64", []string{"Qwen/Chat-8B"})
	offered("DGX Spark", "linux", "arm64", []string{"Qwen/Chat-8B", "nvidia/Llama-3.1-8B-Instruct-FP8"})
}
