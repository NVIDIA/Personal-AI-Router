// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
)

// writeHFRepo builds a repository the way the Hugging Face hub cache lays it
// out: content in blobs/, and snapshots/<rev>/<name> symlinks into it. A
// file content of "" is written empty; names ending in ".incomplete" go to
// blobs/ only, as an interrupted download leaves them.
func writeHFRepo(t *testing.T, cache, id string, files map[string]string) string {
	t.Helper()
	repo := filepath.Join(cache, "models--"+strings.ReplaceAll(id, "/", "--"))
	blobs := filepath.Join(repo, "blobs")
	snapshot := filepath.Join(repo, "snapshots", "abc123")
	for _, dir := range []string{blobs, snapshot, filepath.Join(repo, "refs")} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	// main points at the snapshot, as after an hf download of the main branch.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "refs", "main"), []byte("abc123"), 0o644))
	for name, content := range files {
		blob := filepath.Join(blobs, "blob-"+name)
		require.NoError(t, os.WriteFile(blob, []byte(content), 0o644))
		if strings.HasSuffix(name, ".incomplete") {
			continue
		}
		require.NoError(t, os.Symlink(blob, filepath.Join(snapshot, name)))
	}
	return repo
}

func llmFiles(extra map[string]string) map[string]string {
	files := map[string]string{"config.json": "{}", "tokenizer.json": "{}"}
	for name, content := range extra {
		files[name] = content
	}
	return files
}

