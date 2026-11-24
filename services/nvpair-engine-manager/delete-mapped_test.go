// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// A manifest-declared stop_before, pinned against a fake engine that really binds a
// port and really takes the order to stop. Each expectation below is read from the
// transcript that engine writes — which call reached its control CLI, and whether its
// port answered at that moment — because what this feature promises is a promise
// about an instant: the removal must not be the thing that happens while something is
// still serving the pack. "The engine was told to stop first" is not that promise, and
// neither is "the removal ran afterwards"; only what the port was doing in between is.

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The pack the delete under test names. The spellings are the engine's own — a
	// cache id and a directory — and none of them is a path on the machine running
	// the test: the directory is only ever echoed back by the fake's listing and its
	// health answer, which is all the join needs to join on.
	deleteSeamRepoID = "fixture/Quiet-Pack"
	deleteSeamPack   = "/fixture/packs/quiet-pack"

	deleteSeamOtherRepoID = "fixture/Served-Pack"
	deleteSeamOtherPack   = "/fixture/packs/served-pack"
)

// listedPack is one entry of the pack listing the engine's own CLI answers: which pack
// the cache holds, and the directory it sits in. The running server names only the
// directory it loaded, so joining these two is what says which pack is in use.
type listedPack struct {
	RepoID string `json:"repo_id"`
	Path   string `json:"path"`
}

var (
	deleteQuietPack  = listedPack{RepoID: deleteSeamRepoID, Path: deleteSeamPack}
	deleteServedPack = listedPack{RepoID: deleteSeamOtherRepoID, Path: deleteSeamOtherPack}
)

// deleteSeam is one MTPLX-shaped engine a pack may be deleted from, plus the
// transcript a test reads: what ran against the engine's control CLI, and whether the
// engine's port was answering when it did.
type deleteSeam struct {
	ex         *Executor
	manifest   *Manifest
	port       int
	transcript string
}

type deleteSeamOptions struct {
	// serves is the directory the engine launches on, which is the directory its
	// health answer reports as the one it loaded. Left empty, the engine starts with
	// nothing mapped and answers the residency read with an empty string, which is
	// what an idle MTPLX answers: the field is always there and says "" when no model
	// was loaded. That is an answer about the running server, not a missing read, and
	// the two must not be staged the same way.
	serves string
	// cacheRows is the listing the engine's CLI answers for the packs its cache
	// holds.
	cacheRows []listedPack
	// readPath overrides where the manifest reads the mapped pack's directory from.
	// Set it to a path the engine answers nothing for, to ask a question that cannot
	// be answered.
	readPath string
	// termDrain is how long the engine keeps running after its listener has gone: a
	// port released in the order a real server releases one.
	termDrain string
	// holderPIDFile asks the engine to hand its port to a helper that outlives it,
	// for a port that answers after the process PAIR started is gone.
	holderPIDFile string
}

// newDeleteSeam reserves the port the engine will serve on and sets up the transcript
// it writes. The delete under test is declared through the manifest's JSON shape, so
// see stoppingFirst.
func newDeleteSeam(t *testing.T, deleteAction Action, opts deleteSeamOptions) *deleteSeam {
	t.Helper()

	port := portRun(t, 1)[0]
	record := filepath.Join(t.TempDir(), "engine-transcript")
	t.Setenv("FAKE_RECORD_FILE", record)
	t.Setenv("FAKE_RECORD_PORT", strconv.Itoa(port))
	rows, err := json.Marshal(map[string][]listedPack{"models": opts.cacheRows})
	require.NoError(t, err, "the cache listing the engine answers with")
	t.Setenv("FAKE_PACK_ROWS", string(rows))
	if opts.termDrain != "" {
		t.Setenv("FAKE_TERM_DRAIN", opts.termDrain)
	}
	if opts.holderPIDFile != "" {
		t.Setenv("FAKE_TERM_HOLDER_PID", opts.holderPIDFile)
	}

	manifest := mtplxSeamManifest(t, deleteAction, opts, port)

	ex := newTestExecutor(t, manifest)
	t.Cleanup(func() { _ = ex.Stop("mtplx") })
	return &deleteSeam{ex: ex, manifest: manifest, port: port, transcript: record}
}

