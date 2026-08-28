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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// spawnFakeListener starts a fake-engine copied to binPath, bound to
// 127.0.0.1:port, and skips the test when this host can't resolve the PID/
// image behind a listening port (no lsof/ss, or a /proc-less OS) — the
// reclaim/decline behavior can't be exercised without that resolution.
func spawnFakeListener(t *testing.T, binPath string, port int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), "OLLAMA_HOST=127.0.0.1:"+strconv.Itoa(port))
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, fakeEngineBin, bin)
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
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
	if st, err := ex.Status("fake"); err != nil || !st.Running {
		t.Fatalf("adopt orphan: status=%+v err=%v", st, err)
	}

	if err := ex.Stop("fake"); err != nil {
		t.Fatalf("stop must reclaim our own orphan, got err=%v", err)
	}
	if portServing(port) {
		t.Fatal("orphan on our managed port must be terminated by stop")
	}
	time.Sleep(1500 * time.Millisecond) // outlast a health interval
	if st, _ := ex.Status("fake"); st.Running || st.Healthy {
		t.Fatalf("engine must stay stopped after reclaim, got %+v", st)
	}
	if enabled, known, err := ex.desired.get("fake"); err != nil || !known || enabled {
		t.Fatalf("desired state = (%v, %v, %v), want known OFF", enabled, known, err)
	}
}

func TestStopDoesNotReclaimSameExternalImage(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "external-ollama"+strconv.Itoa(os.Getpid())+exeExt())
	copyFile(t, fakeEngineBin, bin)
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	spawnFakeListener(t, bin, port)

	m := testEngineManifest(bin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	m.Platforms[key] = p
	ex := newTestExecutor(t, m) // managed install directory is not bin's directory
	if st, err := ex.Status("fake"); err != nil || !st.Running {
		t.Fatalf("adopt external image: status=%+v err=%v", st, err)
	}
	if err := ex.Stop("fake"); err == nil || !strings.Contains(err.Error(), "external management") {
		t.Fatalf("external same-image stop error = %v", err)
	}
	if !portServing(port) {
		t.Fatal("external same-image listener was terminated")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	foreign := spawnFakeListener(t, foreignBin, port)

	m := testEngineManifest(ourBin) // Detect resolves st.binPath to ourBin, not the listener's image
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	m.Platforms[key] = p

	ex := newTestExecutor(t, m)
	if st, err := ex.Status("fake"); err != nil || !st.Running {
		t.Fatalf("adopt foreign listener: status=%+v err=%v", st, err)
	}

	err = ex.Stop("fake")
	if err == nil || !strings.Contains(err.Error(), "external management") {
		t.Fatalf("stop error = %v, want external-management decline", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(foreign.Process.Pid)) {
		t.Fatalf("decline error must name the offending pid %d: %v", foreign.Process.Pid, err)
	}
	if !strings.Contains(err.Error(), filepath.Base(foreignBin)) {
		t.Fatalf("decline error must name the offending image %q: %v", foreignBin, err)
	}
	if !portServing(port) {
		t.Fatal("a genuinely foreign listener must be left running")
	}
	if enabled, known, err := ex.desired.get("fake"); err != nil || !known || enabled {
		t.Fatalf("desired state = (%v, %v, %v), want known OFF after a declined stop", enabled, known, err)
	}
}

func TestStopRejectsAdoptedProcessEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = p
	ex := newTestExecutor(t, m)
	if st, err := ex.Status("fake"); err != nil || !st.Running {
		t.Fatalf("adopt external engine: status=%+v err=%v", st, err)
	}
	stateChanged := make(chan EngineStatus, 1)
	ex.emit = func(method string, params any) {
		if method == "engine:state-changed" {
			stateChanged <- params.(EngineStatus)
		}
	}

	err := ex.Stop("fake")
	if err == nil || !strings.Contains(err.Error(), "external management") {
		t.Fatalf("stop error = %v, want actionable external-management error", err)
	}
	if st, _ := ex.Status("fake"); !st.Running || !st.Healthy {
		t.Fatalf("rejected stop must keep the live engine running, got %+v", st)
	}
	select {
	case st := <-stateChanged:
		if !st.Running || !st.Healthy {
			t.Fatalf("rejected stop emitted false terminal state: %+v", st)
		}
	default:
		t.Fatal("rejected stop did not emit the unchanged running state")
	}
}

func TestStopReconcilesAdoptedProcessAfterExternalStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = p
	ex := newTestExecutor(t, m)
	if st, err := ex.Status("fake"); err != nil || !st.Running {
		t.Fatalf("adopt external engine: status=%+v err=%v", st, err)
	}

	srv.Close()
	if err := ex.Stop("fake"); err != nil {
		t.Fatalf("retry after external stop: %v", err)
	}
	if st, _ := ex.Status("fake"); st.Running || st.Healthy {
		t.Fatalf("closed external endpoint must reconcile to stopped, got %+v", st)
	}
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
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = p
	ex := newTestExecutor(t, m)
	if st, err := ex.Status("fake"); err != nil || !st.Running {
		t.Fatalf("adopt external engine: status=%+v err=%v", st, err)
	}

	unhealthy.Store(true)
	if err := ex.Stop("fake"); err == nil || !strings.Contains(err.Error(), "external management") {
		t.Fatalf("stop error = %v, want live external-management error", err)
	}
	if st, _ := ex.Status("fake"); !st.Running {
		t.Fatalf("an unhealthy but reachable endpoint must not report stopped, got %+v", st)
	}
}