func scannedIDs(t *testing.T, cache string) []string {
	t.Helper()
	models, err := scanHFCache(cache)
	require.NoError(t, err)
	ids := []string{}
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestScanHFCache_ListsCompleteModelsOnly(t *testing.T) {
	cache := t.TempDir()

	writeHFRepo(t, cache, "Qwen/Qwen2.5-0.5B-Instruct", llmFiles(map[string]string{"model.safetensors": "weights"}))
	writeHFRepo(t, cache, "plain/no-org-model", llmFiles(map[string]string{"pytorch_model.bin": "weights"}))
	// Both formats of the same weights count once, not as an unverifiable shard set.
	writeHFRepo(t, cache, "sentence-transformers/all-MiniLM-L6-v2", llmFiles(map[string]string{
		"model.safetensors": "a", "pytorch_model.bin": "b", "tf_model.h5": "c",
	}))
	// Sharded, with an index naming every shard.
	writeHFRepo(t, cache, "Qwen/Qwen3-8B", llmFiles(map[string]string{
		"model-00001-of-00002.safetensors": "a",
		"model-00002-of-00002.safetensors": "b",
		"model.safetensors.index.json":     `{"weight_map":{"x":"model-00001-of-00002.safetensors","y":"model-00002-of-00002.safetensors"}}`,
	}))
	// Sharded, no index, but the numbered names prove all four are present.
	writeHFRepo(t, cache, "Qwen/Qwen3-VL-8B-Instruct", llmFiles(map[string]string{
		"model-00001-of-00004.safetensors": "a", "model-00002-of-00004.safetensors": "b",
		"model-00003-of-00004.safetensors": "c", "model-00004-of-00004.safetensors": "d",
	}))

	// Left out: each is missing something a servable model has.
	writeHFRepo(t, cache, "Qwen/partial-index", llmFiles(map[string]string{
		"model-00001-of-00002.safetensors": "a",
		"model.safetensors.index.json":     `{"weight_map":{"x":"model-00001-of-00002.safetensors","y":"model-00002-of-00002.safetensors"}}`,
	}))
	writeHFRepo(t, cache, "Qwen/partial-numbered", llmFiles(map[string]string{
		"model-00001-of-00004.safetensors": "a", "model-00002-of-00004.safetensors": "b", "model-00004-of-00004.safetensors": "d",
	}))
	// Two files for a two-shard model, but numbered from zero, so shard 2 is missing.
	writeHFRepo(t, cache, "Qwen/misnumbered-shards", llmFiles(map[string]string{
		"model-00000-of-00002.safetensors": "a", "model-00001-of-00002.safetensors": "b",
	}))
	writeHFRepo(t, cache, "Qwen/unnumbered-shards-no-index", llmFiles(map[string]string{
		"layers-0.safetensors": "a", "layers-1.safetensors": "b",
	}))
	writeHFRepo(t, cache, "vision/detr", map[string]string{"config.json": "{}", "model.safetensors": "w", "preprocessor_config.json": "{}"})
	writeHFRepo(t, cache, "bartowski/model-GGUF", map[string]string{"model-Q4_K_M.gguf": "w"})
	writeHFRepo(t, cache, "no/config", map[string]string{"tokenizer.json": "{}", "model.safetensors": "w"})
	writeHFRepo(t, cache, "no/weights", llmFiles(nil))
	// Weights still downloading are not linked into the snapshot yet.
	writeHFRepo(t, cache, "downloading/now", llmFiles(map[string]string{
		"model.safetensors.incomplete": "partial",
	}))

	// A file whose blob is gone is not a file we have.
	broken := writeHFRepo(t, cache, "broken/link", llmFiles(map[string]string{"model.safetensors": "w"}))
	require.NoError(t, os.Remove(filepath.Join(broken, "blobs", "blob-model.safetensors")))

	// Not a model repository, and not a directory at all.
	require.NoError(t, os.MkdirAll(filepath.Join(cache, "datasets--org--data", "snapshots", "x"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cache, "models--stray-file"), nil, 0o644))

	// plain/no-org-model is a two-part id here; the order is case-insensitive.
	assert.Equal(t, []string{
		"plain/no-org-model",
		"Qwen/Qwen2.5-0.5B-Instruct",
		"Qwen/Qwen3-8B",
		"Qwen/Qwen3-VL-8B-Instruct",
		"sentence-transformers/all-MiniLM-L6-v2",
	}, scannedIDs(t, cache))
}

func TestScanHFCache_ReportsSnapshotSize(t *testing.T) {
	cache := t.TempDir()
	writeHFRepo(t, cache, "org/model", llmFiles(map[string]string{"model.safetensors": strings.Repeat("x", 1000)}))

	models, err := scanHFCache(cache)

	require.NoError(t, err)
	require.Len(t, models, 1)
	// The weights plus config.json and tokenizer.json, "{}" each.
	assert.Equal(t, int64(1000+2+2), models[0].Size)
}

func TestScanHFCache_TreatsAMissingDirectoryAsAnEmptyCache(t *testing.T) {
	models, err := scanHFCache(filepath.Join(t.TempDir(), "never-created"))

	require.NoError(t, err)
	// An empty non-nil list encodes as [] rather than null.
	assert.NotNil(t, models)
	assert.Empty(t, models)
}

// The wire shape has to be readable by the ordinary list extractor, so that
// list_models for such an engine needs no per-engine code.
func TestHFCacheAction_ResultFeedsTheModelListExtractor(t *testing.T) {
	cache := t.TempDir()
	writeHFRepo(t, cache, "Qwen/Qwen3-8B", llmFiles(map[string]string{"model.safetensors": "w"}))
	e := &Executor{}

	raw, err := e.runHFCacheAction(t.Context(), &engineState{}, "vllm", Action{HFCache: &ActionHFCache{Dir: cache}}, nil)

	require.NoError(t, err)
	ids, ok := extractStringsResult(raw, &ActionResult{Array: "data", Field: "id"})
	assert.True(t, ok, "the list extractor must read %s", raw)
	assert.Equal(t, []string{"Qwen/Qwen3-8B"}, ids)
	var decoded hfCacheResult
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded.Data, 1)
	assert.NotZero(t, decoded.Data[0].Size, "size missing from %s", raw)
}

