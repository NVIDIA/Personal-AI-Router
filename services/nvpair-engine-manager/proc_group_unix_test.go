// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// processGone reports whether pid is gone. A killed process whose parent has
// not reaped it yet is a zombie, which is as dead as a process gets.
func processGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// "pid (comm) S ..." — comm may contain spaces and parentheses, so cut after the last ")".
	fields := strings.Fields(string(data[strings.LastIndex(string(data), ")")+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processGone(pid)
}

// vLLM's main process can exit on a polite stop while the helper it forked
// (EngineCore, holding the GPU) is still alive, or never gets to answer one.
// Stopping an engine has to take the helpers with it: they are in the
// engine's process group and no one else will ever clean them up.
func TestManagedProcStop_KillsHelpersTheMainProcessLeftBehind(t *testing.T) {
	// The main process exits on SIGTERM; its helper ignores it and has closed the
	// engine's output pipes, so the stop sees the main process end and nothing
	// else would ever notice the helper.
	script := `trap 'exit 0' TERM; (trap '' TERM; exec sleep 300 >/dev/null 2>&1) & echo $!; while true; do sleep 0.1; done`
	childPID := make(chan int, 1)
	proc, err := startManagedProc("sh", []string{"-c", script}, nil, func(stream, line string) {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && stream == "stdout" {
			select {
			case childPID <- pid:
			default:
			}
		}
	})
	require.NoError(t, err)
	var helper int
	select {
	case helper = <-childPID:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the helper process never started")
	}
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })
	require.False(t, processGone(helper), "the helper died before the stop")

	started := time.Now()
	proc.stop(5 * time.Second)

	// The main process answered the polite signal, so this must not have waited
	// out the grace: what is under test is the cleanup after it exits.
	assert.LessOrEqual(t, time.Since(started), 2*time.Second, "the main process should have exited on the graceful signal")
	assert.True(t, waitGone(helper, 3*time.Second), "helper process %d survived the engine being stopped", helper)
}

// A process that does not answer the polite signal is force-killed after the
// grace, together with its helpers.
func TestManagedProcStop_ForceKillsAProcessThatIgnoresTheGracefulSignal(t *testing.T) {
	script := `trap '' TERM; (trap '' TERM; exec sleep 300) & echo $!; while true; do sleep 1; done`
	childPID := make(chan int, 1)
	proc, err := startManagedProc("sh", []string{"-c", script}, nil, func(stream, line string) {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && stream == "stdout" {
			select {
			case childPID <- pid:
			default:
			}
		}
	})
	require.NoError(t, err)
	helper := <-childPID
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })

	started := time.Now()
	proc.stop(500 * time.Millisecond)

	assert.LessOrEqual(t, time.Since(started), 3*time.Second, "the stop must not wait much past the grace")
	assert.True(t, waitGone(proc.cmd.Process.Pid, time.Second), "the process survived the forced stop")
	assert.True(t, waitGone(helper, 3*time.Second), "the helper survived the forced stop")
}

// Shutdown is bounded by the parent, so the force kill has to come well inside
// that bound however long the manifest's grace is.
func TestStopGraceFor_CapsTheManifestGraceDuringShutdown(t *testing.T) {
	test := func(name string, shuttingDown bool, stop StopSpec, want time.Duration) {
		t.Run(name, func(t *testing.T) {
			e := &Executor{}
			e.shuttingDown.Store(shuttingDown)

			assert.Equal(t, want, e.stopGraceFor(Runtime{Stop: &stop}))
		})
	}
	test("an ordinary stop gets the manifest's grace", false, StopSpec{Signal: "term", GraceS: 30}, 30*time.Second)
	test("a shutdown stop is capped", true, StopSpec{Signal: "term", GraceS: 30}, shutdownStopGraceCap)
	test("a shutdown keeps a grace below the cap", true, StopSpec{Signal: "term", GraceS: 1}, time.Second)
	test("an immediate-kill spec has no grace", true, StopSpec{Signal: "kill"}, 0)
}

// vLLM that is still reading its weights does not answer a polite stop, and its
// manifest allows a generous grace for an ordinary stop. When the whole service
// shuts down mid-start the parent allows only a few seconds before it kills
// engine-manager, so the engine has to be force-killed inside that time or it is
// orphaned for good (it runs in its own process group, so nothing else will).
func TestStopAll_ForceKillsAStartingEngineThatIgnoresTheGracefulSignal(t *testing.T) {
	// A shell that ignores SIGTERM and forks a helper that does too, like vLLM's
	// API server and its EngineCore while loading.
	script := `trap '' TERM; (trap '' TERM; exec sleep 300) & echo $! > ` + "%s" + `; while true; do sleep 1; done`
	pidFile := t.TempDir() + "/helper.pid"

	m := testEngineManifest("/bin/sh")
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Args = []string{"-c", strings.Replace(script, "%s", pidFile, 1)}
	p.Runtime.Env = nil
	p.Runtime.Stop = &StopSpec{Signal: "term", GraceS: 30} // far longer than a shutdown can wait
	p.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 60}
	p.Runtime.Health = nil
	m.Platforms[key] = p
	m.Actions = nil
	ex := newTestExecutor(t, m)

	started := make(chan error, 1)
	go func() { started <- ex.Start(context.Background(), "fake") }()

	var helper int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				helper = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotZero(t, helper, "the engine never started")
	st, _ := ex.state("fake")
	for i := 0; i < 100; i++ {
		st.mu.Lock()
		proc := st.proc
		st.mu.Unlock()
		if proc != nil {
			t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	began := time.Now()
	ex.StopAll()

	assert.LessOrEqual(t, time.Since(began), shutdownStopGraceCap+3*time.Second, "the parent only waits a few seconds for shutdown")
	assert.True(t, waitGone(helper, 2*time.Second), "the engine's helper process %d was left running", helper)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		assert.Fail(t, "the cancelled start never returned")
	}
}

// (The script ends in a command after the sleep so the shell is not replaced by it.)
// A Python-packaged engine (vLLM) runs as "interpreter script args", so its
// executable never matches the engine path. The script in the command line is
// what proves a leftover process is the engine this service manages.
func TestProcRunsScript_RecognisesAnInterpreterRunningTheEngineScript(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/engine"
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\ntrue\n"), 0o755))
	proc, err := startManagedProc("/bin/sh", []string{script, "serve", "--port", "1"}, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { proc.stop(0) })
	pid := proc.cmd.Process.Pid
	// /proc/<pid>/cmdline reads empty for a moment while a new process finishes
	// exec'ing. A leftover engine has been running far longer than that.
	for i := 0; i < 200; i++ {
		if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recognised := func(name string, pid int, path string) {
		t.Run(name, func(t *testing.T) {
			assert.True(t, procRunsScript(pid, path))
		})
	}
	notRecognised := func(name string, pid int, path string) {
		t.Run(name, func(t *testing.T) {
			assert.False(t, procRunsScript(pid, path))
		})
	}
	recognised("the engine script", pid, script)
	recognised("the engine script through a dot segment", pid, dir+"/./engine")
	notRecognised("an unrelated script", pid, dir+"/other-engine")
	// A bare word is not an engine path, even though it is one of the arguments.
	notRecognised("a relative path among the arguments", pid, "serve")
	notRecognised("pid 0", 0, script)
	notRecognised("an empty path", pid, "")
	notRecognised("a pid that does not exist", 999999999, script)
}
