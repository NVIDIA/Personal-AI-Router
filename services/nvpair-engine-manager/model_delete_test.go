// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLMStudioDeleteModelRemovePath(t *testing.T) {
	root := t.TempDir()
	modelRel := "publisher/demo-model"
	modelDir := filepath.Join(root, modelRel)
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "weights.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := testEngineManifest(fakeEngineBin)
	m.Actions["list_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/api/tags"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
	m.Actions["delete_model"] = Action{
		RemovePath: &ActionRemovePath{
			Root: root,
			Path: filepath.Join(root, "{model}"),
		},
	}

	ex := newTestExecutor(t, m)
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}

	_, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`))
	if err != nil {
		t.Fatalf("delete_model: %v", err)
	}
	if _, err := os.Stat(modelDir); !os.IsNotExist(err) {
		t.Fatalf("model dir still exists: %v", err)
	}
}

// deleteModelRestartManifest mirrors LM Studio's bundled delete_model: a guarded
// filesystem removal that declares restart_after. bin lets a caller point the
// engine at a private copy of the fake binary it is free to break.
func deleteModelRestartManifest(t *testing.T, root, bin string) *Manifest {
	t.Helper()
	m := testEngineManifest(bin)
	m.Actions["delete_model"] = Action{
		RemovePath: &ActionRemovePath{
			Root: root,
			Path: filepath.Join(root, "{model}"),
		},
		RestartAfter: true,
	}
	return m
}

// privateEngineBin copies the shared fake engine into the test's own temp dir so
// a test may delete or corrupt it without disturbing any other test.
func privateEngineBin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(fakeEngineBin)
	if err != nil {
		t.Fatalf("read fake engine: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "fake-engine"+filepath.Ext(fakeEngineBin))
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatalf("write fake engine copy: %v", err)
	}
	return bin
}

// engineRun reports the engine's start generation, which the executor bumps on
// every start, plus whether it is currently running.
func engineRun(t *testing.T, ex *Executor, engine string) (generation int64, running bool) {
	t.Helper()
	st, err := ex.state(engine)
	if err != nil {
		t.Fatalf("state(%s): %v", engine, err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.gen, st.running
}

// TestDeleteModelRestartAfterBouncesRunningEngine pins why LM Studio's
// delete_model declares restart_after: the files are gone, but its server keeps
// answering /v1/models from an index built at startup, and it offers no rescan.
// The engine must come back up, not merely stop.
func TestDeleteModelRestartAfterBouncesRunningEngine(t *testing.T) {
	root := t.TempDir()
	modelRel := "publisher/demo-model"
	if err := os.MkdirAll(filepath.Join(root, modelRel), 0o755); err != nil {
		t.Fatal(err)
	}

	ex := newTestExecutor(t, deleteModelRestartManifest(t, root, fakeEngineBin))
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	before, _ := engineRun(t, ex, "fake")

	if _, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`)); err != nil {
		t.Fatalf("delete_model: %v", err)
	}

	after, running := engineRun(t, ex, "fake")
	if after <= before {
		t.Fatalf("start generation = %d, want > %d (engine was not restarted)", after, before)
	}
	if !running {
		t.Fatal("engine is stopped after delete_model; restart_after must leave it up")
	}
}

// TestDeleteModelRestartAfterLeavesStoppedEngineDown covers the other half of
// the contract: a stopped engine serves nothing, so there is no stale index to
// reconcile and restart_after must not start it behind the user's back.
func TestDeleteModelRestartAfterLeavesStoppedEngineDown(t *testing.T) {
	root := t.TempDir()
	modelRel := "publisher/demo-model"
	if err := os.MkdirAll(filepath.Join(root, modelRel), 0o755); err != nil {
		t.Fatal(err)
	}

	ex := newTestExecutor(t, deleteModelRestartManifest(t, root, fakeEngineBin))
	if _, err := ex.Action(context.Background(), "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`)); err != nil {
		t.Fatalf("delete_model: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, modelRel)); !os.IsNotExist(err) {
		t.Fatalf("model dir still exists: %v", err)
	}
	if _, running := engineRun(t, ex, "fake"); running {
		t.Fatal("delete_model started a stopped engine")
	}
}

// TestDeleteModelWithoutRestartAfterDoesNotBounce is the Ollama side of the
// contract. Ollama reflects a deletion immediately, so its manifest omits
// restart_after and its delete must leave the engine's process completely alone
// — no bounce, no interrupted inference. The restart is opt-in per action, never
// a property of deleting.
func TestDeleteModelWithoutRestartAfterDoesNotBounce(t *testing.T) {
	root := t.TempDir()
	modelRel := "publisher/demo-model"
	if err := os.MkdirAll(filepath.Join(root, modelRel), 0o755); err != nil {
		t.Fatal(err)
	}

	m := deleteModelRestartManifest(t, root, fakeEngineBin)
	act := m.Actions["delete_model"]
	act.RestartAfter = false
	m.Actions["delete_model"] = act

	ex := newTestExecutor(t, m)
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	before, _ := engineRun(t, ex, "fake")

	if _, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`)); err != nil {
		t.Fatalf("delete_model: %v", err)
	}

	after, running := engineRun(t, ex, "fake")
	if after != before {
		t.Fatalf("start generation = %d, want %d (engine was restarted without restart_after)", after, before)
	}
	if !running {
		t.Fatal("engine is stopped after delete_model")
	}
}

// TestDeleteModelRestartFailureFailsTheAction pins the partial-failure contract.
// The destructive half runs first and is never rolled back, so a restart that
// fails has to surface as an error the caller can tell apart from "nothing
// happened" — the desktop bridge keys its "deleted, but the engine did not come
// back" wording off exactly this message rather than re-reporting a failed
// delete and inviting a retry that would hit "not found on disk".
func TestDeleteModelRestartFailureFailsTheAction(t *testing.T) {
	root := t.TempDir()
	modelRel := "publisher/demo-model"
	if err := os.MkdirAll(filepath.Join(root, modelRel), 0o755); err != nil {
		t.Fatal(err)
	}

	bin := privateEngineBin(t)
	ex := newTestExecutor(t, deleteModelRestartManifest(t, root, bin))
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Moving the binary out from under the engine makes Detect fail, so the
	// restart's start half cannot succeed and the engine stays down. Renaming
	// rather than deleting keeps this portable: Windows refuses to delete a
	// running executable but allows moving it.
	if err := os.Rename(bin, bin+".moved"); err != nil {
		t.Fatalf("move engine binary aside: %v", err)
	}

	_, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`))
	if err == nil {
		t.Fatal("delete_model reported success although the engine never came back")
	}
	if !strings.Contains(err.Error(), "failed to restart") {
		t.Fatalf("error = %q, want it to name the restart as what failed", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, modelRel)); !os.IsNotExist(statErr) {
		t.Fatalf("model dir still exists: %v", statErr)
	}
	if _, running := engineRun(t, ex, "fake"); running {
		t.Fatal("engine reports running after a failed restart")
	}
}