func TestHFCacheAction_ValidatesItsDirAndKind(t *testing.T) {
	accept := func(name string, action Action) {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, action.validate("list_models"))
		})
	}
	reject := func(name string, action Action) {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, action.validate("list_models"))
		})
	}
	accept("a valid hf_cache action", Action{HFCache: &ActionHFCache{Dir: "{models_dir}/hub"}})
	reject("an hf_cache action with no dir", Action{HFCache: &ActionHFCache{Dir: " "}})
	reject("an action with two kinds", Action{HFCache: &ActionHFCache{Dir: "{models_dir}/hub"}, Cmd: []string{"x"}})
}

func TestHFCacheDeleteAction_RequiresDeleteModelAndTheTool(t *testing.T) {
	accept := func(name, actionName string, cache ActionHFCache) {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, (&Action{HFCache: &cache}).validate(actionName))
		})
	}
	reject := func(name, actionName string, cache ActionHFCache) {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, (&Action{HFCache: &cache}).validate(actionName))
		})
	}
	accept("delete_model with the tool", "delete_model", ActionHFCache{Dir: "{models_dir}/hub", Delete: true, Tool: "hf"})
	reject("delete outside delete_model", "list_models", ActionHFCache{Dir: "{models_dir}/hub", Delete: true, Tool: "hf"})
	reject("delete without the tool that performs it", "delete_model", ActionHFCache{Dir: "{models_dir}/hub", Delete: true})
	reject("a tool on a listing", "list_models", ActionHFCache{Dir: "{models_dir}/hub", Tool: "hf"})
}

// fakeHFCacheRm stands in for `hf cache rm model/<org/name> --cache-dir <dir> --yes`.
// It records its arguments in <dir>/hf-calls and its HF_TOKEN in <dir>/hf-token and removes the model's folder, or
// fails the way the real tool does when the model's files include one named
// hf-rm-fails. A model with a file named hf-rm-waits makes it create
// <dir>/hf-running and wait for <dir>/hf-release first. Which shared files survive is the real tool's business, and is
// checked against the real tool rather than here.
const fakeHFCacheRm = `#!/bin/sh
[ "$1 $2" = "cache rm" ] || { echo "unexpected: $*" >&2; exit 2; }
dir=$5
echo "$*" >> "$dir/hf-calls"
printf '%s' "${HF_TOKEN:-}" > "$dir/hf-token"
repo="$dir/models--$(echo "${3#model/}" | sed 's#/#--#g')"
if [ -n "$(find "$repo" -name hf-rm-waits)" ]; then touch "$dir/hf-running"; while [ -d "$dir" ] && [ ! -e "$dir/hf-release" ]; do sleep 0.05; done; fi
if [ -n "$(find "$repo" -name hf-rm-fails)" ]; then echo "PermissionError: cannot remove $repo" >&2; exit 1; fi
rm -rf "$repo"
`

// skipWithoutFakeHF skips a test that runs the fake hf tool, a shell script.
func skipWithoutFakeHF(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake hf tool is a shell script; vLLM has no Windows install")
	}
}

