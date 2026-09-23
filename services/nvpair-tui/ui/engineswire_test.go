// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"testing"
)

// actionOf unwraps an engine:action envelope for assertions.
func actionOf(t *testing.T, envelope map[string]any) (string, map[string]any) {
	t.Helper()
	action, _ := envelope["action"].(string)
	params, ok := envelope["params"].(map[string]any)
	if !ok {
		t.Fatalf("params = %T, want map[string]any", envelope["params"])
	}
	return action, params
}

// TestPullSendsBothKeys guards the LM Studio fix: a pull must carry the model
// under BOTH "name" (Ollama's /api/pull body key) and "model" (LM Studio's
// `lms get {model}` CLI placeholder). Sending only "name" silently ran
// `lms get "" --yes`, so the download never reached LM Studio.
func TestPullSendsBothKeys(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio"} {
		envelope := modelActionWire(engine, "pull", "owner/model")
		if envelope["engine"] != engine {
			t.Fatalf("engine = %v, want %s", envelope["engine"], engine)
		}
		action, params := actionOf(t, envelope)
		if action != "pull_model" {
			t.Fatalf("%s: action = %q, want pull_model", engine, action)
		}
		if params["name"] != "owner/model" || params["model"] != "owner/model" {
			t.Errorf("%s: pull params = %v, want both name and model set", engine, params)
		}
	}
}

// TestOllamaLoadUsesRunModel guards the contract the two engines do NOT share.
// Ollama has no load action — warming a model is run_model with streaming off —
// so sending load_model errors, and the failure is quiet enough to look like the
// model simply not loading.
func TestOllamaLoadUsesRunModel(t *testing.T) {
	envelope := modelActionWire("ollama", "load", "llama3.2")
	action, params := actionOf(t, envelope)

	if action != "run_model" {
		t.Errorf("ollama load action = %q, want run_model", action)
	}
	if params["model"] != "llama3.2" {
		t.Errorf("model = %v", params["model"])
	}
	if params["stream"] != false {
		t.Errorf("stream = %v, want false; a streaming load never completes here", params["stream"])
	}

	// LM Studio does declare a real load action.
	lmEnvelope := modelActionWire("lmstudio", "load", "owner/model")
	if lmAction, _ := actionOf(t, lmEnvelope); lmAction != "load_model" {
		t.Errorf("lmstudio load action = %q, want load_model", lmAction)
	}
}

// TestOllamaUnloadSendsKeepAlive guards the other asymmetry: Ollama only frees a
// model when keep_alive is 0. Without it the request succeeds and the model
// stays resident, so eject appears to do nothing.
func TestOllamaUnloadSendsKeepAlive(t *testing.T) {
	envelope := modelActionWire("ollama", "unload", "llama3.2")
	action, params := actionOf(t, envelope)

	if action != "unload_model" {
		t.Errorf("action = %q, want unload_model", action)
	}
	if params["keep_alive"] != 0 {
		t.Errorf("keep_alive = %v, want 0; without it the model is not evicted", params["keep_alive"])
	}

	// LM Studio's unload takes no keep_alive.
	lmEnvelope := modelActionWire("lmstudio", "unload", "owner/model")
	if _, lmParams := actionOf(t, lmEnvelope); lmParams["keep_alive"] != nil {
		t.Errorf("lmstudio unload sent keep_alive = %v, want absent", lmParams["keep_alive"])
	}
}

// TestPullDeadlineReportsDetachedNotSilence guards the acknowledgement for a
// long download. A multi-gigabyte pull outlasts the reply deadline while the
// engine keeps working, so a failure would be wrong — but the previous silence
// was too, leaving the operator unable to tell a started download from a
// keystroke that missed.
func TestPullDeadlineReportsDetachedNotSilence(t *testing.T) {
	msg := classifyOpResult("download big-model", "ollama", "pull", context.DeadlineExceeded)
	if msg == nil {
		t.Fatal("a pull that outran its deadline produced no message at all")
	}
	op, ok := msg.(engineOpMsg)
	if !ok {
		t.Fatalf("got %T, want engineOpMsg", msg)
	}
	if op.err != nil {
		t.Errorf("a still-running download was reported as failed: %v", op.err)
	}
	if !op.detached {
		t.Error("a still-running download was reported as complete")
	}
}

