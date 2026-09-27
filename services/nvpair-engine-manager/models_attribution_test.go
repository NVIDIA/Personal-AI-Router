// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
)

// twoEngineExecutor registers two fake-engine manifests under the real engine
// names so one ModelsResult sweep covers both — the way a host running an
// external Ollama beside LM Studio is swept. The LM Studio manifest uses the
// native REST v1 shape (key field, loaded_instances row filter) so the sweep
// exercises both extractors, not two copies of the Ollama one.
func twoEngineExecutor(t *testing.T) *Executor {
	t.Helper()
	ollama := testEngineManifest(fakeEngineBin)
	ollama.Engine = "ollama"
	ollama.Actions = map[string]Action{
		"list_models": {
			HTTP:   &ActionHTTP{Method: "GET", Path: "/api/tags"},
			Result: &ActionResult{Array: "models", Field: "name"},
		},
		"loaded_models": {
			HTTP:   &ActionHTTP{Method: "GET", Path: "/api/ps"},
			Result: &ActionResult{Array: "models", Field: "name"},
		},
	}
	lmstudio := testEngineManifest(fakeEngineBin)
	lmstudio.Engine = "lmstudio"
	lmstudio.Actions = map[string]Action{
		"list_models": {
			HTTP:   &ActionHTTP{Method: "GET", Path: "/api/v1/models"},
			Result: &ActionResult{Array: "models", Field: "key"},
		},
		"loaded_models": {
			HTTP: &ActionHTTP{Method: "GET", Path: "/api/v1/models"},
			Result: &ActionResult{
				Array: "models",
				Field: "key",
				Match: &ResultMatch{Field: "loaded_instances", Nonempty: true},
			},
		},
	}
	reg := NewRegistry()
	reg.engines[ollama.Engine] = ollama
	reg.engines[lmstudio.Engine] = lmstudio
	return NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
}