func hfCalls(t *testing.T, cache string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cache, "hf-calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestDeleteHFModel_RemovesTheModelWithTheHuggingFaceTool(t *testing.T) {
	skipWithoutFakeHF(t)
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	_, err := loadModel(t, e, "org/served")
	require.NoError(t, err)
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "hf"}}
	writeHFRepo(t, cache, "org/served", llmFiles(map[string]string{"model.safetensors": "w"}))
	writeHFRepo(t, cache, "org/other", llmFiles(map[string]string{"model.safetensors": "wwww"}))
	del := func(model string) (json.RawMessage, error) {
		return e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
	}

	_, err = del("org/served")
	require.ErrorContains(t, err, "or load another model first", "the model being served must be refused")
	require.DirExists(t, filepath.Join(cache, "models--org--served"), "a refused delete must leave the model in place")

	res, err := del("org/other")
	require.NoError(t, err)
	var result struct {
		Deleted string `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(res, &result))
	assert.Equal(t, "org/other", result.Deleted)
	assert.Equal(t, []string{"org/served"}, scannedIDs(t, cache))

	for _, bad := range []string{"org/missing", "../escape", "--help", ""} {
		_, err := del(bad)
		assert.Error(t, err, "deleting %q should fail", bad)
	}
	// Only the one allowed delete reached the tool, as a single model target.
	assert.Equal(t, []string{"cache rm model/org/other --cache-dir " + cache + " --yes"}, hfCalls(t, cache))
}

// When the tool fails, the model stays and a stopped engine keeps it selected,
// so the failed delete changes nothing.
func TestDeleteHFModel_KeepsTheSelectionWhenTheToolFails(t *testing.T) {
	skipWithoutFakeHF(t)
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	_, err := loadModel(t, e, "org/served")
	require.NoError(t, err)
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "hf"}}
	writeHFRepo(t, cache, "org/served", llmFiles(map[string]string{"model.safetensors": "w", "hf-rm-fails": "x"}))
	require.NoError(t, e.Stop("fake"))
	before := launchTextOf(t, e)

	_, err = e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/served"}`))

	assert.ErrorContains(t, err, "PermissionError", "the tool's own error is reported")
	assert.Equal(t, []string{"org/served"}, scannedIDs(t, cache), "the model is kept")
	assert.Equal(t, before, launchTextOf(t, e), "the launch text is restored")
}

// Without the Hugging Face tool nothing is deleted and the selection is untouched.
func TestDeleteHFModel_RefusesWithoutTheHuggingFaceTool(t *testing.T) {
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	_, err := loadModel(t, e, "org/served")
	require.NoError(t, err)
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "no-such-hf"}}
	writeHFRepo(t, cache, "org/served", llmFiles(map[string]string{"model.safetensors": "w"}))
	require.NoError(t, e.Stop("fake"))
	before := launchTextOf(t, e)

	_, err = e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/served"}`))

	assert.ErrorContains(t, err, "was not found", "a missing tool is reported")
	assert.Equal(t, []string{"org/served"}, scannedIDs(t, cache))
	assert.Equal(t, before, launchTextOf(t, e), "the launch text is unchanged")
}

// A stopped engine's model can be deleted. The engine is then left with no model
// selected, so it refuses to start rather than download one, until a model is
// loaded.
func TestDeleteHFModel_LeavesAStoppedEngineWithNoModelSelected(t *testing.T) {
	skipWithoutFakeHF(t)
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	_, err := loadModel(t, e, "org/served")
	require.NoError(t, err)
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "hf"}}
	writeHFRepo(t, cache, "org/served", llmFiles(map[string]string{"model.safetensors": "w"}))
	writeHFRepo(t, cache, "org/other", llmFiles(map[string]string{"model.safetensors": "w"}))
	require.NoError(t, e.Stop("fake"))

	_, err = e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/served"}`))

	require.NoError(t, err, "deleting the model of a stopped engine")
	assert.Equal(t, []string{"org/other"}, scannedIDs(t, cache))
	assert.NotContains(t, launchTextOf(t, e), "--model", "the launch text must no longer name a model")
	assert.ErrorContains(t, e.Start(t.Context(), "fake"), "no model selected")
	_, err = loadModel(t, e, "org/other")
	require.NoError(t, err, "loading another model")
	state, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	assert.True(t, state.Running, "the engine runs after a model is loaded")
}