// mtplxSeamManifest is an MTPLX-shaped manifest whose lifecycle is the real fake
// engine: one pack per process, the packs it holds listed in one place and the
// directory its server has mapped in another, and the delete under test.
func mtplxSeamManifest(t *testing.T, deleteAction Action, opts deleteSeamOptions, port int) *Manifest {
	t.Helper()

	manifest := testEngineManifest(fakeEngineBin)
	manifest.Engine = "mtplx"
	key := hostPlatformKey()
	platform := manifest.Platforms[key]
	platform.Runtime.Port = port
	if opts.serves != "" {
		platform.Runtime.Args = []string{"--model=" + opts.serves}
	}
	manifest.Platforms[key] = platform

	readPath := opts.readPath
	if readPath == "" {
		readPath = "/health"
	}
	manifest.Actions = map[string]Action{
		// MTPLX's two questions, in the two spellings it answers them: its CLI lists
		// the packs the cache holds and names each directory, and the running server
		// names a directory and no id. The join is what makes them one pack.
		"list_models": {
			Cmd:    []string{"{cli}", "models", "--json"},
			Result: &ActionResult{Array: "models", Field: "repo_id"},
		},
		"loaded_models": {
			HTTP: &ActionHTTP{Method: "GET", Path: "/v1/models"},
			Result: &ActionResult{Array: "data", Field: "id", Residency: &ActionResidency{
				Source:       "list_models",
				Read:         &ActionHTTP{Method: "GET", Path: readPath},
				PathField:    "model_path",
				RowPathField: "path",
			}},
		},
		"delete_model": deleteAction,
	}
	require.NoError(t, manifest.Validate(), "the shape every case below starts from")
	return manifest
}

func (s *deleteSeam) run(t *testing.T) {
	t.Helper()
	require.NoError(t, s.ex.Start(t.Context(), "mtplx"), "start the engine on its reserved port")
}

func (s *deleteSeam) delete(t *testing.T, repoID string) (json.RawMessage, error) {
	t.Helper()
	return s.ex.Action(t.Context(), "mtplx", "delete_model", []byte(`{"model":"`+repoID+`"}`))
}

// lines reads the transcript in the order the engine wrote it.
func (s *deleteSeam) lines(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile(s.transcript)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err, "read the engine's transcript")
	lines := []string{}
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// stoppingFirst declares "stop_before": true on an action the way a manifest does, in
// JSON rather than through a Go field. A fixture that could not compile until the
// field existed would fail for the wrong reason; declared this way, the tests below
// fail because nothing stopped the engine before it removed the pack.
func stoppingFirst(t *testing.T, action Action) Action {
	t.Helper()

	body, err := json.Marshal(action)
	require.NoError(t, err, "marshal the action as a manifest would write it")
	var declared map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &declared), "read back what the action declares")
	declared["stop_before"] = json.RawMessage("true")
	patched, err := json.Marshal(declared)
	require.NoError(t, err, "write the manifest's declaration")
	var stopping Action
	require.NoError(t, json.Unmarshal(patched, &stopping), "a manifest that says stop_before has to be readable")
	return stopping
}

// cliDelete is MTPLX's own removal: its control CLI, the cache id as the argument, and
// the --yes that keeps it from waiting for a prompt nobody is going to answer.
func cliDelete() Action {
	return Action{Cmd: []string{"{cli}", "remove", "{model}", "--yes", "--json"}}
}

// shrinkQuietBound spends the shortest wait that still distinguishes waiting from
// not waiting, so these tests do not sit through the whole allowance while they check
// what happened inside it.
func shrinkQuietBound(t *testing.T, bound time.Duration) {
	t.Helper()

	previous := stopBeforeQuietBound
	stopBeforeQuietBound = bound
	t.Cleanup(func() { stopBeforeQuietBound = previous })
}

func portIsAnswering(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TestStopBeforeAction_RemovesOnlyAfterTheStoppedEngineHasStoppedAnswering is the
// sequence MTPLX needs: the engine holding the pack is stopped, its port is waited out,
// and the removal is what runs afterwards. Without the wait the removal runs while the
// listener is still up, and the transcript's last line says so.
func TestStopBeforeAction_RemovesOnlyAfterTheStoppedEngineHasStoppedAnswering(t *testing.T) {
	seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
		serves:    deleteSeamPack,
		cacheRows: []listedPack{deleteQuietPack},
		// The engine stops answering immediately and exits a beat later, which is
		// the order a real server releases a port.
		termDrain: "300ms",
	})
	shrinkQuietBound(t, 5*time.Second)
	seam.run(t)
	require.True(t, portIsAnswering(seam.port), "the engine under test has to be serving")

	_, err := seam.delete(t, deleteSeamRepoID)

	require.NoError(t, err, "delete_model")
	require.False(t, portIsAnswering(seam.port), "the engine was left serving the pack it just had removed")
	assert.Equal(t, []string{
		"cmd models --json port=answered",
		"stop port=answered",
		"cmd remove " + deleteSeamRepoID + " --yes --json port=quiet",
	}, seam.lines(t), "what was mapped is read while the engine can still answer, the engine is stopped, and the removal runs once nothing answers")
}

