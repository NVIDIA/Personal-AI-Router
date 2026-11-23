// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestStopAllStopsEveryEngine guards the concurrent StopAll: with more than one
// engine running, every engine must be stopped — a regression guard for the old
// sequential loop where a slow first engine could leave later engines running.
func TestStopAll_StopsEveryEngine(t *testing.T) {
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
		require.NoError(t, ex.Start(ctx, name), "start (%v)", name)
		st, err := ex.Status(name)
		require.NoError(t, err)
		require.True(t, st.Running, " (%v, %v)", name, st)
		require.NotEqual(t, 0, st.Port, " (%v, %v)", name, st)
		ports[i] = st.Port
	}

	ex.StopAll()

	require.True(t, waitPortClosed(ports[0], 5*time.Second), "engine (%v)", names[0])
	require.True(t, waitPortClosed(ports[1], 5*time.Second), "engine (%v)", names[1])
	require.ErrorIs(t, ex.Start(ctx, names[0]), context.Canceled, "start after StopAll error")
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
	require.NoError(t, err, "listen")
	t.Cleanup(func() { _ = ln.Close() })

	st, err := ex.state("fake")
	require.NoError(t, err)
	st.mu.Lock()
	st.running = true
	st.adopted = true
	st.proc = nil
	st.port = ln.Addr().(*net.TCPAddr).Port
	st.binPath = filepath.Join(t.TempDir(), "someone-elses-engine")
	st.mu.Unlock()
}

// TestStopAll_RunsOneSweepPerProcess is the quit-latency guard. An ordinary quit
// asks for StopAll twice — the broker's engine:prepare-shutdown during its
// teardown, and the stdin-EOF path in Run once the broker closes stdin. An
// engine whose stop can only be declined stays running, so each repeat sweep
// would re-pay doStop's readiness probe for it.
func TestStopAll_RunsOneSweepPerProcess(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	adoptedEngineOnLivePort(t, ex)

	first := time.Now()
	ex.StopAll()
	sweep := time.Since(first)
	require.GreaterOrEqual(t, sweep, 500*time.Millisecond,
		"the first sweep was too fast to have probed, so this no longer covers the repeat")

	// The engine is still there and still un-stoppable, so an unguarded sweep
	// would spend the probe again.
	repeat := time.Now()
	ex.StopAll()
	ex.StopAll()
	assert.LessOrEqual(t, time.Since(repeat), sweep/2, "two further StopAll calls must not repeat the sweep")
}

// TestStopAll_WaitsForTheSweepInFlight is the other half, and the reason this
// is a joining guard rather than a plain skip. A caller that returned while the
// first sweep was still running would report engines stopped before they were:
// the broker would close stdin, engine-manager would exit, and the engines it
// launched would be left running with nothing owning them.
func TestStopAll_WaitsForTheSweepInFlight(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	adoptedEngineOnLivePort(t, ex)
	st, err := ex.state("fake")
	require.NoError(t, err)

	// Holding the engine's opMu parks the sweep before doStop, so it cannot
	// finish until the test lets it, however the goroutines are scheduled.
	st.opMu.Lock()
	releaseSweep := sync.OnceFunc(st.opMu.Unlock)
	t.Cleanup(releaseSweep)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		ex.StopAll()
	}()

	// Join behind the first caller only once it owns the sweep: only the sweep
	// sets shuttingDown, and it does so before stopping anything.
	require.Eventually(t, ex.shuttingDown.Load, 5*time.Second, 5*time.Millisecond,
		"the first StopAll caller never started the sweep")
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		ex.StopAll()
	}()

	require.Never(t, func() bool { return isClosed(secondDone) }, 200*time.Millisecond, 10*time.Millisecond,
		"the second caller returned while the first sweep was still running")

	releaseSweep()
	require.Eventually(t, func() bool { return isClosed(sweepDone) }, 10*time.Second, 10*time.Millisecond,
		"the first StopAll caller never returned once the sweep was released")
	require.Eventually(t, func() bool { return isClosed(secondDone) }, 10*time.Second, 10*time.Millisecond,
		"the second StopAll caller never returned once the sweep was released")
}