// With the bundled manifest and no saved settings the served model is the
// default launch's, even in a manager that has not been asked about the engine
// yet. Deleting also fails closed, rather than assuming nothing is served, when
// the served model cannot be told.
func TestDeleteHFModel_RefusesTheBundledDefaultModel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the bundled vllm manifest is Linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	cache := filepath.Join(home, ".cache", "huggingface", "hub")
	writeHFRepo(t, cache, "Qwen/Qwen3.5-4B", llmFiles(map[string]string{"model.safetensors": "w"}))
	writeHFRepo(t, cache, "org/other", llmFiles(map[string]string{"model.safetensors": "w"}))

	reg := loadWithOverrides(t, t.TempDir())
	base := t.TempDir()
	// The bundled manifest runs `hf` from the vLLM virtualenv.
	venvBin := filepath.Join(base, "vllm", "venv", "bin")
	require.NoError(t, os.MkdirAll(venvBin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(venvBin, "hf"), []byte(fakeHFCacheRm), 0o755))
	e := NewExecutor(reg, NewReporter(nil), func(string, any) {}, base)
	act := func(model string) error {
		_, err := e.Action(t.Context(), "vllm", "delete_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
		return err
	}

	// The bundled default launch serves Qwen/Qwen3.5-4B.
	require.Error(t, act("Qwen/Qwen3.5-4B"), "the default served model was deleted by a manager that had not detected the engine")
	require.DirExists(t, filepath.Join(cache, "models--Qwen--Qwen3.5-4B"), "the served model must stay in the cache")
	assert.NoError(t, act("org/other"), "another model should be deletable")
}

func TestScanHFCache_JudgesAMistralFormatModelOnTheWeightsVLLMLoads(t *testing.T) {
	index := func(shards ...string) string {
		m := map[string]string{}
		for i, shard := range shards {
			m[fmt.Sprintf("layer.%d", i)] = shard
		}
		raw, err := json.Marshal(map[string]any{"weight_map": m})
		require.NoError(t, err)
		return string(raw)
	}
	test := func(name string, files map[string]string, wantListed bool) {
		t.Run(name, func(t *testing.T) {
			cache := t.TempDir()
			writeHFRepo(t, cache, "mistralai/M", files)

			listed := slices.Contains(scannedIDs(t, cache), "mistralai/M")

			assert.Equal(t, wantListed, listed)
		})
	}
	test("consolidated weights with params.json and tekken.json",
		map[string]string{"params.json": "{}", "tekken.json": "{}", "consolidated.safetensors": "w"}, true)
	test("Mistral 7B v0.3 style tokenizer.model.v3",
		map[string]string{"config.json": "{}", "tokenizer.model.v3": "t", "consolidated.safetensors": "w"}, true)
	test("sharded consolidated weights all present",
		map[string]string{"params.json": "{}", "tekken.json": "{}",
			"consolidated.safetensors.index.json":     index("consolidated-00001-of-00002.safetensors", "consolidated-00002-of-00002.safetensors"),
			"consolidated-00001-of-00002.safetensors": "w", "consolidated-00002-of-00002.safetensors": "w"}, true)
	test("sharded consolidated weights with one missing",
		map[string]string{"params.json": "{}", "tekken.json": "{}",
			"consolidated.safetensors.index.json":     index("consolidated-00001-of-00002.safetensors", "consolidated-00002-of-00002.safetensors"),
			"consolidated-00001-of-00002.safetensors": "w"}, false)
	test("Hugging Face shards complete, consolidated copy still downloading",
		map[string]string{"config.json": "{}", "tokenizer.json": "{}",
			"model.safetensors.index.json":     index("model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"),
			"model-00001-of-00002.safetensors": "w", "model-00002-of-00002.safetensors": "w",
			"consolidated.safetensors.index.json":     index("consolidated-00001-of-00002.safetensors", "consolidated-00002-of-00002.safetensors"),
			"consolidated-00001-of-00002.safetensors": "w"}, false)
	test("no tokenizer at all",
		map[string]string{"params.json": "{}", "consolidated.safetensors": "w"}, false)
}