// TestStopBeforeAction_RemovesWithNoStopWhenNothingIsRunning covers the half of the
// rule that keeps a delete of a stopped engine from becoming a stop: nothing is
// serving the pack, so there is no mapping to wait out and no listener to ask about.
func TestStopBeforeAction_RemovesWithNoStopWhenNothingIsRunning(t *testing.T) {
	seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
		serves:    deleteSeamPack,
		cacheRows: []listedPack{deleteQuietPack},
	})

	_, err := seam.delete(t, deleteSeamRepoID)

	require.NoError(t, err, "delete_model against a stopped engine")
	lines := seam.lines(t)
	require.Len(t, lines, 1, "a stopped engine gets no stop order and no residency read: %v", lines)
	assert.Equal(t, "cmd remove "+deleteSeamRepoID+" --yes --json port=quiet", lines[0], "the removal is the only thing that ran")
}

// TestStopBeforeAction_RemovesWithoutStoppingWhenAnotherPackIsMapped keeps stop_before
// from turning every delete into a restart: MTPLX serves one pack, so a pack other
// than the mapped one is not in use, and stopping the engine would be a outage the
// user did not ask for.
func TestStopBeforeAction_RemovesWithoutStoppingWhenAnotherPackIsMapped(t *testing.T) {
	seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
		serves:    deleteSeamOtherPack,
		cacheRows: []listedPack{deleteQuietPack, deleteServedPack},
		termDrain: "300ms",
	})
	seam.run(t)

	_, err := seam.delete(t, deleteSeamRepoID)

	require.NoError(t, err, "delete_model")
	assert.Equal(t, []string{
		"cmd models --json port=answered",
		"cmd remove " + deleteSeamRepoID + " --yes --json port=answered",
	}, seam.lines(t), "the pack is not the one being served, so nothing was stopped and the port answered throughout")
	assert.True(t, portIsAnswering(seam.port), "an engine serving a different pack was taken down for a delete it had no part in")
}

// TestStopBeforeAction_StopsTheEngineItCannotAskAboutAsIfThePackWereMapped pins the
// direction of the doubt, which is the opposite of the eject's: an eject refuses to
// claim a pack is not resident when the read failed, and a delete here treats the pack
// as mapped on the same failure. The two are the same caution about the same unknown —
// neither acts as if it knew — and only one of them can cost the user a running model
// or a pack a server has mapped.
func TestStopBeforeAction_StopsTheEngineItCannotAskAboutAsIfThePackWereMapped(t *testing.T) {
	seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
		serves:    deleteSeamPack,
		cacheRows: []listedPack{deleteQuietPack},
		termDrain: "300ms",
		// The engine is up and answering, and the one question the join asks of it
		// is not answered: no field, so no pack to compare against.
		readPath: "/api/nothing-to-join-here",
	})
	shrinkQuietBound(t, 5*time.Second)
	seam.run(t)

	_, err := seam.delete(t, deleteSeamRepoID)

	require.NoError(t, err, "delete_model")
	lines := seam.lines(t)
	assert.Contains(t, lines, "stop port=answered", "an engine whose mapped pack could not be read was deleted from without being stopped: %v", lines)
	require.NotEmpty(t, lines)
	last := lines[len(lines)-1]
	assert.Equal(t, "cmd remove "+deleteSeamRepoID+" --yes --json port=quiet", last,
		"the removal ran while something still answered on the engine's port: %v", lines)
}

