// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
)

// wireOf builds a model operation's envelope, failing the test if the engine
// or operation is not known.
func wireOf(t *testing.T, engine, op, model string) map[string]any {
	t.Helper()
	envelope, err := modelActionWire(engine, op, model)
	require.NoError(t, err, "%s %s", engine, op)
	return envelope
}

// actionOf unwraps an engine:action envelope for assertions.
func actionOf(t *testing.T, envelope map[string]any) (string, map[string]any) {
	t.Helper()
	action, _ := envelope["action"].(string)
	params, ok := envelope["params"].(map[string]any)
	require.True(t, ok, "params must be map[string]any")
	return action, params
}

// TestPullSendsBothKeys guards the LM Studio fix: a pull must carry the model
// under BOTH "name" (Ollama's /api/pull body key) and "model" (LM Studio's
// `lms get {model}` CLI placeholder). Sending only "name" silently ran
// `lms get "" --yes`, so the download never reached LM Studio.
func TestPullSendsBothKeys(t *testing.T) {
	test := func(name, engine string) {
		t.Run(name, func(t *testing.T) {
			envelope := wireOf(t, engine, "pull", "owner/model")
			require.Equal(t, engine, envelope["engine"])
			action, params := actionOf(t, envelope)
			require.Equal(t, "pull_model", action)
			assert.Equal(t, "owner/model", params["name"], "pull must set name")
			assert.Equal(t, "owner/model", params["model"], "pull must set model")
		})
	}
	test("Ollama", engines.NameOllama)
	test("LM Studio", engines.NameLMStudio)
}

// TestOllamaLoadUsesRunModel guards the contract the two engines do NOT share.
// Ollama has no load action — warming a model is run_model with streaming off —
// so sending load_model errors, and the failure is quiet enough to look like the
// model simply not loading.
func TestOllamaLoadUsesRunModel(t *testing.T) {
	envelope := wireOf(t, engines.NameOllama, "load", "llama3.2")
	action, params := actionOf(t, envelope)

	assert.Equal(t, "run_model", action)
	assert.Equal(t, "llama3.2", params["model"])
	assert.Equal(t, false, params["stream"], "a streaming load never completes here")

	// LM Studio does declare a real load action.
	lmEnvelope := wireOf(t, engines.NameLMStudio, "load", "owner/model")
	lmAction, _ := actionOf(t, lmEnvelope)
	assert.Equal(t, "load_model", lmAction)
}

// TestOllamaUnloadSendsKeepAlive guards the other asymmetry: Ollama only frees a
// model when keep_alive is 0. Without it the request succeeds and the model
// stays resident, so eject appears to do nothing.
func TestOllamaUnloadSendsKeepAlive(t *testing.T) {
	envelope := wireOf(t, engines.NameOllama, "unload", "llama3.2")
	action, params := actionOf(t, envelope)

	assert.Equal(t, "unload_model", action)
	assert.Equal(t, 0, params["keep_alive"], "without keep_alive the model is not evicted")

	// LM Studio's unload takes no keep_alive.
	lmEnvelope := wireOf(t, engines.NameLMStudio, "unload", "owner/model")
	_, lmParams := actionOf(t, lmEnvelope)
	assert.Nil(t, lmParams["keep_alive"], "LM Studio's unload takes no keep_alive")
}

// TestPullDeadlineReportsDetachedNotSilence guards the acknowledgement for a
// long download. A multi-gigabyte pull outlasts the reply deadline while the
// engine keeps working, so a failure would be wrong — but the previous silence
// was too, leaving the operator unable to tell a started download from a
// keystroke that missed.
func TestPullDeadlineReportsDetachedNotSilence(t *testing.T) {
	msg := classifyOpResult("download big-model", "ollama", "pull", context.DeadlineExceeded)
	require.NotNil(t, msg, "a pull that outran its deadline produced no message at all")
	op, ok := msg.(engineOpMsg)
	require.True(t, ok, "result must be engineOpMsg")
	assert.NoError(t, op.err, "a still-running download must not be reported as failed")
	assert.True(t, op.detached, "a still-running download must not be reported as complete")
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
	// Start and restart wait on the engine's readiness probe, which the
	// manifests allow up to ten minutes, so a deadline on either is a slow
	// reply rather than a failed start.
	longRunning := func(name, op string) {
		t.Run(name, func(t *testing.T) {
			result, ok := classifyOpResult("x", "ollama", op, context.DeadlineExceeded).(engineOpMsg)
			require.True(t, ok, "result must be engineOpMsg")
			assert.True(t, result.detached, "a timed-out long operation must be reported as still running")
			assert.NoError(t, result.err, "operation is still running")
		})
	}
	longRunning("pull remains running", "pull")
	longRunning("load remains running", "load")
	longRunning("install remains running", "install")
	longRunning("uninstall remains running", "uninstall")
	longRunning("start remains running", "start")
	longRunning("restart remains running", "restart")

	quick := func(name, op string) {
		t.Run(name, func(t *testing.T) {
			result, ok := classifyOpResult("x", "ollama", op, context.DeadlineExceeded).(engineOpMsg)
			require.True(t, ok, "result must be engineOpMsg")
			assert.False(t, result.detached, "a quick operation that times out has really failed")
			assert.Error(t, result.err, "a timeout must not be reported as success")
		})
	}
	quick("unload times out", "unload")
	quick("delete times out", "delete")
	quick("stop times out", "stop")

	// A real error is still an error, however long the operation usually takes.
	result, _ := classifyOpResult("x", "ollama", "pull", errors.New("no such model")).(engineOpMsg)
	assert.False(t, result.detached, "a genuine pull error must not be detached")
	assert.Error(t, result.err, "a genuine pull error must be reported")
}

