// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mtplxTestManifest describes the parts of MTPLX's real manifest these tests
// depend on: a default port, a /health readiness probe, and a settings file that
// may name the port the engine really listens on. The binary is a path that does
// not exist, so a second copy launched beside a running server would fail with
// "start mtplx" rather than silently passing.
func mtplxTestManifest(t *testing.T, port int, source *PortSource) *Manifest {
	t.Helper()
	key := runtime.GOOS + "/" + runtime.GOARCH
	missing := filepath.Join(t.TempDir(), "mtplx"+exeExt())
	return &Manifest{
		Engine: "mtplx", DisplayName: "MTPLX", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Detect: []string{missing},
			Runtime: Runtime{
				Bin:        missing,
				Port:       port,
				PortSource: source,
				Ready:      &Probe{HTTP: "http://127.0.0.1:{port}/health", Status: 200, TimeoutS: 5},
			},
		}},
	}
}

// mtplxLaunchManifest is mtplxTestManifest with a binary that can actually run:
// the package's fake engine, which answers 200 on any path. A test that has to
// show whether PAIR would bring up a second copy needs a second copy that could
// really come up, so it cannot use a manifest whose binary does not exist.
func mtplxLaunchManifest(t *testing.T, port int, source *PortSource) *Manifest {
	t.Helper()
	key := runtime.GOOS + "/" + runtime.GOARCH
	return &Manifest{
		Engine: "mtplx", DisplayName: "MTPLX", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Detect: []string{fakeEngineBin},
			Runtime: Runtime{
				Bin:        fakeEngineBin,
				Env:        map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}"},
				Port:       port,
				PortSource: source,
				Ready:      &Probe{HTTP: "http://127.0.0.1:{port}/health", Status: 200, TimeoutS: 10},
				Stop:       &StopSpec{Signal: "term", GraceS: 3},
			},
		}},
	}
}

// writePortSettings puts a settings body where a manifest's port_source can find
// it and returns the path.
func writePortSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// keyGatedServer stands in for an engine started with an API key: every route,
// its own health endpoint included, answers a request that carries no
// credentials with the given status.
func keyGatedServer(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(status)
	}))
}

// answerWith replaces a server's behaviour, standing in for the user changing
// their engine's settings and restarting it.
func answerWith(srv *httptest.Server, status int) {
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
}

// noticeFor returns the entry the error feed holds for id, if any.
func noticeFor(errors []serviceError, id string) (serviceError, bool) {
	for _, e := range errors {
		if e.ID == id {
			return e, true
		}
	}
	return serviceError{}, false
}

// lockedBuffer records the codec frames a test produces, so it can count them
// while a status poll may still be writing.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Read([]byte) (int, error) { return 0, io.EOF }

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// framesFor counts wire frames of one method that name an error id. The feed
// keeps one entry per id, so a repeated notice is only visible on the wire.
func framesFor(t *testing.T, buf *lockedBuffer, method, id string) int {
	t.Helper()
	frames := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, `"`+method+`"`) && strings.Contains(line, id) {
			frames++
		}
	}
	return frames
}

// An engine the user started themselves answers on whatever port their own app
// chose, so the port PAIR configured for that engine may be empty while the real
// one sits beside it. A manifest's port_source names where to look.
func TestCandidatePorts_AddsThePortTheEngineKeepsInItsOwnSettingsFile(t *testing.T) {
	const watched = 8101

	tests := []struct {
		name string
		// settings is the contents of the settings file; empty stands for no
		// file at the declared path at all.
		settings string
		field    string
		want     []int
	}{
		{
			name:     "names the port the user picked",
			settings: `{"host":"127.0.0.1","port":6666}`,
			want:     []int{watched, 6666},
		},
		{
			// Launchers have written the same value both ways and the engine
			// reading it accepts both, so neither may be a reason to miss it.
			name:     "accepts a port written as text",
			settings: `{"port":"6666"}`,
			want:     []int{watched, 6666},
		},
		{
			name:     "follows a dotted object path",
			settings: `{"server":{"port":6666}}`,
			field:    "server.port",
			want:     []int{watched, 6666},
		},
		{
			name:     "skips the port already being watched",
			settings: fmt.Sprintf(`{"port":%d}`, watched),
			want:     []int{watched},
		},
		{
			name: "says nothing without a settings file",
			want: []int{watched},
		},
		{
			name:     "says nothing about a file it cannot read",
			settings: `{"port":`,
			want:     []int{watched},
		},
		{
			name:     "says nothing about a port that cannot exist",
			settings: `{"port":70000}`,
			want:     []int{watched},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "missing.json")
			if tc.settings != "" {
				file = writePortSettings(t, tc.settings)
			}
			ex := newTestExecutor(t, mtplxTestManifest(t, watched, &PortSource{File: file, Field: tc.field}))
			st, err := ex.state("mtplx")
			require.NoError(t, err)

			assert.Equal(t, tc.want, ex.candidatePorts(st, watched).anchors)
		})
	}
}

