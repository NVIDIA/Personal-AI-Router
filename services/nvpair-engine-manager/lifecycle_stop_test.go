// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopGrace(t *testing.T) {
	tests := []struct {
		name string
		stop *StopSpec
		want time.Duration
	}{
		{name: "default without stop spec", want: 5 * time.Second},
		{name: "default with zero grace", stop: &StopSpec{Signal: "term"}, want: 5 * time.Second},
		{name: "configured grace", stop: &StopSpec{Signal: "term", GraceS: 10}, want: 10 * time.Second},
		{name: "kill is immediate", stop: &StopSpec{Signal: "kill", GraceS: 10}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, stopGrace(Runtime{Stop: tt.stop}))
		})
	}
}

// spawnFakeListener starts a fake-engine copied to binPath, bound to
// 127.0.0.1:port, and skips the test when this host can't resolve the PID/
// image behind a listening port (no lsof/ss, or a /proc-less OS) — the
// reclaim/decline behavior can't be exercised without that resolution.
func spawnFakeListener(t *testing.T, binPath string, port int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), "OLLAMA_HOST=127.0.0.1:"+strconv.Itoa(port))
	configureSysProcAttr(cmd)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitPortServing(t, port)
	if _, image, ok := pidOnPort(port); !ok || image == "" {
		t.Skip("host cannot resolve the PID/image owning a port; skipping PID-precise stop test")
	}
	return cmd
}

// TestStopReclaimsOurOrphanOnManagedPort proves a user Stop of an adopted
// engine whose listener is our own managed binary (an orphan left on our
// port) terminates it, stays off across a health interval, and records OFF.
func TestStopReclaimsOurOrphanOnManagedPort(t *testing.T) {
	baseDir := t.TempDir()
	bin := filepath.Join(baseDir, "fake", "fakeorphan"+strconv.Itoa(os.Getpid())+exeExt())
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o755))
	copyFile(t, fakeEngineBin, bin)
	port, err := freePort()
	require.NoError(t, err)
	spawnFakeListener(t, bin, port)

	m := testEngineManifest(bin) // Detect resolves st.binPath to bin (our managed binary)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/", Status: http.StatusOK, TimeoutS: 5}
	p.Runtime.Health = &Probe{HTTP: "http://127.0.0.1:{port}/", Status: http.StatusOK, IntervalS: 1}
	m.Platforms[key] = p

	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)
	st, err := ex.Status("fake")
	require.NoError(t, err, "adopt orphan: status (%v, %v)", st, err)
	require.True(t, st.Running, "adopt orphan: status (%v, %v)", st, err)

	require.NoError(t, ex.Stop("fake"), "stop must reclaim our own orphan, got err")
	require.False(t, portServing(port), "orphan on our managed port must be terminated by stop")
	time.Sleep(1500 * time.Millisecond) // outlast a health interval
	st, err = ex.Status("fake")
	assert.NoError(t, err)
	require.False(t, st.Running, "engine must stay stopped after reclaim (%v)", st)
	require.False(t, st.Healthy, "engine must stay stopped after reclaim (%v)", st)
	enabled, known, err := ex.desired.get("fake")
	require.NoError(t, err, "desired state (%v, %v, %v)", enabled, known, err)
	require.True(t, known, "desired state (%v, %v, %v)", enabled, known, err)
	require.False(t, enabled, "desired state (%v, %v, %v)", enabled, known, err)
}

func TestStopDoesNotReclaimSameExternalImage(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "external-ollama"+strconv.Itoa(os.Getpid())+exeExt())
	copyFile(t, fakeEngineBin, bin)
	port, err := freePort()
	require.NoError(t, err)
	spawnFakeListener(t, bin, port)

	m := testEngineManifest(bin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	m.Platforms[key] = p
	ex := newTestExecutor(t, m) // managed install directory is not bin's directory
	st, err := ex.Status("fake")
	require.NoError(t, err, "adopt external image: status (%v, %v)", st, err)
	require.True(t, st.Running, "adopt external image: status (%v, %v)", st, err)
	require.ErrorContains(t, ex.Stop("fake"), "external management", "external same-image stop error")
	require.True(t, portServing(port), "external same-image listener was terminated")
}

