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
	"sync"
	"testing"
	"time"
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
func TestStopKillsHelpersTheMainProcessLeftBehind(t *testing.T) {
	// The main process exits on SIGTERM; its helper ignores it and has closed the
	// engine's output pipes, so the stop sees the main process end and nothing
	// else would ever notice the helper.
	script := `trap 'exit 0' TERM; (trap '' TERM; exec sleep 300 >/dev/null 2>&1) & echo $!; while true; do sleep 0.1; done`
	var mu sync.Mutex
	childPID := make(chan int, 1)
	proc, err := startManagedProc("sh", []string{"-c", script}, nil, func(stream, line string) {
		mu.Lock()
		defer mu.Unlock()
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && stream == "stdout" {
			select {
			case childPID <- pid:
			default:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var helper int
	select {
	case helper = <-childPID:
	case <-time.After(5 * time.Second):
		t.Fatal("the helper process never started")
	}
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })
	if processGone(helper) {
		t.Fatal("the helper died before the stop")
	}

	started := time.Now()
	proc.stop(5 * time.Second)
	// The main process answered the polite signal, so this must not have waited
	// out the grace: what is under test is the cleanup after it exits.
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("stop took %s; the main process should have exited on the graceful signal", took)
	}

	if !waitGone(helper, 3*time.Second) {
		t.Fatalf("helper process %d survived the engine being stopped", helper)
	}
}

// A process that does not answer the polite signal is force-killed after the
// grace, together with its helpers.
func TestStopForceKillsAProcessThatIgnoresTheGracefulSignal(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	helper := <-childPID
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })

	started := time.Now()
	proc.stop(500 * time.Millisecond)
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("stop took %s", took)
	}
	if !waitGone(proc.cmd.Process.Pid, time.Second) || !waitGone(helper, 3*time.Second) {
		t.Fatal("the process or its helper survived the forced stop")
	}
}

// Shutdown is bounded by the parent, so the force kill has to come well inside
// that bound however long the manifest's grace is.
func TestShutdownCapsTheStopGrace(t *testing.T) {
	rt := Runtime{Stop: &StopSpec{Signal: "term", GraceS: 30}}
	e := &Executor{}
	if got := e.stopGraceFor(rt); got != 30*time.Second {
		t.Fatalf("an ordinary stop got %s, want the manifest's 30s", got)
	}
	e.shuttingDown.Store(true)
	if got := e.stopGraceFor(rt); got != shutdownStopGraceCap {
		t.Fatalf("a shutdown stop got %s, want the %s cap", got, shutdownStopGraceCap)
	}
	if got := e.stopGraceFor(Runtime{Stop: &StopSpec{Signal: "term", GraceS: 1}}); got != time.Second {
		t.Fatalf("a short grace was changed to %s", got)
	}
	if got := e.stopGraceFor(Runtime{Stop: &StopSpec{Signal: "kill"}}); got != 0 {
		t.Fatalf("an immediate-kill spec got %s", got)
	}
}

// vLLM that is still reading its weights does not answer a polite stop, and its
// manifest allows a generous grace for an ordinary stop. When the whole service
// shuts down mid-start the parent allows only a few seconds before it kills
// engine-manager, so the engine has to be force-killed inside that time or it is
// orphaned for good (it runs in its own process group, so nothing else will).
func TestShutdownMidStartForceKillsAnEngineThatIgnoresTheGracefulSignal(t *testing.T) {
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
	if helper == 0 {
		t.Fatal("the engine never started")
	}
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
	took := time.Since(began)
	if took > shutdownStopGraceCap+3*time.Second {
		t.Fatalf("shutdown took %s; the parent only waits a few seconds", took)
	}
	if !waitGone(helper, 2*time.Second) {
		t.Fatalf("the engine's helper process %d was left running", helper)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled start never returned")
	}
}

// (The script ends in a command after the sleep so the shell is not replaced by it.)
// A Python-packaged engine (vLLM) runs as "interpreter script args", so its
// executable never matches the engine path. The script in the command line is
// what proves a leftover process is the engine this service manages.
func TestProcRunsScriptRecognisesAnInterpreterRunningTheEngineScript(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/engine"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	proc, err := startManagedProc("/bin/sh", []string{script, "serve", "--port", "1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
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

	if !procRunsScript(pid, script) || !procRunsScript(pid, dir+"/./engine") {
		t.Error("the engine script in the command line was not recognised")
	}
	if procRunsScript(pid, dir+"/other-engine") {
		t.Error("an unrelated script was recognised")
	}
	// A bare word is not an engine path, even though it is one of the arguments.
	if procRunsScript(pid, "serve") {
		t.Error("a relative path was taken for the script")
	}
	if procRunsScript(0, script) || procRunsScript(pid, "") || procRunsScript(999999999, script) {
		t.Error("an invalid pid or path matched")
	}
}
