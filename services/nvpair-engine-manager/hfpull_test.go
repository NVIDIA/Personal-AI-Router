// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectHFFiles_KeepsOnlyTheWeightsVLLMCanLoad(t *testing.T) {
	file := func(name string, size int64) hfSibling { return hfSibling{RFilename: name, Size: size} }
	t.Run("safetensors repo skips pickle weights and other formats", func(t *testing.T) {
		files, total, err := selectHFFiles([]hfSibling{
			file("config.json", 10), file("tokenizer.json", 20),
			file("model-00001-of-00002.safetensors", 1000), file("model-00002-of-00002.safetensors", 1000),
			file("model.safetensors.index.json", 5),
			file("pytorch_model.bin", 2000),
			file("original/params.json", 5), file("model.gguf", 3000), file("-rf", 1), file("../escape", 1),
		})

		require.NoError(t, err)
		assert.Equal(t, []string{"config.json", "tokenizer.json", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "model.safetensors.index.json"}, files)
		assert.Equal(t, int64(1000+1000), total)
	})
	t.Run("a repo with only pickle weights keeps them", func(t *testing.T) {
		files, _, err := selectHFFiles([]hfSibling{file("config.json", 1), file("pytorch_model.bin", 9)})

		require.NoError(t, err)
		assert.Contains(t, files, "pytorch_model.bin")
	})
	t.Run("only a consolidated file is kept when nothing else exists", func(t *testing.T) {
		files, _, err := selectHFFiles([]hfSibling{file("params.json", 1), file("consolidated.safetensors", 9)})

		require.NoError(t, err)
		assert.Contains(t, files, "consolidated.safetensors")
	})
	t.Run("a GGUF-only repo is refused", func(t *testing.T) {
		_, _, err := selectHFFiles([]hfSibling{file("config.json", 1), file("m.gguf", 9)})

		assert.Error(t, err, "nothing in the repo is something vLLM can load")
	})
}

func TestSelectHFFiles_DownloadsTheMistralFormatWeightsVLLMLoads(t *testing.T) {
	// Mistral's repositories carry each model twice, in Mistral's own format and
	// in Hugging Face's. vLLM's load_format auto loads consolidated*.safetensors
	// whenever a repository has any, so those are the weights to download.
	file := func(name string, size int64) hfSibling { return hfSibling{RFilename: name, Size: size} }
	test := func(name string, siblings []hfSibling, wantFiles []string, wantWeights int64) {
		t.Run(name, func(t *testing.T) {
			files, total, err := selectHFFiles(siblings)

			require.NoError(t, err)
			assert.Equal(t, wantFiles, files)
			assert.Equal(t, wantWeights, total)
		})
	}
	test("consolidated beside sharded Hugging Face weights",
		[]hfSibling{ // as mistralai/Mistral-Small-3.2-24B-Instruct-2506
			file("config.json", 1), file("params.json", 1), file("tekken.json", 2), file("tokenizer_config.json", 1),
			file("consolidated.safetensors", 48),
			file("model-00001-of-00002.safetensors", 24), file("model-00002-of-00002.safetensors", 24),
			file("model.safetensors.index.json", 1),
		},
		[]string{"config.json", "params.json", "tekken.json", "tokenizer_config.json", "consolidated.safetensors", "model.safetensors.index.json"}, 48)
	test("consolidated beside a single Hugging Face file",
		[]hfSibling{ // as mistralai/Ministral-3-3B-Instruct-2512
			file("config.json", 1), file("params.json", 1), file("tekken.json", 2), file("tokenizer.json", 1),
			file("consolidated.safetensors", 5), file("model.safetensors", 5),
		},
		[]string{"config.json", "params.json", "tekken.json", "tokenizer.json", "consolidated.safetensors"}, 5)
	test("sharded consolidated weights with their index",
		[]hfSibling{
			file("params.json", 1), file("tekken.json", 2),
			file("consolidated-00001-of-00002.safetensors", 24), file("consolidated-00002-of-00002.safetensors", 24),
			file("consolidated.safetensors.index.json", 1),
			file("model-00001-of-00002.safetensors", 24), file("model-00002-of-00002.safetensors", 24),
		},
		[]string{"params.json", "tekken.json", "consolidated-00001-of-00002.safetensors", "consolidated-00002-of-00002.safetensors", "consolidated.safetensors.index.json"}, 48)
}

