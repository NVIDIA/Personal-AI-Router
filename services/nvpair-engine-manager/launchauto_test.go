// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, os.Truncate(filepath.Join(repo, "blobs", "blob-model.safetensors"), weightBytes))
}

// pendingModel stands in for a model this start downloads: fetch puts its
// config and tokenizer in cache, as the metadata fetch does, and reports its
// weight bytes. Like the fetch a start builds, it runs once.
func pendingModel(t *testing.T, cache, id, config string, weightBytes int64) hfModelFetcher {
	t.Helper()
	return sync.OnceValues(func() (hfPendingModel, bool) {
		repo := writeHFRepo(t, cache, id, map[string]string{"config.json": config, "tokenizer.json": "{}"})
		return hfPendingModel{snapshot: filepath.Join(repo, "snapshots", "abc123"), weights: weightBytes}, true
	})
}

func withGPU(t *testing.T, totalGiB, freeGiB int64) {
	t.Helper()
	old := gpuMemory
	gpuMemory = func(map[string]string) (int64, int64, bool) { return totalGiB * gib, freeGiB * gib, true }
	t.Cleanup(func() { gpuMemory = old })
}

func withoutGPU(t *testing.T) {
	t.Helper()
	old := gpuMemory
	gpuMemory = func(map[string]string) (int64, int64, bool) { return 0, 0, false }
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
	require.NoError(t, err, "no usable --gpu-memory-utilization in %v", args)
	return v
}

func TestLaunchFlagValue_ReadsTheValueVLLMWillUse(t *testing.T) {
	test := func(name string, args []string, wantValue string, wantFound bool) {
		t.Run(name, func(t *testing.T) {
			value, found := launchFlagValue(args, "--model")

			assert.Equal(t, wantValue, value)
			assert.Equal(t, wantFound, found)
		})
	}
	test("separate value", []string{"--model", "org/a", "--port", "8001"}, "org/a", true)
	test("equals form", []string{"--model=org/a"}, "org/a", true)
	test("repeated: the last one wins", []string{"--model", "org/a", "--port", "8001", "--model", "org/b"}, "org/b", true)
	test("repeated in both forms", []string{"--model=org/a", "--model", "org/b", "--model=org/c"}, "org/c", true)
	test("after the terminator is not an option", []string{"--model", "org/a", "--", "--model", "org/b"}, "org/a", true)
	test("no value at the end", []string{"--port", "8001", "--model"}, "", true)
	test("a longer flag with the same prefix", []string{"--model-loader-extra-config", "{}"}, "", false)
	test("absent", []string{"--port", "8001"}, "", false)
}

func TestLaunchFlagValue_AcceptsBothOptionSpellingsWithoutChangingValues(t *testing.T) {
	test := func(name string, args []string, flag, want string) {
		t.Run(name, func(t *testing.T) {
			value, found := launchFlagValue(args, flag)

			assert.True(t, found)
			assert.Equal(t, want, value)
		})
	}
	test("separate underscore option", []string{"--download_dir", "models_with_underscores"}, "--download-dir", "models_with_underscores")
	test("equals form preserves underscores and equals in the value", []string{"--download_dir=models_with_underscores=revision"}, "--download-dir", "models_with_underscores=revision")
	test("last occurrence uses underscores", []string{"--download-dir", "first", "--download_dir=second"}, "--download-dir", "second")
	test("last occurrence uses dashes", []string{"--download_dir=first", "--download-dir", "second"}, "--download-dir", "second")
	test("the declared flag uses underscores", []string{"--download_dir", "models"}, "--download_dir", "models")
	test("an underscore option after the terminator is ignored", []string{"--download-dir", "first", "--", "--download_dir", "second"}, "--download-dir", "first")
}

// The GPU share holds what the launch needs, at least minAutoUtil, and never
// more than is free: a share above free memory fails the launch, so the cap on
// free memory wins over the floor.
func TestAutoGPUShare_NeverClaimsMoreThanIsFree(t *testing.T) {
	share := func(name string, needGiB, totalGiB, freeGiB float64, want float64) {
		t.Run(name, func(t *testing.T) {
			got, ok := autoGPUShare(int64(needGiB*float64(gib)), int64(totalGiB*float64(gib)), int64(freeGiB*float64(gib)))

			require.True(t, ok)
			assert.InDelta(t, want, got, 0.0001)
		})
	}
	none := func(name string, needGiB, totalGiB, freeGiB float64) {
		t.Run(name, func(t *testing.T) {
			_, ok := autoGPUShare(int64(needGiB*float64(gib)), int64(totalGiB*float64(gib)), int64(freeGiB*float64(gib)))

			assert.False(t, ok)
		})
	}
	share("what the launch needs, rounded up", 26, 48, 48, 0.55)
	share("a small launch still gets the floor", 2, 48, 48, 0.15)
	// 7 GiB free less the 1 GiB margin is 0.125 of 48 GiB, below the 0.15 floor
	// (7.2 GiB), which would claim more than is free.
	share("free memory wins over the floor", 6, 48, 7, 0.12)
	share("free memory wins over the need", 30, 48, 20, 0.39)
	share("never above the ceiling", 60, 48, 48, 0.95)
	none("nothing free beyond the margin", 6, 48, 1)
	none("no total reading", 6, 0, 0)
}

func TestAutoContext_SizesTheModelNamedLast(t *testing.T) {
	withGPU(t, 48, 48)
	// Only the second --model is cached. vLLM serves the second, so sizing must
	// use it rather than fall back to the uncached first one.
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)

	extra, _ := planHFContext([]string{"serve", "--model", "org/not-cached", "--model", "Qwen/Qwen3-8B"}, cache, 65536, nil, nil)

	assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"), "sized from Qwen3-8B's config: %v", extra)
}