// TestDeadlineLeniencyTracksOperationLength checks which operations are excused
// for a slow reply.
//
// The set is not "downloads": it is every operation whose duration is set by how
// much data moves or how slow an engine is to become ready. Ollama's load is
// run_model with streaming off, which does not answer until the model is
// resident, so a large model on cold storage exceeds the deadline routinely —
// and reporting "load failed" at the moment the model finishes loading is worse
// than saying nothing. Quick operations get no such excuse, because a deadline
// there is a real fault.
func TestDeadlineLeniencyTracksOperationLength(t *testing.T) {
	// Only operations engineOps actually declares: the TUI offers no engine
	// update, so listing one here would assert against a path nothing reaches.
	for _, op := range []string{"pull", "load", "install", "uninstall"} {
		result, ok := classifyOpResult("x", "ollama", op, context.DeadlineExceeded).(engineOpMsg)
		if !ok {
			t.Fatalf("%s: unexpected message type", op)
		}
		if !result.detached {
			t.Errorf("%s timing out was reported as a failure, but it is still running", op)
		}
		if result.err != nil {
			t.Errorf("%s carried an error despite still running: %v", op, result.err)
		}
	}

	for _, op := range []string{"unload", "delete", "start", "stop", "restart"} {
		result, ok := classifyOpResult("x", "ollama", op, context.DeadlineExceeded).(engineOpMsg)
		if !ok {
			t.Fatalf("%s: unexpected message type", op)
		}
		if result.detached {
			t.Errorf("%s timing out was excused as still running; a quick operation "+
				"that times out has really failed", op)
		}
		if result.err == nil {
			t.Errorf("%s timing out was reported as success", op)
		}
	}

	// A real error is still an error, however long the operation usually takes.
	result, _ := classifyOpResult("x", "ollama", "pull", errors.New("no such model")).(engineOpMsg)
	if result.detached || result.err == nil {
		t.Errorf("a genuine pull error was not reported: %+v", result)
	}
}

// TestDeleteSendsBothKeys checks delete works on either engine, since Ollama
// keys it as "name" and LM Studio as "model".
func TestDeleteSendsBothKeys(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio"} {
		envelope := modelActionWire(engine, "delete", "victim")
		action, params := actionOf(t, envelope)
		if action != "delete_model" {
			t.Errorf("%s: action = %q", engine, action)
		}
		if params["name"] != "victim" || params["model"] != "victim" {
			t.Errorf("%s: delete params = %v, want both keys", engine, params)
		}
	}
}

// TestLongRunningOpsAreRealOperations keeps the leniency set honest: every
// entry must be an operation the interface can actually issue, or the set
// documents behaviour nothing exercises.
func TestLongRunningOpsAreRealOperations(t *testing.T) {
	for op := range longRunningOps {
		_, isLifecycle := engineOps[op]
		_, isModel := modelActions[op]
		if !isLifecycle && !isModel {
			t.Errorf("longRunningOps names %q, which is neither a lifecycle nor a model operation", op)
		}
	}
}

// TestModelActionsCarryRemoteEquivalents checks every model operation has a
// remote method, since all four are offered on a peer's node.
func TestModelActionsCarryRemoteEquivalents(t *testing.T) {
	for name, act := range modelActions {
		if act.op == "" {
			t.Errorf("%s: no operation name", name)
		}
		if act.remote == "" {
			t.Errorf("%s: no remote method, but model operations are offered on remote nodes", name)
		}
		if act.what == "" {
			t.Errorf("%s: no operator-facing verb", name)
		}
	}
}

// TestLifecycleRemoteCoverageMatchesManager pins which lifecycle operations have
// a remote counterpart. The engine manager has remote install, start, and stop
// but no remote restart, uninstall, or port change — those need process
// ownership on the target host — so those three must be marked local-only or the
// UI would offer an operation that always fails.
func TestLifecycleRemoteCoverageMatchesManager(t *testing.T) {
	for name, op := range engineOps {
		_, hasRemote := remoteEngineMethods[op.method]
		if op.localOnly && hasRemote {
			t.Errorf("%s is marked local-only but a remote method exists", name)
		}
		if !op.localOnly && !hasRemote {
			t.Errorf("%s is offered on remote nodes but has no remote method", name)
		}
	}

	for _, name := range []string{"restart", "uninstall"} {
		if !engineOps[name].localOnly {
			t.Errorf("%s must be local-only: the manager exposes no remote variant", name)
		}
	}
	for _, name := range []string{"install", "start", "stop"} {
		if engineOps[name].localOnly {
			t.Errorf("%s has a remote variant and should not be local-only", name)
		}
	}
}