// Every other engine serves on the port PAIR configures. Nothing is asked about
// any other port, so the candidate list stays what it was before any of this.
func TestCandidatePorts_WithoutAPortSource_JustWatchesTheConfiguredPort(t *testing.T) {
	const watched = 8101
	ex := newTestExecutor(t, mtplxTestManifest(t, watched, nil))
	st, err := ex.state("mtplx")
	require.NoError(t, err)

	assert.Equal(t, []int{watched}, ex.candidatePorts(st, watched).anchors)
}

// A status check has to ask about every candidate port, or an engine running
// right now is reported as absent because it happens to run on 6666 instead of
// PAIR's default.
func TestStatus_FindsTheEngineOnThePortFromItsOwnSettingsFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	servedPort := portOf(t, srv.URL)

	configured, err := freePort()
	require.NoError(t, err)
	require.NotEqual(t, configured, servedPort, "the test needs an empty configured port")

	source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"host":"127.0.0.1","port": %d}`, servedPort))}
	ex := newTestExecutor(t, mtplxTestManifest(t, configured, source))

	st, err := ex.Status("mtplx")

	require.NoError(t, err)
	assert.True(t, st.Installed, "a running MTPLX on its own port reported as not installed (%v)", st)
	assert.True(t, st.Running, "a running MTPLX on its own port reported as offline (%v)", st)
	assert.True(t, st.Healthy, "a running MTPLX on its own port reported as unhealthy (%v)", st)
	assert.Equal(t, servedPort, st.Port, "PAIR must report where the engine really answers")
}

// Finding the engine is only half of it: Start must join the copy that is
// already running. A second MTPLX beside the user's own would fail to bind its
// port anyway, and would leave two routers for one model.
func TestStart_AdoptsTheEngineOnItsOwnPort_WithoutLaunchingASecondCopy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	servedPort := portOf(t, srv.URL)

	configured, err := freePort()
	require.NoError(t, err)
	require.NotEqual(t, configured, servedPort, "the test needs an empty configured port")

	source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"port": %d}`, servedPort))}
	ex := newTestExecutor(t, mtplxTestManifest(t, configured, source))

	require.NoError(t, ex.Start(t.Context(), "mtplx"), "Start refused although the engine is answering")

	state, err := ex.state("mtplx")
	require.NoError(t, err)
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.True(t, state.adopted, "Start did not adopt the running engine")
	assert.Nil(t, state.proc, "Start launched a second MTPLX beside the running one")
	assert.Equal(t, servedPort, state.port, "the adopted engine is tracked at the wrong port")
}

