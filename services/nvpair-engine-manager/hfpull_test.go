// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	old := hfDefaultEndpoint
	hfDefaultEndpoint = api.URL
	t.Cleanup(func() { hfDefaultEndpoint = old })

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
	e, _, _ := hfPullFixture(t, oneFileRepo, http.StatusOK,
		`echo "connection reset by peer" >&2; exit 1`)

	_, err := e.PullModelStream(context.Background(), "vllm", "Qwen/Test", nil)

	assert.ErrorContains(t, err, "connection reset by peer")
}

// oneFileRepo is the smallest repository vLLM can load: config, tokenizer and
// one weights file.
var oneFileRepo = []hfSibling{{RFilename: "config.json", Size: 2}, {RFilename: "tokenizer.json", Size: 2}, {RFilename: "model.safetensors", Size: 10}}

// A repository whose files would not make a model vLLM can load is refused
// before anything is downloaded. It used to download in full, be reported
// incomplete, and stay in the cache where the model list does not show it.
func TestPullHFModel_RefusesALayoutVLLMCannotLoadBeforeDownloading(t *testing.T) {
	file := func(name string) hfSibling { return hfSibling{RFilename: name, Size: 10} }
	test := func(name string, siblings []hfSibling, wantErr string) {
		t.Run(name, func(t *testing.T) {
			e, hub, _ := hfPullFixture(t, siblings, http.StatusOK, `touch "$(dirname "$0")/ran-anyway"; exit 0`)

			_, err := e.PullModelStream(context.Background(), "vllm", "Qwen/Test", nil)

			if wantErr == "" {
				assert.NotContains(t, fmt.Sprint(err), "not one model")
				assert.FileExists(t, filepath.Join(filepath.Dir(hub), "ran-anyway"), "the download ran")
				return
			}
			assert.ErrorContains(t, err, wantErr)
			assert.NoFileExists(t, filepath.Join(filepath.Dir(hub), "ran-anyway"), "nothing was downloaded")
		})
	}
	test("two unnumbered weight files and no index",
		[]hfSibling{file("config.json"), file("tokenizer.json"), file("model.safetensors"), file("adapter_model.safetensors")},
		"2 weight files are not one model")
	test("a shard missing from a numbered set",
		[]hfSibling{file("config.json"), file("tokenizer.json"), file("model-00001-of-00003.safetensors"), file("model-00002-of-00003.safetensors")},
		"not one model")
	test("no tokenizer",
		[]hfSibling{file("config.json"), file("model.safetensors")},
		"lacks the config, tokenizer or weights")
	test("several files with an index", // the index is checked after the download
		[]hfSibling{file("config.json"), file("tokenizer.json"), file("a.safetensors"), file("b.safetensors"), file("model.safetensors.index.json")},
		"")
	test("a complete numbered set",
		[]hfSibling{file("config.json"), file("tokenizer.json"), file("model-00001-of-00002.safetensors"), file("model-00002-of-00002.safetensors")},
		"")
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
	e, _, _ := hfPullFixture(t, oneFileRepo, http.StatusOK, "sleep 30")
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

// PAIR reads the same Hugging Face settings vLLM's huggingface_hub reads, in
// the same order, from the engine's environment over the service's.
func TestHFEnv_ResolvesTheCacheEndpointAndTokenAsHuggingFaceHubDoes(t *testing.T) {
	for _, key := range []string{"HF_HOME", "HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "XDG_CACHE_HOME", "HF_ENDPOINT", "HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "HF_TOKEN_PATH"} {
		t.Setenv(key, "")
	}
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	cache := func(name string, env hfEnv, want string) {
		t.Run("cache: "+name, func(t *testing.T) {
			assert.Equal(t, want, env.hubCache())
		})
	}
	cache("HF_HUB_CACHE first", hfEnv{"HF_HUB_CACHE": "/hub", "HUGGINGFACE_HUB_CACHE": "/legacy", "HF_HOME": "/home"}, "/hub")
	cache("the legacy HUGGINGFACE_HUB_CACHE next", hfEnv{"HUGGINGFACE_HUB_CACHE": "/legacy", "HF_HOME": "/home"}, "/legacy")
	cache("then HF_HOME/hub", hfEnv{"HF_HOME": "/home"}, "/home/hub")
	cache("then XDG_CACHE_HOME/huggingface/hub", hfEnv{"XDG_CACHE_HOME": "/xdg"}, "/xdg/huggingface/hub")
	cache("else ~/.cache/huggingface/hub", hfEnv{}, filepath.Join(home, ".cache", "huggingface", "hub"))
	cache("~ expanded", hfEnv{"HF_HOME": "~/hf"}, filepath.Join(home, "hf", "hub"))
	cache("$VARS expanded from the engine's environment", hfEnv{"MODELS": "/models", "HF_HOME": "$MODELS/hf"}, "/models/hf/hub")

	endpoint := func(name string, env hfEnv, want string) {
		t.Run("endpoint: "+name, func(t *testing.T) {
			assert.Equal(t, want, env.endpoint())
		})
	}
	endpoint("the Hub by default", hfEnv{}, "https://huggingface.co")
	endpoint("a mirror, without its trailing slash", hfEnv{"HF_ENDPOINT": "https://mirror.example/"}, "https://mirror.example")

	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("from-file\n"), 0o600))
	hfHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hfHome, "token"), []byte("from-login\n"), 0o600))
	token := func(name string, env hfEnv, want string) {
		t.Run("token: "+name, func(t *testing.T) {
			assert.Equal(t, want, env.token())
		})
	}
	token("HF_TOKEN first", hfEnv{"HF_TOKEN": "env", "HUGGING_FACE_HUB_TOKEN": "legacy", "HF_HOME": hfHome}, "env")
	token("the legacy HUGGING_FACE_HUB_TOKEN next", hfEnv{"HUGGING_FACE_HUB_TOKEN": "legacy", "HF_HOME": hfHome}, "legacy")
	token("then HF_TOKEN_PATH", hfEnv{"HF_TOKEN_PATH": tokenFile, "HF_HOME": hfHome}, "from-file")
	token("then the token hf auth login stored in HF_HOME", hfEnv{"HF_HOME": hfHome}, "from-login")
	token("none", hfEnv{"HF_HOME": t.TempDir()}, "")
}