func TestAutoContext_ExtendsAShortModelToTheTarget(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)

	extra, _ := planHFContext([]string{"serve", "--model", "Qwen/Qwen3-8B"}, cache, 65536, nil, nil)

	assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"), "the model alone stops at 40960")
	var overrides struct {
		RopeScaling struct {
			Type     string  `json:"rope_type"`
			Factor   float64 `json:"factor"`
			Original int64   `json:"original_max_position_embeddings"`
		} `json:"rope_scaling"`
		MaxPositions int64 `json:"max_position_embeddings"`
	}
	require.NoError(t, json.Unmarshal([]byte(flagOf(t, extra, "--hf-overrides")), &overrides))
	assert.Equal(t, "yarn", overrides.RopeScaling.Type)
	assert.Equal(t, int64(40960), overrides.RopeScaling.Original)
	assert.InDelta(t, 1.6, overrides.RopeScaling.Factor, 0.01)
	assert.GreaterOrEqual(t, overrides.MaxPositions, int64(65536))
	// 16.4 GB of weights plus about 9.7 GB of KV cache for 64K plus overhead is
	// about 60% of a 48 GiB card; vLLM's own default would take 92%.
	assert.InDelta(t, 0.62, utilOf(t, extra), 0.07)
}

func TestAutoContext_CapsALongModelAtTheTarget(t *testing.T) {
	withGPU(t, 48, 48)
	cfg := strings.Replace(qwen8BConfig, "40960", "262144", 1)
	cache := cachedModel(t, "Qwen/Long", cfg, 16_400_000_000)

	extra, _ := planHFContext([]string{"--model", "Qwen/Long"}, cache, 65536, nil, nil)

	assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"), "the target, not the 262144 native window")
	assert.NotContains(t, extra, "--hf-overrides", "a model that already supports the target needs no rope override")
}

func TestAutoContext_LeavesModelsWithRopeScalingAlone(t *testing.T) {
	withGPU(t, 48, 48)
	cfg := strings.Replace(qwen8BConfig, `"torch_dtype"`, `"rope_scaling":{"rope_type":"yarn","factor":4.0},"torch_dtype"`, 1)
	cache := cachedModel(t, "Qwen/Scaled", cfg, 16_400_000_000)

	extra, _ := planHFContext([]string{"--model", "Qwen/Scaled"}, cache, 131072, nil, nil)

	assert.NotContains(t, extra, "--hf-overrides", "must not override a model's own rope scaling")
	assert.Equal(t, "40960", flagOf(t, extra, "--max-model-len"))
}

func TestAutoContext_ShrinksToWhatFitsInFreeMemory(t *testing.T) {
	withGPU(t, 48, 22)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)

	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, cache, 65536, nil, nil)

	n, err := strconv.ParseInt(flagOf(t, extra, "--max-model-len"), 10, 64)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(4096))
	assert.Less(t, n, int64(65536))
	assert.Zero(t, n%1024, "a multiple of 1024")
	assert.NotContains(t, extra, "--hf-overrides", "a window inside the native limit needs no rope override")
	assert.LessOrEqual(t, utilOf(t, extra), 21.0/48.0, "no more than is free less the margin")
}

// The free-memory cap wins over the minimum share: on a 48 GiB GPU with 7 GiB
// free, the context is fitted into 6 GiB and the share must not then be raised
// to 0.15 (7.2 GiB), more than is free.
func TestAutoContext_KeepsTheShareWithinFreeMemoryOnALargeBusyGPU(t *testing.T) {
	withGPU(t, 48, 7)
	cache := cachedModel(t, "Qwen/Qwen3-0.6B", qwen8BConfig, 1_200_000_000)

	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-0.6B"}, cache, 65536, nil, nil)

	assert.InDelta(t, 0.12, utilOf(t, extra), 0.0001)
	assert.LessOrEqual(t, utilOf(t, extra)*48, 6.0, "the share must fit the 6 GiB free beyond the margin")
}

func TestAutoContext_NeverOverridesWhatTheUserSet(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)

	t.Run("the user's context", func(t *testing.T) {
		extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--max-model-len=8192"}, cache, 65536, nil, nil)

		assert.NotContains(t, extra, "--max-model-len")
		assert.NotContains(t, extra, "--hf-overrides")
		assert.LessOrEqual(t, utilOf(t, extra), 0.45, "an 8K window needs far less of the GPU")
	})
	t.Run("the user's GPU share", func(t *testing.T) {
		extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--gpu-memory-utilization", "0.5"}, cache, 65536, nil, nil)

		assert.NotContains(t, extra, "--gpu-memory-utilization")
		n, err := strconv.ParseInt(flagOf(t, extra, "--max-model-len"), 10, 64)
		require.NoError(t, err)
		assert.Less(t, n, int64(65536), "a 0.5 share (24 GiB) cannot hold 64K of Qwen3-8B")
		assert.GreaterOrEqual(t, n, int64(4096))
	})
	t.Run("both", func(t *testing.T) {
		extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", "--max-model-len", "4096", "--gpu-memory-utilization", "0.3"}, cache, 65536, nil, nil)

		assert.Empty(t, extra)
	})
}

func TestAutoContext_SkipsShardedLaunches(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	test := func(flag string) {
		t.Run(flag, func(t *testing.T) {
			extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B", flag, "2"}, cache, 65536, nil, nil)

			assert.Empty(t, extra, "sizing a sharded launch needs more than GPU 0")
		})
	}
	test("--tensor-parallel-size")
	test("--pipeline-parallel-size")
}

func TestAutoContext_GivesAModelNotInTheCacheWhatIsFree(t *testing.T) {
	withGPU(t, 48, 20)

	extra, _ := planHFContext([]string{"--model", "org/not-downloaded"}, t.TempDir(), 65536, nil, nil)

	assert.NotContains(t, extra, "--max-model-len", "an unknown model's limits are unknown")
	assert.LessOrEqual(t, utilOf(t, extra), 19.0/48.0+0.001, "must leave a margin of the 20 GiB free")
}

func TestAutoContext_BoundsTheWindowWithoutAGPUReading(t *testing.T) {
	withoutGPU(t)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)

	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, cache, 65536, nil, nil)

	assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"), "the window does not depend on the GPU reading")
	assert.NotContains(t, extra, "--gpu-memory-utilization", "without a reading the GPU share is left to vLLM")
}