// A repository can ship the same weights twice, as indexed shards and as one
// file. vLLM loads only the shards the index lists, so launch sizing counts
// only those, while the cache list still reports everything on disk.
func TestCompleteHFSnapshot_CountsOnlyTheWeightsTheIndexLists(t *testing.T) {
	cache := t.TempDir()
	repo := writeHFRepo(t, cache, "org/both-copies", llmFiles(map[string]string{
		"model.safetensors.index.json":     `{"weight_map": {"a": "model-00001-of-00002.safetensors", "b": "model-00002-of-00002.safetensors"}}`,
		"model-00001-of-00002.safetensors": "11111",
		"model-00002-of-00002.safetensors": "22222",
		"model.safetensors":                "the same ten",
	}))

	disk, weights, ok := completeHFSnapshot(filepath.Join(repo, "snapshots", "abc123"))

	require.True(t, ok)
	assert.Equal(t, int64(5+5), weights, "the indexed shards only")
	assert.Greater(t, disk, int64(5+5+12), "everything on disk")
}

// A settings save made while hf cache rm runs waits for the delete, so a failed
// delete puts back only its own change and the save is kept.
func TestDeleteHFModel_KeepsASettingsSaveMadeWhileTheToolRuns(t *testing.T) {
	skipWithoutFakeHF(t)
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	_, err := loadModel(t, e, "org/served")
	require.NoError(t, err)
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "hf"}}
	writeHFRepo(t, cache, "org/served", llmFiles(map[string]string{"model.safetensors": "w", "hf-rm-waits": "x", "hf-rm-fails": "x"}))
	require.NoError(t, e.Stop("fake"))
	state, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	edited := state.LaunchText + " --max-model-len 4096"

	// However the test ends, release the fake hf so it does not wait forever.
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(cache, "hf-release"), nil, 0o600) })
	deleted := make(chan error, 1)
	go func() {
		_, err := e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/served"}`))
		deleted <- err
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(cache, "hf-running"))
		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "hf cache rm never started")

	saved := make(chan error, 1)
	go func() {
		_, err := e.ConfigureLaunch(t.Context(), settings.Configure{
			Engine:   "fake",
			Settings: settings.Config{ServerPort: state.ServerPort, LaunchText: edited},
		}, func() error { return nil })
		saved <- err
	}()
	select {
	case err := <-saved:
		require.FailNow(t, "the settings save finished while hf cache rm was running", "save error: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, os.WriteFile(filepath.Join(cache, "hf-release"), nil, 0o600))
	require.ErrorContains(t, <-deleted, "PermissionError")
	require.NoError(t, <-saved)
	assert.Equal(t, edited, launchTextOf(t, e), "the settings save must survive the delete's rollback")
}

func TestDeleteHFModel_UsesTheCacheSavedBeforeItGetsTheOperationLock(t *testing.T) {
	toolDir := t.TempDir()
	toolName := "hf"
	if runtime.GOOS == "windows" {
		toolName += ".exe"
	}
	// Build a portable CLI because the shell fixtures cannot run on Windows.
	out, err := exec.Command("go", "build", "-o", filepath.Join(toolDir, toolName), "./testdata/fakehf").CombinedOutput()
	require.NoError(t, err, "%s", out)

	test := func(name string, downloadDir bool) {
		t.Run(name, func(t *testing.T) {
			oldCache, newCache := t.TempDir(), t.TempDir()
			// Only the new cache has the model, so reading the old location before
			// taking the operation lock would reject this queued deletion.
			repo := filepath.Join(newCache, "models--org--downloaded")
			require.NoError(t, os.Mkdir(repo, 0o755))
			m := testEngineManifest(filepath.Join(toolDir, "engine"))
			platform := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
			platform.Runtime.Port = 13001
			platform.Runtime.Args = []string{"serve", "--model", "org/initial"}
			platform.Runtime.EditableLaunch = &EditableLaunch{FixedArgs: []string{"serve"}}
			platform.Runtime.LaunchArgs = []string{"--model", "org/initial"}
			if downloadDir {
				platform.Runtime.LaunchArgs = append(platform.Runtime.LaunchArgs, "--download-dir", oldCache)
			} else {
				platform.Runtime.LaunchEnv = []string{"HF_HUB_CACHE=" + oldCache}
			}
			m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = platform
			m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flag: "--model"}}
			m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: "{hf_hub_cache}", Delete: true, Tool: toolName}}
			e := newTestExecutor(t, m)
			e.overrideDir = t.TempDir()
			st, err := e.state("fake")
			require.NoError(t, err)
			st.binPath = filepath.Join(toolDir, "engine")

			// The settings operation owns the lock before Delete is requested.
			st.opMu.Lock()
			release := sync.OnceFunc(st.opMu.Unlock)
			defer release()
			started := make(chan struct{})
			deleted := make(chan error, 1)
			go func() {
				close(started)
				_, err := e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/downloaded"}`))
				deleted <- err
			}()
			<-started
			// Give a premature cache read time to fail while the lock is held.
			// A sync.Mutex exposes no signal when this call starts waiting.
			select {
			case err := <-deleted:
				require.FailNow(t, "Delete completed before the settings operation released the lock", "delete error: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			args := []string{"--model", "org/current"}
			env := map[string]string{}
			if downloadDir {
				args = append(args, "--download-dir", newCache)
			} else {
				env["HF_HUB_CACHE"] = newCache
			}
			launchText, err := formatLaunchParts(env, args)
			require.NoError(t, err)
			saved, err := e.configureLaunchLocked(t.Context(), st, settings.Configure{
				Engine: "fake", Settings: settings.Config{ServerPort: 13001, LaunchText: launchText},
			}, func() error { return nil })
			require.NoError(t, err)
			release()

			select {
			case err := <-deleted:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "Delete did not finish after the settings operation")
			}
			assert.NoDirExists(t, repo)
			assert.Equal(t, saved.LaunchText, launchTextOf(t, e), "deleting another model must preserve the saved launch")
		})
	}
	test("download directory in the launch arguments", true)
	test("cache directory in the launch environment", false)
}

