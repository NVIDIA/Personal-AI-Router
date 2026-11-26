// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listedEngine is snapshotEngine with o/r listed in the manifest's catalog.
func listedEngine(t *testing.T, hub *fakeHub, entry CatalogEntry) (*Executor, *engineState, string) {
	t.Helper()
	e, store, _ := snapshotEngine(t, hub)
	st, _ := e.state("fake")
	entry.Repo, entry.Name = "o/r", "Listed"
	st.manifest.Catalog = []CatalogEntry{entry}
	if err := st.manifest.Validate(); err != nil {
		t.Fatalf("listed manifest: %v", err)
	}
	return e, st, store
}

// fakeTool installs a script the fake interpreter follows (see fakePython in
// testdata/fakeengine) and makes that interpreter the only Python found.
func fakeTool(t *testing.T, st *engineState, name, script string) {
	t.Helper()
	path := filepath.Join(st.installDir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAIR_FAKE_PYTHON", "1")
	saved := pythonCandidates
	pythonCandidates = [][]string{{fakeEngineBin}}
	t.Cleanup(func() { pythonCandidates = saved })
}

func TestValidateCatalogRejects(t *testing.T) {
	sha := strings.Repeat("a", 40)
	cases := map[string]CatalogEntry{
		"not a repository id":   {Repo: "just-a-name", Name: "x"},
		"no name":               {Repo: "o/r"},
		"negative size":         {Repo: "o/r", Name: "x", SizeGB: -1},
		"branch revision":       {Repo: "o/r", Name: "x", Revision: "main"},
		"short commit":          {Repo: "o/r", Name: "x", Revision: sha[:12]},
		"exclude climbs out":    {Repo: "o/r", Name: "x", Exclude: []string{"../x"}},
		"exclude absolute":      {Repo: "o/r", Name: "x", Exclude: []string{"/etc/*"}},
		"exclude backslash":     {Repo: "o/r", Name: "x", Exclude: []string{`a\b`}},
		"step not python":       {Repo: "o/r", Name: "x", AfterDownload: []AfterDownloadStep{{Python: "tools/run.sh", Creates: "t.json"}}},
		"step climbs out":       {Repo: "o/r", Name: "x", AfterDownload: []AfterDownloadStep{{Python: "../evil.py", Creates: "t.json"}}},
		"step absolute":         {Repo: "o/r", Name: "x", AfterDownload: []AfterDownloadStep{{Python: "/tmp/evil.py", Creates: "t.json"}}},
		"creates a nested path": {Repo: "o/r", Name: "x", AfterDownload: []AfterDownloadStep{{Python: "tools/t.py", Creates: "sub/t.json"}}},
	}
	for name, entry := range cases {
		m := testEngineManifest(fakeEngineBin)
		m.Actions = map[string]Action{pullModelAction: {HFSnapshot: &ActionHFSnapshot{Dest: "{models_dir}/{model}"}}}
		m.Catalog = []CatalogEntry{entry}
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, entry)
		}
	}

	m := testEngineManifest(fakeEngineBin)
	m.Catalog = []CatalogEntry{{Repo: "o/r", Name: "x"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "hf_snapshot") {
		t.Errorf("a catalog without an hf_snapshot pull was accepted: %v", err)
	}

	m = testEngineManifest(fakeEngineBin)
	m.Actions = map[string]Action{pullModelAction: {HFSnapshot: &ActionHFSnapshot{Dest: "{models_dir}/{model}"}}}
	m.Catalog = []CatalogEntry{{Repo: "o/r", Name: "x"}, {Repo: "o/r", Name: "y"}}
	if err := m.Validate(); err == nil {
		t.Error("a repository listed twice was accepted")
	}
	m.Catalog = []CatalogEntry{{Repo: "o/r", Name: "x", Revision: sha, Exclude: []string{"dflash/*"},
		AfterDownload: []AfterDownloadStep{{Python: "tools/k3_tokenizer.py", Creates: "tokenizer.json"}}}}
	if err := m.Validate(); err != nil {
		t.Errorf("a full entry was refused: %v", err)
	}
}