func TestAutoContext_WrapsOverridesForAMultimodalConfig(t *testing.T) {
	withGPU(t, 48, 48)
	cfg := `{"architectures":["Qwen3VLForConditionalGeneration"],"text_config":` + qwen8BConfig + `}`
	cache := cachedModel(t, "Qwen/VL", cfg, 16_400_000_000)

	extra, _ := planHFContext([]string{"--model", "Qwen/VL"}, cache, 65536, nil, nil)

	var overrides map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(flagOf(t, extra, "--hf-overrides")), &overrides))
	assert.Contains(t, overrides, "text_config", "a multimodal model's rope settings live under text_config")
}

// A snapshot that carries the same weights twice, in safetensors and as an
// unused PyTorch copy, is sized by the safetensors vLLM loads, not by what the
// snapshot takes on disk.
func TestAutoContext_SizesByTheWeightsVLLMLoadsNotTheDiskTheSnapshotUses(t *testing.T) {
	withGPU(t, 48, 48)
	single := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	both := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	repo := writeHFRepo(t, both, "Qwen/Qwen3-8B", map[string]string{"pytorch_model.bin": "x"})
	require.NoError(t, os.Truncate(filepath.Join(repo, "blobs", "blob-pytorch_model.bin"), 16_400_000_000))

	alone, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, single, 65536, nil, nil)
	withCopy, _ := planHFContext([]string{"--model", "Qwen/Qwen3-8B"}, both, 65536, nil, nil)

	assert.Equal(t, alone, withCopy)
	models, err := scanHFCache(both)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Greater(t, models[0].Size, int64(32_000_000_000), "the cache list still shows what the snapshot takes on disk")
}

// hfRevisions writes the cache Kaylee's example describes: two snapshots of one
// model, with refs/main pointing at the newer.
func hfRevisions(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	short := strings.Replace(qwen8BConfig, "40960", "8192", 1)
	long := strings.Replace(qwen8BConfig, "40960", "131072", 1)
	repo := writeHFRepo(t, cache, "org/m", llmFiles(map[string]string{"model.safetensors": "w"}))
	require.NoError(t, os.Rename(filepath.Join(repo, "snapshots", "abc123"), filepath.Join(repo, "snapshots", "aaa111")))
	writeHFRepo(t, cache, "org/m", llmFiles(map[string]string{"model.safetensors": "w"}))
	require.NoError(t, os.Rename(filepath.Join(repo, "snapshots", "abc123"), filepath.Join(repo, "snapshots", "bbb222")))
	// Each revision has its own config.json; the other files are shared blobs.
	for revision, config := range map[string]string{"aaa111": short, "bbb222": long} {
		path := filepath.Join(repo, "snapshots", revision, "config.json")
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.WriteFile(path, []byte(config), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(repo, "refs", "main"), []byte("bbb222"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "refs", "v1"), []byte("aaa111"), 0o644))
	return cache
}

func TestHFSnapshotDir_ResolvesARevisionTheWayHuggingFaceDoes(t *testing.T) {
	cache := hfRevisions(t)
	resolves := func(name, revision, wantSnapshot string) {
		t.Run(name, func(t *testing.T) {
			dir, ok := hfSnapshotDir(cache, "org/m", revision)

			require.True(t, ok)
			assert.Equal(t, filepath.Join(cache, "models--org--m", "snapshots", wantSnapshot), dir)
		})
	}
	missing := func(name, revision string) {
		t.Run(name, func(t *testing.T) {
			_, ok := hfSnapshotDir(cache, "org/m", revision)

			assert.False(t, ok)
		})
	}
	resolves("main through refs, not the first snapshot by name", "main", "bbb222")
	resolves("a tag through refs", "v1", "aaa111")
	resolves("a commit names its snapshot", "aaa111", "aaa111")
	missing("a revision that is not cached", "ccc333")
	missing("a revision that is not a name in the cache", "../../models--org--m")
}

// Automatic configuration reads the revision vLLM loads: refs/main by default,
// or --revision. A revision that is not cached is not configured from another.
func TestAutoContext_ReadsTheRevisionVLLMLoads(t *testing.T) {
	withGPU(t, 48, 48)
	cache := hfRevisions(t)

	t.Run("main", func(t *testing.T) {
		extra, _ := planHFContext([]string{"--model", "org/m"}, cache, 65536, nil, nil)

		assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"))
		assert.NotContains(t, extra, "--hf-overrides", "main's 131072-token window needs no rope override")
	})
	t.Run("an explicit older revision", func(t *testing.T) {
		extra, _ := planHFContext([]string{"--model", "org/m", "--revision", "aaa111"}, cache, 65536, nil, nil)

		assert.Contains(t, extra, "--hf-overrides", "aaa111's 8192-token window is extended to the target")
	})
	t.Run("a revision that is not cached", func(t *testing.T) {
		extra, _ := planHFContext([]string{"--model", "org/m", "--revision", "ccc333"}, cache, 65536, nil, nil)

		assert.NotContains(t, extra, "--max-model-len", "no other revision's config is read")
	})
}