// An adopted engine can serve another model than its saved arguments name. That
// model is still refused, because the check asks the engine what it serves.
func TestDeleteHFModel_RefusesTheModelAnAdoptedEngineServes(t *testing.T) {
	skipWithoutFakeHF(t)
	t.Setenv("FAKE_SERVE_MODEL", "org/adopted")
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "hf"}}
	writeHFRepo(t, cache, "org/adopted", llmFiles(map[string]string{"model.safetensors": "w"}))
	require.Contains(t, launchTextOf(t, e), "--model org/initial")

	_, err := e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/adopted"}`))
	require.ErrorContains(t, err, "is the model fake is serving")
	assert.Equal(t, []string{"org/adopted"}, scannedIDs(t, cache), "a refused delete must leave the model in place")
	assert.Empty(t, hfCalls(t, cache), "a refused delete must not reach hf cache rm")
}

// A running engine that cannot say what it serves gets no delete at all, rather
// than a guess from its saved arguments.
func TestDeleteHFModel_RefusesWhenTheRunningEngineCannotSayWhatItServes(t *testing.T) {
	skipWithoutFakeHF(t)
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "hf"}}
	delete(m.Actions, "loaded_models")
	writeHFRepo(t, cache, "org/other", llmFiles(map[string]string{"model.safetensors": "w"}))

	_, err := e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/other"}`))
	require.ErrorContains(t, err, "cannot tell which model fake is serving")
	assert.Equal(t, []string{"org/other"}, scannedIDs(t, cache))
	assert.Empty(t, hfCalls(t, cache))
}