// hfPullFixture builds an executor whose engine binary sits beside a fake `hf`
// tool, plus a fake Hugging Face API. script is the body of the fake tool.
func hfPullFixture(t *testing.T, siblings []hfSibling, apiStatus int, script string) (e *Executor, hub string, events *[]ProgressEvent) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the hf tool")
	}
	dir := t.TempDir()
	hub = filepath.Join(dir, "hub")
	bin := filepath.Join(dir, "vllm")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hf"), []byte("#!/bin/sh\n"+script), 0o755))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiStatus != http.StatusOK {
			http.Error(w, "x", apiStatus)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"siblings": siblings}))
	}))
	t.Cleanup(api.Close)
	old := hfAPIBase
	hfAPIBase = api.URL
	t.Cleanup(func() { hfAPIBase = old })

	m := testEngineManifest(bin)
	m.Engine = "vllm"
	m.Actions = map[string]Action{"pull_model": {HFPull: &ActionHFPull{Tool: "hf", Dir: hub}}}
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	var mu sync.Mutex
	collected := []ProgressEvent{}
	e = NewExecutor(reg, NewReporter(nil), func(method string, params any) {
		fields, ok := params.(map[string]any)
		if !ok || method != "engine:pull-progress" {
			return
		}
		ev := ProgressEvent{Stage: fields["stage"].(string)}
		if pct, ok := fields["percent"].(int); ok {
			ev.Percent = pct
		}
		mu.Lock()
		collected = append(collected, ev)
		mu.Unlock()
	}, t.TempDir())
	return e, hub, &collected
}

// The fake tool lays out a Hugging Face cache the way `hf download` does.
const fakeHFDownload = `
repo="$(echo "$@" | sed 's/.*--cache-dir //')/models--Qwen--Test"
mkdir -p "$repo/blobs" "$repo/snapshots/abc"
printf '{}' > "$repo/blobs/c"; printf '{}' > "$repo/blobs/t"; head -c 1000 /dev/zero > "$repo/blobs/w"
ln -s ../../blobs/c "$repo/snapshots/abc/config.json"
ln -s ../../blobs/t "$repo/snapshots/abc/tokenizer.json"
ln -s ../../blobs/w "$repo/snapshots/abc/model.safetensors"
`

func TestPullHFModel_DownloadsAndReportsSuccess(t *testing.T) {
	e, hub, events := hfPullFixture(t, []hfSibling{
		{RFilename: "config.json", Size: 2}, {RFilename: "tokenizer.json", Size: 2}, {RFilename: "model.safetensors", Size: 1000},
	}, http.StatusOK, fakeHFDownload)

	res, err := e.PullModelStream(context.Background(), "vllm", "Qwen/Test", nil)

	require.NoError(t, err)
	var result struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(res, &result))
	assert.Equal(t, "Qwen/Test", result.Model)
	models, err := scanHFCache(hub)
	require.NoError(t, err)
	require.Len(t, models, 1, "the downloaded model must show in the cache list")
	assert.Equal(t, "Qwen/Test", models[0].ID)
	require.NotEmpty(t, *events)
	assert.Equal(t, ProgressEvent{Stage: "success", Percent: 100}, (*events)[len(*events)-1])
}

func TestPullHFModel_ReportsTheToolsOwnMessageWhenTheDownloadFails(t *testing.T) {
	e, _, _ := hfPullFixture(t, []hfSibling{{RFilename: "model.safetensors", Size: 10}}, http.StatusOK,
		`echo "connection reset by peer" >&2; exit 1`)

	_, err := e.PullModelStream(context.Background(), "vllm", "Qwen/Test", nil)

	assert.ErrorContains(t, err, "connection reset by peer")
}

func TestPullHFModel_RefusesBadInputsBeforeDownloading(t *testing.T) {
	reject := func(name, model string, apiStatus int, wantMessage string) {
		t.Run(name, func(t *testing.T) {
			e, _, _ := hfPullFixture(t, nil, apiStatus, "touch ran-anyway; exit 0")

			_, err := e.PullModelStream(context.Background(), "vllm", model, nil)

			assert.ErrorContains(t, err, wantMessage)
		})
	}
	reject("not a repo id", "../etc/passwd", http.StatusOK, "valid Hugging Face model id")
	reject("option-looking id", "--help", http.StatusOK, "valid Hugging Face model id")
	reject("unknown repo", "Qwen/Nope", http.StatusNotFound, "no such model")
	reject("gated repo", "meta/Gated", http.StatusForbidden, "HF_TOKEN")
}

func TestPullHFModel_StopsPromptlyWhenCancelled(t *testing.T) {
	e, _, _ := hfPullFixture(t, []hfSibling{{RFilename: "model.safetensors", Size: 10}}, http.StatusOK, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()

	_, err := e.PullModelStream(ctx, "vllm", "Qwen/Test", nil)

	assert.Error(t, err, "a cancelled download must fail")
	assert.LessOrEqual(t, time.Since(start), 10*time.Second, "a cancelled download must stop promptly")
}

// A vllm on PATH is often a link into the environment it was installed in, as
// pipx and uv tool install it. hf sits beside the real binary, not the link.
func TestHFToolPath_FindsHFBesideTheEngineALinkPointsTo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("vLLM has no Windows install")
	}
	env := filepath.Join(t.TempDir(), "venv", "bin")
	bin := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(env, 0o755))
	require.NoError(t, os.MkdirAll(bin, 0o755))
	for _, name := range []string{"vllm", "hf"} {
		require.NoError(t, os.WriteFile(filepath.Join(env, name), []byte("#!/bin/sh\n"), 0o755))
	}
	link := filepath.Join(bin, "vllm")
	require.NoError(t, os.Symlink(filepath.Join(env, "vllm"), link))
	st := &engineState{binPath: link}

	tool, err := hfToolPath(st, "hf")

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(env, "hf"), tool)
}