// TestStopDeclinesForeignListenerWithActionableError proves a user Stop of an
// adopted engine whose listener is a genuinely foreign process (a different
// image) is declined with an error naming the offending PID and image, the
// foreign process is left running, and OFF intent is still recorded.
func TestStopDeclinesForeignListenerWithActionableError(t *testing.T) {
	dir := t.TempDir()
	ourBin := filepath.Join(dir, "ourengine"+exeExt())
	foreignBin := filepath.Join(dir, "foreignengine"+exeExt())
	copyFile(t, fakeEngineBin, ourBin)
	copyFile(t, fakeEngineBin, foreignBin)
	port, err := freePort()
	require.NoError(t, err)
	foreign := spawnFakeListener(t, foreignBin, port)

	m := testEngineManifest(ourBin) // Detect resolves st.binPath to ourBin, not the listener's image
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	m.Platforms[key] = p

	ex := newTestExecutor(t, m)
	st, err := ex.Status("fake")
	require.NoError(t, err, "adopt foreign listener: status (%v, %v)", st, err)
	require.True(t, st.Running, "adopt foreign listener: status (%v, %v)", st, err)

	err = ex.Stop("fake")
	require.ErrorContains(t, err, "external management", "stop error")
	require.ErrorContains(t, err, strconv.Itoa(foreign.Process.Pid), "decline error must name the offending pid")
	require.ErrorContains(t, err, filepath.Base(foreignBin), "decline error must name the offending image (%v)", foreignBin)
	require.True(t, portServing(port), "a genuinely foreign listener must be left running")
	enabled, known, err := ex.desired.get("fake")
	require.NoError(t, err, "desired state (%v, %v, %v)", enabled, known, err)
	require.True(t, known, "desired state (%v, %v, %v)", enabled, known, err)
	require.False(t, enabled, "desired state (%v, %v, %v)", enabled, known, err)
}

func TestStopRejectsAdoptedProcessEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)

	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = p
	ex := newTestExecutor(t, m)
	st, err := ex.Status("fake")
	require.NoError(t, err, "adopt external engine: status (%v, %v)", st, err)
	require.True(t, st.Running, "adopt external engine: status (%v, %v)", st, err)
	stateChanged := make(chan EngineStatus, 1)
	ex.emit = func(method string, params any) {
		if method == "engine:state-changed" {
			stateChanged <- params.(EngineStatus)
		}
	}

	require.ErrorContains(t, ex.Stop("fake"), "external management", "stop error")
	st, err = ex.Status("fake")
	assert.NoError(t, err)
	require.True(t, st.Running, "rejected stop must keep the live engine running (%v)", st)
	require.True(t, st.Healthy, "rejected stop must keep the live engine running (%v)", st)
	select {
	case st := <-stateChanged:
		require.True(t, st.Running, "rejected stop emitted false terminal state (%v)", st)
		require.True(t, st.Healthy, "rejected stop emitted false terminal state (%v)", st)
	default:
		require.FailNow(t, "rejected stop did not emit the unchanged running state")
	}
}

func TestStopReconcilesAdoptedProcessAfterExternalStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	port := portOf(t, srv.URL)
	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = p
	ex := newTestExecutor(t, m)
	st, err := ex.Status("fake")
	require.NoError(t, err, "adopt external engine: status (%v, %v)", st, err)
	require.True(t, st.Running, "adopt external engine: status (%v, %v)", st, err)

	srv.Close()
	require.NoError(t, ex.Stop("fake"), "retry after external stop")
	st, err = ex.Status("fake")
	assert.NoError(t, err)
	require.False(t, st.Running, "closed external endpoint must reconcile to stopped (%v)", st)
	require.False(t, st.Healthy, "closed external endpoint must reconcile to stopped (%v)", st)
}

func TestStopKeepsAdoptedProcessRunningWhileEndpointIsUnhealthy(t *testing.T) {
	var unhealthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if unhealthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = p
	ex := newTestExecutor(t, m)
	st, err := ex.Status("fake")
	require.NoError(t, err, "adopt external engine: status (%v, %v)", st, err)
	require.True(t, st.Running, "adopt external engine: status (%v, %v)", st, err)

	unhealthy.Store(true)
	require.ErrorContains(t, ex.Stop("fake"), "external management", "stop error")
	st, err = ex.Status("fake")
	assert.NoError(t, err)
	require.True(t, st.Running, "an unhealthy but reachable endpoint must not report stopped (%v)", st)
}

func TestCommandStopFailureKeepsLiveEngineRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "failmark", marker}, 1, false)

	require.ErrorContains(t, ex.Stop("command"), "stop command", "stop error")
	require.FileExists(t, marker, "stop command did not run")
	st, err := ex.Status("command")
	assert.NoError(t, err)
	require.True(t, st.Running, "failed stop must keep the live engine running (%v)", st)
	require.True(t, st.Healthy, "failed stop must keep the live engine running (%v)", st)
}

