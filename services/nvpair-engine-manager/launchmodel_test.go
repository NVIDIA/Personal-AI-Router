// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
)

func TestWithLaunchFlag_SetsTheFlagOnceAndKeepsTheRestOfTheText(t *testing.T) {
	test := func(name, text, want string, wantChanged bool) {
		t.Run(name, func(t *testing.T) {
			got, changed, err := withLaunchFlag(text, "--model", "org/new")

			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, wantChanged, changed, "changed")
		})
	}
	test("added when absent", "--max-model-len 4096", "--max-model-len 4096 --model org/new", true)
	test("added to empty text", "", "--model org/new", true)
	test("replaces a separate value", "--model org/old --max-model-len 4096", "--model org/new --max-model-len 4096", true)
	test("replaces an equals form", "--max-model-len 4096 --model=org/old --dtype auto", "--max-model-len 4096 --model org/new --dtype auto", true)
	test("keeps leading environment assignments", "VLLM_LOGGING_LEVEL=DEBUG --model org/old", "VLLM_LOGGING_LEVEL=DEBUG --model org/new", true)
	test("collapses a repeated flag", "--model a --x 1 --model=b", "--model org/new --x 1", true)
	test("goes before a terminator", "--x 1 -- positional", "--x 1 --model org/new -- positional", true)
	test("keeps quoted values", `--served-model-name "two words" --model org/old`, `--served-model-name "two words" --model org/new`, true)
	test("leaves a prefix-sharing flag alone", "--model-impl vllm --model org/old", "--model-impl vllm --model org/new", true)
	test("unchanged when already set", "--x 1 --model org/new", "--x 1 --model org/new", false)
	test("unchanged despite different quoting", `OLLAMA_HOST="127.0.0.1:1" --model org/new`, "OLLAMA_HOST=127.0.0.1:1 --model org/new", false)
}

func TestWithLaunchFlag_RejectsLaunchTextThatDoesNotParse(t *testing.T) {
	_, _, err := withLaunchFlag(`--model "unterminated`, "--model", "x")

	assert.Error(t, err)
}

// switchModelExecutor is a running fake engine with editable launch settings
// and a launch_model action. The fake ignores arguments it does not know, so
// the "model" flag is carried in the saved launch text without changing how it
// serves.
func switchModelExecutor(t *testing.T, flag string) *Executor {
	t.Helper()
	e := settingsExecutor(t, false)
	m, ok := e.reg.Get("fake")
	require.True(t, ok, "fake manifest missing")
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flag: flag}}
	// Like vLLM, the engine answers which model it serves, under the name
	// requests use (id) and the model it loaded (root).
	m.AdvertisedModels = advertisedModelsLoaded
	m.Actions["loaded_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/v1/models"},
		Result: &ActionResult{Array: "data", Field: "id", InventoryField: "root"},
	}
	require.NoError(t, m.Validate())
	// An engine that chooses its model by launch argument needs one to start.
	state, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	text, _, err := withLaunchFlag(state.LaunchText, flag, "org/initial")
	require.NoError(t, err)
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{
		Engine:   "fake",
		Settings: settings.Config{ServerPort: state.ServerPort, LaunchText: text},
	}, func() error { return nil })
	require.NoError(t, err)
	require.NoError(t, e.Start(t.Context(), "fake"), "start")
	return e
}

func loadModel(t *testing.T, e *Executor, model string) (json.RawMessage, error) {
	t.Helper()
	return e.Action(t.Context(), "fake", "load_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
}

func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func launchTextOf(t *testing.T, e *Executor) string {
	t.Helper()
	return launchTextOfEngine(t, e, "fake")
}

func launchTextOfEngine(t *testing.T, e *Executor, engine string) string {
	t.Helper()
	state, err := e.LaunchSettings(engine)
	require.NoError(t, err)
	return state.LaunchText
}

func TestWithoutLaunchFlag_RemovesEveryFormOfTheFlag(t *testing.T) {
	test := func(name, text, want string, wantChanged bool) {
		t.Run(name, func(t *testing.T) {
			got, changed, err := withoutLaunchFlag(text, "--model")

			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, wantChanged, changed, "changed")
		})
	}
	test("separate value", "--model org/a --port 8001", "--port 8001", true)
	test("equals form", "--model=org/a --port 8001", "--port 8001", true)
	test("repeated", "--model org/a --port 8001 --model=org/b", "--port 8001", true)
	test("after the terminator is kept", "--port 8001 -- --model org/a", "--port 8001 -- --model org/a", false)
	test("absent", "--port 8001", "--port 8001", false)
}

