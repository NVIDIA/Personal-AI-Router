// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLMStudioDeleteModelRemovePath(t *testing.T) {
	root := t.TempDir()
	modelRel := "publisher/demo-model"
	modelDir := filepath.Join(root, modelRel)
	require.NoError(t, os.MkdirAll(modelDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "weights.gguf"), []byte("x"), 0o644))

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
	require.NoError(t, ex.Start(ctx, "fake"), "start")

	_, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`))
	require.NoError(t, err, "delete_model")
	{
		_, err := os.Stat(modelDir)
		require.ErrorIs(t, err, os.ErrNotExist, "model dir still exists")
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
	require.NoError(t, err, "read fake engine")
	bin := filepath.Join(t.TempDir(), "fake-engine"+filepath.Ext(fakeEngineBin))
	require.NoError(t, os.WriteFile(bin, data, 0o755), "write fake engine copy")
	return bin
}

// engineRun reports the engine's start generation, which the executor bumps on
// every start, plus whether it is currently running.
func engineRun(t *testing.T, ex *Executor, engine string) (generation int64, running bool) {
	t.Helper()
	st, err := ex.state(engine)
	require.NoError(t, err, "state (%v, %v)", engine, err)
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
	require.NoError(t, os.MkdirAll(filepath.Join(root, modelRel), 0o755))

	ex := newTestExecutor(t, deleteModelRestartManifest(t, root, fakeEngineBin))
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"), "start")
	before, _ := engineRun(t, ex, "fake")

	{
		_, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`))
		require.NoError(t, err, "delete_model")
	}

	after, running := engineRun(t, ex, "fake")
	require.Greater(t, after, before, "start generation")
	require.True(t, running, "engine is stopped after delete_model; restart_after must leave it up")
}

// TestDeleteModelRestartAfterLeavesStoppedEngineDown covers the other half of
// the contract: a stopped engine serves nothing, so there is no stale index to
// reconcile and restart_after must not start it behind the user's back.
func TestDeleteModelRestartAfterLeavesStoppedEngineDown(t *testing.T) {
	root := t.TempDir()
	modelRel := "publisher/demo-model"
	require.NoError(t, os.MkdirAll(filepath.Join(root, modelRel), 0o755))

	ex := newTestExecutor(t, deleteModelRestartManifest(t, root, fakeEngineBin))
	{
		_, err := ex.Action(context.Background(), "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`))
		require.NoError(t, err, "delete_model")
	}

	{
		_, err := os.Stat(filepath.Join(root, modelRel))
		require.ErrorIs(t, err, os.ErrNotExist, "model dir still exists")
	}
	{
		_, running := engineRun(t, ex, "fake")
		require.False(t, running, "delete_model started a stopped engine")
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
	require.NoError(t, os.MkdirAll(filepath.Join(root, modelRel), 0o755))

	m := deleteModelRestartManifest(t, root, fakeEngineBin)
	act := m.Actions["delete_model"]
	act.RestartAfter = false
	m.Actions["delete_model"] = act

	ex := newTestExecutor(t, m)
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"), "start")
	before, _ := engineRun(t, ex, "fake")

	{
		_, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`))
		require.NoError(t, err, "delete_model")
	}

	after, running := engineRun(t, ex, "fake")
	require.Equal(t, before, after, "start generation")
	require.True(t, running, "engine is stopped after delete_model")
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
	require.NoError(t, os.MkdirAll(filepath.Join(root, modelRel), 0o755))

	bin := privateEngineBin(t)
	ex := newTestExecutor(t, deleteModelRestartManifest(t, root, bin))
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"), "start")

	// Moving the binary out from under the engine makes Detect fail, so the
	// restart's start half cannot succeed and the engine stays down. Renaming
	// rather than deleting keeps this portable: Windows refuses to delete a
	// running executable but allows moving it.
	require.NoError(t, os.Rename(bin, bin+".moved"), "move engine binary aside")

	_, err := ex.Action(ctx, "fake", "delete_model", []byte(`{"model":"`+modelRel+`"}`))
	require.ErrorContains(t, err, "failed to restart", "delete_model reported success although the engine never came back")
	{
		_, statErr := os.Stat(filepath.Join(root, modelRel))
		require.ErrorIs(t, statErr, os.ErrNotExist, "model dir still exists")
	}
	{
		_, running := engineRun(t, ex, "fake")
		require.False(t, running, "engine reports running after a failed restart")
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
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"), "LoadFS bundled")
	for engine, m := range reg.engines {
		for name, act := range m.Actions {
			if !act.RestartAfter {
				continue
			}
			assert.Equal(t, "lmstudio", engine, "engine (%v, %v)", engine, name)
			assert.Equal(t, "delete_model", name, "lmstudio action")
		}
	}
	require.True(t, reg.engines["lmstudio"].Actions["delete_model"].RestartAfter, "lmstudio delete_model lost restart_after; a deleted model would keep being served")
	require.False(t, reg.engines["ollama"].Actions["delete_model"].RestartAfter, "ollama delete_model declares restart_after; Ollama reflects deletions without a bounce")
}

// TestRestartAfterFitsRemoteReadinessBudget ties the manifest's readiness
// allowance to what an initiating peer is willing to wait for. A remote delete
// gets no reply until the post-delete restart is ready, and cutting it off
// cancels the peer's handler mid-restart: files gone, engine down, reported as
// a plain delete failure. Every restart_after platform must therefore finish
// inside the remote readiness budget.
func TestRestartAfterFitsRemoteReadinessBudget(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"), "LoadFS bundled")
	for engine, m := range reg.engines {
		if !hasRestartAfter(m) {
			continue
		}
		for key, p := range m.Platforms {
			if p.Runtime.Ready == nil {
				assert.Fail(t, "test expectation failed", "%s/%s: restart_after without a readiness probe", engine, key)
				continue
			}
			readiness := time.Duration(p.Runtime.Ready.TimeoutS) * time.Second
			assert.Greater(t, remoteReadyResponseHeaderTimeout, readiness, " (%v, %v, %v, %v)", engine, key, readiness, remoteReadyResponseHeaderTimeout)
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
	require.Error(t, err, "Validate accepted restart_after without a readiness probe")
	require.ErrorContains(t, err, "runtime.ready")
}

func TestModelActionWireOllamaUnload(t *testing.T) {
	action, params, err := modelActionWire("ollama", "unload", "llama3.2")
	require.NoError(t, err)
	require.Equal(t, "unload_model", action, "action")
	require.Contains(t, string(params), `"keep_alive":0`, "params missing keep_alive (%v)", params)
}