func TestApplyAutoLaunch_AppendsAfterTheUsersArguments(t *testing.T) {
	withGPU(t, 48, 48)
	// The engine's hub cache.
	parent := t.TempDir()
	cachedModelAt(t, filepath.Join(parent, "hub"), "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	rt := Runtime{AutoLaunch: &AutoLaunch{Kind: autoLaunchHFContext, ContextTokens: 65536}}
	in := launchCommand{Bin: "vllm", Args: []string{"serve", "--model", "Qwen/Qwen3-8B"}}

	out := applyAutoLaunch(t.Context(), "vllm", rt, in, filepath.Join(parent, "hub"), nil)

	assert.Equal(t, in.Args, out.Args[:3])
	assert.Equal(t, "65536", flagOf(t, out.Args, "--max-model-len"))
	assert.Len(t, in.Args, 3, "the caller's arguments were modified")
	assert.Equal(t, in.Args, applyAutoLaunch(t.Context(), "vllm", Runtime{}, in, filepath.Join(parent, "hub"), nil).Args, "an engine without auto_launch must be untouched")
}

func TestPlanToolCalling_UsesTheParserThatReadsTheModelsToolCalls(t *testing.T) {
	cache := cachedModel(t, "org/m", qwen8BConfig, 1_000_000)
	var probedSnapshot string
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		probedSnapshot = snapshot
		return toolParserProbe{Matches: []string{"ernie45", "granite4", "hermes", "qwen3_json"}}, nil
	})

	extra, parser := planToolCalling(t.Context(), []string{"--model", "org/m"}, cache, "/opt/vllm/venv/bin/vllm", nil)

	assert.Equal(t, "hermes", parser)
	assert.Equal(t, "hermes", flagOf(t, extra, "--tool-call-parser"))
	assert.Contains(t, extra, "--enable-auto-tool-choice")
	assert.Equal(t, filepath.Join(cache, "models--org--m", "snapshots", "abc123"), probedSnapshot)
}

func TestPlanToolCalling_PrefersTheModelsOwnResponseTemplate(t *testing.T) {
	cache := cachedModel(t, "org/m", qwen8BConfig, 1_000_000)
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		return toolParserProbe{HF: true, Matches: []string{"hermes"}}, nil
	})

	extra, _ := planToolCalling(t.Context(), []string{"--model", "org/m"}, cache, "vllm", nil)

	assert.Equal(t, "hf", flagOf(t, extra, "--tool-call-parser"), "the hf parser for a model that ships response_template")
}

func TestPlanToolCalling_AddsNothingWhenNoParserCanBeChosen(t *testing.T) {
	cache := cachedModel(t, "org/m", qwen8BConfig, 1_000_000)
	test := func(name, model string, probe func(bin, snapshot string) (toolParserProbe, error)) {
		t.Run(name, func(t *testing.T) {
			withToolParserProbe(t, probe)

			extra, parser := planToolCalling(t.Context(), []string{"--model", model}, cache, "vllm", nil)

			assert.Empty(t, extra)
			assert.Empty(t, parser)
		})
	}
	test("no parser reads the format", "org/m", func(bin, snapshot string) (toolParserProbe, error) { return toolParserProbe{}, nil })
	test("the probe fails", "org/m", func(bin, snapshot string) (toolParserProbe, error) { return toolParserProbe{}, errors.New("no python") })
	test("the model is not cached and cannot be fetched", "org/not-cached", func(bin, snapshot string) (toolParserProbe, error) {
		t.Fatal("a model with no template has nothing to probe")
		return toolParserProbe{}, nil
	})
}

// A model this start downloads has its tokenizer and chat template fetched
// first, so its first start gets the same tool-call parser as every later one.
// Without that, vLLM would serve the model and refuse agent tool requests until
// a restart.
func TestPlanToolCalling_ChoosesTheParserOnTheFirstStartOfAnUncachedModel(t *testing.T) {
	cache := t.TempDir()
	var probedSnapshot string
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		probedSnapshot = snapshot
		return toolParserProbe{Matches: []string{"hermes"}}, nil
	})

	extra, parser := planToolCalling(t.Context(), []string{"--model", "org/new"}, cache, "vllm", pendingModel(t, cache, "org/new", qwen8BConfig, 1_000_000))

	assert.Equal(t, "hermes", parser)
	assert.Contains(t, extra, "--enable-auto-tool-choice")
	assert.Equal(t, filepath.Join(cache, "models--org--new", "snapshots", "abc123"), probedSnapshot)
}

func TestPlanToolCalling_KeepsTheParserTheUserChose(t *testing.T) {
	cache := t.TempDir()
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		t.Fatal("a parser the user named needs no probe")
		return toolParserProbe{}, nil
	})

	extra, _ := planToolCalling(t.Context(), []string{"--model", "org/q", "--tool-call-parser", "qwen3_xml", "--enable-auto-tool-choice"}, cache, "vllm", nil)
	assert.Empty(t, extra, "the user set both")

	extra, parser := planToolCalling(t.Context(), []string{"--model", "org/q", "--tool-call-parser", "qwen3_xml"}, cache, "vllm", nil)
	assert.Equal(t, "qwen3_xml", parser)
	assert.Equal(t, []string{"--enable-auto-tool-choice"}, extra, "only the enable flag beside the user's parser")
}

func TestParseGPUMemory_ReadsADiscreteGPUAndFallsBackOnUnifiedMemory(t *testing.T) {
	system := func() (int64, int64, bool) { return 128 * gib, 100 * gib, true }
	reads := func(name, out string, wantTotal, wantFree int64) {
		t.Run(name, func(t *testing.T) {
			total, free, ok := parseGPUMemory(out, system)

			require.True(t, ok)
			assert.Equal(t, wantTotal, total)
			assert.Equal(t, wantFree, free)
		})
	}
	unreadable := func(name, out string) {
		t.Run(name, func(t *testing.T) {
			_, _, ok := parseGPUMemory(out, system)

			assert.False(t, ok)
		})
	}
	reads("discrete GPU, in MiB", "49140, 48600\n", 49140<<20, 48600<<20)
	unreadable("multiple GPUs without an identified CUDA device", "24576, 20000\n49140, 48600\n")
	reads("DGX Spark reports N/A", "[N/A], [N/A]\n", 128*gib, 100*gib)
	reads("not supported", "[Not Supported], [Not Supported]\n", 128*gib, 100*gib)
	unreadable("malformed", "error\n")
	unreadable("empty", "")
}

func TestParseMeminfo_ReadsTotalAndAvailable(t *testing.T) {
	text := "MemTotal:       130567000 kB\nMemFree:         2000000 kB\nMemAvailable:   104000000 kB\nBuffers:          100 kB\n"

	total, available, ok := parseMeminfo(text)

	require.True(t, ok)
	assert.Equal(t, int64(130567000*1024), total)
	assert.Equal(t, int64(104000000*1024), available)
	_, _, ok = parseMeminfo("MemTotal: 100 kB\n")
	assert.False(t, ok, "a reading without MemAvailable must not count as known")
}

