// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
)

func TestEject_ReturnsEnabledEngineToModelSelection(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err := loadModel(t, e, "org/selected")
	require.NoError(t, err)

	_, err = ejectEngine(t, e, "org/selected")
	require.NoError(t, err)

	status := mustStatus(t, e)
	assert.True(t, status.AwaitingModel)
	assert.False(t, status.Running)
	assert.False(t, status.Healthy)
	tokens, err := parseLaunchText(launchTextOf(t, e))
	require.NoError(t, err)
	_, selected := launchFlagValue(tokens, "--model")
	assert.False(t, selected, "Eject must not select a model for the next start")
	enabled, known, err := e.desired.get("fake")
	require.NoError(t, err)
	assert.True(t, known)
	assert.True(t, enabled)
}

func TestEject_AllowsLoadingAnotherModelWithoutEnablingAgain(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err := loadModel(t, e, "org/first")
	require.NoError(t, err)
	_, err = ejectEngine(t, e, "org/first")
	require.NoError(t, err)

	_, err = loadModel(t, e, "org/second")
	require.NoError(t, err)

	assert.True(t, mustStatus(t, e).Running)
	assert.False(t, mustStatus(t, e).AwaitingModel)
}

func TestEject_CancelsFirstLoadAndReturnsToModelSelection(t *testing.T) {
	e := waitingModelExecutor(t)
	before, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{ServerPort: before.ServerPort, LaunchText: "FAKE_START_DELAY=8s " + before.LaunchText}}, func() error { return nil })
	require.NoError(t, err)
	require.NoError(t, e.Start(t.Context(), "fake"))
	result := make(chan error, 1)
	go func() {
		_, err := loadModel(t, e, "org/first")
		result <- err
	}()
	waitForStartInFlight(t, e)

	began := time.Now()
	_, err = ejectEngine(t, e, "org/first")
	require.NoError(t, err)
	require.NoError(t, <-result)
	assert.Less(t, time.Since(began), 4*time.Second)
	assert.True(t, mustStatus(t, e).AwaitingModel)
	assert.False(t, mustStatus(t, e).Running)
}

func TestEject_DoesNotEnableAnExplicitlyStoppedEngine(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	require.NoError(t, e.Stop("fake"))

	_, err := ejectEngine(t, e, "org/selected")
	require.NoError(t, err)

	assert.False(t, mustStatus(t, e).AwaitingModel)
	enabled, known, err := e.desired.get("fake")
	require.NoError(t, err)
	assert.True(t, known)
	assert.False(t, enabled)
}

func TestLaunchModel_StartsTheFirstSelectedModel(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err := loadModel(t, e, "org/selected")
	require.NoError(t, err)

	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.True(t, status.Running)
	assert.False(t, status.AwaitingModel)
	tokens, err := parseLaunchText(launchTextOf(t, e))
	require.NoError(t, err)
	model, found := launchFlagValue(tokens, "--model")
	assert.True(t, found)
	assert.Equal(t, "org/selected", model)
}

func TestLaunchModel_RestoresWaitingWhenTheFirstModelFails(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	before := launchTextOf(t, e)
	_, err := loadModel(t, e, "org/fails-to-load")
	require.ErrorContains(t, err, "previous model was restored")

	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.True(t, status.AwaitingModel)
	assert.False(t, status.Running)
	assert.Equal(t, before, launchTextOf(t, e))
}

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
// and a launch_model action that writes the model into every flag named here.
// The fake ignores arguments it does not know, so a model flag is carried in the
// saved launch text without changing how it serves.
func switchModelExecutor(t *testing.T, flags ...string) *Executor {
	t.Helper()
	e := settingsExecutor(t, false)
	m, ok := e.reg.Get("fake")
	require.True(t, ok, "fake manifest missing")
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flags: flags}}
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
	text, _, err := withLaunchFlags(state.LaunchText, flags, "org/initial")
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

// An engine's model can be named in more than one launch flag, and PAIR has to move
// or clear every one of them: a name left in a flag PAIR stopped writing is a second
// pack's name still being handed to the engine. So both helpers take the whole list
// a manifest declares rather than one flag.

func TestWithLaunchFlags_SetsTheModelInEveryDeclaredFlag(t *testing.T) {
	flags := []string{"--model", "--model-id"}
	test := func(name, text, want string, wantChanged bool) {
		t.Run(name, func(t *testing.T) {
			got, changed, err := withLaunchFlags(text, flags, "org/new")

			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, wantChanged, changed, "changed")
		})
	}
	test("adds both to a launch that names no model", "--port 8101", "--port 8101 --model org/new --model-id org/new", true)
	test("moves both, keeping their order", "--model org/old --model-id org/old --port 8101", "--model org/new --model-id org/new --port 8101", true)
	// --model-id is its own flag, not a spelling of --model, so a launch whose
	// two flags disagree is corrected rather than left half-replaced.
	test("corrects a flag that disagrees with the other", "--model org/new --model-id org/old", "--model org/new --model-id org/new", true)
	test("moves a pair written in the other order", "--model-id org/old --model org/old", "--model-id org/new --model org/new", true)
	test("unchanged when both already say the model", "--model org/new --model-id org/new", "--model org/new --model-id org/new", false)
}

func TestWithoutLaunchFlags_ClearsEveryDeclaredFlag(t *testing.T) {
	flags := []string{"--model", "--model-id"}
	test := func(name, text, want string, wantChanged bool) {
		t.Run(name, func(t *testing.T) {
			got, changed, err := withoutLaunchFlags(text, flags)

			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, wantChanged, changed, "changed")
		})
	}
	test("removes both", "--model org/a --model-id org/a --port 8101", "--port 8101", true)
	test("removes the equals forms", "--model=org/a --model-id=org/a", "", true)
	test("removes one", "--model-id org/a --port 8101", "--port 8101", true)
	test("leaves a launch that names no model", "--port 8101", "--port 8101", false)
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

// flagsSaying is what each of flags says in the engine's saved launch text. A list
// rather than a count, so a model left behind in one flag shows up here rather than
// as a name mismatch somewhere else.
func flagsSaying(t *testing.T, e *Executor, flags ...string) map[string]string {
	t.Helper()
	tokens, err := parseLaunchText(launchTextOf(t, e))
	require.NoError(t, err)
	said := make(map[string]string, len(flags))
	for _, flag := range flags {
		if value, found := launchFlagValue(tokens, flag); found {
			said[flag] = value
		}
	}
	return said
}