// TestBundledManifestsRestartOnlyLMStudio guards the blast radius of
// restart_after. Restarting interrupts whatever the engine is serving, so it is
// justified only for an engine that cannot see a deletion any other way. If a
// new manifest ever needs it, that is a deliberate decision — and it also has to
// be mirrored in the desktop's EngineCapabilities so the UI still confirms
// first (see desktop/tests/modular/delete-model-restart.test.ts).
func TestBundledManifestsRestartOnlyLMStudio(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("LoadFS bundled: %v", err)
	}
	for engine, m := range reg.engines {
		for name, act := range m.Actions {
			if !act.RestartAfter {
				continue
			}
			if engine != "lmstudio" {
				t.Errorf("engine %q action %q declares restart_after; only lmstudio needs one", engine, name)
			}
			if name != "delete_model" {
				t.Errorf("lmstudio action %q declares restart_after; only delete_model needs one", name)
			}
		}
	}
	if !reg.engines["lmstudio"].Actions["delete_model"].RestartAfter {
		t.Fatal("lmstudio delete_model lost restart_after; a deleted model would keep being served")
	}
	if reg.engines["ollama"].Actions["delete_model"].RestartAfter {
		t.Fatal("ollama delete_model declares restart_after; Ollama reflects deletions without a bounce")
	}
}

// TestRestartAfterFitsRemoteReadinessBudget ties the manifest's readiness
// allowance to what an initiating peer is willing to wait for. A remote delete
// gets no reply until the post-delete restart is ready, and cutting it off
// cancels the peer's handler mid-restart: files gone, engine down, reported as
// a plain delete failure. Every restart_after platform must therefore finish
// inside the remote readiness budget.
func TestRestartAfterFitsRemoteReadinessBudget(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("LoadFS bundled: %v", err)
	}
	for engine, m := range reg.engines {
		if !hasRestartAfter(m) {
			continue
		}
		for key, p := range m.Platforms {
			if p.Runtime.Ready == nil {
				t.Errorf("%s/%s: restart_after without a readiness probe", engine, key)
				continue
			}
			readiness := time.Duration(p.Runtime.Ready.TimeoutS) * time.Second
			if remoteReadyResponseHeaderTimeout <= readiness {
				t.Errorf("%s/%s: readiness timeout %s is not covered by the remote readiness budget %s",
					engine, key, readiness, remoteReadyResponseHeaderTimeout)
			}
		}
	}
}

func hasRestartAfter(m *Manifest) bool {
	for _, act := range m.Actions {
		if act.RestartAfter {
			return true
		}
	}
	return false
}

// TestRestartAfterRequiresReadinessProbe covers the load-time guard: the promise
// restart_after makes ("the effect is visible when this replies") is only
// keepable if doStart waits for a readiness probe.
func TestRestartAfterRequiresReadinessProbe(t *testing.T) {
	m := deleteModelRestartManifest(t, t.TempDir(), fakeEngineBin)
	for key, p := range m.Platforms {
		p.Runtime.Ready = nil
		m.Platforms[key] = p
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("Validate accepted restart_after without a readiness probe")
	}
	if !strings.Contains(err.Error(), "runtime.ready") {
		t.Fatalf("error = %q, want it to name runtime.ready", err)
	}
}

func TestModelActionWireOllamaUnload(t *testing.T) {
	action, params, err := modelActionWire("ollama", "unload", "llama3.2")
	if err != nil {
		t.Fatal(err)
	}
	if action != "unload_model" {
		t.Fatalf("action = %q, want unload_model", action)
	}
	if !strings.Contains(string(params), `"keep_alive":0`) {
		t.Fatalf("params missing keep_alive: %s", params)
	}
}