// TestDeleteSendsBothKeys checks delete works on either engine, since Ollama
// keys it as "name" and LM Studio as "model".
func TestDeleteSendsBothKeys(t *testing.T) {
	test := func(name, engine string) {
		t.Run(name, func(t *testing.T) {
			envelope := wireOf(t, engine, "delete", "victim")
			action, params := actionOf(t, envelope)
			assert.Equal(t, "delete_model", action)
			assert.Equal(t, "victim", params["name"], "delete must set name")
			assert.Equal(t, "victim", params["model"], "delete must set model")
		})
	}
	test("Ollama", engines.NameOllama)
	test("LM Studio", engines.NameLMStudio)
}

// TestLlamaCPPSendsTheModelAlone checks every llama.cpp model operation carries
// the model under "model" and nothing else, as the engine manager's own wire
// for it does. Its delete sends the params as a query string, so an extra
// "name" would reach the engine as a parameter it does not take.
func TestLlamaCPPSendsTheModelAlone(t *testing.T) {
	test := func(name, op, action string) {
		t.Run(name, func(t *testing.T) {
			envelope := wireOf(t, engines.NameLlamaCPP, op, "ggml-org/gemma-3-1b-it-GGUF:Q4_K_M")
			got, params := actionOf(t, envelope)
			assert.Equal(t, action, got)
			assert.Equal(t, map[string]any{"model": "ggml-org/gemma-3-1b-it-GGUF:Q4_K_M"}, params, "params must contain only the model")
		})
	}
	test("load", "load", "load_model")
	test("unload", "unload", "unload_model")
	test("delete", "delete", "delete_model")
	test("pull", "pull", "pull_model")
}

// TestDownloadPromptUsesTheEnginesSpelling is the regression guard for an
// Ollama example offered for every engine. "llama3.2" is not a name LM Studio
// or llama.cpp can download, and llama.cpp's needs a quantization after a colon.
func TestDownloadPromptUsesTheEnginesSpelling(t *testing.T) {
	for _, e := range engines.All() {
		assert.Contains(t, downloadExamples, e.Name, "every engine needs a download example")
	}
	assert.Contains(t, downloadPrompt(engines.NameLlamaCPP, "llama.cpp"), ":Q4_K_M", "llama.cpp prompt must show the quantization suffix")
	assert.Equal(t, "model name for vLLM", downloadPrompt("vllm", "vLLM"))
}

// TestEveryEngineHasAModelWire is what makes adding an engine a matter of
// adding its entry: an engine in the shared table without a spelling for every
// model operation fails here, rather than being sent another engine's action
// names at runtime.
func TestEveryEngineHasAModelWire(t *testing.T) {
	for _, e := range engines.All() {
		for op := range modelActions {
			_, err := modelActionWire(e.Name, op, "m")
			assert.NoError(t, err, "%s must have a local wire for %s", e.Name, op)
		}
	}
	_, err := modelActionWire("vllm", "load", "m")
	assert.Error(t, err, "an engine with no wire must not be given one")
}

// TestLongRunningOpsAreRealOperations keeps the leniency set honest: every
// entry must be an operation the interface can actually issue, or the set
// documents behaviour nothing exercises.
func TestLongRunningOpsAreRealOperations(t *testing.T) {
	for op := range longRunningOps {
		_, isLifecycle := engineOps[op]
		_, isModel := modelActions[op]
		assert.True(t, isLifecycle || isModel, "%q must be a lifecycle or model operation", op)
	}
}

// TestModelActionsCarryRemoteEquivalents checks every model operation has a
// remote method, since all four are offered on a peer's node.
func TestModelActionsCarryRemoteEquivalents(t *testing.T) {
	for name, act := range modelActions {
		assert.NotEmpty(t, act.op, "%s: no operation name", name)
		assert.NotEmpty(t, act.remote, "%s: model operations are offered on remote nodes", name)
		assert.NotEmpty(t, act.what, "%s: no operator-facing verb", name)
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
		assert.Equal(t, !op.localOnly, hasRemote, "%s: remote coverage must match the manager", name)
	}

	for _, name := range []string{"restart", "uninstall"} {
		assert.True(t, engineOps[name].localOnly, "%s must be local-only: the manager exposes no remote variant", name)
	}
	for _, name := range []string{"install", "start", "stop"} {
		assert.False(t, engineOps[name].localOnly, "%s has a remote variant and should not be local-only", name)
	}
}
