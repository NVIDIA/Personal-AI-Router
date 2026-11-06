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
		}, nil)

		require.NoError(t, err)
		assert.Equal(t, []string{"config.json", "tokenizer.json", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "model.safetensors.index.json"}, files)
		assert.Equal(t, int64(1000+1000), total)
	})
	t.Run("a repo with only pickle weights keeps them", func(t *testing.T) {
		files, _, err := selectHFFiles([]hfSibling{file("config.json", 1), file("pytorch_model.bin", 9)}, nil)

		require.NoError(t, err)
		assert.Contains(t, files, "pytorch_model.bin")
	})
	t.Run("only a consolidated file is kept when nothing else exists", func(t *testing.T) {
		files, _, err := selectHFFiles([]hfSibling{file("params.json", 1), file("consolidated.safetensors", 9)}, nil)

		require.NoError(t, err)
		assert.Contains(t, files, "consolidated.safetensors")
	})
	t.Run("a GGUF-only repo is refused", func(t *testing.T) {
		_, _, err := selectHFFiles([]hfSibling{file("config.json", 1), file("m.gguf", 9)}, nil)

		assert.Error(t, err, "nothing in the repo is something vLLM can load")
	})
}

// A repository can ship the same weights twice, as indexed shards and as one
// file. vLLM loads only what the index lists, so only that copy is downloaded
// and counted.
func TestSelectHFFiles_KeepsOnlyTheShardsTheIndexLists(t *testing.T) {
	file := func(name string, size int64) hfSibling { return hfSibling{RFilename: name, Size: size} }
	siblings := []hfSibling{
		file("config.json", 1), file("tokenizer.json", 1),
		file("model-00001-of-00002.safetensors", 24), file("model-00002-of-00002.safetensors", 24),
		file("model.safetensors.index.json", 1), file("model.safetensors", 48),
	}

	files, total, err := selectHFFiles(siblings, []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"})

	require.NoError(t, err)
	assert.Equal(t, []string{"config.json", "tokenizer.json", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "model.safetensors.index.json"}, files)
	assert.Equal(t, int64(48), total)

	_, _, err = selectHFFiles(siblings, []string{"model-00001-of-00002.safetensors", "missing.safetensors"})
	assert.ErrorContains(t, err, "index lists files the repository does not have")
}

func TestSelectHFFiles_DownloadsTheMistralFormatWeightsVLLMLoads(t *testing.T) {
	// Mistral's repositories carry each model twice, in Mistral's own format and
	// in Hugging Face's. vLLM's load_format auto loads consolidated*.safetensors
	// whenever a repository has any, so those are the weights to download.
	file := func(name string, size int64) hfSibling { return hfSibling{RFilename: name, Size: size} }
	test := func(name string, siblings []hfSibling, wantFiles []string, wantWeights int64) {
		t.Run(name, func(t *testing.T) {
			files, total, err := selectHFFiles(siblings, nil)

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
mkdir -p "$repo/blobs" "$repo/snapshots/abc" "$repo/refs"
printf abc > "$repo/refs/main"
printf '{}' > "$repo/blobs/c"; printf '{}' > "$repo/blobs/t"; head -c 1000 /dev/zero > "$repo/blobs/w"
ln -s ../../blobs/c "$repo/snapshots/abc/config.json"
ln -s ../../blobs/t "$repo/snapshots/abc/tokenizer.json"
ln -s ../../blobs/w "$repo/snapshots/abc/model.safetensors"
`

func TestPullHFModel_DownloadsAndReportsSuccess(t *testing.T) {
	e, hub, events := hfPullFixture(t, []hfSibling{
		{RFilename: "config.json", Size: 2}, {RFilename: "tokenizer.json", Size: 2}, {RFilename: "model.safetensors", Size: 1000},
	}, http.StatusOK, fakeHFDownload)

	res, err := e.PullModelStream(t.Context(), "vllm", "Qwen/Test", nil)

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

// fakeHFDownloadWithIndex records each run's files and lays them out in the
// cache, writing FAKE_HF_INDEX as the index.
const fakeHFDownloadWithIndex = `
id=$2; shift 2
files=""
while [ $# -gt 0 ]; do
  case "$1" in
    --cache-dir) dir=$2; shift 2 ;;
    *) files="$files $1"; shift ;;
  esac
done
echo "$files" >> "$(dirname "$dir")/hf-calls"
repo="$dir/models--$(echo "$id" | sed 's#/#--#g')"
mkdir -p "$repo/refs" "$repo/blobs" "$repo/snapshots/abc"
printf abc > "$repo/refs/main"
for f in $files; do
  case "$f" in
    model.safetensors.index.json) printf '%s' "$FAKE_HF_INDEX" > "$repo/blobs/$f" ;;
    *) printf '{}' > "$repo/blobs/$f" ;;
  esac
  ln -sf "../../blobs/$f" "$repo/snapshots/abc/$f"
done
`

// A Pull reads the index first, as vLLM's own download does, and then fetches
// only the shards it lists, not a second copy of the same weights.
func TestPullHFModel_DownloadsOnlyTheShardsTheIndexLists(t *testing.T) {
	file := func(name string) hfSibling { return hfSibling{RFilename: name, Size: 10} }
	e, hub, _ := hfPullFixture(t, []hfSibling{
		file("config.json"), file("tokenizer.json"),
		file("model-00001-of-00002.safetensors"), file("model-00002-of-00002.safetensors"),
		file("model.safetensors.index.json"), file("model.safetensors"),
	}, http.StatusOK, fakeHFDownloadWithIndex)
	t.Setenv("FAKE_HF_INDEX", `{"weight_map": {"a": "model-00001-of-00002.safetensors", "b": "model-00002-of-00002.safetensors"}}`)

	res, err := e.PullModelStream(t.Context(), "vllm", "Qwen/Test", nil)

	require.NoError(t, err)
	var result struct {
		Bytes int64 `json:"bytes"`
	}
	require.NoError(t, json.Unmarshal(res, &result))
	assert.Equal(t, int64(50), result.Bytes, "config, tokenizer, two shards and the index")
	calls, err := os.ReadFile(filepath.Join(filepath.Dir(hub), "hf-calls"))
	require.NoError(t, err)
	assert.Equal(t, " model.safetensors.index.json\n config.json tokenizer.json model-00001-of-00002.safetensors model-00002-of-00002.safetensors model.safetensors.index.json\n", string(calls))
}

// Pull checks the revision it downloaded. An older complete revision of the
// same model in the cache does not make a broken download a success.
func TestPullHFModel_ReportsAnIncompleteDownloadBesideAnOlderCompleteRevision(t *testing.T) {
	e, hub, _ := hfPullFixture(t, oneFileRepo, http.StatusOK, `
dir="$(echo "$@" | sed 's/.*--cache-dir //')"
repo="$dir/models--Qwen--Test"
mkdir -p "$repo/snapshots/new" "$repo/refs"
printf new > "$repo/refs/main"
ln -sf ../../blobs/blob-config.json "$repo/snapshots/new/config.json"
ln -sf ../../blobs/blob-tokenizer.json "$repo/snapshots/new/tokenizer.json"
`)
	writeHFRepo(t, hub, "Qwen/Test", llmFiles(map[string]string{"model.safetensors": "w"}))

	_, err := e.PullModelStream(t.Context(), "vllm", "Qwen/Test", nil)

	assert.ErrorContains(t, err, "the model in the cache is incomplete")
}

func TestPullHFModel_ReportsTheToolsOwnMessageWhenTheDownloadFails(t *testing.T) {
	e, _, _ := hfPullFixture(t, oneFileRepo, http.StatusOK,
		`echo "connection reset by peer" >&2; exit 1`)

	_, err := e.PullModelStream(t.Context(), "vllm", "Qwen/Test", nil)

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

			_, err := e.PullModelStream(t.Context(), "vllm", "Qwen/Test", nil)

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

			_, err := e.PullModelStream(t.Context(), "vllm", model, nil)

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
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
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
	for _, key := range []string{"HF_HOME", "HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "XDG_CACHE_HOME", "HF_ENDPOINT", "HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "HF_TOKEN_PATH", "HF_HUB_DISABLE_IMPLICIT_TOKEN", "HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"} {
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
	cache("then HF_HOME/hub", hfEnv{"HF_HOME": "/home"}, filepath.Join("/home", "hub"))
	cache("then XDG_CACHE_HOME/huggingface/hub", hfEnv{"XDG_CACHE_HOME": "/xdg"}, filepath.Join("/xdg", "huggingface", "hub"))
	cache("else ~/.cache/huggingface/hub", hfEnv{}, filepath.Join(home, ".cache", "huggingface", "hub"))
	cache("~ expanded", hfEnv{"HF_HOME": "~/hf"}, filepath.Join(home, "hf", "hub"))
	cache("$VARS expanded from the engine's environment", hfEnv{"MODELS": "/models", "HF_HOME": "$MODELS/hf"}, filepath.Join("/models", "hf", "hub"))

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
	token("none when HF_HUB_DISABLE_IMPLICIT_TOKEN is set", hfEnv{"HF_HUB_DISABLE_IMPLICIT_TOKEN": "true", "HF_TOKEN": "env", "HF_HOME": hfHome}, "")

	offline := func(name string, env hfEnv, want bool) {
		t.Run("offline: "+name, func(t *testing.T) {
			assert.Equal(t, want, env.offline())
		})
	}
	offline("online by default", hfEnv{}, false)
	offline("HF_HUB_OFFLINE", hfEnv{"HF_HUB_OFFLINE": "1"}, true)
	offline("TRANSFORMERS_OFFLINE when HF_HUB_OFFLINE is unset", hfEnv{"TRANSFORMERS_OFFLINE": "YES"}, true)
	offline("HF_HUB_OFFLINE wins over TRANSFORMERS_OFFLINE", hfEnv{"HF_HUB_OFFLINE": "0", "TRANSFORMERS_OFFLINE": "1"}, false)
	token("none", hfEnv{"HF_HOME": t.TempDir()}, "")
}

// The model list, Pull and Delete use the cache vLLM itself uses. With
// HF_HUB_CACHE set in its launch settings, that is not {models_dir}/hub.
// A finished blob that huggingface_hub moved to its shared store is a link;
// download progress counts the file it points to, so it does not fall back.
func TestHFBlobBytes_CountsAFinishedBlobLinkedToTheSharedStore(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "models--org--model")
	blobs := filepath.Join(repo, "blobs")
	require.NoError(t, os.MkdirAll(blobs, 0o755))
	shared := filepath.Join(t.TempDir(), "shared-blob")
	require.NoError(t, os.WriteFile(shared, make([]byte, 4096), 0o644))
	if err := os.Symlink(shared, filepath.Join(blobs, "finished")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(blobs, "other.incomplete"), make([]byte, 100), 0o644))

	assert.Equal(t, int64(4096+100), hfBlobBytes(repo))
}

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

	downloadDir := t.TempDir()
	writeHFRepo(t, downloadDir, "org/download-dir-place", llmFiles(map[string]string{"model.safetensors": "w"}))
	st.mu.Lock()
	st.plat.Runtime.LaunchArgs = []string{"--download-dir", downloadDir}
	st.mu.Unlock()
	assert.Equal(t, []string{"org/download-dir-place"}, listed(), "--download-dir in the launch arguments, where vLLM keeps its models")
}

// vLLM hands --download-dir to huggingface_hub as cache_dir, which expands ~
// and resolves a relative path against the working directory.
func TestHFLaunchHub_UsesTheDownloadDirVLLMUses(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	env := hfEnv{"HF_HUB_CACHE": filepath.Join(home, "env-hub")}
	test := func(name string, args []string, want string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, hfLaunchHub(args, env))
		})
	}
	test("no --download-dir: the environment's cache", []string{"serve", "--model", "org/m"}, filepath.Join(home, "env-hub"))
	test("an absolute directory", []string{"--download-dir", filepath.Join(home, "models")}, filepath.Join(home, "models"))
	test("the = form", []string{"--download-dir=" + filepath.Join(home, "models")}, filepath.Join(home, "models"))
	test("the underscore spelling", []string{"--download_dir", filepath.Join(home, "models_with_underscores")}, filepath.Join(home, "models_with_underscores"))
	test("the underscore spelling with =", []string{"--download_dir=" + filepath.Join(home, "models_with_underscores")}, filepath.Join(home, "models_with_underscores"))
	test("~ is the home folder", []string{"--download-dir", "~/models"}, filepath.Join(home, "models"))
	test("a relative directory", []string{"--download-dir", "models"}, filepath.Join(cwd, "models"))
	test("an empty value: the environment's cache", []string{"--download-dir="}, filepath.Join(home, "env-hub"))
}

func TestHFActionVars_UsesSavedArgumentsInPlaceOfManifestDefaults(t *testing.T) {
	envCache, defaultCache, savedCache := t.TempDir(), t.TempDir(), t.TempDir()
	test := func(name string, args []string, want string) {
		t.Run(name, func(t *testing.T) {
			st := &engineState{plat: &Platform{Runtime: Runtime{
				Args:           []string{"serve", "--model", "org/default", "--download-dir", defaultCache},
				Env:            map[string]string{"HF_HUB_CACHE": envCache},
				LaunchArgs:     args,
				EditableLaunch: &EditableLaunch{FixedArgs: []string{"serve"}},
			}}}

			assert.Equal(t, want, hfActionVars(st)[hfHubCachePlaceholder])
		})
	}
	test("no saved arguments inherits the manifest directory", nil, defaultCache)
	test("empty saved arguments removes the manifest directory", []string{}, envCache)
	test("saved arguments omit the manifest directory", []string{"--model", "org/saved"}, envCache)
	test("saved arguments select another directory", []string{"--model", "org/saved", "--download-dir", savedCache}, savedCache)
	test("saved arguments use the underscore spelling", []string{"--model", "org/saved", "--download_dir", savedCache}, savedCache)
}

// Cache inspection also runs while stopped. Unrelated unresolved launch
// templates must not hide readable HF settings, and user values stay literal.
func TestEngineHFEnv_KeepsReadableDefaultsAndLiteralUserValues(t *testing.T) {
	test := func(name string, assignments []string, want map[string]string) {
		t.Run(name, func(t *testing.T) {
			st := &engineState{modelsDir: "/models", plat: &Platform{Runtime: Runtime{
				Env:       map[string]string{"HF_HOME": "{models_dir}", "UNRESOLVED": "{port}", "KEEP": "default"},
				LaunchEnv: assignments,
			}}}

			assert.Equal(t, hfEnv(want), engineHFEnv(st))
		})
	}
	test("unavailable launch variables are skipped", nil, map[string]string{"HF_HOME": "/models", "KEEP": "default"})
	test("invalid assignments retain readable defaults", []string{"INVALID"}, map[string]string{"HF_HOME": "/models", "KEEP": "default"})
	test("user paths stay literal and defaults survive", []string{"HF_HOME=$HOME/{models_dir}", "EMPTY="}, map[string]string{
		"HF_HOME": "$HOME/{models_dir}", "EMPTY": "", "KEEP": "default",
	})
	want := map[string]string{"HF_HOME": "/models", "hf_home": "literal", "KEEP": "default"}
	if runtime.GOOS == "windows" {
		delete(want, "HF_HOME")
	}
	test("user name casing follows the platform", []string{"hf_home=literal"}, want)
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