// fakeEnginePost drives a running fake engine's HTTP surface directly: its
// /api/pull adds a model to the inventory and /testctl/loaded replaces the
// resident set. Reaching past the executor keeps the assertions about the
// sweep itself, not about the pull/load action plumbing.
func fakeEnginePost(t *testing.T, ex *Executor, engine, path string, body any) {
	t.Helper()
	st, err := ex.Status(engine)
	if err != nil {
		t.Fatalf("status %s: %v", engine, err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s body: %v", path, err)
	}
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d%s", st.Port, path), "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s %s: %v", engine, path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// attributionEqual compares two per-engine maps key-by-key as order-insensitive
// sets: the fake engine lists its inventory from a Go map, so bucket order is
// not stable and must not be part of the contract.
func attributionEqual(got, want map[string][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for engine, wantNames := range want {
		gotNames, ok := got[engine]
		if !ok || !sameStringSet(gotNames, wantNames) {
			return false
		}
	}
	return true
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestModelsChangedSnapshotKeepsPerEngineAttribution reproduces the
// 2026-09-26 desktop.log shape that read like cross-wiring: an
// engine:models-changed push tagged with one engine whose snapshot carried
// another engine's model names, and an Ollama inventory listing names that
// look like they belong elsewhere ("local_llamacpp", an LM Studio embedding
// key — what a federating gateway answering on 11434 returns from /api/tags).
// Both are by design: the push is {trigger engine, FULL multi-engine snapshot},
// and ByEngine repeats exactly what each engine's own list_models returned.
// What this pins is that no engine's bucket ever gains a name only another
// engine reported, that a name both engines serve stays in both buckets while
// the flat union carries it once, and that the trigger is the engine whose
// loaded set moved — not the one whose names dominate the union.
func TestModelsChangedSnapshotKeepsPerEngineAttribution(t *testing.T) {
	ex := twoEngineExecutor(t)
	ctx := context.Background()
	for _, engine := range []string{"ollama", "lmstudio"} {
		engine := engine
		if err := ex.Start(ctx, engine); err != nil {
			t.Fatalf("start %s: %v", engine, err)
		}
		t.Cleanup(func() { _ = ex.Stop(engine) })
	}

	// Both fakes seed llama3.2:1b. Ollama additionally reports a llama.cpp-looking
	// name and the LM Studio embedding key; LM Studio reports that same key.
	const federated = "text-embedding-nomic-embed-text-v1.5"
	fakeEnginePost(t, ex, "ollama", "/api/pull", map[string]string{"name": "local_llamacpp"})
	fakeEnginePost(t, ex, "ollama", "/api/pull", map[string]string{"name": federated})
	fakeEnginePost(t, ex, "lmstudio", "/api/pull", map[string]string{"name": federated})
	fakeEnginePost(t, ex, "ollama", "/testctl/loaded", map[string][]string{"names": {"llama3.2:1b"}})
	fakeEnginePost(t, ex, "lmstudio", "/testctl/loaded", map[string][]string{"names": {}})

	// Seed the watcher baseline, then move ONLY LM Studio's residency.
	_, prev, _ := ex.sweepLoaded(ctx, nil)
	fakeEnginePost(t, ex, "lmstudio", "/testctl/loaded", map[string][]string{"names": {federated}})
	changed, _, res := ex.sweepLoaded(ctx, prev)
	if !reflect.DeepEqual(changed, []string{"lmstudio"}) {
		t.Fatalf("changed = %v, want [lmstudio]: the trigger is the engine whose loaded set moved", changed)
	}

	wantByEngine := map[string][]string{
		"ollama":   {"llama3.2:1b", "local_llamacpp", federated},
		"lmstudio": {"llama3.2:1b", federated},
	}
	if !attributionEqual(res.ByEngine, wantByEngine) {
		t.Fatalf("ByEngine = %v, want each engine's own inventory %v", res.ByEngine, wantByEngine)
	}
	wantLoaded := map[string][]string{
		"ollama":   {"llama3.2:1b"},
		"lmstudio": {federated},
	}
	if !attributionEqual(res.LoadedByEngine, wantLoaded) {
		t.Fatalf("LoadedByEngine = %v, want %v", res.LoadedByEngine, wantLoaded)
	}
	seen := 0
	for _, n := range res.Models {
		if n == federated {
			seen++
		}
	}
	if seen != 1 || !hasName(res.Models, "local_llamacpp") {
		t.Fatalf("Models = %v, want the shared name once and every engine's names present", res.Models)
	}

	// The wire payload consumers parse: trigger engine + full snapshot. Pin the
	// key names the desktop and TUI read (models.modelsByEngine /
	// models.loadedByEngine) and that an lmstudio-tagged push still carries
	// Ollama's names in the union without leaking them into the lmstudio bucket.
	raw, err := json.Marshal(modelsChangedParams{Engine: changed[0], Models: res})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	var wire struct {
		Engine string `json:"engine"`
		Models struct {
			Models         []string            `json:"models"`
			ModelsByEngine map[string][]string `json:"modelsByEngine"`
			LoadedByEngine map[string][]string `json:"loadedByEngine"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal push: %v", err)
	}
	if wire.Engine != "lmstudio" {
		t.Fatalf("push engine = %q, want lmstudio", wire.Engine)
	}
	if !hasName(wire.Models.Models, "local_llamacpp") {
		t.Fatalf("push models = %v, want Ollama's names carried in the union of an lmstudio-tagged push", wire.Models.Models)
	}
	if hasName(wire.Models.ModelsByEngine["lmstudio"], "local_llamacpp") {
		t.Fatalf("push modelsByEngine.lmstudio = %v, must not gain a name only ollama reported", wire.Models.ModelsByEngine["lmstudio"])
	}
	if hasName(wire.Models.LoadedByEngine["ollama"], federated) {
		t.Fatalf("push loadedByEngine.ollama = %v, must not inherit lmstudio's residency for a shared name", wire.Models.LoadedByEngine["ollama"])
	}
}