// TestStopBeforeAction_RemovesWithoutStoppingAnIdleEngineThatMapsNothing is the case
// the three states exist for: an engine is up, it answers the residency read clearly,
// and what it answers is that it has nothing mapped. That is not "we could not get an
// answer" — the server always sends the field and sends it empty when it loaded no
// model — so PAIR removes the file and leaves the engine up with its port still
// answering. Reading the empty answer as unknown instead would quit an idle engine, and
// a model-less server the user started themselves, to delete a pack neither one had
// open.
func TestStopBeforeAction_RemovesWithoutStoppingAnIdleEngineThatMapsNothing(t *testing.T) {
	seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
		// No --model, so the engine answers /health with {"ok":true,"model_path":""}.
		cacheRows: []listedPack{deleteQuietPack},
		// The drain is here so a stop would leave a mark: it keeps the port answering
		// for 300 ms after the order arrives. Without it the transcript could not tell
		// "nothing was stopped" from "stopped and gone instantly".
		termDrain: "300ms",
	})
	seam.run(t)
	require.True(t, portIsAnswering(seam.port), "the engine under test has to be up and answering")

	_, err := seam.delete(t, deleteSeamRepoID)

	require.NoError(t, err, "delete_model")
	assert.Equal(t, []string{
		// The removal is the only thing that ran, and it ran with the port answering:
		// no stop order, and no wait for a quiet that was never coming.
		"cmd remove " + deleteSeamRepoID + " --yes --json port=answered",
	}, seam.lines(t), "an engine that had nothing mapped was taken down to delete a file it never touched")
	assert.True(t, portIsAnswering(seam.port), "the engine was left down after a delete it had no part in")
}

// TestStopBeforeAction_StopsForEveryAnswerThatCannotNameAnotherPack pins the third
// state beside the second: everything that is not a readable answer naming a provably
// different pack stops the engine first. These two tests are what prove the states are
// three — this one fails if the middle state swallows either arm of "cannot tell", and
// the idle test above fails if it swallows a clear "nothing mapped".
func TestStopBeforeAction_StopsForEveryAnswerThatCannotNameAnotherPack(t *testing.T) {
	cases := []struct {
		name      string
		serves    string
		cacheRows []listedPack
		readPath  string
	}{
		{
			name:      "a JSON answer that names no loaded path",
			serves:    deleteSeamPack,
			cacheRows: []listedPack{deleteQuietPack},
			// The fake answers any path it was not told about, /api/tags included, with a
			// real listing: valid JSON carrying no model_path. That is what an engine
			// with the field renamed or dropped answers, and it is the exact place where
			// reading "no model loaded" instead of "no answer" destroys a pack a server
			// has open — this engine really does have this pack's directory mapped.
			readPath: "/api/tags",
		},
		{
			name: "a directory no cached pack sits in",
			// The engine is serving served-pack's directory, spelled so that no row in
			// the listing holds it. The listing does hold that directory spelled the way
			// the listing writes it, so a join that folded case or cleaned the path would
			// answer "a different pack is mapped" and remove this one with the engine
			// still serving. PAIR joins on exact strings: it cannot pair what is served
			// with either pack, and that is not evidence of an empty server.
			serves:    "/FIXTURE/PACKS/SERVED-PACK",
			cacheRows: []listedPack{deleteQuietPack, deleteServedPack},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
				serves:    test.serves,
				cacheRows: test.cacheRows,
				readPath:  test.readPath,
				termDrain: "300ms",
			})
			shrinkQuietBound(t, 5*time.Second)
			seam.run(t)

			_, err := seam.delete(t, deleteSeamRepoID)

			require.NoError(t, err, "delete_model")
			lines := seam.lines(t)
			assert.Contains(t, lines, "stop port=answered",
				"the engine was deleted from without being stopped, although its answer named no provably different pack: %v", lines)
			require.NotEmpty(t, lines)
			assert.Equal(t, "cmd remove "+deleteSeamRepoID+" --yes --json port=quiet", lines[len(lines)-1],
				"the removal ran while something still answered on the engine's port: %v", lines)
		})
	}
}

// TestStopBeforeAction_ReportsTheRefusalToStopAnEngineUnderExternalManagement keeps
// stop_before from inventing a second way to be refused: the engine that PAIR cannot
// take down says so in doStop's own words, and the pack stays.
func TestStopBeforeAction_ReportsTheRefusalToStopAnEngineUnderExternalManagement(t *testing.T) {
	seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
		serves:    deleteSeamPack,
		cacheRows: []listedPack{deleteQuietPack},
	})
	// Someone else's MTPLX is answering on the port, in a form PAIR adopted rather
	// than started: running, with no process of its own to terminate.
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(seam.port))
	require.NoError(t, err, "bind the port the adopted engine is answering on")
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}()
	st, err := seam.ex.state("mtplx")
	require.NoError(t, err, "engine state")
	st.mu.Lock()
	st.running, st.adopted, st.proc, st.port = true, true, nil, seam.port
	st.mu.Unlock()

	_, err = seam.delete(t, deleteSeamRepoID)

	require.Error(t, err, "the delete removed a pack out from under an engine it had not stopped")
	assert.Contains(t, err.Error(), "running under external management",
		"a delete that cannot stop the engine says so in the words Stop already uses")
	assert.NotContains(t, strings.Join(seam.lines(t), "\n"), "remove",
		"the removal ran after the stop was refused: %v", seam.lines(t))
}