// loadReply is the load_model result.
type loadReply struct {
	Model     string `json:"model"`
	Restarted bool   `json:"restarted"`
	Stopped   bool   `json:"stopped,omitempty"`
}

func decodeLoadReply(t *testing.T, raw json.RawMessage) loadReply {
	t.Helper()
	var reply loadReply
	require.NoError(t, json.Unmarshal(raw, &reply))
	return reply
}

func TestLaunchModel_SwitchesAndRestartsARunningEngine(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	gen := startGeneration(t, e)

	raw, err := loadModel(t, e, "org/first")

	require.NoError(t, err, "load first")
	assert.Equal(t, loadReply{Model: "org/first", Restarted: true}, decodeLoadReply(t, raw))
	assert.Contains(t, launchTextOf(t, e), "--model org/first", "the launch text carries the model")
	after := mustStatus(t, e)
	assert.True(t, after.Running, "the engine runs the new model")
	assert.NotEqual(t, gen, startGeneration(t, e), "the engine was restarted")

	// A second switch replaces the first and keeps the rest of the launch text.
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{
		ServerPort: after.Port, LaunchText: launchTextOf(t, e) + " --max-model-len 4096",
	}}, func() error { return nil })
	require.NoError(t, err)
	_, err = loadModel(t, e, "org/second")
	require.NoError(t, err, "load second")
	text := launchTextOf(t, e)
	assert.Contains(t, text, "--model org/second")
	assert.NotContains(t, text, "org/first")
	assert.Contains(t, text, "--max-model-len 4096")

	// Asking for the model already being served does not restart anything.
	gen = startGeneration(t, e)
	raw, err = loadModel(t, e, "org/second")
	require.NoError(t, err)
	assert.False(t, decodeLoadReply(t, raw).Restarted, "a no-op switch reports no restart")
	assert.Equal(t, gen, startGeneration(t, e), "a no-op switch restarted the engine")
}

// startGeneration counts how many times the engine has been started.
func startGeneration(t *testing.T, e *Executor) int64 {
	t.Helper()
	st := settingsState(t, e)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.gen
}

func mustStatus(t *testing.T, e *Executor) EngineStatus {
	t.Helper()
	st, err := e.Status("fake")
	require.NoError(t, err)
	return st
}

// Load on a stopped engine (after Eject, say) serves the model: it saves the
// choice and starts the engine, whether or not the model changed.
func TestLaunchModel_StartsAStoppedEngine(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	require.NoError(t, e.Stop("fake"))

	raw, err := loadModel(t, e, "org/later")

	require.NoError(t, err, "load on a stopped engine")
	assert.True(t, decodeLoadReply(t, raw).Restarted)
	assert.True(t, mustStatus(t, e).Running, "loading a model into a stopped engine must start it")
	assert.Contains(t, launchTextOf(t, e), "--model org/later")

	// The same model again, on an engine stopped once more, still starts it.
	require.NoError(t, e.Stop("fake"))
	_, err = loadModel(t, e, "org/later")
	require.NoError(t, err)
	assert.True(t, mustStatus(t, e).Running, "an unchanged model on a stopped engine must start it")
}

func TestLaunchModel_RestoresTheChoiceWhenAStoppedEngineWillNotStart(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	before := launchTextOf(t, e)
	require.NoError(t, e.Stop("fake"))

	_, err := loadModel(t, e, "org/fails-to-load")

	assert.ErrorContains(t, err, "previous model was restored", "the failed start is reported with the restore")
	assert.False(t, mustStatus(t, e).Running, "the engine must stay stopped when the model does not start")
	assert.Equal(t, before, launchTextOf(t, e))
}