func TestCommandStopFailureKeepsLiveEngineRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "failmark", marker}, 1, false)

	err := ex.Stop("command")
	if err == nil || !strings.Contains(err.Error(), "stop command") {
		t.Fatalf("stop error = %v, want command failure", err)
	}
	if !fileExists(marker) {
		t.Fatal("stop command did not run")
	}
	if st, _ := ex.Status("command"); !st.Running || !st.Healthy {
		t.Fatalf("failed stop must keep the live engine running, got %+v", st)
	}
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

	if err := ex.Stop("command"); err != nil {
		t.Fatalf("nonzero stop command with a closed endpoint must succeed: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("test endpoint did not close")
	}
	if st, _ := ex.Status("command"); st.Running || st.Healthy {
		t.Fatalf("closed endpoint must report stopped, got %+v", st)
	}
}

func TestCommandStopWithoutReadinessProbeUsesConfiguredCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, "http://127.0.0.1:1", []string{fakeEngineBin, "touch", marker}, 1, false)
	st, _ := ex.state("command")
	st.plat.Runtime.Ready = nil

	if err := ex.Stop("command"); err != nil {
		t.Fatalf("stop without readiness probe: %v", err)
	}
	if !fileExists(marker) {
		t.Fatal("configured stop command did not run")
	}
	if status, _ := ex.Status("command"); status.Running || status.Healthy {
		t.Fatalf("legacy no-probe command stop must report stopped, got %+v", status)
	}
}

func TestCommandStopRejectsSuccessWhileEndpointIsLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "touch", marker}, 1, false)

	err := ex.Stop("command")
	if err == nil || !strings.Contains(err.Error(), "still serving") {
		t.Fatalf("stop error = %v, want endpoint-still-live error", err)
	}
	if st, _ := ex.Status("command"); !st.Running || !st.Healthy {
		t.Fatalf("live endpoint must remain reported running, got %+v", st)
	}
}

func TestCommandStopRejectsSuccessWhileEndpointIsUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "touch", marker}, 1, false)

	err := ex.Stop("command")
	if err == nil || !strings.Contains(err.Error(), "still serving") {
		t.Fatalf("stop error = %v, want endpoint-still-live error", err)
	}
	if st, _ := ex.Status("command"); !st.Running {
		t.Fatalf("an unhealthy but reachable endpoint must remain running, got %+v", st)
	}
}

func TestCommandStopFailureRestartsHealthMonitoring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	ex := commandStopTestExecutor(t, srv.URL, []string{fakeEngineBin, "touch", marker}, 1, false)
	st, _ := ex.state("command")
	st.plat.Runtime.Health = &Probe{HTTP: srv.URL, Status: http.StatusOK, IntervalS: 1}
	t.Cleanup(func() {
		st.mu.Lock()
		if st.healthStop != nil {
			st.healthStop()
		}
		st.mu.Unlock()
		srv.Close()
	})

	if err := ex.Stop("command"); err == nil {
		t.Fatal("expected the live endpoint to reject the stop")
	}
	srv.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if status := ex.snapshot("command", st); status.Running && !status.Healthy {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("health monitoring did not resume after failed stop: %+v", ex.snapshot("command", st))
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

	if err := ex.Stop("command"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("test endpoint did not close")
	}
	if st, _ := ex.Status("command"); st.Running || st.Healthy {
		t.Fatalf("closed endpoint must report stopped, got %+v", st)
	}
}

func commandStopTestExecutor(t *testing.T, readyURL string, stopCmd []string, graceS int, adopted bool) *Executor {
	t.Helper()
	u, err := url.Parse(readyURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.installed = true
	st.running = true
	st.healthy = true
	st.adopted = adopted
	st.port = port
	st.mu.Unlock()
	return ex
}