// One shard of several is an interrupted download, not a model: without an
// index, the shard's own name says how many files there should be.
func TestScanHFCache_LeavesOutALoneShardOfASplitModel(t *testing.T) {
	test := func(name string, weights map[string]string, wantListed bool) {
		t.Run(name, func(t *testing.T) {
			cache := t.TempDir()
			writeHFRepo(t, cache, "org/m", llmFiles(weights))

			listed := slices.Contains(scannedIDs(t, cache), "org/m")

			assert.Equal(t, wantListed, listed)
		})
	}
	test("the first of four shards alone", map[string]string{"model-00001-of-00004.safetensors": "w"}, false)
	test("the only shard of one", map[string]string{"model-00001-of-00001.safetensors": "w"}, true)
	test("one unnumbered weight file", map[string]string{"model.safetensors": "w"}, true)
	test("all four shards", map[string]string{
		"model-00001-of-00004.safetensors": "w", "model-00002-of-00004.safetensors": "w",
		"model-00003-of-00004.safetensors": "w", "model-00004-of-00004.safetensors": "w",
	}, true)
}

// HF_TOKEN set in the engine's launch settings reaches hf cache rm, as it does
// the engine itself.
func TestDeleteHFModel_RunsTheToolWithTheEnginesHuggingFaceSettings(t *testing.T) {
	skipWithoutFakeHF(t)
	t.Setenv("HF_TOKEN", "")
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true, Tool: "hf"}}
	writeHFRepo(t, cache, "org/other", llmFiles(map[string]string{"model.safetensors": "w"}))
	st, err := e.state("fake")
	require.NoError(t, err)
	st.mu.Lock()
	st.plat.Runtime.LaunchEnv = []string{"HF_TOKEN=engine-token"}
	st.mu.Unlock()

	_, err = e.Action(t.Context(), "fake", "delete_model", json.RawMessage(`{"model":"org/other"}`))

	require.NoError(t, err)
	token, err := os.ReadFile(filepath.Join(cache, "hf-token"))
	require.NoError(t, err)
	assert.Equal(t, "engine-token", string(token))
}

// A complete snapshot stays listed whatever else in the repository is still
// downloading or was interrupted: huggingface_hub links a file into a snapshot
// only once it is whole, so each snapshot is judged on its own files. A
// leftover or running download used to hide the whole repository, and so a
// usable model could not be loaded or deleted from the model list.
func TestScanHFCache_ListsACompleteSnapshotBesideAnUnfinishedDownload(t *testing.T) {
	test := func(name string, unfinished func(t *testing.T, repo string), want []string) {
		t.Run(name, func(t *testing.T) {
			cache := t.TempDir()
			repo := writeHFRepo(t, cache, "org/model", llmFiles(map[string]string{"model.safetensors": "w"}))
			unfinished(t, repo)

			assert.Equal(t, want, scannedIDs(t, cache))
		})
	}
	partial := func(t *testing.T, repo, name string) {
		require.NoError(t, os.WriteFile(filepath.Join(repo, "blobs", name), []byte("part"), 0o644))
	}
	test("a leftover from an interrupted download the retry finished", func(t *testing.T, repo string) {
		partial(t, repo, "5af571cbf074e6d21a03528d2330792e532ca608f24ac70a143f6b369968ab8c.86b892a3.incomplete")
	}, []string{"org/model"})
	test("a file vLLM does not load, still downloading", func(t *testing.T, repo string) {
		partial(t, repo, "0123abcd.incomplete")
	}, []string{"org/model"})
	test("another revision, still downloading", func(t *testing.T, repo string) {
		newer := filepath.Join(repo, "snapshots", "def456")
		require.NoError(t, os.MkdirAll(newer, 0o755))
		require.NoError(t, os.Symlink(filepath.Join(repo, "blobs", "blob-config.json"), filepath.Join(newer, "config.json")))
		require.NoError(t, os.Symlink(filepath.Join(repo, "blobs", "blob-tokenizer.json"), filepath.Join(newer, "tokenizer.json")))
		partial(t, repo, "fedcba98.incomplete")
	}, []string{"org/model"})
	test("only an unfinished revision", func(t *testing.T, repo string) {
		require.NoError(t, os.Remove(filepath.Join(repo, "snapshots", "abc123", "model.safetensors")))
		partial(t, repo, "fedcba98.incomplete")
	}, []string{})
}