// TestStopBeforeAction_HoldsOneEngineOperationAcrossTheReadStopAndRemoval pins the
// shape that makes the sequence mean anything: a Stop that arrives while a delete is
// working cannot interleave with it, because the residency read, the stop and the
// removal are one operation. Held from the test the way any operation holds it, the
// delete does not begin at all until the lock is free.
func TestStopBeforeAction_HoldsOneEngineOperationAcrossTheReadStopAndRemoval(t *testing.T) {
	seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
		serves:    deleteSeamPack,
		cacheRows: []listedPack{deleteQuietPack},
		termDrain: "300ms",
	})
	shrinkQuietBound(t, 5*time.Second)
	seam.run(t)
	st, err := seam.ex.state("mtplx")
	require.NoError(t, err, "engine state")

	st.opMu.Lock() // another lifecycle operation is in flight on this engine
	unlock := sync.OnceFunc(st.opMu.Unlock)
	t.Cleanup(unlock)
	finished := make(chan error, 1)
	go func() {
		_, err := seam.delete(t, deleteSeamRepoID)
		finished <- err
	}()

	select {
	case err := <-finished:
		unlock()
		require.NoError(t, err, "read the failure")
		require.FailNow(t, "delete_model ran while another operation held the engine")
	case <-time.After(750 * time.Millisecond):
	}
	assert.Empty(t, seam.lines(t), "the delete read what was mapped and removed the pack outside the operation it was supposed to hold")

	unlock()
	require.NoError(t, <-finished, "delete_model once the engine was free")
	assert.Equal(t, []string{
		"cmd models --json port=answered",
		"stop port=answered",
		"cmd remove " + deleteSeamRepoID + " --yes --json port=quiet",
	}, seam.lines(t), "all three ran in the one hold, in that order")
}

// TestActionValidate_StopBeforeRequiresResidencyAndRefusesRestartAfter covers the two
// declarations that cannot be obeyed. Without a residency read there is no answer to
// "is this the pack being served", so a manifest asking to stop first is asking to
// stop whatever happens to be up. And restart_after cannot follow a stop the action
// itself carries out under the engine's operation lock: the restart would take the
// same lock again, and an engine lock is not reentrant.
func TestActionValidate_StopBeforeRequiresResidencyAndRefusesRestartAfter(t *testing.T) {
	manifestWith := func(t *testing.T, deleteAction Action) *Manifest {
		t.Helper()
		return mtplxSeamManifest(t, deleteAction, deleteSeamOptions{}, 0)
	}

	t.Run("with nothing declared to ask what is mapped", func(t *testing.T) {
		manifest := manifestWith(t, stoppingFirst(t, cliDelete()))
		loaded := manifest.Actions["loaded_models"]
		loaded.Result.Residency = nil
		manifest.Actions["loaded_models"] = loaded

		err := manifest.Validate()

		require.Error(t, err, "a stop_before with no residency read cannot know which pack is in use")
		assert.Contains(t, err.Error(), "stop_before")
		assert.Contains(t, err.Error(), "residency")
	})

	t.Run("with a restart declared after it", func(t *testing.T) {
		manifest := manifestWith(t, stoppingFirst(t, cliDelete()))
		deleting := manifest.Actions["delete_model"]
		deleting.RestartAfter = true
		manifest.Actions["delete_model"] = deleting

		err := manifest.Validate()

		require.Error(t, err, "an action that stops the engine itself cannot also ask for a restart")
		assert.Contains(t, err.Error(), "stop_before")
		assert.Contains(t, err.Error(), "restart_after")
	})

	t.Run("with the stop itself declared as the action", func(t *testing.T) {
		manifest := manifestWith(t, stoppingFirst(t, cliDelete()))
		delete(manifest.Actions, "delete_model")
		manifest.Actions["unload_model"] = Action{StopEngine: true, StopBefore: true}

		err := manifest.Validate()

		require.Error(t, err, "an eject is the stop, so a stop_before in front of it would never run")
		assert.Contains(t, err.Error(), "stop_before")
		assert.Contains(t, err.Error(), "stop_engine")
	})

	t.Run("with the residency read declared and no restart", func(t *testing.T) {
		manifest := manifestWith(t, stoppingFirst(t, cliDelete()))

		assert.NoError(t, manifest.Validate(), "the declaration MTPLX's manifest makes")
	})
}