// Once the user quits their own engine, its port was never PAIR's to keep: the
// next start goes back to the port PAIR configured for this engine rather than
// relaunching on a stranger's.
func TestStatus_DroppedExternalService_GivesUpTheStrangersPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	servedPort := portOf(t, srv.URL)

	configured, err := freePort()
	require.NoError(t, err)
	require.NotEqual(t, configured, servedPort, "the test needs an empty configured port")

	source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"port": %d}`, servedPort))}
	ex := newTestExecutor(t, mtplxTestManifest(t, configured, source))
	st, err := ex.Status("mtplx")
	require.NoError(t, err)
	require.True(t, st.Running, "the external engine was never adopted (%v)", st)

	srv.Close()
	st, err = ex.Status("mtplx")

	require.NoError(t, err)
	assert.False(t, st.Running, "an engine that exited is still shown running (%v)", st)
	assert.False(t, st.Healthy, "an engine that exited is still shown healthy (%v)", st)
	assert.Equal(t, configured, st.Port, "PAIR kept holding the port of a service that is gone (%v)", st)
}

// A port that refuses PAIR's health check for credentials is this engine, set up
// in a way PAIR cannot work with: it is not offline, and it is not somebody
// else's service. The engine stays shown offline, with one standing notice that
// says why, for as long as the port behaves that way.
func TestStatus_KeyProtectedPort_StaysOfflineWithOneStandingNotice(t *testing.T) {
	srv := keyGatedServer(http.StatusUnauthorized)
	defer srv.Close()
	gatedPort := portOf(t, srv.URL)

	configured, err := freePort()
	require.NoError(t, err)
	require.NotEqual(t, configured, gatedPort, "the test needs an empty configured port")

	// The codec sink lets the test see each errors:report frame, which is the
	// only place a repeated notice would show up.
	var frames lockedBuffer
	source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"port": %d}`, gatedPort))}
	manifest := mtplxTestManifest(t, configured, source)
	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	ex := NewExecutor(reg, NewReporter(NewCodec(&frames)), func(string, any) {}, t.TempDir())
	// Only an engine the user turned on deserves a notice about it.
	require.NoError(t, ex.desired.set("mtplx", true))

	st, err := ex.Status("mtplx")

	require.NoError(t, err)
	assert.False(t, st.Running, "a key-gated port must not be reported as a working engine (%v)", st)
	assert.False(t, st.Healthy, "a key-gated port must not be reported as healthy (%v)", st)

	notice, listed := noticeFor(ex.Errors(), "engine-manager:auth-blocked:mtplx")
	require.True(t, listed, "a key-gated port reported no notice")
	assert.Contains(t, notice.Message, fmt.Sprintf("port %d", gatedPort), "the notice named the wrong port")
	assert.Contains(t, notice.Message, "HTTP 401")
	assert.Contains(t, notice.Message, "requires an API key")
	assert.Contains(t, notice.Message, "never sends an API key")
	assert.Equal(t, "warning", notice.Severity)
	assert.Equal(t, "none", notice.Action, "there is nothing for the user to retry")
	assert.Equal(t, "mtplx", notice.EngineType)

	// The next poll sees the same refusal. It must not re-report it: the feed
	// keeps one entry per id and a refresh re-raises a dialog already dismissed.
	_, err = ex.Status("mtplx")
	require.NoError(t, err)
	require.Equal(t, 1, framesFor(t, &frames, "errors:report", "engine-manager:auth-blocked:mtplx"),
		"every poll re-reported the standing notice")

	// The user removes the key: the same port now answers, so the engine is
	// adopted and the notice goes away with the condition that raised it.
	answerWith(srv, http.StatusOK)
	st, err = ex.Status("mtplx")

	require.NoError(t, err)
	assert.True(t, st.Running, "the engine started answering and is still shown offline (%v)", st)
	assert.Equal(t, gatedPort, st.Port)
	_, stillListed := noticeFor(ex.Errors(), "engine-manager:auth-blocked:mtplx")
	assert.False(t, stillListed, "a port that no longer refuses still holds its notice")
	assert.Contains(t, frames.String(), `"errors:clear"`, "the cleared notice was never taken off the feed")
}