// qwen35_4BConfig is the part of Qwen/Qwen3.5-4B's config.json that sizing
// reads: a vision-language model whose language model sits in text_config.
const qwen35_4BConfig = `{"model_type":"qwen3_5","text_config":{"max_position_embeddings":262144,"num_hidden_layers":32,"num_attention_heads":16,"num_key_value_heads":4,"head_dim":256,"hidden_size":2560,"dtype":"bfloat16"}}`

// qwen35_4BBytes is about the weights a download of Qwen/Qwen3.5-4B fetches.
const qwen35_4BBytes = 9_440_000_000

func TestAutoContext_FirstStartOfAnUncachedModelGetsRoomForItsWeights(t *testing.T) {
	// The default model on a 16 GiB GPU, before vLLM has downloaded it. A blind
	// half share (8 GiB) cannot even hold the weights.
	withGPU(t, 16, 15)
	cache := t.TempDir()

	extra, note := planHFContext([]string{"--model", "Qwen/Qwen3.5-4B"}, cache, 65536, pendingModel(t, cache, "Qwen/Qwen3.5-4B", qwen35_4BConfig, qwen35_4BBytes), nil)

	weightsAndOverhead := float64(qwen35_4BBytes)*weightsSlack + float64(vllmOverhead)
	assert.GreaterOrEqual(t, utilOf(t, extra)*16*float64(gib), weightsAndOverhead, "the share must hold the weights and overhead: %v", extra)
	assert.LessOrEqual(t, utilOf(t, extra), 14.0/16.0+0.001, "must leave a margin of the 15 GiB free")
	assert.NotEmpty(t, flagOf(t, extra, "--max-model-len"), "without it vLLM would try the model's full 262144-token window")
	assert.Contains(t, note, "sized from Hugging Face")
}

func TestAutoContext_DoesNotFetchACachedModel(t *testing.T) {
	withGPU(t, 48, 48)
	cache := cachedModel(t, "Qwen/Qwen3-8B", qwen8BConfig, 16_400_000_000)
	fetch := func() (hfPendingModel, bool) {
		t.Fatal("fetched a model although it is cached")
		return hfPendingModel{}, false
	}

	extra, _ := planHFContext([]string{"--model", "Qwen/Qwen3.5-8B", "--model", "Qwen/Qwen3-8B"}, cache, 65536, fetch, nil)

	assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"))
}

// hfFetch is a metadata fetch against a fake Hugging Face API and a fake hf
// tool beside the engine binary. The tool records its arguments and HF_TOKEN
// and writes each file it is asked for, with config.json from FAKE_HF_CONFIG.
type hfFetch struct {
	e        *Executor
	st       *engineState
	hub      string
	requests []*http.Request
	mu       sync.Mutex
}

const fakeHFMetadataDownload = `
id=$2; shift 2
files=""
while [ $# -gt 0 ]; do
  case "$1" in
    --cache-dir) dir=$2; shift 2 ;;
    --revision) rev=$2; shift 2 ;;
    *) files="$files $1"; shift ;;
  esac
done
echo "$id$files --revision $rev" >> "$(dirname "$dir")/hf-calls"
printf '%s' "${HF_TOKEN:-}" > "$(dirname "$dir")/hf-token"
repo="$dir/models--$(echo "$id" | sed 's#/#--#g')"
mkdir -p "$repo/refs" "$repo/snapshots/fetched1"
printf fetched1 > "$repo/refs/$rev"
for f in $files; do
  case "$f" in
    config.json) printf '%s' "$FAKE_HF_CONFIG" > "$repo/snapshots/fetched1/$f" ;;
    model.safetensors.index.json) printf '%s' "$FAKE_HF_INDEX" > "$repo/snapshots/fetched1/$f" ;;
    *) printf '{}' > "$repo/snapshots/fetched1/$f" ;;
  esac
done
`

func newHFFetch(t *testing.T, status int, siblings []hfSibling) *hfFetch {
	t.Helper()
	e, hub, _ := hfPullFixture(t, siblings, status, fakeHFMetadataDownload)
	st, err := e.state("vllm")
	require.NoError(t, err)
	f := &hfFetch{e: e, st: st, hub: hub}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "x", status)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"siblings": siblings}))
	}))
	t.Cleanup(api.Close)
	old := hfDefaultEndpoint
	hfDefaultEndpoint = api.URL
	t.Cleanup(func() { hfDefaultEndpoint = old })
	t.Setenv("FAKE_HF_CONFIG", qwen35_4BConfig)
	t.Setenv("FAKE_HF_INDEX", "")
	t.Setenv("HF_HUB_OFFLINE", "")
	t.Setenv("TRANSFORMERS_OFFLINE", "")
	t.Setenv("HF_HUB_DISABLE_IMPLICIT_TOKEN", "")
	t.Setenv("HF_ENDPOINT", "")
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", "")
	// No token a real `hf auth login` stored may reach the fake server.
	t.Setenv("HF_HOME", t.TempDir())
	return f
}

func (f *hfFetch) fetch(t *testing.T, id, revision string) (hfPendingModel, bool) {
	t.Helper()
	return f.e.fetchHFMetadata(t.Context(), f.st, f.hub, id, revision, engineHFEnv(f.st))
}

func (f *hfFetch) setLaunchEnv(assignments ...string) {
	f.st.mu.Lock()
	f.st.plat.Runtime.LaunchEnv = assignments
	f.st.mu.Unlock()
}

func (f *hfFetch) toolCalls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(f.hub), "hf-calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

var qwen35Siblings = []hfSibling{
	{RFilename: "config.json", Size: 2_000},
	{RFilename: "model.safetensors-00001-of-00002.safetensors", Size: 5_000_000_000},
	{RFilename: "model.safetensors-00002-of-00002.safetensors", Size: 4_440_000_000},
	{RFilename: "tokenizer.json", Size: 8_000_000},
	{RFilename: "chat_template.jinja", Size: 7_000},
	{RFilename: "model.gguf", Size: 3_000_000_000}, // not downloaded, so not counted
}