func ejectEngine(t *testing.T, e *Executor, model string) (json.RawMessage, error) {
	t.Helper()
	m, _ := e.reg.Get("fake")
	m.Actions["unload_model"] = Action{StopEngine: true}
	require.NoError(t, m.Validate())
	return e.Action(t.Context(), "fake", "unload_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
}

func TestEject_StopsTheEngineAndKeepsItsModelChoice(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	_, err := loadModel(t, e, "org/served")
	require.NoError(t, err)
	type ejectReply struct {
		Stopped bool `json:"stopped"`
	}
	eject := func(model string) ejectReply {
		t.Helper()
		raw, err := ejectEngine(t, e, model)
		require.NoError(t, err)
		var reply ejectReply
		require.NoError(t, json.Unmarshal(raw, &reply))
		return reply
	}

	_, err = ejectEngine(t, e, "org/other")
	assert.ErrorContains(t, err, "not the model", "ejecting a model that is not served must be refused")
	require.True(t, mustStatus(t, e).Running, "a refused eject stopped the engine")

	assert.Equal(t, ejectReply{Stopped: true}, eject("org/served"))
	assert.False(t, mustStatus(t, e).Running, "eject must stop the engine")
	assert.Contains(t, launchTextOf(t, e), "--model org/served", "eject must keep the model choice so Load can serve it again")

	assert.Equal(t, ejectReply{Stopped: false}, eject("org/served"), "ejecting from a stopped engine is a no-op")
}

func TestLaunchModel_RefusesAnythingThatIsNotAModelID(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	before := launchTextOf(t, e)
	reject := func(name, params string) {
		t.Run(name, func(t *testing.T) {
			_, err := e.Action(t.Context(), "fake", "load_model", json.RawMessage(params))

			assert.Error(t, err)
		})
	}
	rejectModel := func(name, model string) {
		reject(name, `{"model":`+quoteJSON(model)+`}`)
	}
	accept := func(name, model string) {
		t.Run(name, func(t *testing.T) {
			_, err := loadModel(t, e, model)

			assert.NoError(t, err)
		})
	}
	rejectModel("empty", "")
	rejectModel("blank", " ")
	rejectModel("an option", "--help")
	rejectModel("a short option", "-x")
	rejectModel("an id followed by an option", "org/name --extra")
	rejectModel("a space", "a b")
	rejectModel("a relative path", "../etc/passwd")
	rejectModel("an absolute path", "/abs/path")
	rejectModel("an empty path segment", "org//name")
	rejectModel("three path segments", "a/b/c")
	rejectModel("a shell separator", "x;y")
	rejectModel("a command substitution", "$(id)")
	rejectModel("a newline", "org/na\nme")
	reject("params without a model", `{"nope":1}`)
	t.Run("missing params", func(t *testing.T) {
		_, err := e.Action(t.Context(), "fake", "load_model", nil)

		assert.Error(t, err)
	})
	require.Equal(t, before, launchTextOf(t, e), "a refused switch changed the launch text")
	require.True(t, mustStatus(t, e).Running, "a refused switch stopped the engine")

	accept("a name without an organization", "gpt2")
	accept("an organization and a name", "Qwen/Qwen2.5-0.5B-Instruct")
	accept("dots, underscores and dashes", "org/name.v1_2-x")
}

// If the new model does not come up, the engine goes back to the one it was
// serving instead of staying down. The fake engine exits at once when it is
// given the model org/fails-to-load, which stands in for a model that does not fit.
func TestLaunchModel_RestoresThePreviousModelWhenTheNewOneFails(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	before := launchTextOf(t, e)

	_, err := loadModel(t, e, "org/fails-to-load")

	require.Error(t, err, "a model that failed to start was reported as loaded")
	// The note comes first: the engine's own error that follows it is a long log tail.
	assert.True(t, strings.HasPrefix(err.Error(), "switching to org/fails-to-load failed; the previous model was restored: "),
		"error does not lead with the restore note: %v", err)
	assert.Equal(t, before, launchTextOf(t, e))
	assert.True(t, mustStatus(t, e).Running, "the engine was left down after a failed switch")
}

// A remote caller that stops waiting cancels the switch while the new model is
// still starting. The previous model must still come back: restoring it under
// the cancelled context would fail at once and leave the engine down.
func TestLaunchModel_RestoresThePreviousModelWhenTheCallerGivesUp(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	// Every launch from here on takes a second to become ready, so the switch
	// below is still starting when its caller gives up.
	_, err := e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{
		ServerPort: mustStatus(t, e).Port, LaunchText: "FAKE_START_DELAY=1s " + launchTextOf(t, e),
	}}, func() error { return nil })
	require.NoError(t, err)
	before := launchTextOf(t, e)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	_, err = e.Action(ctx, "fake", "load_model", json.RawMessage(`{"model":"org/slow"}`))

	require.Error(t, err, "a switch whose caller gave up was reported as done")
	assert.ErrorContains(t, err, "the previous model was restored")
	assert.Equal(t, before, launchTextOf(t, e))
	assert.True(t, mustStatus(t, e).Running, "the engine was left down after the caller gave up")
}

