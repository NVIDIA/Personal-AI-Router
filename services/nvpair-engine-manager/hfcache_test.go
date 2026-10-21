// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
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
	for _, dir := range []string{blobs, snapshot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		blob := filepath.Join(blobs, "blob-"+name)
		if err := os.WriteFile(blob, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, ".incomplete") {
			continue
		}
		if err := os.Symlink(blob, filepath.Join(snapshot, name)); err != nil {
			t.Fatal(err)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestScanHFCacheListsCompleteModelsOnly(t *testing.T) {
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
	writeHFRepo(t, cache, "Qwen/unnumbered-shards-no-index", llmFiles(map[string]string{
		"layers-0.safetensors": "a", "layers-1.safetensors": "b",
	}))
	writeHFRepo(t, cache, "vision/detr", map[string]string{"config.json": "{}", "model.safetensors": "w", "preprocessor_config.json": "{}"})
	writeHFRepo(t, cache, "bartowski/model-GGUF", map[string]string{"model-Q4_K_M.gguf": "w"})
	writeHFRepo(t, cache, "no/config", map[string]string{"tokenizer.json": "{}", "model.safetensors": "w"})
	writeHFRepo(t, cache, "no/weights", llmFiles(nil))
	writeHFRepo(t, cache, "downloading/now", llmFiles(map[string]string{
		"model.safetensors": "w", "blob.incomplete": "partial",
	}))

	// A file whose blob is gone is not a file we have.
	broken := writeHFRepo(t, cache, "broken/link", llmFiles(map[string]string{"model.safetensors": "w"}))
	if err := os.Remove(filepath.Join(broken, "blobs", "blob-model.safetensors")); err != nil {
		t.Fatal(err)
	}

	// Not a model repository, and not a directory at all.
	if err := os.MkdirAll(filepath.Join(cache, "datasets--org--data", "snapshots", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "models--stray-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"plain/no-org-model",
		"Qwen/Qwen2.5-0.5B-Instruct",
		"Qwen/Qwen3-8B",
		"Qwen/Qwen3-VL-8B-Instruct",
		"sentence-transformers/all-MiniLM-L6-v2",
	}
	// plain/no-org-model is a two-part id here; the order is case-insensitive.
	if got := scannedIDs(t, cache); !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

func TestScanHFCacheReportsSnapshotSize(t *testing.T) {
	cache := t.TempDir()
	writeHFRepo(t, cache, "org/model", llmFiles(map[string]string{"model.safetensors": strings.Repeat("x", 1000)}))
	models, err := scanHFCache(cache)
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %v, err = %v", models, err)
	}
	if want := int64(1000 + len("{}") + len("{}")); models[0].Size != want {
		t.Fatalf("size = %d, want %d", models[0].Size, want)
	}
}

func TestScanHFCacheMissingDirectoryIsAnEmptyCache(t *testing.T) {
	models, err := scanHFCache(filepath.Join(t.TempDir(), "never-created"))
	if err != nil || models == nil || len(models) != 0 {
		t.Fatalf("models = %#v, err = %v, want an empty non-nil list", models, err)
	}
}

// The wire shape has to be readable by the ordinary list extractor, so that
// list_models for such an engine needs no per-engine code.
func TestHFCacheActionResultFeedsTheModelListExtractor(t *testing.T) {
	cache := t.TempDir()
	writeHFRepo(t, cache, "Qwen/Qwen3-8B", llmFiles(map[string]string{"model.safetensors": "w"}))

	e := &Executor{}
	raw, err := e.runHFCacheAction(&engineState{}, "vllm", Action{HFCache: &ActionHFCache{Dir: cache}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := extractStringsResult(raw, &ActionResult{Array: "data", Field: "id"})
	if !ok || !slices.Equal(ids, []string{"Qwen/Qwen3-8B"}) {
		t.Fatalf("extracted %v, %v from %s", ids, ok, raw)
	}
	var decoded hfCacheResult
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Data[0].Size == 0 {
		t.Fatalf("size missing from %s: %v", raw, err)
	}
}

func TestHFCacheActionValidation(t *testing.T) {
	if err := (&Action{HFCache: &ActionHFCache{Dir: " "}}).validate("list_models"); err == nil {
		t.Error("an hf_cache action with no dir was accepted")
	}
	if err := (&Action{HFCache: &ActionHFCache{Dir: "{models_dir}/hub"}, Cmd: []string{"x"}}).validate("list_models"); err == nil {
		t.Error("an action with two kinds was accepted")
	}
	if err := (&Action{HFCache: &ActionHFCache{Dir: "{models_dir}/hub"}}).validate("list_models"); err != nil {
		t.Errorf("a valid hf_cache action was rejected: %v", err)
	}
}

func TestHFCacheDeleteActionValidation(t *testing.T) {
	del := &ActionHFCache{Dir: "{models_dir}/hub", Delete: true}
	if err := (&Action{HFCache: del}).validate("delete_model"); err != nil {
		t.Errorf("a delete_model hf_cache delete was rejected: %v", err)
	}
	if err := (&Action{HFCache: del}).validate("list_models"); err == nil {
		t.Error("hf_cache.delete is only for delete_model")
	}
}

func TestDeleteHFModel(t *testing.T) {
	cache := t.TempDir()
	e := switchModelExecutor(t, "--model")
	if _, err := loadModel(t, e, "org/served"); err != nil {
		t.Fatalf("load: %v", err)
	}
	m, _ := e.reg.Get("fake")
	m.Actions["delete_model"] = Action{HFCache: &ActionHFCache{Dir: cache, Delete: true}}
	writeHFRepo(t, cache, "org/served", llmFiles(map[string]string{"model.safetensors": "w"}))
	writeHFRepo(t, cache, "org/other", llmFiles(map[string]string{"model.safetensors": "wwww"}))
	if err := os.MkdirAll(filepath.Join(cache, ".locks", "models--org--other"), 0o755); err != nil {
		t.Fatal(err)
	}
	del := func(model string) (json.RawMessage, error) {
		return e.Action(context.Background(), "fake", "delete_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
	}

	if _, err := del("org/served"); err == nil || !strings.Contains(err.Error(), "switch to another model first") {
		t.Fatalf("the model being served must be refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "models--org--served")); err != nil {
		t.Fatal("a refused delete must leave the model in place")
	}

	res, err := del("org/other")
	if err != nil || !strings.Contains(string(res), `"deleted":"org/other"`) {
		t.Fatalf("delete: %s %v", res, err)
	}
	for _, gone := range []string{"models--org--other", filepath.Join(".locks", "models--org--other")} {
		if _, err := os.Stat(filepath.Join(cache, gone)); err == nil {
			t.Fatalf("%s should have been removed", gone)
		}
	}
	if ids := scannedIDs(t, cache); !slices.Equal(ids, []string{"org/served"}) {
		t.Fatalf("remaining models = %v", ids)
	}

	for _, bad := range []string{"org/missing", "../escape", "--help", ""} {
		if _, err := del(bad); err == nil {
			t.Errorf("deleting %q should fail", bad)
		}
	}
}

// With the bundled manifest and no saved settings the served model is the
// default launch's, even in a manager that has not been asked about the engine
// yet. Deleting also fails closed, rather than assuming nothing is served, when
// the served model cannot be told.
func TestDeleteHFModelRefusesTheBundledDefaultModel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the bundled vllm manifest is Linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	cache := filepath.Join(home, ".cache", "huggingface", "hub")
	writeHFRepo(t, cache, "Qwen/Qwen2.5-0.5B-Instruct", llmFiles(map[string]string{"model.safetensors": "w"}))
	writeHFRepo(t, cache, "org/other", llmFiles(map[string]string{"model.safetensors": "w"}))

	reg := loadWithOverrides(t, t.TempDir())
	e := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	act := func(model string) error {
		_, err := e.Action(context.Background(), "vllm", "delete_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
		return err
	}

	// The bundled default launch serves Qwen/Qwen2.5-0.5B-Instruct.
	if err := act("Qwen/Qwen2.5-0.5B-Instruct"); err == nil {
		t.Fatal("the default served model was deleted by a manager that had not detected the engine")
	}
	if _, err := os.Stat(filepath.Join(cache, "models--Qwen--Qwen2.5-0.5B-Instruct")); err != nil {
		t.Fatal("the served model is gone from the cache")
	}
	if err := act("org/other"); err != nil {
		t.Fatalf("another model should be deletable: %v", err)
	}
}