// TestManifestValidate_RefusesAResidencySourceThatDeletesOrStops closes the gap the
// join's own fence left open: the source was already barred from stopping, restarting
// or relaunching the engine it is asked about, and a source that deletes is just as
// unkind to the pack it is only meant to look up. A stop_before source is worse again,
// because reading the rows would take the engine down before the read finished, and the
// operation lock it holds is not reentrant.
func TestManifestValidate_RefusesAResidencySourceThatDeletesOrStops(t *testing.T) {
	// Every source below declares rows to read, so the rule that refuses it is the
	// fence and not the earlier "the source declares no result" check.
	rows := &ActionResult{Array: "models", Field: "repo_id"}
	cases := []struct {
		name    string
		source  Action
		mustSay []string
		reason  string
	}{
		{
			name:    "a source that removes a pack's directory",
			source:  Action{RemovePath: &ActionRemovePath{Root: "/fixture/cache", Path: "/fixture/cache/{model}"}, Result: rows},
			mustSay: []string{"result.residency.source", "remove_path"},
			reason:  "a residency lookup that deletes the pack it looked up",
		},
		{
			name:    "a source that stops the engine before reading",
			source:  Action{Cmd: []string{"{cli}", "models", "--json"}, StopBefore: true, Result: rows},
			mustSay: []string{"result.residency.source", "stop_before"},
			reason:  "a residency lookup that takes the engine down on its way to a question, on a lock that is not reentrant",
		},
		{
			// The hub-cache route is already refused, by the older rule that reserves
			// hf_cache.delete for delete_model. It is here because the fence is meant to
			// be the reason a residency source never deletes, and a reader who checks
			// that rule should see all three kinds.
			name:    "a source that deletes through the hub cache",
			source:  Action{HFCache: &ActionHFCache{Dir: "/fixture/cache", Delete: true, Tool: "hf"}, Result: rows},
			mustSay: []string{"hf_cache.delete"},
			reason:  "a residency lookup that deletes the pack it looked up",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manifest := mtplxSeamManifest(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{}, 0)
			manifest.Actions["list_models"] = test.source

			err := manifest.Validate()

			require.Error(t, err, "%s is not a lookup", test.reason)
			for _, phrase := range test.mustSay {
				assert.Contains(t, err.Error(), phrase, "the refusal is not about the declaration it is refusing")
			}
		})
	}

	t.Run("a source that only reads is still accepted", func(t *testing.T) {
		manifest := mtplxSeamManifest(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{}, 0)
		manifest.Actions["list_models"] = Action{HFCache: &ActionHFCache{Dir: "/fixture/cache"}, Result: rows}

		assert.NoError(t, manifest.Validate(), "a cache listing is what a residency source is for")
	})
}

// TestBundledMTPLXManifest_DeclaresDeleteModelStoppingFirst is the change that reaches
// the user: the shipped manifest says the pack in use is stopped before it is removed,
// keeps the argument that made the removal run by itself, and no longer tells the user
// to eject first as if that were their job.
func TestBundledMTPLXManifest_DeclaresDeleteModelStoppingFirst(t *testing.T) {
	raw, err := fs.ReadFile(bundledManifests, "manifests/mtplx.json")
	require.NoError(t, err, "read the shipped manifest")
	var manifest Manifest
	require.NoError(t, json.Unmarshal(raw, &manifest), "the shipped manifest has to parse into the same shape a test builds")
	require.NoError(t, manifest.Validate())

	deleting := manifest.Actions["delete_model"]
	assert.True(t, deleting.StopBefore, "MTPLX serves one pack per process, so deleting it can be deleting what is running")
	assert.False(t, deleting.RestartAfter, "a stopped MTPLX has nothing to serve the deleted pack, and a restart would relaunch it")
	assert.Contains(t, deleting.Cmd, "--yes", "the removal still runs without waiting for a prompt")
	assert.Contains(t, deleting.Cmd, "{model}", "the argument is still the cache id, and not the directory it resolves to")
	assert.Contains(t, manifest.Actions["loaded_models"].Result.Residency.Source, "list_models",
		"stop_before asks that join which pack is mapped")
	assert.Contains(t, deleting.Description, "stop", "the description still tells the user to eject it first, which is now what the action does")
	assert.NotContains(t, deleting.Description, "Eject it first", "the advice a user follows has to match what the action now does")
}