func TestLaunchModelAction_AcceptsOnlyALongOptionFlag(t *testing.T) {
	accept := func(name, flag string) {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, (&Action{LaunchModel: &ActionLaunchModel{Flag: flag}}).validate("load_model"))
		})
	}
	reject := func(name, flag string) {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, (&Action{LaunchModel: &ActionLaunchModel{Flag: flag}}).validate("load_model"))
		})
	}
	accept("a long option", "--model")
	reject("no dashes", "model")
	reject("a space in the flag", "--mod el")
	reject("a value in the flag", "--model=x")
	reject("empty", "")
}

// The restart needs editable launch settings and a readiness probe on every platform.
func TestLaunchModelAction_RequiresEditableLaunch(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flag: "--model"}}

	assert.ErrorContains(t, m.Validate(), "editable_launch")
}

// slowStarts stops the engine and makes every later launch take 8 seconds to
// become ready (under the fake manifest's 10-second readiness limit), so a
// start is still in flight when the test acts on it.
func slowStarts(t *testing.T, e *Executor) {
	t.Helper()
	require.NoError(t, e.Stop("fake"))
	_, err := e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{
		ServerPort: mustStatus(t, e).Port, LaunchText: "FAKE_START_DELAY=8s " + launchTextOf(t, e),
	}}, func() error { return nil })
	require.NoError(t, err)
}

// waitForStartInFlight returns once the engine's start has begun.
func waitForStartInFlight(t *testing.T, e *Executor) {
	t.Helper()
	st, err := e.state("fake")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.startCancel != nil
	}, 10*time.Second, 10*time.Millisecond, "the start never began")
}

// Stopping an engine that is still loading cancels the start instead of
// waiting out its readiness window, and the user's stop is not reported as a
// failed start.
func TestStop_CancelsAStartThatIsStillLoading(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	slowStarts(t, e)
	started := make(chan error, 1)
	go func() { started <- e.Start(t.Context(), "fake") }()
	waitForStartInFlight(t, e)

	began := time.Now()
	require.NoError(t, e.Stop("fake"))

	assert.Less(t, time.Since(began), 4*time.Second, "Stop waited out the start")
	require.ErrorIs(t, <-started, errStartStopped, "the start says the user stopped it")
	assert.False(t, mustStatus(t, e).Running)
	for _, reported := range e.reporter.snapshot() {
		assert.NotEqual(t, startFailedID("fake"), reported.ID, "a start the user stopped was reported as failed")
	}
}

// engine:start that a Stop cut short replies with the engine's status, stopped,
// not an error: the user asked for it, and the error would carry the cancelled
// engine's partial output. A caller that waits for the reply, such as the
// desktop's first-open start, would otherwise report a failed start.
func TestRunOp_AnswersAStartTheUserStoppedWithTheStoppedStatus(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	slowStarts(t, e)

	status := stopDuringOp(t, e, "engine:start", `{"engine":"fake"}`)

	assert.False(t, status.Running)
}

// The broker no longer holds a Stop back while a Restart is starting the engine
// again, so a Restart is cut short the same way and answers the same way: the
// desktop and the TUI wait for its reply.
func TestRunOp_AnswersARestartTheUserStoppedWithTheStoppedStatus(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	slowStarts(t, e)
	require.NoError(t, e.Start(t.Context(), "fake"))

	status := stopDuringOp(t, e, "engine:restart", `{"engine":"fake"}`)

	assert.False(t, status.Running)
	enabled, known, err := e.desired.get("fake")
	require.NoError(t, err)
	assert.True(t, known)
	assert.False(t, enabled, "the user turned the engine off")
}