// Starting over a credential refusal has to say so, and name the port the
// refusal was found on. Telling the user to look elsewhere for a service that
// "did not identify as MTPLX" sends them hunting for a different engine while
// the right one is already running.
func TestStart_KeyProtectedPort_RefusesWithoutLaunchingASecondCopy(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantReason string
	}{
		{
			name:       "a port that requires an API key",
			status:     http.StatusUnauthorized,
			wantReason: "it requires an API key",
		},
		{
			name:       "a port that refuses requests carrying no credentials",
			status:     http.StatusForbidden,
			wantReason: "it refuses requests that carry no credentials",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := keyGatedServer(tc.status)
			defer srv.Close()
			gatedPort := portOf(t, srv.URL)

			configured, err := freePort()
			require.NoError(t, err)
			require.NotEqual(t, configured, gatedPort, "the test needs an empty configured port")

			source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"port": %d}`, gatedPort))}
			ex := newTestExecutor(t, mtplxTestManifest(t, configured, source))

			err = ex.Start(t.Context(), "mtplx")

			require.Error(t, err, "Start accepted a port that refuses PAIR for credentials")
			require.ErrorContains(t, err, "cannot start engine")
			require.ErrorContains(t, err, fmt.Sprintf("port %d", gatedPort), "the refusal named the wrong port")
			require.ErrorContains(t, err, tc.wantReason)
			require.ErrorContains(t, err, "never sends an API key")
			require.NotContains(t, err.Error(), "did not identify as",
				"a credential refusal is this engine, not somebody else's service")
			require.NotContains(t, err.Error(), "start mtplx",
				"Start launched a second copy beside the refusing service")

			state, err := ex.state("mtplx")
			require.NoError(t, err)
			state.mu.Lock()
			defer state.mu.Unlock()
			assert.Nil(t, state.proc, "Start launched a second copy beside the refusing service")
			assert.False(t, state.running, "a refused start left the engine marked running")
		})
	}
}

// A service that answers on the port named in the engine's settings file, but
// does not answer as that engine, is somebody else's listener on a port PAIR
// would never bind: an MTPLX launched from PAIR takes the port PAIR gives it.
// Refusing the start over it left the user with no engine at all because of a
// service they never asked PAIR about, so the start must go ahead on the empty
// configured port and stay there.
func TestStart_ForeignServiceOnTheSearchPort_StartsOnTheConfiguredPort(t *testing.T) {
	foreign := httptest.NewServer(http.NotFoundHandler())
	defer foreign.Close()
	foreignPort := portOf(t, foreign.URL)

	configured, err := freePort()
	require.NoError(t, err)
	require.NotEqual(t, configured, foreignPort, "the test needs an empty configured port")

	source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"port": %d}`, foreignPort))}
	ex := newTestExecutor(t, mtplxLaunchManifest(t, configured, source))
	t.Cleanup(func() { _ = ex.Stop("mtplx") })

	require.NoError(t, ex.Start(t.Context(), "mtplx"),
		"Start refused although the port it would bind was empty")

	st, err := ex.Status("mtplx")
	require.NoError(t, err)
	assert.True(t, st.Running, "no engine came up on the configured port (%v)", st)
	assert.Equal(t, configured, st.Port, "the engine PAIR started is tracked at the stranger's port")

	state, err := ex.state("mtplx")
	require.NoError(t, err)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.NotNil(t, state.proc, "Start launched no engine")
	assert.False(t, state.adopted, "PAIR adopted a service that never answered as this engine")
}

// Uninstall deletes files, so it still declines while a service PAIR cannot
// identify is answering on a port this engine uses. That refusal has to name
// where the service really is: naming PAIR's own configured port, which is
// empty, sends the user looking for a listener that is not there.
func TestUninstall_ForeignServiceOnTheSearchPort_NamesThePortHoldingTheService(t *testing.T) {
	baseDir := t.TempDir()
	managedBin := filepath.Join(baseDir, "mtplx", "mtplx"+exeExt())
	require.NoError(t, os.MkdirAll(filepath.Dir(managedBin), 0o755))
	copyFile(t, fakeEngineBin, managedBin)

	foreign := httptest.NewServer(http.NotFoundHandler())
	defer foreign.Close()
	foreignPort := portOf(t, foreign.URL)

	configured, err := freePort()
	require.NoError(t, err)
	require.NotEqual(t, configured, foreignPort, "the test needs an empty configured port")

	source := &PortSource{File: writePortSettings(t, fmt.Sprintf(`{"port": %d}`, foreignPort))}
	manifest := mtplxLaunchManifest(t, configured, source)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := manifest.Platforms[key]
	// The managed copy is what Uninstall would delete, and it is inside PAIR's
	// managed install directory, so the only thing refusing the job is the port.
	p.Detect = []string{managedBin}
	p.Runtime.Bin = managedBin
	p.Uninstall = &Uninstall{Run: rmCmd(managedBin)}
	manifest.Platforms[key] = p

	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)

	err = ex.Uninstall(t.Context(), "mtplx")

	require.ErrorContains(t, err, fmt.Sprintf("port %d is occupied by an unidentified service", foreignPort),
		"the refusal did not name the port the service answers on")
	require.NotContains(t, err.Error(), fmt.Sprintf("port %d is occupied", configured),
		"the refusal named PAIR's configured port, which is empty")
	require.True(t, portServing(foreignPort), "uninstall stopped a service PAIR does not own")
	require.FileExists(t, managedBin, "uninstall deleted managed files while a service was answering")
}
