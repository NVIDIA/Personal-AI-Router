// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
)

func TestWithLaunchFlag(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		same             bool // the arguments already said this
	}{
		{"added when absent", "--max-model-len 4096", "--max-model-len 4096 --model org/new", false},
		{"added to empty text", "", "--model org/new", false},
		{"replaces a separate value", "--model org/old --max-model-len 4096", "--model org/new --max-model-len 4096", false},
		{"replaces an equals form", "--max-model-len 4096 --model=org/old --dtype auto", "--max-model-len 4096 --model org/new --dtype auto", false},
		{"keeps leading environment assignments", "VLLM_LOGGING_LEVEL=DEBUG --model org/old", "VLLM_LOGGING_LEVEL=DEBUG --model org/new", false},
		{"collapses a repeated flag", "--model a --x 1 --model=b", "--model org/new --x 1", false},
		{"goes before a terminator", "--x 1 -- positional", "--x 1 --model org/new -- positional", false},
		{"keeps quoted values", `--served-model-name "two words" --model org/old`, `--served-model-name "two words" --model org/new`, false},
		{"leaves a prefix-sharing flag alone", "--model-impl vllm --model org/old", "--model-impl vllm --model org/new", false},
		{"unchanged when already set", "--x 1 --model org/new", "--x 1 --model org/new", true},
		{"unchanged despite different quoting", `OLLAMA_HOST="127.0.0.1:1" --model org/new`, "OLLAMA_HOST=127.0.0.1:1 --model org/new", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := withLaunchFlag(tc.text, "--model", "org/new")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || changed != !tc.same {
				t.Fatalf("withLaunchFlag(%q) = %q (changed %v), want %q (changed %v)", tc.text, got, changed, tc.want, !tc.same)
			}
		})
	}
	if _, _, err := withLaunchFlag(`--model "unterminated`, "--model", "x"); err == nil {
		t.Error("launch text that does not parse was accepted")
	}
}

// switchModelExecutor is a running fake engine with editable launch settings
// and a launch_model action. The fake ignores arguments it does not know, so
// the "model" flag is carried in the saved launch text without changing how it
// serves.
func switchModelExecutor(t *testing.T, flag string) *Executor {
	t.Helper()
	e := settingsExecutor(t, false)
	m, ok := e.reg.Get("fake")
	if !ok {
		t.Fatal("fake manifest missing")
	}
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flag: flag}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	return e
}