// TestExcludedFollowsFnmatch checks exclusion matches as Python's fnmatch does,
// so a list copied from Colibri's own catalog means the same thing here: "*"
// crosses "/", the rest is literal.
func TestExcludedFollowsFnmatch(t *testing.T) {
	patterns := []string{"dflash/*", "audio_tokenizer/*", "model_mtp.safetensors", "docs/[!r]*.md", "shard-?.bin"}
	for name, want := range map[string]bool{
		"dflash/model.safetensors":       true,
		"dflash/deep/inside.bin":         true,
		"audio_tokenizer/config.json":    true,
		"model_mtp.safetensors":          true,
		"docs/guide.md":                  true,
		"docs/readme.md":                 false,
		"shard-1.bin":                    true,
		"shard-10.bin":                   false,
		"model-00001.safetensors":        false,
		"config.json":                    false,
		"other/dflash/model.safetensors": false,
		"model_mtpXsafetensors":          false,
	} {
		if got := excluded(name, patterns); got != want {
			t.Errorf("excluded(%q) = %t, want %t", name, got, want)
		}
	}
}

// TestBundledColibriCatalog checks the shipped list: every Colibri text model
// is listed under its repository with a size, Qwen3.8 is pinned, MiMo skips the
// files its engine never loads, and Kimi K3 writes the tokenizer.json its
// engine reads.
func TestBundledColibriCatalog(t *testing.T) {
	m, ok := buildBundledRegistry().Get("colibri")
	if !ok {
		t.Fatal("no colibri manifest")
	}
	if len(m.Catalog) < 11 {
		t.Fatalf("catalog has %d entries, want Colibri's eleven text models", len(m.Catalog))
	}
	for _, entry := range m.Catalog {
		if entry.SizeGB <= 0 || entry.Parameters == "" {
			t.Errorf("%s lists no size or parameter count", entry.Repo)
		}
	}
	for repo, check := range map[string]func(CatalogEntry) bool{
		"deepseek-ai/DeepSeek-V4-Flash-0731":      func(e CatalogEntry) bool { return e.Revision == "" && len(e.AfterDownload) == 0 },
		"puwaer/DeepSeek-V4-Flash-0731-reap-150b": func(e CatalogEntry) bool { return e.Revision == "" },
		"Qwen/Qwen3.8-Flash-Next-FP8":             func(e CatalogEntry) bool { return e.Revision == "bcd9f01ddc9cff2316eb84281bebcd5b058bddce" },
		"XiaomiMiMo/MiMo-V2.6-Flash-MOPD":         func(e CatalogEntry) bool { return excluded("model_mtp.safetensors", e.Exclude) },
		"moonshotai/Kimi-K3": func(e CatalogEntry) bool {
			return len(e.AfterDownload) == 1 && e.AfterDownload[0] == AfterDownloadStep{Python: "tools/k3_tokenizer.py", Creates: "tokenizer.json"}
		},
	} {
		entry, ok := m.catalogEntry(repo)
		if !ok || !check(entry) {
			t.Errorf("%s: listed %t as %+v", repo, ok, entry)
		}
	}
}