// The model list, Pull and Delete use the cache vLLM itself uses. With
// HF_HUB_CACHE set in its launch settings, that is not {models_dir}/hub.
func TestHFCacheAction_ListsTheCacheTheEngineUses(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	modelsDir := t.TempDir()
	elsewhere := t.TempDir()
	writeHFRepo(t, filepath.Join(modelsDir, "hub"), "org/default-place", llmFiles(map[string]string{"model.safetensors": "w"}))
	writeHFRepo(t, elsewhere, "org/engine-place", llmFiles(map[string]string{"model.safetensors": "w"}))
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Env = map[string]string{"HF_HOME": "{models_dir}"}
	p.ModelsDir = modelsDir
	m.Platforms[key] = p
	m.Actions["list_models"] = Action{HFCache: &ActionHFCache{Dir: "{" + hfHubCachePlaceholder + "}"}, Result: &ActionResult{Array: "data", Field: "id"}}
	require.NoError(t, m.Validate())
	e := newTestExecutor(t, m)
	st, err := e.state("fake")
	require.NoError(t, err)
	listed := func() []string {
		raw, err := e.Action(t.Context(), "fake", "list_models", nil)
		require.NoError(t, err)
		return extractStrings(raw, &ActionResult{Array: "data", Field: "id"})
	}

	assert.Equal(t, []string{"org/default-place"}, listed(), "by default the manifest's HF_HOME, {models_dir}/hub")

	st.mu.Lock()
	st.plat.Runtime.LaunchEnv = []string{"HF_HUB_CACHE=" + elsewhere}
	st.mu.Unlock()
	assert.Equal(t, []string{"org/engine-place"}, listed(), "HF_HUB_CACHE in the engine's launch settings")
}

func TestHFHubCachePlaceholder_IsOnlyForHuggingFaceActionDirs(t *testing.T) {
	accept := func(name string, edit func(*Manifest)) {
		t.Run(name, func(t *testing.T) {
			m := testEngineManifest(fakeEngineBin)
			edit(m)
			assert.NoError(t, m.Validate())
		})
	}
	reject := func(name string, edit func(*Manifest)) {
		t.Run(name, func(t *testing.T) {
			m := testEngineManifest(fakeEngineBin)
			edit(m)
			assert.ErrorContains(t, m.Validate(), "hf_hub_cache")
		})
	}
	accept("an hf_cache dir", func(m *Manifest) {
		m.Actions["list_models"] = Action{HFCache: &ActionHFCache{Dir: "{hf_hub_cache}"}}
	})
	accept("an hf_pull dir", func(m *Manifest) {
		m.Actions["pull_model"] = Action{HFPull: &ActionHFPull{Tool: "hf", Dir: "{hf_hub_cache}"}}
	})
	reject("a remove_path root", func(m *Manifest) {
		m.Actions["delete_model"] = Action{RemovePath: &ActionRemovePath{Root: "{hf_hub_cache}", Path: "{hf_hub_cache}/{model}"}}
	})
}