// inventoryRow is one pack as an engine's own listing carries it: the name it calls
// the pack and the directory that holds it.
type inventoryRow struct {
	repoID string
	path   string
}

// packListingFixture stages an engine's pack listing in a temporary directory and
// returns the file the fake engine prints as its listing. The directories it names are
// never created: the listing is text a lookup reads, and a case that put real ones on
// disk would pass on the machine it was written on rather than on the answer.
func packListingFixture(t *testing.T, rows ...inventoryRow) string {
	t.Helper()

	entries := make([]string, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, `{"repo_id":`+quoteJSON(row.repoID)+`,"path":`+quoteJSON(row.path)+`}`)
	}
	file := filepath.Join(t.TempDir(), "models.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"models":[`+strings.Join(entries, ",")+`]}`), 0o600))
	return file
}

// mtplxPackListing is the listing the residency fixtures measured on one Mac: the one
// pack it caches, named by its repo id and kept in a directory spelled with a double
// dash.
func mtplxPackListing(t *testing.T) string {
	t.Helper()

	return packListingFixture(t, inventoryRow{repoID: mtplxPackRepoID, path: mtplxServedPath})
}

// packServingExecutor is a stopped fake engine on a manifest that selects its pack the
// way the shipped MTPLX manifest does: the launch flag carries the pack's directory,
// the model the caller names is looked up in the engine's own listing to find that
// directory, and --model-id is a flag PAIR clears rather than writes. The listing is
// the fixture's, staged in a temporary directory and printed by the fake engine as a
// cmd action, so the lookup runs the real action machinery with a real subprocess
// behind it and no case here asks a real MTPLX anything or reads a real cache.
func packServingExecutor(t *testing.T, listing string, launchText string) *Executor {
	t.Helper()

	e := settingsExecutor(t, false)
	m, ok := e.reg.Get("fake")
	require.True(t, ok, "fake manifest missing")
	platform := m.Platforms[hostPlatformKey()]
	// An engine that serves one pack per process waits for a model and gives it back by
	// stopping, which is what makes clearing a model flag a step the user can see.
	platform.Runtime.WaitForModel = true
	platform.Runtime.CLI = fakeEngineBin
	m.Platforms[hostPlatformKey()] = platform
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{
		Flags:      []string{"--model"},
		ClearFlags: []string{"--model-id"},
		Resolve:    launchModelResolvePackPath,
	}}
	m.Actions["unload_model"] = Action{StopEngine: true}
	m.Actions["list_models"] = Action{
		Cmd:    []string{"{cli}", "listfrom", listing},
		Result: &ActionResult{Array: "models", Field: "repo_id"},
	}
	// The residency block is what the lookup reads its rows from, and what says which pack
	// a running server holds: the listing action it names carries the rows, and /health
	// says the directory the server mapped. Where a case's eject names the pack by its
	// cache id and the engine was launched by directory, that join is the only thing that
	// can tell them they mean the same pack.
	m.Actions["loaded_models"] = Action{
		HTTP: &ActionHTTP{Method: "GET", Path: "/v1/models"},
		Result: &ActionResult{Array: "data", Field: "id", Residency: &ActionResidency{
			Source: "list_models", Read: &ActionHTTP{Method: "GET", Path: "/health"},
			PathField: "model_path", RowPathField: "path",
		}},
	}
	require.NoError(t, m.Validate(), "a manifest that is not legal never reaches the code under test")
	// A running engine reads the platform it was loaded with, not the manifest, so an
	// engine that waits for a pack has to be told so where it will actually look.
	st, err := e.state("fake")
	require.NoError(t, err)
	st.plat.Runtime.WaitForModel = true

	state, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{
		Engine:   "fake",
		Settings: settings.Config{ServerPort: state.ServerPort, LaunchText: launchText},
	}, func() error { return nil })
	require.NoError(t, err)
	return e
}

// MTPLX answers requests as a name that comes out of the pack, not as whatever --model-id
// says: its own app runs `serve` with the pack's directory and no --model-id anywhere in
// its arguments, and that server answered
// "youssofal-qwen3.5-4b-mtplx-optimized-speed" for
// Youssofal/Qwen3.5-4B-MTPLX-Optimized-Speed, on two machines whose packs sit under
// different roots. So a launched server's name is no handle on the flag, while a value
// left in saved launch text is a name for whichever pack was selected when it was last
// written. What has to hold, at every change of model, is that no model name survives in
// a flag PAIR manages.
func TestLaunchModel_ClearsTheFlagItNoLongerWrites(t *testing.T) {
	listing := mtplxPackListing(t)
	e := packServingExecutor(t, listing, "serve --host 127.0.0.1 --port 8101 --model-id "+mtplxPackRepoID)
	require.NoError(t, e.Start(t.Context(), "fake"), "start")
	require.Equal(t, map[string]string{"--model-id": mtplxPackRepoID},
		flagsSaying(t, e, "--model", "--model-id"), "the case is about a launch with a stale name in it")

	_, err := loadModel(t, e, mtplxPackRepoID)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"--model": mtplxServedPath}, flagsSaying(t, e, "--model", "--model-id"),
		"a Load moved --model to the pack's directory and left the stale name in --model-id")
}

// The pack PAIR launches is named by the directory its engine's own listing gives,
// because that is the spelling the app launches with, the spelling /health answers
// under, and the only one of MTPLX's three spellings that says which of two machines'
// caches a pack sits in.
func TestLaunchModel_ServesThePackAtThePathItsOwnListingGives(t *testing.T) {
	listing := mtplxPackListing(t)
	e := packServingExecutor(t, listing, "serve --host 127.0.0.1 --port 8101")
	require.NoError(t, e.Start(t.Context(), "fake"), "start")

	raw, err := loadModel(t, e, mtplxPackRepoID)

	require.NoError(t, err)
	assert.Equal(t, loadReply{Model: mtplxPackRepoID, Restarted: true}, decodeLoadReply(t, raw),
		"the reply keeps naming the pack the caller asked for, not the directory PAIR wrote")
	assert.Equal(t, map[string]string{"--model": mtplxServedPath}, flagsSaying(t, e, "--model", "--model-id"),
		"the launch names the pack by the directory its own listing gives")

	// The pack is already the one being served, so nothing restarts: the check is
	// against what is in the launch, which is a directory, not against the name the
	// caller used.
	gen := startGeneration(t, e)
	raw, err = loadModel(t, e, mtplxPackRepoID)
	require.NoError(t, err)
	assert.False(t, decodeLoadReply(t, raw).Restarted, "a load of the pack already served restarted the engine")
	assert.Equal(t, gen, startGeneration(t, e), "a load of the pack already served restarted the engine")
}