func TestHFSnapshotListedEntryPinsRevisionAndExcludes(t *testing.T) {
	files := colibriFiles()
	files["dflash/draft.safetensors"] = []byte("draft")
	files["model_mtp.safetensors"] = []byte("mtp")
	hub := newFakeHub(t, files)
	hub.revision = strings.Repeat("b", 40)
	e, _, store := listedEngine(t, hub, CatalogEntry{Revision: hub.revision, Exclude: []string{"dflash/*", "model_mtp.safetensors"}})

	raw, err := e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Files int `json:"files"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.Files != 3 {
		t.Fatalf("result %s, want the three files left after the exclusions", raw)
	}
	for _, name := range []string{"dflash", "model_mtp.safetensors"} {
		if _, err := os.Stat(filepath.Join(store, "o", "r", name)); !os.IsNotExist(err) {
			t.Errorf("excluded %s was downloaded: %v", name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(store, "o", "r", "config.json"))
	if err != nil || !bytes.Equal(got, files["config.json"]) {
		t.Fatalf("config.json from the pinned revision: %v", err)
	}
}

func TestHFSnapshotRunsAfterDownloadBeforeListing(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	e, st, store := listedEngine(t, hub, CatalogEntry{AfterDownload: []AfterDownloadStep{{Python: "tools/tok.py", Creates: "tokenizer.json"}}})
	fakeTool(t, st, "tools/tok.py", "create tokenizer.json\n")

	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err != nil {
		t.Fatal(err)
	}
	if !hasMarker(filepath.Join(store, "o", "r"), "tokenizer.json") {
		t.Fatal("the step's file is missing from the finished model")
	}
	if _, err := os.Stat(filepath.Join(store, "o", "r"+partialSuffix)); !os.IsNotExist(err) {
		t.Errorf("the partial directory was left behind: %v", err)
	}
}

// TestHFSnapshotAfterDownloadFailureIsNotListed checks a failed step leaves the
// download as a partial, so the store does not list a model its engine cannot
// read, and a later pull resumes rather than starting over.
func TestHFSnapshotAfterDownloadFailureIsNotListed(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	e, st, store := listedEngine(t, hub, CatalogEntry{AfterDownload: []AfterDownloadStep{{Python: "tools/tok.py", Creates: "tokenizer.json"}}})
	fakeTool(t, st, "tools/tok.py", "fail the vocabulary is unreadable\n")

	_, err := e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err == nil || !strings.Contains(err.Error(), "the vocabulary is unreadable") {
		t.Fatalf("pull = %v, want the step's own error", err)
	}
	if _, err := os.Stat(filepath.Join(store, "o", "r")); !os.IsNotExist(err) {
		t.Errorf("the model was moved into the store: %v", err)
	}
	if !hasMarker(filepath.Join(store, "o", "r"+partialSuffix), "config.json") {
		t.Error("the downloaded files were not kept for the next pull")
	}

	fakeTool(t, st, "tools/tok.py", "create tokenizer.json\n")
	hits := hub.fileHits.Load()
	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err != nil {
		t.Fatal(err)
	}
	if hub.fileHits.Load() != hits || !hasMarker(filepath.Join(store, "o", "r"), "tokenizer.json") {
		t.Errorf("retry fetched %d files again; tokenizer present %t", hub.fileHits.Load()-hits, hasMarker(filepath.Join(store, "o", "r"), "tokenizer.json"))
	}
}

// TestHFSnapshotFinishesAnEarlierCopy checks a model already in the store but
// missing a step's file (downloaded before the step was listed) gets the step on
// its next pull, without downloading anything.
func TestHFSnapshotFinishesAnEarlierCopy(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	e, st, store := listedEngine(t, hub, CatalogEntry{AfterDownload: []AfterDownloadStep{{Python: "tools/tok.py", Creates: "tokenizer.json"}}})
	dest := filepath.Join(store, "o", "r")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeTool(t, st, "tools/tok.py", "create tokenizer.json\n")

	raw, err := e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err != nil || !strings.Contains(string(raw), `"already":true`) {
		t.Fatalf("pull = %s, %v", raw, err)
	}
	if hub.fileHits.Load() != 0 || !hasMarker(dest, "tokenizer.json") {
		t.Errorf("downloads %d, tokenizer present %t", hub.fileHits.Load(), hasMarker(dest, "tokenizer.json"))
	}
}

// TestHFSnapshotAfterDownloadNeedsTheEngine checks a step whose script is not
// installed says what to do instead of failing on a missing file.
func TestHFSnapshotAfterDownloadNeedsTheEngine(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	e, _, _ := listedEngine(t, hub, CatalogEntry{AfterDownload: []AfterDownloadStep{{Python: "tools/tok.py", Creates: "tokenizer.json"}}})

	_, err := e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err == nil || !strings.Contains(err.Error(), "install") || !strings.Contains(err.Error(), "tools/tok.py") {
		t.Fatalf("pull = %v, want a hint to install the engine first", err)
	}
}

func TestValidateModelTypesRejects(t *testing.T) {
	for name, types := range map[string][]string{
		"uppercase": {"Qwen3_moe"},
		"padded":    {" qwen3_moe"},
		"empty":     {""},
		"twice":     {"qwen3_moe", "qwen3_moe"},
	} {
		m := testEngineManifest(fakeEngineBin)
		m.Actions = map[string]Action{pullModelAction: {HFSnapshot: &ActionHFSnapshot{Dest: "{models_dir}/{model}"}}}
		m.ModelTypes = types
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted %q", name, types)
		}
	}
	m := testEngineManifest(fakeEngineBin)
	m.ModelTypes = []string{"qwen3_moe"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "hf_snapshot") {
		t.Errorf("model_types without an hf_snapshot pull was accepted: %v", err)
	}
}

// TestBundledColibriRunsItsOwnCatalog checks the model_types list covers every
// model Colibri's own setup list offers, by the model_type each repository's
// config.json declared when the list was written, and leaves out what it has
// no engine for.
func TestBundledColibriRunsItsOwnCatalog(t *testing.T) {
	m, ok := buildBundledRegistry().Get("colibri")
	if !ok {
		t.Fatal("no colibri manifest")
	}
	for repo, modelType := range map[string]string{
		"Kreuzzelg/qwen36-35b-a3b-colibri-i4-gs64":        "qwen3_5_moe_text",
		"Justvugg/Qwen3-Coder-30B-A3B-colibri-int4":       "qwen3_moe",
		"deepseek-ai/DeepSeek-V4-Flash-0731":              "deepseek_v4",
		"Qwen/Qwen3.8-Flash-Next-FP8":                     "qwen4_exp",
		"XiaomiMiMo/MiMo-V2.6-Flash-MOPD":                 "mimo_v2",
		"mastouri/GLM-5.2-colibri-int4-g64-with-int8-mtp": "glm_moe_dsa",
		"nbeerbower/Inkling-colibri-int4":                 "inkling_mm_model",
		"moonshotai/Kimi-K3":                              "kimi_k3",
	} {
		if !m.servesModelType(modelType) {
			t.Errorf("%s is a %s model, which the list does not run", repo, modelType)
		}
	}
	for _, modelType := range []string{"hy_v3", "laguna", "minimax_m2", "nemotron_h", "qwenimage21pipeline", "laya_modernbert", ""} {
		if m.servesModelType(modelType) {
			t.Errorf("model_type %q is listed, but Colibri v2.1.0 has no chat engine for it", modelType)
		}
	}
	if !m.servesModelType(" Qwen3_MoE ") {
		t.Error("a model_type is not compared the way Colibri compares it")
	}
}

// TestHFSnapshotRefusesAModelTheEngineCannotRun checks a tagged repository
// whose config.json names a model_type the engine has no code for is refused
// before anything is written, while a listed one is taken at its word.
func TestHFSnapshotRefusesAModelTheEngineCannotRun(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	hub.modelType = "hy_v3"
	e, store, _ := snapshotEngine(t, hub)
	st, _ := e.state("fake")
	st.manifest.ModelTypes = []string{"qwen3_moe"}

	_, err := e.PullModelStream(context.Background(), "fake", "o/r", nil)
	if err == nil || !strings.Contains(err.Error(), `"hy_v3"`) {
		t.Fatalf("pull = %v, want a refusal naming the model_type", err)
	}
	if hub.fileHits.Load() != 0 {
		t.Errorf("%d files were fetched before the refusal", hub.fileHits.Load())
	}
	if _, err := os.Stat(filepath.Join(store, "o", "r"+partialSuffix)); !os.IsNotExist(err) {
		t.Errorf("a refused download left a partial directory: %v", err)
	}

	hub.modelType = ""
	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err == nil || !strings.Contains(err.Error(), "no model_type") {
		t.Errorf("pull of a repository with no model_type = %v, want a refusal", err)
	}

	hub.modelType = "Qwen3_MoE"
	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err != nil {
		t.Fatalf("a served model_type was refused: %v", err)
	}
}

func TestHFSnapshotTrustsAListedModelsType(t *testing.T) {
	hub := newFakeHub(t, colibriFiles())
	hub.modelType = "some_new_type"
	e, st, _ := listedEngine(t, hub, CatalogEntry{})
	st.manifest.ModelTypes = []string{"qwen3_moe"}
	if _, err := e.PullModelStream(context.Background(), "fake", "o/r", nil); err != nil {
		t.Fatalf("a listed model was refused for its model_type: %v", err)
	}
}