func TestFetchHFMetadata_PutsTheSmallFilesInTheCacheAtTheRevision(t *testing.T) {
	f := newHFFetch(t, http.StatusOK, qwen35Siblings)

	pending, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "v2")

	require.True(t, ok)
	assert.Equal(t, int64(5_000_000_000+4_440_000_000), pending.weights, "the weights vLLM loads, not the other files")
	assert.Equal(t, filepath.Join(f.hub, "models--Qwen--Qwen3.5-4B", "snapshots", "fetched1"), pending.snapshot)
	assert.Equal(t, []string{"Qwen/Qwen3.5-4B config.json tokenizer.json chat_template.jinja --revision v2"}, f.toolCalls(t), "only the small files, at the revision")
	require.Len(t, f.requests, 1)
	assert.Equal(t, "/api/models/Qwen/Qwen3.5-4B/revision/v2", f.requests[0].URL.Path)
	cfg, ok := readHFConfig(pending.snapshot)
	require.True(t, ok)
	native, layers, _, _, _ := cfg.text().kvShape()
	assert.Equal(t, int64(262144), native)
	assert.Equal(t, int64(32), layers)
}

// A repository that ships its weights twice, as indexed shards and as one
// file, is sized by the shards its index lists, the copy vLLM loads.
func TestFetchHFMetadata_SizesOnlyTheWeightsTheIndexLists(t *testing.T) {
	f := newHFFetch(t, http.StatusOK, []hfSibling{
		{RFilename: "config.json", Size: 2_000},
		{RFilename: "tokenizer.json", Size: 8_000_000},
		{RFilename: "model-00001-of-00002.safetensors", Size: 5_000_000_000},
		{RFilename: "model-00002-of-00002.safetensors", Size: 4_440_000_000},
		{RFilename: "model.safetensors.index.json", Size: 30_000},
		{RFilename: "model.safetensors", Size: 9_440_000_000}, // the same weights again
	})
	t.Setenv("FAKE_HF_INDEX", `{"weight_map": {"a": "model-00001-of-00002.safetensors", "b": "model-00002-of-00002.safetensors"}}`)

	pending, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "main")

	require.True(t, ok)
	assert.Equal(t, int64(5_000_000_000+4_440_000_000), pending.weights, "the indexed shards, not both copies")
	assert.Equal(t, []string{"Qwen/Qwen3.5-4B config.json tokenizer.json model.safetensors.index.json --revision main"}, f.toolCalls(t), "the index comes down with the metadata")
}

// HF_HUB_OFFLINE or HF_TOKEN set in the engine's launch settings applies to
// PAIR's own Hugging Face requests and hf runs, as it does to vLLM's.
func TestFetchHFMetadata_UsesTheEnginesHuggingFaceSettings(t *testing.T) {
	t.Run("offline in the engine settings", func(t *testing.T) {
		f := newHFFetch(t, http.StatusOK, qwen35Siblings)
		f.setLaunchEnv("HF_HUB_OFFLINE=1")

		_, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "main")

		assert.False(t, ok)
		assert.Empty(t, f.requests, "no request may reach Hugging Face")
		assert.Empty(t, f.toolCalls(t), "the hf tool must not run")
	})
	t.Run("TRANSFORMERS_OFFLINE in the engine settings", func(t *testing.T) {
		f := newHFFetch(t, http.StatusOK, qwen35Siblings)
		f.setLaunchEnv("TRANSFORMERS_OFFLINE=1")

		_, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "main")

		assert.False(t, ok)
		assert.Empty(t, f.requests, "no request may reach Hugging Face")
		assert.Empty(t, f.toolCalls(t), "the hf tool must not run")
	})
	t.Run("implicit token disabled in the engine settings", func(t *testing.T) {
		f := newHFFetch(t, http.StatusOK, qwen35Siblings)
		home := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(home, "token"), []byte("stored-token\n"), 0o600))
		f.setLaunchEnv("HF_HUB_DISABLE_IMPLICIT_TOKEN=1", "HF_TOKEN=engine-token", "HF_HOME="+home)

		_, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "main")

		require.True(t, ok)
		require.Len(t, f.requests, 1)
		assert.Empty(t, f.requests[0].Header.Get("Authorization"), "neither HF_TOKEN nor a stored token is sent")
	})
	t.Run("a token in the engine settings", func(t *testing.T) {
		f := newHFFetch(t, http.StatusOK, qwen35Siblings)
		f.setLaunchEnv("HF_TOKEN=engine-token")

		_, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "main")

		require.True(t, ok)
		require.Len(t, f.requests, 1)
		assert.Equal(t, "Bearer engine-token", f.requests[0].Header.Get("Authorization"))
		token, err := os.ReadFile(filepath.Join(filepath.Dir(f.hub), "hf-token"))
		require.NoError(t, err)
		assert.Equal(t, "engine-token", string(token), "the hf tool gets the engine's token")
	})
	t.Run("a token stored by hf auth login", func(t *testing.T) {
		f := newHFFetch(t, http.StatusOK, qwen35Siblings)
		home := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(home, "token"), []byte("stored-token\n"), 0o600))
		f.setLaunchEnv("HF_HOME=" + home)

		_, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "main")

		require.True(t, ok)
		require.Len(t, f.requests, 1)
		assert.Equal(t, "Bearer stored-token", f.requests[0].Header.Get("Authorization"))
	})
	t.Run("a mirror in the engine settings", func(t *testing.T) {
		f := newHFFetch(t, http.StatusOK, qwen35Siblings)
		mirror := hfDefaultEndpoint
		hfDefaultEndpoint = "http://127.0.0.1:1" // nothing listens: the Hub must not be asked
		f.setLaunchEnv("HF_ENDPOINT=" + mirror + "/")

		_, ok := f.fetch(t, "Qwen/Qwen3.5-4B", "main")

		require.True(t, ok)
		require.Len(t, f.requests, 1, "the listing came from the mirror")
		assert.Equal(t, "/api/models/Qwen/Qwen3.5-4B", f.requests[0].URL.Path)
	})
}