// isClosed reports whether done has been closed, without blocking.
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// A download's tool is no engine, so stopping the engines would leave it
// running past engine-manager's exit, above all one a peer started, whose
// request nothing else cancels; these run on the test's context, which nothing
// cancels either. StopAll stops every download, one queued behind another
// included, and returns only once each has stopped. Shutdown is no cancel
// someone asked for, which alone may delete partial files, and each download
// reports that shutdown stopped it.
func TestStopAll_StopsAndWaitsForEveryDownload(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	type stop struct {
		requested bool
		cause     error
	}
	downloading := make(chan struct{})
	stopped := make(chan stop, 1)
	first := make(chan error, 1)
	go func() {
		_, err := ex.trackedPull(t.Context(), "fake", "first", func(ctx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-ctx.Done()
			// Its tool takes a moment to exit.
			time.Sleep(100 * time.Millisecond)
			stopped <- stop{requested: cancelRequested(ctx), cause: context.Cause(ctx)}
			return nil, pullStopped(ctx, "first")
		})
		first <- err
	}()
	<-downloading
	queued := make(chan error, 1)
	go func() {
		_, err := ex.trackedPull(t.Context(), "fake", "queued", func(context.Context) (json.RawMessage, error) {
			t.Error("a download queued behind another started once shutdown had begun")
			return nil, nil
		})
		queued <- err
	}()
	require.Eventually(t, func() bool { return ex.registeredDownload("fake", "queued") != 0 }, 5*time.Second, 5*time.Millisecond,
		"the second download never queued")

	ex.StopAll()

	select {
	case got := <-stopped:
		assert.False(t, got.requested, "shutdown was taken for a cancel someone asked for, which may delete partial files")
		assert.ErrorIs(t, got.cause, errShuttingDown)
	default:
		require.FailNow(t, "StopAll returned before the download had stopped")
	}
	assert.Zero(t, ex.registeredDownload("fake", "queued"), "StopAll returned before the queued download had stopped")
	assert.ErrorIs(t, <-first, errShuttingDown)
	assert.ErrorIs(t, <-queued, errShuttingDown)
}

// A cancel can land while a download shutdown stopped is still stopping. The
// download stopped, as was asked, so whichever engine ran it, it settles as
// the cancellation rather than failing as shutdown's, and so does the cancel.
func TestStopAll_SettlesACancelThatLandsAfterItAsCancelled(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	stopping := make(chan struct{})
	cancelled := make(chan struct{})
	done := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(t.Context(), "fake", "demo", func(ctx context.Context) (json.RawMessage, error) {
			<-ctx.Done()
			close(stopping)
			<-cancelled
			return nil, pullStopped(ctx, "demo")
		})
		done <- got
	}()
	require.Eventually(t, func() bool { return ex.registeredDownload("fake", "demo") != 0 }, 5*time.Second, 5*time.Millisecond,
		"the download never registered")
	stoppedAll := make(chan struct{})
	go func() {
		defer close(stoppedAll)
		ex.StopAll()
	}()
	<-stopping
	wait := ex.latchPullCancel("fake", "demo")
	close(cancelled)

	require.NoError(t, wait(t.Context()), "the cancel should report the download stopped")
	got := <-done
	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
	<-stoppedAll
}

// setDownloadShutdownBudget changes how long StopAll waits for the downloads
// it stopped, for the rest of the test.
func setDownloadShutdownBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	previous := downloadShutdownBudget
	downloadShutdownBudget = budget
	t.Cleanup(func() { downloadShutdownBudget = previous })
}

// The broker gives the whole shutdown sweep a few seconds, so StopAll waits for
// a download no longer than downloadShutdownBudget, and one still stopping then
// is left to engine-manager's exit rather than holding up the sweep's reply. A
// later StopAll, which retries only failed engine stops, does not wait for it
// again.
func TestStopAll_StopsWaitingForADownloadAfterItsBudget(t *testing.T) {
	setDownloadShutdownBudget(t, 100*time.Millisecond)
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	downloading := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		_, _ = ex.trackedPull(t.Context(), "fake", "demo", func(context.Context) (json.RawMessage, error) {
			close(downloading)
			<-release
			return nil, context.Canceled
		})
	}()
	<-downloading
	stopAll := func(t *testing.T, failure string) {
		t.Helper()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			ex.StopAll()
		}()
		select {
		case <-returned:
		case <-time.After(10 * time.Second):
			require.FailNow(t, failure)
		}
	}

	stopAll(t, "StopAll waited for a download past its budget")
	assert.NotZero(t, ex.registeredDownload("fake", "demo"), "the download should still be stopping")
	setDownloadShutdownBudget(t, time.Hour)
	stopAll(t, "a later StopAll waited for the download again")
}

