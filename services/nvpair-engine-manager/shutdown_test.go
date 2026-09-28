// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStopAllStopsEveryEngine guards the concurrent StopAll: with more than one
// engine running, every engine must be stopped — a regression guard for the old
// sequential loop where a slow first engine could leave later engines running.
func TestStopAllStopsEveryEngine(t *testing.T) {
	reg := NewRegistry()
	names := []string{"fake-a", "fake-b"}
	for _, name := range names {
		m := testEngineManifest(fakeEngineBin)
		m.Engine = name
		reg.engines[name] = m
	}
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	ctx := context.Background()

	ports := make([]int, len(names))
	for i, name := range names {
		if err := ex.Start(ctx, name); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		st, _ := ex.Status(name)
		if !st.Running || st.Port == 0 {
			t.Fatalf("%s not running: %+v", name, st)
		}
		ports[i] = st.Port
	}

	ex.StopAll()

	for i, name := range names {
		if !waitPortClosed(ports[i], 5*time.Second) {
			t.Fatalf("engine %s still serving on port %d after StopAll", name, ports[i])
		}
	}
	if err := ex.Start(ctx, names[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("start after StopAll error = %v, want context canceled", err)
	}
}

// adoptedEngineOnLivePort seeds an engine that this process did not launch and
// cannot stop: running, adopted, no owned proc, and a real listener on its port
// so doStop's readiness probe can never confirm it went away. That is the one
// shape whose stop is refused, leaving st.running true — so a repeated sweep
// does the whole thing again instead of hitting the !st.running early return.
// Its bin path is outside the managed install dir, so the port-reclaim branch
// declines it rather than terminating a stranger's process.
func adoptedEngineOnLivePort(t *testing.T, ex *Executor) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	st, err := ex.state("fake")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	st.mu.Lock()
	st.running = true
	st.adopted = true
	st.proc = nil
	st.port = ln.Addr().(*net.TCPAddr).Port
	st.binPath = filepath.Join(t.TempDir(), "someone-elses-engine")
	st.mu.Unlock()
}

// TestStopAllSweepsOnce is the quit-latency guard. An ordinary quit asks for
// StopAll three times — the desktop's engine:prepare-shutdown, the broker's own
// teardown call, and the stdin-EOF path in Run — and every sweep used to re-pay
// doStop's readiness probe for an engine whose stop it can only decline.
// Measured against a real worker tree that had adopted an externally-managed
// Ollama, that was 1068ms + 1052ms + 1040ms, essentially the whole quit.
func TestStopAllSweepsOnce(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	adoptedEngineOnLivePort(t, ex)

	first := time.Now()
	ex.StopAll()
	sweep := time.Since(first)
	if sweep < 500*time.Millisecond {
		t.Fatalf("first sweep took %v; too fast to have probed, so this no longer covers the repeat", sweep)
	}

	// The engine is still there and still un-stoppable, so an unguarded sweep
	// would spend the probe again.
	repeat := time.Now()
	ex.StopAll()
	ex.StopAll()
	if elapsed := time.Since(repeat); elapsed > sweep/2 {
		t.Fatalf("two further StopAll calls took %v against a %v sweep; the sweep is repeating", elapsed, sweep)
	}
}

// TestStopAllWaitsForTheSweepInFlight is the other half, and the reason this is
// a joining guard rather than a plain skip. A caller that returned while the
// first sweep was still running would report engines stopped before they were:
// the broker would close stdin, engine-manager would exit, and the engines it
// launched would be left running with nothing owning them.
func TestStopAllWaitsForTheSweepInFlight(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	adoptedEngineOnLivePort(t, ex)

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		ex.StopAll()
	}()

	// Let the first caller take the Once, then join behind it.
	time.Sleep(100 * time.Millisecond)
	second := time.Now()
	ex.StopAll()
	waited := time.Since(second)

	// Both callers return when the sweep ends, but the first goroutine's deferred
	// close can land a moment later. The grace is far shorter than the probe an
	// early return would have skipped.
	select {
	case <-sweepDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("the second caller returned while the first sweep was still running")
	}
	if waited < 100*time.Millisecond {
		t.Fatalf("second caller waited only %v; it did not join the sweep in flight", waited)
	}
}