func TestFetchHFMetadata_FallsBackWhenHuggingFaceCannotAnswer(t *testing.T) {
	test := func(name string, status int, offline string) {
		t.Run(name, func(t *testing.T) {
			f := newHFFetch(t, status, qwen35Siblings)
			t.Setenv("HF_HUB_OFFLINE", offline)

			_, ok := f.fetch(t, "org/model", "main")
			assert.False(t, ok)
			assert.Empty(t, f.toolCalls(t))

			// The start then falls back to the capped share, as before: 15 GiB
			// free less the 1 GiB margin is 0.87 of 16 GiB.
			withGPU(t, 16, 15)
			extra, _ := planHFContext([]string{"--model", "org/model"}, f.hub, 65536, func() (hfPendingModel, bool) {
				return f.fetch(t, "org/model", "main")
			}, nil)
			assert.InDelta(t, 0.87, utilOf(t, extra), 0.0001)
			assert.NotContains(t, extra, "--max-model-len")
		})
	}
	test("model not found", http.StatusNotFound, "")
	test("gated model without a token", http.StatusUnauthorized, "")
	test("server error", http.StatusInternalServerError, "")
	test("HF_HUB_OFFLINE set for the service", http.StatusOK, "1")
}

func TestApplyAutoLaunch_LimitsConcurrentRequestsUnlessTheUserSetsALimit(t *testing.T) {
	withoutGPU(t)
	test := func(name string, args []string, limit int, want string) {
		t.Run(name, func(t *testing.T) {
			rt := Runtime{AutoLaunch: &AutoLaunch{Kind: autoLaunchHFContext, ContextTokens: 65536, MaxNumSeqs: limit}}

			out := applyAutoLaunch(t.Context(), "vllm", rt, launchCommand{Bin: "vllm", Args: args}, t.TempDir(), nil)

			assert.Equal(t, want, flagOf(t, out.Args, "--max-num-seqs"))
			assert.LessOrEqual(t, strings.Count(strings.Join(out.Args, " "), "--max-num-seqs"), 1)
		})
	}
	test("limit added", []string{"serve", "--model", "org/m"}, 32, "32")
	test("the user's own limit wins", []string{"serve", "--model", "org/m", "--max-num-seqs", "4"}, 32, "4")
	test("the user's own limit in = form wins", []string{"serve", "--model", "org/m", "--max-num-seqs=64"}, 32, "64")
	test("a sharded launch gets it too", []string{"serve", "--model", "org/m", "--tensor-parallel-size", "2"}, 32, "32")
	test("no limit in the manifest leaves vLLM's default", []string{"serve", "--model", "org/m"}, 0, "")
}

func TestBundledVLLMLaunch_FirstStartOfTheDefaultModelFitsA16GiBGPU(t *testing.T) {
	// The default model on a 16 GiB GPU with an empty cache, through the bundled
	// manifest. Each added argument is one vLLM needs to start there: the share
	// must hold the weights, the context must be bounded, and the concurrent
	// requests must leave room for Qwen3.5's per-request state.
	withGPU(t, 16, 15)
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	require.True(t, ok, "vllm manifest not loaded")
	rt := manifest.Platforms["linux/amd64"].Runtime
	parent := t.TempDir()
	hub := filepath.Join(parent, "hub")
	in := launchCommand{Bin: "vllm", Args: []string{"serve", "--model", "Qwen/Qwen3.5-4B"}}

	out := applyAutoLaunch(t.Context(), "vllm", rt, in, hub, pendingModel(t, hub, "Qwen/Qwen3.5-4B", qwen35_4BConfig, qwen35_4BBytes))

	weightsAndOverhead := float64(qwen35_4BBytes)*weightsSlack + float64(vllmOverhead)
	assert.GreaterOrEqual(t, utilOf(t, out.Args)*16*float64(gib), weightsAndOverhead, "the share cannot hold the weights and overhead: %v", out.Args)
	assert.NotEmpty(t, flagOf(t, out.Args, "--max-model-len"))
	assert.Equal(t, "32", flagOf(t, out.Args, "--max-num-seqs"), "vLLM's default of 256 does not start on this GPU")
}

// The probe reads the chat format vLLM will use: the tokenizer --tokenizer and
// --tokenizer-revision name, and the --chat-template the launch gives. When
// that cannot be read here, no parser is chosen rather than one read from the
// model's own template, which vLLM would not be using.
func TestPlanToolCalling_ProbesTheTokenizerAndTemplateVLLMWillUse(t *testing.T) {
	cache := cachedModel(t, "org/m", qwen8BConfig, 1_000_000)
	writeHFRepo(t, cache, "org/tok", map[string]string{"tokenizer.json": "{}", "tokenizer_config.json": "{}"})
	template := filepath.Join(t.TempDir(), "tools.jinja")
	require.NoError(t, os.WriteFile(template, []byte("{{ messages }}"), 0o644))
	snapshot := func(id string) string {
		return filepath.Join(cache, "models--"+strings.ReplaceAll(id, "/", "--"), "snapshots", "abc123")
	}
	probes := func(t *testing.T) *[][2]string {
		t.Helper()
		var seen [][2]string
		old := probeToolParsers
		probeToolParsers = func(_ context.Context, _, tokenizer, template string) (toolParserProbe, error) {
			seen = append(seen, [2]string{tokenizer, template})
			return toolParserProbe{Matches: []string{"hermes"}}, nil
		}
		toolParserCache.Clear()
		t.Cleanup(func() {
			probeToolParsers = old
			toolParserCache.Clear()
		})
		return &seen
	}
	probed := func(name string, extra []string, wantTokenizer, wantTemplate string) {
		t.Run(name, func(t *testing.T) {
			seen := probes(t)

			tools, parser := planToolCalling(t.Context(), append([]string{"--model", "org/m"}, extra...), cache, "vllm", nil)

			assert.Equal(t, "hermes", parser)
			assert.Contains(t, tools, "--tool-call-parser")
			assert.Equal(t, [][2]string{{wantTokenizer, wantTemplate}}, *seen)
		})
	}
	noParser := func(name string, extra []string) {
		t.Run(name, func(t *testing.T) {
			seen := probes(t)

			tools, parser := planToolCalling(t.Context(), append([]string{"--model", "org/m"}, extra...), cache, "vllm", nil)

			assert.Empty(t, tools)
			assert.Empty(t, parser)
			assert.Empty(t, *seen, "nothing to probe")
		})
	}
	probed("the model's own tokenizer and template", nil, snapshot("org/m"), "")
	probed("another cached tokenizer repository", []string{"--tokenizer", "org/tok"}, snapshot("org/tok"), "")
	probed("the model's revision named as the tokenizer revision", []string{"--tokenizer-revision", "main"}, snapshot("org/m"), "")
	probed("a tokenizer directory", []string{"--tokenizer", cache}, cache, "")
	probed("a chat template file", []string{"--chat-template", template}, snapshot("org/m"), template)
	probed("a chat template given inline", []string{"--chat-template", "{% for m in messages %}{{ m.content }}{% endfor %}"}, snapshot("org/m"), "{% for m in messages %}{{ m.content }}{% endfor %}")
	noParser("a tokenizer repository that is not cached", []string{"--tokenizer", "org/not-cached"})
	noParser("a tokenizer revision that is not cached", []string{"--tokenizer-revision", "v9"})
	noParser("a chat template given as a relative path", []string{"--chat-template", "tools.jinja"})
}