// A model that is neither a pack in the listing nor a directory one of them sits in is
// left exactly as it was typed. PAIR does not know what it is, and a launch argument a
// user wrote is theirs to keep: the alternative is a lookup failure turning into a
// wrong path, or into a refused Load for an engine the user can start by hand.
func TestLaunchModel_LeavesAModelNoListingHoldsExactlyAsTyped(t *testing.T) {
	listing := mtplxPackListing(t)
	e := packServingExecutor(t, listing, "serve --host 127.0.0.1 --port 8101 --model-id "+mtplxPackRepoID)
	require.NoError(t, e.Start(t.Context(), "fake"), "start")
	const handTyped = "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed-Experimental"

	_, err := loadModel(t, e, handTyped)

	require.NoError(t, err, "a model PAIR cannot place must still be loadable")
	said := flagsSaying(t, e, "--model", "--model-id")
	assert.Equal(t, handTyped, said["--model"], "the name was rewritten, not left alone")
	assert.NotContains(t, said, "--model-id", "and the flag PAIR only clears was not left holding another pack's name")
}

// The other way a lookup comes back knowing nothing: the listing itself did not arrive.
// That is not the statement that this pack has no directory, and a user who chose a pack
// should not be refused a load because a helper command failed — so the pack is launched
// under the name it was given, exactly as a name no listing holds is.
func TestLaunchModel_NamesThePackAsGivenWhenItsListingDoesNotArrive(t *testing.T) {
	missingListing := filepath.Join(t.TempDir(), "models-that-never-came.json")
	e := packServingExecutor(t, missingListing, "serve --host 127.0.0.1 --port 8101")
	require.NoError(t, e.Start(t.Context(), "fake"), "start")

	_, err := loadModel(t, e, mtplxPackRepoID)

	require.NoError(t, err, "a listing that did not arrive refused the load the user asked for")
	assert.Equal(t, map[string]string{"--model": mtplxPackRepoID}, flagsSaying(t, e, "--model", "--model-id"),
		"the pack was launched under the name it was given, which is all PAIR knows about it")
}

// Clearing is not only a Load's job: ejecting is the other way a model stops being the
// selected one, and a name left in any flag PAIR manages comes back as the selected
// model the next time the engine starts.
func TestEjectByStopping_ClearsEveryFlagTheModelWasNamedIn(t *testing.T) {
	listing := mtplxPackListing(t)
	e := packServingExecutor(t, listing, "serve --host 127.0.0.1 --port 8101 --model-id Youssofal/SomeOtherPack")
	require.NoError(t, e.Start(t.Context(), "fake"), "start")
	_, err := loadModel(t, e, mtplxPackRepoID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"--model": mtplxServedPath}, flagsSaying(t, e, "--model", "--model-id"))

	_, err = ejectEngine(t, e, mtplxPackRepoID)

	require.NoError(t, err)
	assert.Empty(t, flagsSaying(t, e, "--model", "--model-id"),
		"an eject left a model selected in a flag the engine still gets")
	assert.True(t, mustStatus(t, e).AwaitingModel, "an engine with no model selected is waiting for one")
	assert.False(t, mustStatus(t, e).Running, "the eject did not stop the engine")
}

// An engine whose model is a launch argument cannot start without one, and a name in a
// flag PAIR only ever clears is not a selection: it is the leftover of one. Reading it
// as a selection starts the engine with no pack to load, which is worse than leaving it
// waiting for the user to choose.
func TestStart_KeepsAnEngineWaitingWhenOnlyAClearedFlagNamesAModel(t *testing.T) {
	listing := mtplxPackListing(t)
	e := packServingExecutor(t, listing, "serve --host 127.0.0.1 --port 8101 --model-id "+mtplxPackRepoID)

	require.NoError(t, e.Start(t.Context(), "fake"))

	status := mustStatus(t, e)
	assert.True(t, status.AwaitingModel, "an engine with no pack flag started anyway: %v", launchTextOf(t, e))
	assert.False(t, status.Running, "an engine with no pack flag was launched")
	assert.Equal(t, map[string]string{"--model-id": mtplxPackRepoID}, flagsSaying(t, e, "--model", "--model-id"),
		"waiting for a model must not rewrite the user's launch text")
}

// A model flag left behind gets into saved launch text more than one way — PAIR wrote
// it when it was the flag it wrote, a user typed it, or a user copied a launch line from
// somewhere — so the clear is tried against each spelling rather than the one this
// machine happens to have. Each spelling is checked at both jobs that end a model: the
// load that replaces it and the eject that gives it back. The eject case starts with the
// pack flag already holding the right directory, so what is left for the eject to do about
// the other flag is its own, not something the load did first.
func TestLaunchModel_ClearsAStaleModelFlagHoweverItIsWritten(t *testing.T) {
	const base = "serve --host 127.0.0.1 --port 8101"
	for _, spelling := range []struct{ name, stale string }{
		{"named after the flag", "--model-id " + mtplxPackRepoID},
		{"joined to it by an equals sign", "--model-id=" + mtplxPackRepoID},
		{"quoted after the equals sign", `--model-id="` + mtplxPackRepoID + `"`},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			t.Run("when a load takes the model's place", func(t *testing.T) {
				e := packServingExecutor(t, mtplxPackListing(t), base+" "+spelling.stale)
				require.NoError(t, e.Start(t.Context(), "fake"), "start")
				require.Equal(t, map[string]string{"--model-id": mtplxPackRepoID},
					flagsSaying(t, e, "--model", "--model-id"), "the launch has the stale name to begin with")

				_, err := loadModel(t, e, mtplxPackRepoID)

				require.NoError(t, err)
				assert.Equal(t, map[string]string{"--model": mtplxServedPath},
					flagsSaying(t, e, "--model", "--model-id"))
			})
			t.Run("when an eject gives the model back", func(t *testing.T) {
				e := packServingExecutor(t, mtplxPackListing(t), base+" --model "+mtplxServedPath+" "+spelling.stale)
				require.NoError(t, e.Start(t.Context(), "fake"), "start")

				_, err := ejectEngine(t, e, mtplxPackRepoID)

				require.NoError(t, err)
				assert.Empty(t, flagsSaying(t, e, "--model", "--model-id"))
				assert.True(t, mustStatus(t, e).AwaitingModel, "an eject that cleared the model left the engine not waiting for one")
			})
		})
	}
}

