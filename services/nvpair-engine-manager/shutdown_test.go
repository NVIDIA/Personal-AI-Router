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

	"github.com/stretchr/testify/require"
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
		{
			err := ex.Start(ctx, name)
			require.NoError(t, err, "start (%v, %v)", name, err)
		}
		st, _ := ex.Status(name)
		require.True(t, st.Running, " (%v, %v)", name, st)
		require.NotEqual(t, 0, st.Port, " (%v, %v)", name, st)
		ports[i] = st.Port
	}

	ex.StopAll()

	require.True(t, waitPortClosed(ports[0], 5*time.Second), "engine (%v)", names[0])
	require.True(t, waitPortClosed(ports[1], 5*time.Second), "engine (%v)", names[1])
	{
		err := ex.Start(ctx, names[0])
		require.ErrorIs(t, err, context.Canceled, "start after StopAll error")
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
		require.FailNow(t, "test expectation failed", "command-mode start did not return after StopAll")
	}
	require.False(t, pidAlive(pid), "command-mode fake engine PID (%v)", pid)
	require.False(t, hasErr(ex.Errors(), startFailedID("fake")), "shutdown cancellation retained a command-mode start-failed error")
}

func TestStopAllStopsDetachedCommandDaemonBeforeReadiness(t *testing.T) {
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
	require.NoError(t, <-daemonStarted, "detached command daemon")
	require.True(t, portServing(port), "detached command daemon did not start serving")

	ex.StopAll()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test expectation failed", "command-mode start did not return after StopAll")
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
	{
		err := json.Unmarshal(waitResult(t, m.frames, "1", 20*time.Second), &started)
		require.NoError(t, err, "start status (%v, %v)", started, err)
		require.True(t, started.Running, "start status (%v, %v)", started, err)
	}

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