// commandDaemon stands in for a command-mode engine's daemon: it serves the
// readiness endpoint once startMarker exists and closes its listener once
// stopMarker does, as a daemon told to stop by its control CLI would. It
// returns the daemon's port.
func commandDaemon(t *testing.T, startMarker, stopMarker string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fileExists(startMarker) && !fileExists(stopMarker) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	stopWatch := make(chan struct{})
	t.Cleanup(func() { close(stopWatch) })
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(50 * time.Millisecond):
				if fileExists(stopMarker) {
					srv.Close()
					return
				}
			}
		}
	}()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse daemon URL")
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "parse daemon port")
	return port
}

// TestStopAll_RetriesAFailedStopCommand checks that a later StopAll caller
// retries an engine whose stop command failed during the sweep, so a stop
// command that errors once does not leave the engine running after quit.
func TestStopAll_RetriesAFailedStopCommand(t *testing.T) {
	dir := t.TempDir()
	startMarker := filepath.Join(dir, "started")
	stopAttempt := filepath.Join(dir, "stop-attempted")
	stopMarker := filepath.Join(dir, "stopped")
	port := commandDaemon(t, startMarker, stopMarker)
	ex := newTestExecutor(t, &Manifest{
		Engine: "daemon", DisplayName: "Daemon Engine", ManifestVersion: 1,
		Platforms: map[string]Platform{
			hostKey(): {
				Detect: []string{fakeEngineBin},
				Runtime: Runtime{
					Mode:  "command",
					Port:  port,
					Start: [][]string{{fakeEngineBin, "touch", startMarker}},
					Stop:  &StopSpec{Cmd: []string{fakeEngineBin, "failoncethentouch", stopAttempt, stopMarker}},
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: http.StatusOK, TimeoutS: 10},
				},
			},
		},
	})
	require.NoError(t, ex.Start(t.Context(), "daemon"))
	ex.StopAll()
	st, err := ex.Status("daemon")
	require.NoError(t, err)
	require.True(t, st.Running, "the failed stop must leave the engine running after the sweep")

	ex.StopAll()

	assert.FileExists(t, stopMarker, "the later StopAll must run the stop command again")
	st, err = ex.Status("daemon")
	require.NoError(t, err)
	assert.False(t, st.Running, "the retried stop must leave the engine stopped")
}

func TestStopAll_CancelsCommandStartWithoutError(t *testing.T) {
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
	require.NotEqual(t, 0, pid, "command-mode fake engine did not publish its PID")
	t.Cleanup(func() {
		if pidAlive(pid) {
			_ = signalPID(pid, true)
		}
	})

	ex.StopAll()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "command-mode start did not return after StopAll")
	}
	require.False(t, pidAlive(pid), "command-mode fake engine PID (%v)", pid)
	require.False(t, hasErr(ex.Errors(), startFailedID("fake")), "shutdown cancellation retained a command-mode start-failed error")
}

func TestStopAll_StopsDetachedCommandDaemonBeforeReadiness(t *testing.T) {
	dir := t.TempDir()
	startMarker := filepath.Join(dir, "started")
	stopMarker := filepath.Join(dir, "stopped")
	port, err := freePort()
	require.NoError(t, err, "free port")

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
	readinessRequested := make(chan struct{})
	noteReadiness := sync.OnceFunc(func() { close(readinessRequested) })
	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		noteReadiness()
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

	watchCtx := t.Context()
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
	require.NoError(t, <-daemonStarted, "detached command daemon")
	require.True(t, portServing(port), "detached command daemon did not start serving")
	// A readiness request proves the start CLI has returned, not merely written its marker.
	select {
	case <-readinessRequested:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "start CLI did not return to the readiness check")
	}

	ex.StopAll()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "command-mode start did not return after StopAll")
	}
	require.FileExists(t, stopMarker, "StopAll skipped the command-mode stop CLI after start detached")
	require.True(t, waitPortClosed(port, 5*time.Second), "detached command daemon remained on port (%v)", port)
	require.False(t, hasErr(ex.Errors(), startFailedID("fake")), "shutdown cancellation retained a command-mode start-failed error")
}

// TestE2EStdinCloseStopsEngine drives the real engine-manager binary and stops
// it the way the broker now does on shutdown: by closing stdin (EOF), with no
// prepare-shutdown or shutdown RPC. The EOF path must run StopAll and take the
// engine's port down — this is the backstop the broker relies on now that it no
// longer force-kills engine-manager on a timeout.
func TestE2EStdinClose_StopsEngine(t *testing.T) {
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
	require.NoError(t, json.Unmarshal(waitResult(t, m.frames, "1", 20*time.Second), &started), "start status")
	require.True(t, started.Running, "start status (%v)", started)

	// stop() closes stdin and waits for the process to exit on its own.
	m.stop(t)

	require.True(t, waitPortClosed(started.Port, 5*time.Second), "engine port")
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