// The whole reason for clearing a second flag is that PAIR does not otherwise touch the
// launch it is given, so an argument it does not manage has to come through a load and an
// eject exactly as it went in — including the ones that look like they belong to a model.
func TestLaunchModel_LeavesUnmanagedArgumentsWhereTheyWere(t *testing.T) {
	listing := mtplxPackListing(t)
	e := packServingExecutor(t, listing, "serve --host 127.0.0.1 --port 8101 --unsafe-force-unverified --yes --memory-limit 48g --model-id "+mtplxPackRepoID)
	require.NoError(t, e.Start(t.Context(), "fake"), "start")

	_, err := loadModel(t, e, mtplxPackRepoID)
	require.NoError(t, err)
	_, err = ejectEngine(t, e, mtplxPackRepoID)
	require.NoError(t, err)

	text := launchTextOf(t, e)
	for _, kept := range []string{"--unsafe-force-unverified", "--yes", "--memory-limit 48g"} {
		assert.Contains(t, text, kept, "an argument PAIR does not manage did not survive the model changing")
	}
	assert.NotContains(t, text, "model-id", "a flag PAIR clears left a trace")
	assert.Empty(t, flagsSaying(t, e, "--model", "--model-id"))
}

// The other half of reading a selection from the load flag: text that names a pack the
// engine can load, beside a cleared flag holding another pack's name, is a selected model,
// and the directory is what answers for it. The running engine's own arguments named a
// different pack than the launch PAIR saves, so the load has to start it again on the
// saved ones — the name it carried is the mis-identification this clears.
func TestStart_TakesTheLoadFlagAsTheSelectedModelBesideAClearedFlag(t *testing.T) {
	listing := mtplxPackListing(t)
	e := packServingExecutor(t, listing, "serve --host 127.0.0.1 --port 8101 --model "+mtplxServedPath+" --model-id Youssofal/SomeOtherPack")

	require.NoError(t, e.Start(t.Context(), "fake"), "an engine with a load flag started")

	status := mustStatus(t, e)
	assert.False(t, status.AwaitingModel, "an engine with a pack selected was left waiting for one")
	assert.True(t, status.Running, "an engine with a pack selected was not started")

	raw, err := loadModel(t, e, mtplxPackRepoID)

	require.NoError(t, err)
	reply := decodeLoadReply(t, raw)
	assert.Equal(t, mtplxPackRepoID, reply.Model, "the reply names the pack the caller asked for, not the directory")
	assert.True(t, reply.Restarted, "an engine started on arguments naming another pack kept them")
	assert.Equal(t, map[string]string{"--model": mtplxServedPath}, flagsSaying(t, e, "--model", "--model-id"),
		"the pack flag kept the directory it already held, and the other flag's name for another pack is gone")
}

// The manifest is where this answer has to live, so the shipped one is pinned, not a
// fixture's copy: the launch names the pack by the directory its own listing gives, the
// flag PAIR no longer writes is named as one it clears, and the lookup it asks for is one
// the residency declaration can actually answer.
func TestMTPLXManifest_NamesItsPackByTheDirectoryItsOwnListingGives(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("mtplx")
	require.True(t, ok, "mtplx manifest not loaded")

	load := m.Actions["load_model"]
	require.NotNil(t, load.LaunchModel, "load_model must switch the pack through a launch argument")
	assert.Equal(t, []string{"--model"}, load.LaunchModel.Flags,
		"the flag that picks the weights is the only one PAIR writes")
	assert.Equal(t, []string{"--model-id"}, load.LaunchModel.ClearFlags,
		"--model-id has to be cleared wherever the model changes, not merely stopped being written")
	assert.Equal(t, launchModelResolvePackPath, load.LaunchModel.Resolve,
		"the caller names a pack, and the engine has to be given the directory that pack sits in")

	loaded := m.Actions["loaded_models"]
	require.NotNil(t, loaded.Result, "loaded_models has to say which pack the server serves")
	residency := loaded.Result.Residency
	require.NotNil(t, residency,
		"resolving a pack to its directory reads the same listing residency reads, so it has to be declared")
	assert.Equal(t, "list_models", residency.Source)
	assert.Equal(t, "path", residency.RowPathField,
		"the launch gets the directory the listing itself gives, not one derived from a name")

	assert.True(t, m.Actions["unload_model"].StopEngine,
		"ejecting is the other way a model stops being selected, and it is the one that clears")
}

func TestLaunchModelAction_ClearFlagsAreFlagsPAIRStopsWriting(t *testing.T) {
	accept := func(name string, spec ActionLaunchModel) {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, (&Action{LaunchModel: &spec}).validate("load_model"))
		})
	}
	reject := func(name string, spec ActionLaunchModel) {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, (&Action{LaunchModel: &spec}).validate("load_model"))
		})
	}
	with := func(clearFlags []string, resolve string) ActionLaunchModel {
		return ActionLaunchModel{Flags: []string{"--model"}, ClearFlags: clearFlags, Resolve: resolve}
	}
	accept("a flag to clear alongside the flag it writes", with([]string{"--model-id"}, ""))
	accept("clearing several flags", with([]string{"--model-id", "--served-model-name"}, ""))
	accept("resolving the model through the engine's pack listing", with([]string{"--model-id"}, launchModelResolvePackPath))
	accept("no extra flags at all", with(nil, ""))
	reject("a clear flag that is not a flag", with([]string{"model-id"}, ""))
	reject("a clear flag with a value in it", with([]string{"--model-id=x"}, ""))
	reject("a clear flag listed twice", with([]string{"--model-id", "--model-id"}, ""))
	// A flag in both lists is a manifest that both writes a model into a flag and
	// removes it, in the same transaction, in an order nothing in the manifest says.
	reject("a flag both written and cleared", with([]string{"--model"}, ""))
	reject("a resolution no lookup implements", with(nil, "public-model-id"))
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

// The cases below are the eject guard's, and they come from the residency
// fixtures because that is where the pack this guard has to recognise is measured:
// one MTPLX answers a request as "mtplx-flash-next-optimized-speed", its own cache
// listing calls the same pack "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Speed",
// and the directory holding it is spelled with a double dash. The manifest tells the
// user to eject by cache id, and the model list it builds reports the listing's
// spelling, so a guard that reads only the served names refuses the pack that really
// is resident. The answers here are those measurements; the engine they are staged on
// is one this machine never had.

// ejectableMTPLX is that engine: up, stoppable for real, and asked only the questions
// a fixture declared the answers to.
type ejectableMTPLX struct {
	ex      *Executor
	asked   []residencyAsk
	process *managedProc
	stalled *stalledResidencyTransport
}