func loadModel(t *testing.T, e *Executor, model string) (json.RawMessage, error) {
	t.Helper()
	return e.Action(context.Background(), "fake", "load_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
}

func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func launchTextOf(t *testing.T, e *Executor) string {
	t.Helper()
	state, err := e.LaunchSettings("fake")
	if err != nil {
		t.Fatal(err)
	}
	return state.LaunchText
}

func TestLaunchModelSwitchesAndRestartsARunningEngine(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	gen := startGeneration(t, e)

	raw, err := loadModel(t, e, "org/first")
	if err != nil {
		t.Fatalf("load first: %v", err)
	}
	var reply struct {
		Model     string `json:"model"`
		Restarted bool   `json:"restarted"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Model != "org/first" || !reply.Restarted {
		t.Fatalf("reply = %s, %v", raw, err)
	}
	if text := launchTextOf(t, e); !strings.Contains(text, "--model org/first") {
		t.Fatalf("launch text %q does not carry the model", text)
	}
	after := mustStatus(t, e)
	if !after.Running || startGeneration(t, e) == gen {
		t.Fatalf("engine was not restarted: generation %d, status %+v", gen, after)
	}

	// A second switch replaces the first and keeps the rest of the launch text.
	if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: settings.Config{
		ServerPort: after.Port, LaunchText: launchTextOf(t, e) + " --max-model-len 4096",
	}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModel(t, e, "org/second"); err != nil {
		t.Fatalf("load second: %v", err)
	}
	text := launchTextOf(t, e)
	if !strings.Contains(text, "--model org/second") || strings.Contains(text, "org/first") || !strings.Contains(text, "--max-model-len 4096") {
		t.Fatalf("launch text = %q", text)
	}

	// Asking for the model already being served does not restart anything.
	gen = startGeneration(t, e)
	raw, err = loadModel(t, e, "org/second")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Restarted || startGeneration(t, e) != gen {
		t.Fatalf("a no-op switch restarted the engine: %s", raw)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Load on a stopped engine (after Eject, say) serves the model: it saves the
// choice and starts the engine, whether or not the model changed.
func TestLaunchModelOnAStoppedEngineStartsIt(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	if err := e.Stop("fake"); err != nil {
		t.Fatal(err)
	}
	raw, err := loadModel(t, e, "org/later")
	if err != nil {
		t.Fatalf("load on a stopped engine: %v", err)
	}
	if !strings.Contains(string(raw), `"restarted":true`) {
		t.Fatalf("reply = %s", raw)
	}
	if !mustStatus(t, e).Running {
		t.Fatal("loading a model into a stopped engine must start it")
	}
	if text := launchTextOf(t, e); !strings.Contains(text, "--model org/later") {
		t.Fatalf("launch text = %q", text)
	}

	// The same model again, on an engine stopped once more, still starts it.
	if err := e.Stop("fake"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModel(t, e, "org/later"); err != nil || !mustStatus(t, e).Running {
		t.Fatalf("an unchanged model on a stopped engine must start it: %v", err)
	}
}

func TestLaunchModelOnAStoppedEngineRestoresTheChoiceWhenItWillNotStart(t *testing.T) {
	e := switchModelExecutor(t, "--fail-launch")
	before := launchTextOf(t, e)
	if err := e.Stop("fake"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModel(t, e, "org/broken"); err == nil || !strings.Contains(err.Error(), "previous model was restored") {
		t.Fatalf("err = %v, want the failed start reported with the restore", err)
	}
	if mustStatus(t, e).Running {
		t.Fatal("the engine must stay stopped when the model does not start")
	}
	if got := launchTextOf(t, e); got != before {
		t.Fatalf("launch text = %q, want the previous %q", got, before)
	}
}

func ejectEngine(t *testing.T, e *Executor, model string) (json.RawMessage, error) {
	t.Helper()
	m, _ := e.reg.Get("fake")
	m.Actions["unload_model"] = Action{StopEngine: true}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return e.Action(context.Background(), "fake", "unload_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
}

func TestEjectStopsTheEngineAndFreesItsMemory(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	if _, err := loadModel(t, e, "org/served"); err != nil {
		t.Fatal(err)
	}
	if _, err := ejectEngine(t, e, "org/other"); err == nil || !strings.Contains(err.Error(), "not the model") {
		t.Fatalf("ejecting a model that is not served must be refused: %v", err)
	}
	if !mustStatus(t, e).Running {
		t.Fatal("a refused eject stopped the engine")
	}

	raw, err := ejectEngine(t, e, "org/served")
	if err != nil || !strings.Contains(string(raw), `"stopped":true`) {
		t.Fatalf("eject: %s %v", raw, err)
	}
	if mustStatus(t, e).Running {
		t.Fatal("eject must stop the engine")
	}
	if text := launchTextOf(t, e); !strings.Contains(text, "--model org/served") {
		t.Fatalf("eject must keep the model choice so Load can serve it again: %q", text)
	}

	raw, err = ejectEngine(t, e, "org/served")
	if err != nil || !strings.Contains(string(raw), `"stopped":false`) {
		t.Fatalf("ejecting from a stopped engine is a no-op: %s %v", raw, err)
	}
}

func TestLaunchModelRefusesAnythingThatIsNotAModelID(t *testing.T) {
	e := switchModelExecutor(t, "--model")
	before := launchTextOf(t, e)
	for _, model := range []string{"", " ", "--help", "-x", "org/name --extra", "a b", "../etc/passwd", "/abs/path", "org//name", "a/b/c", "x;y", "$(id)", "org/na\nme"} {
		if _, err := loadModel(t, e, model); err == nil {
			t.Errorf("model %q was accepted", model)
		}
	}
	if _, err := e.Action(context.Background(), "fake", "load_model", json.RawMessage(`{"nope":1}`)); err == nil {
		t.Error("params without a model were accepted")
	}
	if _, err := e.Action(context.Background(), "fake", "load_model", nil); err == nil {
		t.Error("missing params were accepted")
	}
	if got := launchTextOf(t, e); got != before || !mustStatus(t, e).Running {
		t.Fatalf("a refused switch changed state: %q (was %q)", got, before)
	}
	for _, model := range []string{"gpt2", "Qwen/Qwen2.5-0.5B-Instruct", "org/name.v1_2-x"} {
		if _, err := loadModel(t, e, model); err != nil {
			t.Errorf("model %q was refused: %v", model, err)
		}
	}
}

// If the new model does not come up, the engine goes back to the one it was
// serving instead of staying down. The fake engine exits at once when it is
// given "--fail-launch", which here stands in for a model that fails to load.
func TestLaunchModelRestoresThePreviousModelWhenTheNewOneFails(t *testing.T) {
	e := switchModelExecutor(t, "--fail-launch")
	before := launchTextOf(t, e)

	_, err := loadModel(t, e, "org/too-big")
	if err == nil {
		t.Fatal("a model that failed to start was reported as loaded")
	}
	// The note comes first: the engine's own error that follows it is a long log tail.
	if !strings.HasPrefix(err.Error(), "switching to org/too-big failed; the previous model was restored: ") {
		t.Fatalf("error does not lead with the restore note: %v", err)
	}
	if got := launchTextOf(t, e); got != before {
		t.Fatalf("launch text = %q, want the previous %q", got, before)
	}
	if !mustStatus(t, e).Running {
		t.Fatal("the engine was left down after a failed switch")
	}
}

func TestLaunchModelValidation(t *testing.T) {
	for _, flag := range []string{"model", "--mod el", "--model=x", ""} {
		if err := (&Action{LaunchModel: &ActionLaunchModel{Flag: flag}}).validate("load_model"); err == nil {
			t.Errorf("launch_model flag %q was accepted", flag)
		}
	}
	if err := (&Action{LaunchModel: &ActionLaunchModel{Flag: "--model"}}).validate("load_model"); err != nil {
		t.Errorf("a valid launch_model was rejected: %v", err)
	}

	// The restart needs editable launch settings and a readiness probe on every platform.
	m := testEngineManifest(fakeEngineBin)
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flag: "--model"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "editable_launch") {
		t.Errorf("launch_model without editable_launch: %v", err)
	}
}