func TestAutoContext_KeepsTheNativeLimitWithUserConfigOverrides(t *testing.T) {
	withGPU(t, 48, 48)
	// A first-start metadata fixture needs no cache symlinks, including on Windows.
	snapshot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(snapshot, "config.json"), []byte(qwen8BConfig), 0o644))
	fetch := func() (hfPendingModel, bool) {
		return hfPendingModel{snapshot: snapshot, weights: 16_400_000_000}, true
	}
	args := []string{"--model", "Qwen/Qwen3-8B", "--hf-overrides", `{"attention_dropout":0.0}`}

	extra, _ := planHFContext(args, t.TempDir(), 65536, fetch, nil)

	assert.Equal(t, "40960", flagOf(t, extra, "--max-model-len"))
	assert.NotContains(t, extra, "--hf-overrides")
}

func TestApplyAutoLaunch_UsesTheEngineDeviceEnvironment(t *testing.T) {
	env := map[string]string{"CUDA_VISIBLE_DEVICES": "1"}
	old := gpuMemory
	var received map[string]string
	gpuMemory = func(got map[string]string) (int64, int64, bool) {
		received = got
		return 0, 0, false
	}
	t.Cleanup(func() { gpuMemory = old })
	rt := Runtime{AutoLaunch: &AutoLaunch{Kind: autoLaunchHFContext, ContextTokens: 65536}}
	command := launchCommand{
		Args: []string{"--model", "org/model", "--tool-call-parser", "hermes", "--enable-auto-tool-choice"},
		Env:  env,
	}

	launch := applyAutoLaunch(t.Context(), "vllm", rt, command, t.TempDir(), nil)

	assert.Equal(t, env, received)
	assert.NotContains(t, launch.Args, "--gpu-memory-utilization")
}

func TestNvidiaSMIMemory_LeavesExplicitDeviceSelectionToTheEngine(t *testing.T) {
	// Register restoration before testing without an inherited selection.
	t.Setenv("CUDA_VISIBLE_DEVICES", "")
	require.NoError(t, os.Unsetenv("CUDA_VISIBLE_DEVICES"))
	test := func(name string, env map[string]string) {
		t.Run(name, func(t *testing.T) {
			total, free, ok := nvidiaSMIMemory(env)

			assert.False(t, ok)
			assert.Zero(t, total)
			assert.Zero(t, free)
		})
	}
	test("selected device", map[string]string{"CUDA_VISIBLE_DEVICES": "1"})
	test("no visible devices", map[string]string{"CUDA_VISIBLE_DEVICES": ""})
	t.Setenv("CUDA_VISIBLE_DEVICES", "1")
	test("inherited selection", nil)
}

func TestAutoContext_LeavesExplicitDtypeMemorySizingToTheEngine(t *testing.T) {
	withGPU(t, 80, 80)
	// A first-start metadata fixture needs no cache symlinks, including on Windows.
	snapshot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(snapshot, "config.json"), []byte(qwen8BConfig), 0o644))
	fetch := func() (hfPendingModel, bool) {
		return hfPendingModel{snapshot: snapshot, weights: 16_400_000_000}, true
	}
	test := func(name, flag, value string) {
		t.Run(name, func(t *testing.T) {
			args := []string{"--model", "Qwen/Qwen3-8B", flag, value}

			extra, _ := planHFContext(args, t.TempDir(), 65536, fetch, nil)

			assert.NotContains(t, extra, "--gpu-memory-utilization")
			assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"))
		})
	}
	test("float32 weights and cache", "--dtype", "float32")
	test("explicit half precision", "--dtype", "half")
	test("quantized cache", "--kv-cache-dtype", "fp8")
}

func TestAutoContext_PreservesTheUnderscoreContextLimit(t *testing.T) {
	withoutGPU(t)
	// A first-start metadata fixture needs no cache symlinks, including on Windows.
	snapshot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(snapshot, "config.json"), []byte(qwen8BConfig), 0o644))
	fetch := func() (hfPendingModel, bool) {
		return hfPendingModel{snapshot: snapshot, weights: 16_400_000_000}, true
	}
	args := []string{"--model", "Qwen/Qwen3-8B", "--max_model_len", "8192"}

	extra, _ := planHFContext(args, t.TempDir(), 65536, fetch, nil)

	assert.Empty(t, extra, "the explicit limit must prevent automatic context and rope overrides")
}