// stalledResidencyTransport is an engine that is up on its port and stuck inside one of
// its answers: the reads named in stalled are held until the test lets them go, and the
// rest are answered from bodies. A missing key cannot stand in for this, because a
// missing key fails the moment it is asked and says nothing about what a caller does with
// a read it is still waiting for.
type stalledResidencyTransport struct {
	inner   http.RoundTripper
	stalled map[string]bool
	release chan struct{}
	once    sync.Once
}

func (s *stalledResidencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !s.stalled[req.URL.Path] {
		return s.inner.RoundTrip(req)
	}
	select {
	case <-s.release:
		return nil, fmt.Errorf("the fixture answered %s only after PAIR stopped waiting for it", req.URL.Path)
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func (s *stalledResidencyTransport) letGo() {
	s.once.Do(func() { close(s.release) })
}

// ejectableMTPLXStalledOn is the resident engine whose answer to `stalled` never comes
// back, for the cases about how long an eject is willing to wait for one. Those cases
// lower the bound they are waiting out, so they stay about the waiting rather than about
// how long production is willing to wait.
func ejectableMTPLXStalledOn(t *testing.T, bodies map[string]string, stalled ...string) *ejectableMTPLX {
	t.Helper()

	probeReadTimeoutGiven(t, 250*time.Millisecond)
	paths := make(map[string]bool, len(stalled))
	for _, path := range stalled {
		paths[path] = true
	}
	transport := &stalledResidencyTransport{
		inner:   residencyStubTransport(bodies),
		stalled: paths,
		release: make(chan struct{}),
	}
	t.Cleanup(transport.letGo)
	return ejectableMTPLXStaged(t, func(f *ejectableMTPLX) {
		f.ex.client.Transport = transport
		f.stalled = transport
	})
}

// ejectableMTPLXAsking stages that engine answering exactly bodies.
func ejectableMTPLXAsking(t *testing.T, bodies map[string]string) *ejectableMTPLX {
	t.Helper()

	return ejectableMTPLXStaged(t, func(f *ejectableMTPLX) {
		f.ex.client.Transport = residencyRecordingTransport(bodies, &f.asked)
	})
}

// ejectableMTPLXListingRunByCLI is the resident engine with its cache listing read the
// way the shipped manifest reads MTPLX's — as a cmd action, the engine's own control tool
// rather than the fixture's HTTP stand-in. That difference is the whole case: a subprocess
// is bounded by different machinery from a request.
func ejectableMTPLXListingRunByCLI(t *testing.T, cmd []string, bodies map[string]string) *ejectableMTPLX {
	t.Helper()

	return ejectableMTPLXStagedManifest(t, func(manifest *Manifest) {
		platform := manifest.Platforms[hostPlatformKey()]
		// {cli} follows the tool the platform is observed to have, so the residency read
		// would run the fixture's Detect entry rather than anything the case asked for.
		// With nothing observed, the literal is the only candidate, and the literal is the
		// fake engine: the guard runs a real subprocess of a real manifest-shaped argv.
		platform.Detect = nil
		platform.Runtime.CLI = fakeEngineBin
		manifest.Platforms[hostPlatformKey()] = platform
		manifest.Actions["list_models"] = Action{
			Cmd:    cmd,
			Result: &ActionResult{Array: "models", Field: "repo_id"},
		}
	}, func(f *ejectableMTPLX) {
		f.ex.client.Transport = residencyRecordingTransport(bodies, &f.asked)
	})
}

// ejectableMTPLXStaged is the engine every eject-guard case shares: a MTPLX running on a
// port with nothing behind it, with the launch controls that make eject a stop, and with
// a process that really exits when the stop comes. It takes the answers as a function
// rather than a map because the cases differ in how much of an answer ever arrives, and
// one of them is "none of it, ever".
func ejectableMTPLXStaged(t *testing.T, answers func(f *ejectableMTPLX)) *ejectableMTPLX {
	t.Helper()

	return ejectableMTPLXStagedManifest(t, nil, answers)
}

// ejectableMTPLXStagedManifest is ejectableMTPLXStaged for a case that has to change the
// manifest it stages on. The change runs before the manifest is validated, so a fixture
// that edits itself into an illegal shape fails there rather than quietly never reaching
// the guard it is meant to be exercising.
func ejectableMTPLXStagedManifest(t *testing.T, changeManifest func(manifest *Manifest), answers func(f *ejectableMTPLX)) *ejectableMTPLX {
	t.Helper()

	manifest := residencyManifestWithRestart(t)
	platform := manifest.Platforms[hostPlatformKey()]
	// The five keys the shipped manifest declares for an engine that serves one pack per
	// process: it waits for a model, it serves on a host and port it is told, it names
	// the pack to serve with the directory its own listing gives, it clears a flag it no
	// longer writes, and ejecting that model stops the engine rather than dropping it.
	// Without them the eject these cases are about is a different action, and an engine
	// with no launch arguments has no launch to keep.
	platform.Runtime.WaitForModel = true
	platform.Runtime.Args = []string{"serve", "--host", "{host}", "--port", "{port}"}
	manifest.Platforms[hostPlatformKey()] = platform
	manifest.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{
		Flags:      []string{"--model"},
		ClearFlags: []string{"--model-id"},
		Resolve:    launchModelResolvePackPath,
	}}
	manifest.Actions["unload_model"] = Action{StopEngine: true}
	if changeManifest != nil {
		changeManifest(manifest)
	}
	require.NoError(t, manifest.Validate(), "a manifest that is not legal never reaches the guard")

	fixture := &ejectableMTPLX{}
	fixture.ex = newTestExecutor(t, manifest)
	residencyMarkRunning(t, fixture.ex, manifest.Engine)
	answers(fixture)
	// Ejecting the pack keeps the launch it was served on, and keeping that is a write to
	// the engine's settings override — a directory only settingsExecutor hands out, and
	// one an eject needs whether or not the case is about settings.
	fixture.ex.overrideDir = t.TempDir()
	// The stop has to be a stop. Marking the engine running is enough to reach the
	// guard, but not enough to tell an eject that stopped something from one that
	// stopped nothing, so the state carries a process that really is running and
	// really does exit. It binds no port, so no case here meets a listener it did not
	// declare.
	process, err := startManagedProc(fakeEngineBin, []string{"noserve"}, nil, nil)
	require.NoError(t, err, "the engine being ejected has to be a live process")
	t.Cleanup(func() { process.stop(2 * time.Second) })
	fixture.process = process
	st, err := fixture.ex.state("mtplx")
	require.NoError(t, err)
	st.mu.Lock()
	st.proc = process
	st.mu.Unlock()
	return fixture
}

// residentMTPLXAnswers is one running MTPLX's three answers about the pack it serves:
// the listing its cache gives, the directory it loaded that pack from, and the name it
// answers requests as. Each case gets its own copy because each changes one entry.
func residentMTPLXAnswers() map[string]string {
	return map[string]string{
		residencyInventoryPath: sagittariusModelsBody,
		residencyHealthPath:    residencyHealthBody(mtplxServedPath),
		residencyServedPath:    residencyServedBody,
	}
}

// probeReadTimeoutGiven gives every read of the engine a shorter bound than it is really
// given, and puts the real one back when the case ends. The bound being a var is what
// lets a case watch a caller stop waiting for a read without the case being about how
// long a stalled engine can hold up a suite.
func probeReadTimeoutGiven(t *testing.T, bound time.Duration) {
	t.Helper()

	was := probeReadTimeout
	probeReadTimeout = bound
	t.Cleanup(func() { probeReadTimeout = was })
}

func (f *ejectableMTPLX) eject(t *testing.T, model string) (json.RawMessage, error) {
	t.Helper()
	return f.ex.Action(t.Context(), "mtplx", "unload_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
}

// requireEjected asserts the eject of this name was carried out end to end: the engine
// stopped, the process PAIR was handed gone with it, and what is left is what ejecting a
// pack leaves on an engine that serves one model per process — enabled, and waiting for
// the next model to be chosen.
func (f *ejectableMTPLX) requireEjected(t *testing.T, model, why string) {
	t.Helper()

	raw, err := f.eject(t, model)
	f.requireEjectionOf(t, raw, err, model, why)
}

// requireEjectedWithin is requireEjected for the eject that has to stop waiting for
// something: it runs the eject on its own goroutine and the case fails if the eject is
// still running when limit passes. The waiting is what those cases are about, and an
// eject that waits for the read rather than for the bound it set would otherwise hold the
// suite for the whole action timeout — which is the same thing it does to the click.
func (f *ejectableMTPLX) requireEjectedWithin(t *testing.T, model string, limit time.Duration, why string) {
	t.Helper()

	type ejection struct {
		raw json.RawMessage
		err error
	}
	finished := make(chan ejection, 1)
	eject := t.Context()
	go func() {
		raw, err := f.ex.Action(eject, "mtplx", "unload_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
		finished <- ejection{raw, err}
	}()
	select {
	case got := <-finished:
		f.requireEjectionOf(t, got.raw, got.err, model, why)
	case <-time.After(limit):
		if f.stalled != nil {
			f.stalled.letGo()
		}
		require.FailNow(t, "the eject is still waiting on a read that never answers", "%s: ejecting %s waited %s for it", why, model, limit)
	}
}

func (f *ejectableMTPLX) requireEjectionOf(t *testing.T, raw json.RawMessage, err error, model, why string) {
	t.Helper()

	require.NoError(t, err, "%s: ejecting %s", why, model)
	var reply struct {
		Model   string `json:"model"`
		Stopped bool   `json:"stopped"`
	}
	require.NoError(t, json.Unmarshal(raw, &reply))
	assert.Equal(t, model, reply.Model, "the reply names the model it ejected")
	assert.True(t, reply.Stopped, "%s: the reply says stopped", why)
	select {
	case <-f.process.done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the engine process is still running", why)
	}
	f.requireState(t, false, true, why)
}

// requireRefusedWithin is requireRefused for a case measuring the waiting: the refusal has
// to arrive within limit, because a refusal that spent the whole action timeout getting
// there is the same stall with a message on it. It hands back the error so the case can
// say which refusal it saw.
func (f *ejectableMTPLX) requireRefusedWithin(t *testing.T, model string, limit time.Duration, why string) error {
	t.Helper()

	eject := t.Context()
	finished := make(chan error, 1)
	go func() {
		_, err := f.ex.Action(eject, "mtplx", "unload_model", json.RawMessage(`{"model":`+quoteJSON(model)+`}`))
		finished <- err
	}()

	var err error
	select {
	case err = <-finished:
	case <-time.After(limit):
		if f.stalled != nil {
			f.stalled.letGo()
		}
		require.FailNow(t, "the eject is still waiting on a read that never answers",
			"%s: refusing to eject %s waited %s for the read to answer", why, model, limit)
	}

	require.Error(t, err, "%s: ejecting %s must be refused", why, model)
	select {
	case <-f.process.done:
		require.FailNow(t, "a refused eject stopped the engine", why)
	default:
	}
	f.requireState(t, true, false, why)
	return err
}

// requireRefused asserts the eject of this name was refused and the engine was left
// serving: an eject that refuses must not have stopped anything.
func (f *ejectableMTPLX) requireRefused(t *testing.T, model, why string) error {
	t.Helper()

	_, err := f.eject(t, model)
	require.Error(t, err, "%s: ejecting %s must be refused", why, model)
	select {
	case <-f.process.done:
		require.FailNow(t, "a refused eject stopped the engine", why)
	default:
	}
	f.requireState(t, true, false, why)
	return err
}

// requireState reads back the fact the guard is gated on: it only asks an engine what
// it has resident when the executor believes that engine is up. A case that never
// reached the guard would otherwise pass against an engine that was never running.
func (f *ejectableMTPLX) requireState(t *testing.T, wantRunning, wantAwaitingModel bool, why string) {
	t.Helper()

	st, err := f.ex.state("mtplx")
	require.NoError(t, err)
	st.mu.Lock()
	running, awaiting := st.running, st.awaitingModel
	st.mu.Unlock()
	assert.Equal(t, wantRunning, running, "%s: running", why)
	assert.Equal(t, wantAwaitingModel, awaiting, "%s: awaiting a model", why)
}

func TestEjectByStopping_ServesTheEjectForTheResidentPackUnderItsServedName(t *testing.T) {
	// The same pack, resident on the same engine, named both ways a user can have it:
	// the name the engine answers requests as, which the guard has always known, and the
	// cache id the manifest and PAIR's own model list use, which is the one that was
	// refused.
	for _, asked := range []struct{ name, model string }{
		{"the name the engine answers requests as", mtplxServedModel},
		{"the cache id the model list carries", mtplxPackRepoID},
	} {
		t.Run(asked.name, func(t *testing.T) {
			fixture := ejectableMTPLXAsking(t, residentMTPLXAnswers())
			fixture.requireState(t, true, false, "the engine starts up")

			fixture.requireEjected(t, asked.model, "the resident pack is ejectable however this MTPLX spells it")
		})
	}
}

func TestEjectByStopping_StillRefusesAPackThatIsNotResident(t *testing.T) {
	// The other side of the same join. The server says it loaded from a directory its
	// own listing holds no row for — where a pack handed over as a pair bundle lands —
	// while that listing does hold a pack whose folder ends with the same name. So which
	// pack is resident is genuinely open, and the pack the cache names is not the one
	// this server is running off. A guard that joined loosely enough to let this eject
	// through is a guard that stops the engine to eject a pack nobody is serving, on the
	// strength of a folder name.
	answers := residentMTPLXAnswers()
	answers[residencyHealthPath] = residencyHealthBody(residencyPairBundlePath)
	fixture := ejectableMTPLXAsking(t, answers)

	err := fixture.requireRefused(t, mtplxPackRepoID, "a cached pack this engine did not load")
	assert.ErrorContains(t, err, mtplxPackRepoID+" is not the model mtplx is serving")
	assert.Contains(t, err.Error(), mtplxServedModel, "the refusal names what the engine does serve, in the spelling that would work")
	assert.NotContains(t, err.Error(), residencyPairBundlePath, "a refusal quotes names, never the directory a model was loaded from")
	require.Len(t, asksForPath(fixture.asked, residencyInventoryPath), 1,
		"the refusal came from asking the engine and the cache, not from skipping the join")
}

// TestEjectByStopping_StopsTheEngineThatCannotSayWhatIsResident is the half of the guard
// with nothing to refuse on. `TestEjectByStopping_StillRefusesAPackThatIsNotResident` is
// the other half and keeps refusing: the engine answered where it loaded from, the
// listing answered what it holds, and the name the user gave matches neither, so the
// refusal is evidence and it quotes the name that would work. Every stage here is a way
// that join fails to produce evidence at all — an answer that is not the engine's, an
// answer bigger than PAIR reads, no answer, and an answer that arrives after PAIR stopped
// waiting — and what is left in those cases is the served names alone, which for an
// engine that serves one pack per process is a spelling no model list and no listing ever
// showed the user. The eject button carries the cache id this join produces; refuse here
// and the click does nothing, the model stays resident, and the message names a string
// nobody can type, to prevent an outcome the Stop button beside it reaches unasked.
func TestEjectByStopping_StopsTheEngineThatCannotSayWhatIsResident(t *testing.T) {
	stages := []struct {
		name    string
		fixture func(t *testing.T) *ejectableMTPLX
	}{
		{"answers with a web app's bundle", func(t *testing.T) *ejectableMTPLX {
			answers := residentMTPLXAnswers()
			answers[residencyHealthPath] = residencyForeignBody
			return ejectableMTPLXAsking(t, answers)
		}},
		{"answers with more than PAIR reads", func(t *testing.T) *ejectableMTPLX {
			answers := residentMTPLXAnswers()
			answers[residencyHealthPath] = residencyOversizedHealthBody(t)
			return ejectableMTPLXAsking(t, answers)
		}},
		{"says nothing about its own model", func(t *testing.T) *ejectableMTPLX {
			answers := residentMTPLXAnswers()
			delete(answers, residencyHealthPath)
			return ejectableMTPLXAsking(t, answers)
		}},
		// The two reads of the join held open one at a time, because they are two
		// different reads and only one of them was ever unbounded: the loaded path is a
		// probe read of a server that is answering, and the listing the answer joins
		// against runs through the action machinery, where the eject's own bound is all
		// it gets — left to that machinery's own budget, the read has the action timeout
		// instead.
		{"takes longer than the eject will wait for the loaded path", func(t *testing.T) *ejectableMTPLX {
			return ejectableMTPLXStalledOn(t, residentMTPLXAnswers(), residencyHealthPath)
		}},
		{"takes longer than the eject will wait for the cache listing", func(t *testing.T) *ejectableMTPLX {
			return ejectableMTPLXStalledOn(t, residentMTPLXAnswers(), residencyInventoryPath)
		}},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			// An eject that does not wait for a stalled read is done in milliseconds;
			// one that waits for the read is waiting for the action timeout, which is
			// 30 minutes. Three seconds tells those apart without leaning on the
			// machine the test runs on.
			const limit = 3 * time.Second
			for _, named := range []struct{ label, model string }{
				{"the name the engine answers requests as", mtplxServedModel},
				{"the cache id the model list carries", mtplxPackRepoID},
			} {
				t.Run("it ejects "+named.label, func(t *testing.T) {
					fixture := stage.fixture(t)
					fixture.requireState(t, true, false, "the engine starts up")

					fixture.requireEjectedWithin(t, named.model, limit,
						"an engine that cannot say what it has mapped does not get to hold its model hostage")
				})
			}
		})
	}
}

func TestEjectByStopping_RefusesANearMissSpellingOfTheResidentPack(t *testing.T) {
	// Joining the two halves is a comparison, so what it must not accept is part of the
	// contract. The same pack has other spellings nearby: the folder its files sit in,
	// differing from the cache id by a doubled separator; that folder's full directory,
	// which is what the join reads rather than a name; and the cache id with its org
	// dropped or its case changed. A guard that folded case, rewrote separators, settled
	// for a substring or read a path as a name would let all of them through, and then an
	// eject typed as a typo stops the engine on the grounds that some part of the request
	// looks like what is resident.
	for _, asked := range []struct{ name, model string }{
		{"the cache id in another case", strings.ToUpper(mtplxPackRepoID)},
		{"the folder the pack sits in", filepath.Base(mtplxServedPath)},
		{"the cache id without its org", strings.SplitN(mtplxPackRepoID, "/", 2)[1]},
		{"the directory the pack sits in", mtplxServedPath},
	} {
		t.Run(asked.name, func(t *testing.T) {
			fixture := ejectableMTPLXAsking(t, residentMTPLXAnswers())

			err := fixture.requireRefused(t, asked.model, "what is resident is ejectable under its two names and no other")
			assert.ErrorContains(t, err, asked.model+" is not the model mtplx is serving")
			assert.ErrorContains(t, err, mtplxPackRepoID, "the refusal gives the spelling that does work")
		})
	}
}

func TestEjectByStopping_ReadsThePackInventoryNoMoreThanOnce(t *testing.T) {
	// For this engine the inventory is a `mtplx models --json` subprocess rather than a
	// request, so what the guard pays for its answer is worth a case of its own: a
	// second read per eject is invisible in a stubbed transport unless the transport
	// counts.
	t.Run("once, when the engine says where its model came from", func(t *testing.T) {
		fixture := ejectableMTPLXAsking(t, residentMTPLXAnswers())

		fixture.requireEjected(t, mtplxPackRepoID, "the eject runs the join")

		require.Len(t, asksForPath(fixture.asked, residencyInventoryPath), 1,
			"one eject runs the cache listing once")
		require.Len(t, asksForPath(fixture.asked, residencyHealthPath), 1,
			"one eject asks where the model was loaded from once")
	})
	t.Run("not at all, when the engine cannot say", func(t *testing.T) {
		answers := residentMTPLXAnswers()
		delete(answers, residencyHealthPath)
		fixture := ejectableMTPLXAsking(t, answers)

		fixture.requireEjected(t, mtplxServedModel, "the eject of a served name runs without the inventory")

		require.Empty(t, asksForPath(fixture.asked, residencyInventoryPath),
			"an engine that says nothing about its own pack must not cost a cache listing run")
	})
}

// TestEjectByStopping_BoundsACacheListingRunAsASubprocess is the eject's bound on the
// transport the shipped manifest actually uses.
//
// Every case above stages the cache listing as an HTTP action, because that is how the
// fixture delivers a measured body without a real MTPLX. manifests/mtplx.json runs it as
// `mtplx models --json` instead, and a subprocess is not bounded the way a request is:
// exec.CommandContext signals the CLI when the caller's deadline passes, but Wait ends
// only when the command's output pipes reach EOF — and a process the CLI started inherits
// those pipes and outlives it. Measured on this machine against a helper that sleeps a
// minute, the read came back when the helper finally went away, and the deadline had
// nothing to say about the wait. For an eject that means the action timeout: 30 minutes of
// an eject button that does nothing while it holds the engine's operation lock.
//
// The limit below is not a tightened copy of the bound: the read gets probeReadTimeout,
// the 3 s every other case here pays for, and a bounded read of this fixture's shape is
// done about a sixth of the way in (measured: 1.02 s of the 6 s, the drain timeout being
// what it waits on). A case that goes past this limit is waiting on the action timeout
// instead.
func TestEjectByStopping_BoundsACacheListingRunAsASubprocess(t *testing.T) {
	const limit = 6 * time.Second

	dir := t.TempDir()
	listing := filepath.Join(dir, "models.json")
	require.NoError(t, os.WriteFile(listing, []byte(sagittariusModelsBody), 0o600))
	pidFile := filepath.Join(dir, "listing-helper.pid")

	t.Run("a listing whose CLI leaves a helper holding its output is not waited on", func(t *testing.T) {
		// The finding. The CLI prints the listing and exits 0 while something it started
		// keeps the pipe the listing came down open, so the wait never sees EOF and the
		// read runs to the helper's schedule instead of the eject's bound. This fails
		// before the fix by still being in flight at the limit: the helper goes away a
		// minute later, and the read returns then, which is the same class of stall the
		// guard is not allowed to put a click through.
		fixture := ejectableMTPLXListingRunByCLI(t,
			[]string{"{cli}", "listleavesahelper", listing, pidFile}, residentMTPLXAnswers())
		t.Cleanup(func() { killFakeHelper(pidFile) })
		fixture.requireState(t, true, false, "the engine starts up")

		fixture.requireEjectedWithin(t, mtplxPackRepoID, limit,
			"an eject is a click, and a listing whose CLI stopped answering a second ago is not a reason to keep waiting")

		// The other direction on the same run: if the CLI had never got as far as leaving
		// a helper, this case would pass on a read that was never wedged and pin nothing.
		require.FileExists(t, pidFile, "the listing's CLI never left a helper holding its output")
	})

	t.Run("a listing a helper is still holding is the listing the guard refuses on", func(t *testing.T) {
		// Bounding the wait must not throw the answer away. The CLI exited of its own
		// accord, so the listing arrived whole, and what the join does with the rows in
		// front of it is refuse — an eject that proceeded here stopped an engine serving
		// a different pack. Before the fix this case parks exactly as long as the one
		// above; a fix that gave up the listing to get the timing right turns it into an
		// engine stopped in error.
		fixture := ejectableMTPLXListingRunByCLI(t,
			[]string{"{cli}", "listleavesahelper", listing, pidFile}, residentMTPLXAnswers())
		t.Cleanup(func() { killFakeHelper(pidFile) })

		err := fixture.requireRefusedWithin(t, "Youssofal/Gemma4-MTPLX-Optimized-Speed", limit,
			"the pack this MTPLX's own listing does not hold")
		assert.ErrorContains(t, err, "is not the model mtplx is serving")
	})

	t.Run("a listing whose CLI never answers is stopped at the bound", func(t *testing.T) {
		// A pin, not a fix: this one already came back inside the bound, because the CLI
		// is still the only writer on its output when the deadline kills it and the pipe
		// closes with it. It is here so that a fix which dropped the caller's context into
		// the command, or gave the read a budget of its own — which is what the action
		// timeout already is, and the thing this is meant to shorten — fails something
		// rather than quietly putting the eject back on 30 minutes.
		fixture := ejectableMTPLXListingRunByCLI(t, []string{"{cli}", "holdoutput"}, residentMTPLXAnswers())
		fixture.requireState(t, true, false, "the engine starts up")

		fixture.requireEjectedWithin(t, mtplxPackRepoID, limit,
			"a CLI that never answers gets killed at the bound the eject set, not waited out")
	})
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

func TestLaunchModelAction_AcceptsOnlyAListOfLongOptionFlags(t *testing.T) {
	accept := func(name string, flags ...string) {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, (&Action{LaunchModel: &ActionLaunchModel{Flags: flags}}).validate("load_model"))
		})
	}
	reject := func(name string, flags ...string) {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, (&Action{LaunchModel: &ActionLaunchModel{Flags: flags}}).validate("load_model"))
		})
	}
	accept("one long option", "--model")
	accept("a model pair that moves together", "--model", "--model-id")
	reject("no flag at all")
	reject("no dashes", "model")
	reject("a space in a flag", "--mod el")
	reject("a value in a flag", "--model=x")
	reject("an empty flag", "")
	reject("a flag listed twice", "--model", "--model")
}

// The restart needs editable launch settings and a readiness probe on every platform.
func TestLaunchModelAction_RequiresEditableLaunch(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flags: []string{"--model"}}}

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