func TestCommandStopFailureSucceedsWhenEndpointIsConfirmedDown(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	closed := make(chan struct{})
	go func() {
		for !fileExists(marker) {
			time.Sleep(10 * time.Millisecond)
		}
		srv.Close()
		close(closed)
	}()
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "failmark", marker}, 2, false)

	require.NoError(t, ex.Stop("command"), "nonzero stop command with a closed endpoint must succeed")
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "test endpoint did not close")
	}
	st, err := ex.Status("command")
	assert.NoError(t, err)
	require.False(t, st.Running, "closed endpoint must report stopped (%v)", st)
	require.False(t, st.Healthy, "closed endpoint must report stopped (%v)", st)
}

func TestCommandStopWithoutReadinessProbeUsesConfiguredCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, "http://127.0.0.1:1", []string{fakeEngineBin, "touch", marker}, 1, false)
	st, err := ex.state("command")
	assert.NoError(t, err)
	require.NotNil(t, st)
	st.plat.Runtime.Ready = nil

	require.NoError(t, ex.Stop("command"), "stop without readiness probe")
	require.FileExists(t, marker, "configured stop command did not run")
	status, err := ex.Status("command")
	assert.NoError(t, err)
	require.False(t, status.Running, "legacy no-probe command stop must report stopped (%v)", status)
	require.False(t, status.Healthy, "legacy no-probe command stop must report stopped (%v)", status)
}

func TestCommandStopRejectsSuccessWhileEndpointIsLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "touch", marker}, 1, false)

	require.ErrorContains(t, ex.Stop("command"), "still serving", "stop error")
	st, err := ex.Status("command")
	assert.NoError(t, err)
	require.True(t, st.Running, "live endpoint must remain reported running (%v)", st)
	require.True(t, st.Healthy, "live endpoint must remain reported running (%v)", st)
}

func TestCommandStopRejectsSuccessWhileEndpointIsUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "touch", marker}, 1, false)

	require.ErrorContains(t, ex.Stop("command"), "still serving", "stop error")
	st, err := ex.Status("command")
	assert.NoError(t, err)
	require.True(t, st.Running, "an unhealthy but reachable endpoint must remain running (%v)", st)
}

func TestCommandStopFailureRestartsHealthMonitoring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "touch", marker}, 1, false)
	st, err := ex.state("command")
	assert.NoError(t, err)
	require.NotNil(t, st)
	st.plat.Runtime.Health = &Probe{HTTP: srv.URL, Status: http.StatusOK, IntervalS: 1}
	t.Cleanup(func() {
		st.mu.Lock()
		if st.healthStop != nil {
			st.healthStop()
		}
		st.mu.Unlock()
		srv.Close()
	})

	require.Error(t, ex.Stop("command"), "expected the live endpoint to reject the stop")
	srv.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if status := ex.snapshot("command", st); status.Running && !status.Healthy {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.FailNowf(t, "health monitoring did not resume after failed stop", "%+v", ex.snapshot("command", st))
}

func TestCommandStopReportsStoppedOnlyAfterEndpointCloses(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	closed := make(chan struct{})
	go func() {
		for !fileExists(marker) {
			time.Sleep(10 * time.Millisecond)
		}
		srv.Close()
		close(closed)
	}()
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "touch", marker}, 2, true)

	require.NoError(t, ex.Stop("command"), "stop")
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "test endpoint did not close")
	}
	st, err := ex.Status("command")
	assert.NoError(t, err)
	require.False(t, st.Running, "closed endpoint must report stopped (%v)", st)
	require.False(t, st.Healthy, "closed endpoint must report stopped (%v)", st)
}

func commandStopTestExecutor(t *testing.T, readyURL string, stopCmd []string, graceS int, adopted bool) *Executor {
	t.Helper()
	u, err := url.Parse(readyURL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "command", DisplayName: "Command Engine", ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect: []string{fakeEngineBin},
				Runtime: Runtime{
					Mode:  "command",
					Port:  port,
					Ready: &Probe{HTTP: readyURL, Status: http.StatusOK},
					Stop:  &StopSpec{Cmd: stopCmd, GraceS: graceS},
				},
			},
		},
	}
	ex := newTestExecutor(t, m)
	st, err := ex.state("command")
	require.NoError(t, err)
	st.mu.Lock()
	st.installed = true
	st.running = true
	st.healthy = true
	st.adopted = adopted
	st.port = port
	st.mu.Unlock()
	return ex
}