func TestStopAllCancelsCommandStartWithoutError(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "fake.pid")
	t.Setenv("FAKE_PID_FILE", pidFile)

	manifest := testEngineManifest(fakeEngineBin)
	platform := manifest.Platforms[hostKey()]
	platform.Runtime.Mode = "command"
	platform.Runtime.Bin = ""
	platform.Runtime.Start = [][]string{{fakeEngineBin}}
	platform.Runtime.Ready = nil
	platform.Runtime.Health = nil
	platform.Runtime.Stop = nil
	manifest.Platforms[hostKey()] = platform
	ex := newTestExecutor(t, manifest)

	started := make(chan error, 1)
	go func() { started <- ex.Start(context.Background(), "fake") }()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid == 0 {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatal("command-mode fake engine did not publish its PID")
	}
	t.Cleanup(func() {
		if pidAlive(pid) {
			_ = signalPID(pid, true)
		}
	})

	ex.StopAll()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("command-mode start did not return after StopAll")
	}
	if pidAlive(pid) {
		t.Fatalf("command-mode fake engine PID %d survived StopAll", pid)
	}
	if hasErr(ex.Errors(), startFailedID("fake")) {
		t.Fatalf("shutdown cancellation retained a command-mode start-failed error: %+v", ex.Errors())
	}
}

func TestStopAllStopsDetachedCommandDaemonBeforeReadiness(t *testing.T) {
	dir := t.TempDir()
	startMarker := filepath.Join(dir, "started")
	stopMarker := filepath.Join(dir, "stopped")
	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}

	manifest := testEngineManifest(fakeEngineBin)
	platform := manifest.Platforms[hostKey()]
	platform.Runtime.Mode = "command"
	platform.Runtime.Bin = ""
	platform.Runtime.Port = port
	platform.Runtime.Start = [][]string{{fakeEngineBin, "touch", startMarker}}
	platform.Runtime.Stop = &StopSpec{Cmd: []string{fakeEngineBin, "touch", stopMarker}, GraceS: 1}
	platform.Runtime.Ready = &Probe{
		HTTP:     "http://127.0.0.1:{port}/",
		Status:   http.StatusOK,
		TimeoutS: 60,
	}
	platform.Runtime.Health = nil
	manifest.Platforms[hostKey()] = platform
	ex := newTestExecutor(t, manifest)

	// Model a control CLI that launches a detached daemon and returns before
	// that daemon becomes ready. The daemon deliberately answers 503 until its
	// stop CLI runs, which keeps Start inside waitReady.
	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	t.Cleanup(func() { _ = daemon.Close() })
	daemonStarted := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for !fileExists(startMarker) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !fileExists(startMarker) {
			daemonStarted <- errors.New("start command did not run")
			return
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			daemonStarted <- err
			return
		}
		daemonStarted <- nil
		_ = daemon.Serve(ln)
	}()

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				if fileExists(stopMarker) {
					_ = daemon.Close()
					return
				}
			}
		}
	}()

	started := make(chan error, 1)
	go func() { started <- ex.Start(context.Background(), "fake") }()
	if err := <-daemonStarted; err != nil {
		t.Fatalf("detached command daemon: %v", err)
	}
	if !portServing(port) {
		t.Fatal("detached command daemon did not start serving")
	}

	ex.StopAll()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("command-mode start did not return after StopAll")
	}
	if !fileExists(stopMarker) {
		t.Fatal("StopAll skipped the command-mode stop CLI after start detached")
	}
	if !waitPortClosed(port, 5*time.Second) {
		t.Fatalf("detached command daemon remained on port %d after StopAll", port)
	}
	if hasErr(ex.Errors(), startFailedID("fake")) {
		t.Fatalf("shutdown cancellation retained a command-mode start-failed error: %+v", ex.Errors())
	}
}

// TestE2EStdinCloseStopsEngine drives the real engine-manager binary and stops
// it the way the broker now does on shutdown: by closing stdin (EOF), with no
// prepare-shutdown or shutdown RPC. The EOF path must run StopAll and take the
// engine's port down — this is the backstop the broker relies on now that it no
// longer force-kills engine-manager on a timeout.
func TestE2EStdinCloseStopsEngine(t *testing.T) {
	cfg := t.TempDir()
	home := t.TempDir()
	for _, dir := range []string{
		filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		writeFakeManifest(t, dir)
	}

	m := startE2EManager(t, cfg, home)
	send(t, m.stdin, 1, "engine:start", map[string]any{"engine": "fake"})
	var started EngineStatus
	if err := json.Unmarshal(waitResult(t, m.frames, "1", 20*time.Second), &started); err != nil || !started.Running {
		t.Fatalf("start status=%+v err=%v", started, err)
	}

	// stop() closes stdin and waits for the process to exit on its own.
	m.stop(t)

	if !waitPortClosed(started.Port, 5*time.Second) {
		t.Fatalf("engine port %d remained open after stdin close", started.Port)
	}
}

// waitPortClosed polls until nothing is serving on the port or the timeout
// elapses. The process is gone by the time StopAll returns, but the listener
// socket can take a beat to be reclaimed by the OS.
func waitPortClosed(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !portServing(port) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !portServing(port)
}