// stopDuringOp sends a lifecycle method through the manager, stops the engine once the
// start it begins is in flight, and returns the reply, which must be the
// engine's status, not an error: the user asked for the stop, and the error
// would carry the cancelled engine's partial output.
func stopDuringOp(t *testing.T, e *Executor, method, params string) EngineStatus {
	t.Helper()
	var out bytes.Buffer // read only after the reply, so no lock is needed
	m := NewManager(NewCodec(&out), e, nil)
	id := json.RawMessage("7")
	replied := make(chan struct{})
	go func() {
		m.runOp(t.Context(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
		close(replied)
	}()
	waitForStartInFlight(t, e)

	began := time.Now()
	require.NoError(t, e.Stop("fake"))
	<-replied

	assert.Less(t, time.Since(began), 4*time.Second, "the reply waited out the start")
	var reply struct {
		Result *EngineStatus    `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &reply), out.String())
	require.Nil(t, reply.Error, "a start the user stopped is not an error: %s", out.String())
	require.NotNil(t, reply.Result)
	return *reply.Result
}

// loadOutcome is a load_model call's result and error.
type loadOutcome struct {
	result json.RawMessage
	err    error
}

// assertLoadStopped checks the reply to a Load that a Stop or Eject ended: the
// user asked for it, so it is not an error, and it says the Load was stopped.
func assertLoadStopped(t *testing.T, outcome loadOutcome) {
	t.Helper()
	require.NoError(t, outcome.err, "a Load the user stopped is not a failure")
	assert.Equal(t, loadReply{Model: "org/slow", Stopped: true}, decodeLoadReply(t, outcome.result))
}

// Eject during a Load that is starting the engine stops it at once, puts the
// previous model back without starting it, and leaves the engine off.
func TestEject_DuringALoadStopsTheEngineAndKeepsThePreviousModel(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	slowStarts(t, e)
	before := launchTextOf(t, e)
	loaded := make(chan loadOutcome, 1)
	go func() {
		result, err := loadModel(t, e, "org/slow")
		loaded <- loadOutcome{result, err}
	}()
	waitForStartInFlight(t, e)

	began := time.Now()
	_, err := ejectEngine(t, e, "")
	require.NoError(t, err)

	assert.Less(t, time.Since(began), 4*time.Second, "Eject waited out the Load")
	assertLoadStopped(t, <-loaded)
	assert.Equal(t, before, launchTextOf(t, e))
	assert.False(t, mustStatus(t, e).Running, "a stopped Load must not start the previous model again")
	enabled, known, err := e.desired.get("fake")
	require.NoError(t, err)
	assert.True(t, known)
	assert.False(t, enabled, "the user turned the engine off")
}

// The same while a Load is restarting a running engine: the restore that would
// normally bring the previous model back up must leave the engine stopped.
func TestEject_DuringALoadOfARunningEngineLeavesItStopped(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	slowStarts(t, e)
	require.NoError(t, e.Start(t.Context(), "fake"))
	before := launchTextOf(t, e)
	loaded := make(chan loadOutcome, 1)
	go func() {
		result, err := loadModel(t, e, "org/slow")
		loaded <- loadOutcome{result, err}
	}()
	waitForStartInFlight(t, e)

	began := time.Now()
	_, err := ejectEngine(t, e, "")
	require.NoError(t, err)

	// Restarting the previous model first, only to stop it again, would hold
	// the Eject for another 8-second start.
	assert.Less(t, time.Since(began), 4*time.Second, "Eject waited for the previous model to start again")
	assertLoadStopped(t, <-loaded)
	assert.Equal(t, before, launchTextOf(t, e))
	assert.False(t, mustStatus(t, e).Running, "a stopped Load must not start the previous model again")
}

// A start reports that it is starting for as long as it runs. A vLLM start
// can download and load for minutes, and a client that only knew "not
// running" showed the engine as off for all of that time.
func TestStart_ReportsStartingUntilTheEngineIsReady(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	slowStarts(t, e)
	assert.False(t, mustStatus(t, e).Starting, "a stopped engine is not starting")

	started := make(chan error, 1)
	go func() { started <- e.Start(t.Context(), "fake") }()
	waitForStartInFlight(t, e)
	require.Eventually(t, func() bool { return mustStatus(t, e).Starting }, 5*time.Second, 20*time.Millisecond,
		"the engine is reported starting while it loads")
	assert.False(t, mustStatus(t, e).Running)

	require.NoError(t, <-started)
	status := mustStatus(t, e)
	assert.True(t, status.Running)
	assert.False(t, status.Starting, "a running engine is no longer starting")
}

// A start that is stopped, or fails, is no longer reported as starting.
func TestStart_StopsReportingStartingWhenTheStartEnds(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	slowStarts(t, e)
	started := make(chan error, 1)
	go func() { started <- e.Start(t.Context(), "fake") }()
	waitForStartInFlight(t, e)
	require.Eventually(t, func() bool { return mustStatus(t, e).Starting }, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, e.Stop("fake"))
	require.Error(t, <-started)
	status := mustStatus(t, e)
	assert.False(t, status.Starting)
	assert.False(t, status.Running)
}
