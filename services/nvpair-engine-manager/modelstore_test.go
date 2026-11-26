// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeModel creates a complete model directory (one holding the marker) at
// id below store.
func writeModel(t *testing.T, store, id string) {
	t.Helper()
	dir := filepath.Join(store, filepath.FromSlash(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanModelDirsSkipsPartialsAndWrongDepth(t *testing.T) {
	store := t.TempDir()
	writeModel(t, store, "z/y")
	writeModel(t, store, "a/b")
	writeModel(t, store, "a/d.partial")   // still downloading
	writeModel(t, store, ".hidden/model") // not a model id
	writeModel(t, store, "flat")          // one level, as `coli setup` writes it
	writeModel(t, store, "x/y/z")         // three levels: too deep
	if err := os.MkdirAll(filepath.Join(store, "a", "nomarker"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory named like the marker is not the marker.
	if err := os.MkdirAll(filepath.Join(store, "a", "dirmarker", "config.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := scanModelDirs(store, "config.json", 2)
	if want := []string{"a/b", "flat", "z/y"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scanModelDirs = %v, want %v", got, want)
	}
	if got := scanModelDirs(filepath.Join(store, "missing"), "config.json", 2); len(got) != 0 {
		t.Fatalf("a missing store listed %v", got)
	}
}

func TestValidModelID(t *testing.T) {
	for _, id := range []string{"owner/repo", "Justvugg/Qwen3-Coder-30B-A3B-colibri-int4", "a.b/c_d-e", "qwen36-35b"} {
		if !validModelID(id, 2) {
			t.Errorf("validModelID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "a/b/c", "../x", "a/..", "-a/b", "a/-b", "/a/b", `a\b`, "a/b.partial", "a /b", "a/b?"} {
		if validModelID(id, 2) {
			t.Errorf("validModelID(%q) = true, want false", id)
		}
	}
}

// modelEngine is the fake engine set up like Colibri: a directory store, a
// launch environment that names the model, and the store actions.
func modelEngine(t *testing.T) (*Executor, string, string) {
	t.Helper()
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	store := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env.json")
	p.ModelsDir = store
	p.Runtime.Env = map[string]string{
		"OLLAMA_HOST":        "127.0.0.1:{port}",
		"PAIR_TEST_ENV_FILE": envFile,
		"PAIR_TEST_LITERAL":  "{models_dir}/{model}",
	}
	m.Platforms[key] = p
	m.Actions = map[string]Action{
		"list_models": {
			ScanDir: &ActionScanDir{Root: "{models_dir}", Marker: "config.json", Depth: 2},
			Result:  &ActionResult{Array: "models", Field: "name"},
		},
		"load_model": {
			LaunchModel:  &ActionLaunchModel{Path: "{models_dir}/{model}", Marker: "config.json"},
			RestartAfter: true,
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("model engine manifest: %v", err)
	}
	e := newTestExecutor(t, m)
	e.overrideDir = t.TempDir()
	if _, err := e.Detect("fake"); err != nil {
		t.Fatal(err)
	}
	st, err := e.state("fake")
	if err != nil {
		t.Fatal(err)
	}
	st.modelsDir = store
	t.Cleanup(func() { _ = e.Stop("fake") })
	return e, store, envFile
}

// launchedModel reads the model path the fake engine was started with.
func launchedModel(t *testing.T, envFile string) string {
	t.Helper()
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	return env["PAIR_TEST_LITERAL"]
}

func TestStartFailsWithoutLaunchModel(t *testing.T) {
	e, _, _ := modelEngine(t)
	err := e.Start(context.Background(), "fake")
	if err == nil || !errors.Is(err, errNoLaunchModel) {
		t.Fatalf("Start with an empty store = %v, want errNoLaunchModel", err)
	}
	if !strings.Contains(err.Error(), "download a model before starting Fake Engine") {
		t.Fatalf("error %q does not tell the user what to do", err)
	}
	if !hasErr(e.Errors(), startFailedID("fake")) {
		t.Fatal("the refused start was not reported")
	}
	if st, _ := e.Status("fake"); st.Running {
		t.Fatal("engine reported running with no model")
	}
}

func TestStartPrefersPersistedModelThenFirstFound(t *testing.T) {
	e, store, envFile := modelEngine(t)
	writeModel(t, store, "a/b")
	writeModel(t, store, "c/d")

	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if got, want := launchedModel(t, envFile), store+"/a/b"; got != want {
		t.Fatalf("with nothing persisted the launch served %q, want the first model %q", got, want)
	}
	if err := e.Stop("fake"); err != nil {
		t.Fatal(err)
	}

	st, _ := e.state("fake")
	st.plat.Runtime.Model = "c/d"
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if got, want := launchedModel(t, envFile), store+"/c/d"; got != want {
		t.Fatalf("persisted model: launch served %q, want %q", got, want)
	}
	if err := e.Stop("fake"); err != nil {
		t.Fatal(err)
	}

	// The persisted model was deleted: fall back rather than fail.
	if err := os.RemoveAll(filepath.Join(store, "c")); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if got, want := launchedModel(t, envFile), store+"/a/b"; got != want {
		t.Fatalf("deleted persisted model: launch served %q, want %q", got, want)
	}
}

func TestScanDirActionListsModelsWhileStopped(t *testing.T) {
	e, store, _ := modelEngine(t)
	writeModel(t, store, "o/r")
	raw, err := e.Action(context.Background(), "fake", "list_models", nil)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := e.state("fake")
	models, ok := extractStringsResult(raw, st.manifest.Actions["list_models"].Result)
	if !ok || !reflect.DeepEqual(models, []string{"o/r"}) {
		t.Fatalf("list_models = %s (%v, %v), want [o/r]", raw, models, ok)
	}
}

func TestLaunchModelPersistsAndRestarts(t *testing.T) {
	e, store, envFile := modelEngine(t)
	writeModel(t, store, "a/b")
	writeModel(t, store, "c/d")
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if got := launchedModel(t, envFile); got != store+"/a/b" {
		t.Fatalf("initial launch served %q", got)
	}

	raw, err := e.Action(context.Background(), "fake", "load_model", json.RawMessage(`{"model":"c/d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"model":"c/d"}` {
		t.Fatalf("load_model result = %s", raw)
	}
	if st, _ := e.Status("fake"); !st.Running {
		t.Fatal("the engine did not come back after switching models")
	}
	if got := launchedModel(t, envFile); got != store+"/c/d" {
		t.Fatalf("after load_model the engine serves %q, want %q", got, store+"/c/d")
	}
	override, err := os.ReadFile(filepath.Join(e.overrideDir, "fake.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Runtime struct {
			Model string `json:"model"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(override, &saved); err != nil || saved.Runtime.Model != "c/d" {
		t.Fatalf("override %s does not persist the launch model (err %v)", override, err)
	}
}

func TestLaunchModelStoppedEngineStaysStopped(t *testing.T) {
	e, store, _ := modelEngine(t)
	writeModel(t, store, "a/b")
	if _, err := e.Action(context.Background(), "fake", "load_model", json.RawMessage(`{"model":"a/b"}`)); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status("fake"); st.Running {
		t.Fatal("selecting a model started a stopped engine")
	}
	if st, _ := e.state("fake"); st.plat.Runtime.Model != "a/b" {
		t.Fatalf("in-memory launch model = %q, want a/b", st.plat.Runtime.Model)
	}
}

func TestLaunchModelRejectsTraversalAndMissingMarker(t *testing.T) {
	e, store, _ := modelEngine(t)
	writeModel(t, store, "a/b")
	for _, tc := range []struct{ model, want string }{
		{"", "model is required"},
		{"../x", "not a valid model name"},
		{"-x/y", "not a valid model name"},
		{"a/b/c", "not a valid model name"},
		{"a", "is not downloaded"}, // an owner folder, not a model
		{"a/missing", "is not downloaded"},
	} {
		params, _ := json.Marshal(map[string]string{"model": tc.model})
		_, err := e.Action(context.Background(), "fake", "load_model", params)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("load_model %q = %v, want an error containing %q", tc.model, err, tc.want)
		}
	}
	if entries, _ := os.ReadDir(e.overrideDir); len(entries) != 0 {
		t.Fatal("a refused selection wrote the override")
	}
}

// The launch settings show the model the engine will serve, and saving them
// unchanged must not pin that model as a literal environment value — a later
// model switch would otherwise be silently overridden.
func TestLaunchSettingsRenderModelPlaceholder(t *testing.T) {
	e := settingsExecutor(t, false)
	st := settingsState(t, e)
	store := t.TempDir()
	st.modelsDir = store
	st.plat.ModelsDir = store
	st.plat.Runtime.Env["PAIR_TEST_LITERAL"] = "{models_dir}/{model}"
	st.manifest.Actions = map[string]Action{
		"list_models": {
			ScanDir: &ActionScanDir{Root: "{models_dir}", Marker: "config.json", Depth: 2},
			Result:  &ActionResult{Array: "models", Field: "name"},
		},
	}

	// No model yet: the settings still render.
	state, err := e.LaunchSettings("fake")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Editable {
		t.Fatalf("settings with no model downloaded are not editable: %q", state.Reason)
	}

	writeModel(t, store, "o/r")
	state, err = e.LaunchSettings("fake")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Editable || !strings.Contains(state.LaunchText, "o/r") {
		t.Fatalf("launch text %q (reason %q) does not show the launch model", state.LaunchText, state.Reason)
	}
	preview := previewSettings(t, e, settingsRequest(t, e))
	if len(preview.Errors) != 0 {
		t.Fatalf("preview errors: %v", preview.Errors)
	}
	if len(preview.Env) != 0 {
		t.Fatalf("an unchanged save pinned %v as literal environment", preview.Env)
	}
}

// A model that lands in the store while the engine runs is listed on the next
// sweep, since the inventory is the store itself.
func TestModelsResultListsTheStore(t *testing.T) {
	e, store, _ := modelEngine(t)
	writeModel(t, store, "a/b")
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	writeModel(t, store, "c/d")
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := e.ModelsResult(context.Background())
		if reflect.DeepEqual(res.ByEngine["fake"], []string{"a/b", "c/d"}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine:models = %+v, want both models", res)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// delete_model on a layered store removes exactly one model, never the store
// or an owner folder holding several.
func TestDeleteModelRemovesOnlyOneModel(t *testing.T) {
	e, store, _ := modelEngine(t)
	st, _ := e.state("fake")
	st.manifest.Actions["delete_model"] = Action{RemovePath: &ActionRemovePath{Root: "{models_dir}", Path: "{models_dir}/{model}"}}
	writeModel(t, store, "o/a")
	writeModel(t, store, "o/b")
	for _, model := range []string{"", "o", "o/..", "../o"} {
		params, _ := json.Marshal(map[string]string{"model": model})
		if _, err := e.Action(context.Background(), "fake", "delete_model", params); err == nil {
			t.Errorf("delete_model %q succeeded", model)
		}
	}
	if got := scanModelDirs(store, "config.json", 2); !reflect.DeepEqual(got, []string{"o/a", "o/b"}) {
		t.Fatalf("refused deletes changed the store: %v", got)
	}
	if _, err := e.Action(context.Background(), "fake", "delete_model", json.RawMessage(`{"model":"o/a"}`)); err != nil {
		t.Fatal(err)
	}
	if got := scanModelDirs(store, "config.json", 2); !reflect.DeepEqual(got, []string{"o/b"}) {
		t.Fatalf("after deleting o/a the store holds %v", got)
	}
}

// Installing a single-model engine before any model exists is the normal first
// step, so the install's start is skipped silently rather than reported as a
// failure; once a model is downloaded the same call starts it.
func TestStartAfterInstallWaitsForAModel(t *testing.T) {
	e, store, envFile := modelEngine(t)
	if err := e.StartAfterInstall(context.Background(), "fake", startOpts{}); err != nil {
		t.Fatalf("StartAfterInstall with an empty store = %v, want nil", err)
	}
	if st, _ := e.Status("fake"); st.Running {
		t.Fatal("the engine started with no model")
	}
	if len(e.Errors()) != 0 {
		t.Fatalf("an expected post-install state was reported as errors: %+v", e.Errors())
	}
	if enabled, known, _ := e.desired.get("fake"); known && enabled {
		t.Fatal("the skipped start recorded the engine as switched on")
	}

	writeModel(t, store, "o/r")
	if err := e.StartAfterInstall(context.Background(), "fake", startOpts{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status("fake"); !st.Running {
		t.Fatal("the engine did not start once a model was downloaded")
	}
	if got := launchedModel(t, envFile); got != store+"/o/r" {
		t.Fatalf("launch served %q", got)
	}
}
