// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nvpair-shared/enginelogs"
)

// managedProc is a spawned engine process with its stdout/stderr
// captured line-by-line and a `done` channel that closes when it
// exits. The OS-specific primitives it relies on (console hiding,
// graceful signal, force kill) live in proc_windows.go / proc_unix.go
// so the same logic here runs on every platform.
type managedProc struct {
	cmd    *exec.Cmd
	exited chan struct{} // closes when the OS process exits
	done   chan struct{} // closes after remaining output has drained
}

// startManagedProc launches bin with args and the extra env (merged
// over the current environment), hides any console window, and streams
// stdout/stderr lines to onLine. The returned proc's done channel
// closes once the process exits.
func startManagedProc(bin string, args []string, env map[string]string, onLine func(stream, line string)) (*managedProc, error) {
	return startManagedProcWithEnv(bin, args, env, true, onLine)
}

func startManagedProcWithEnv(bin string, args []string, env map[string]string, inherit bool, onLine func(stream, line string)) (*managedProc, error) {
	cmd := exec.Command(bin, args...)
	if inherit {
		cmd.Env = os.Environ()
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	configureSysProcAttr(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	mp := &managedProc{cmd: cmd, exited: make(chan struct{}), done: make(chan struct{})}
	var scanWg sync.WaitGroup
	scanWg.Add(2)
	go func() { defer scanWg.Done(); scanLines(stdout, "stdout", onLine) }()
	go func() { defer scanWg.Done(); scanLines(stderr, "stderr", onLine) }()
	go func() {
		_ = cmd.Wait()
		close(mp.exited)
		scanWg.Wait()
		close(mp.done)
	}()
	return mp, nil
}

func scanLines(r io.Reader, stream string, onLine func(stream, line string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), enginelogs.MaxLineBytes)
	for sc.Scan() {
		if onLine != nil {
			onLine(stream, sc.Text())
		}
	}
}

// stop bounds graceful shutdown, escalates the exact owned process group, and
// returns only after OS process exit is observed. Output draining is separate
// so an inherited pipe cannot wedge lifecycle ownership under opMu.
//
// The graceful signal is platform-appropriate (see gracefulSignal):
//   - Unix: SIGTERM to the process group. A well-behaved engine (Ollama, and
//     the test fake) exits on it.
//   - Windows: taskkill /T /F. Our engines run windowless, and a windowless
//     process can't receive a graceful (non-/F) close, so /F is the only signal
//     that actually stops it.
//
// An engine still running when grace expires, together with any child left in
// its process group, is force-killed as a group.
func (mp *managedProc) stop(grace time.Duration) error {
	if mp == nil || mp.cmd == nil || mp.cmd.Process == nil {
		return nil
	}
	select {
	case <-mp.exited:
		return nil // already exited
	default:
	}
	if grace <= 0 {
		grace = 5 * time.Second
	}
	gracefulErr := gracefulSignal(mp.cmd)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-mp.exited:
		return nil
	case <-timer.C:
	}
	forceErr := signalPID(mp.cmd.Process.Pid, true)
	forceTimer := time.NewTimer(5 * time.Second)
	defer forceTimer.Stop()
	select {
	case <-mp.exited:
		return nil
	case <-forceTimer.C:
		return fmt.Errorf("managed process did not exit after graceful and forced stop: graceful=%v force=%v", gracefulErr, forceErr)
	}
}

// terminatePID stops the process with the given PID (and its tree on
// Windows, or its process group on Unix when available): a graceful signal
// first, escalating to a forced kill if the process hasn't exited within
// grace. It exists to reclaim a PAIR-managed engine orphan adopted on our
// own port — an instance a prior run spawned and then lost the handle to, so
// we can only address it by PID rather than through the *exec.Cmd handle
// managedProc.stop needs. Best-effort: a process that's already gone counts
// as success. The platform primitives (signalPID, pidAlive) live in
// proc_windows.go / proc_unix.go.
func terminatePID(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	_ = signalPID(pid, false)
	if grace <= 0 {
		grace = 5 * time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = signalPID(pid, true)
	}
	forceDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(forceDeadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// normalizeEngineImage cleans an executable path for comparison. Linux
// /proc/<pid>/exe can suffix " (deleted)" when the file was replaced while
// the process still runs.
func normalizeEngineImage(path string) string {
	path = strings.TrimSuffix(path, " (deleted)")
	return filepath.Clean(path)
}

// isOurEngineImage reports whether the listener on our managed port is
// running the same binary this service manages for the engine (st.binPath).
// The compare is path-cleaned and case-insensitive so a PAIR-owned orphan on
// our managed port is reclaimed by Stop, while an unrelated process that
// merely grabbed the port is left for the caller to decline. An empty image
// (e.g. a PID we can't introspect) never matches, so Stop fails closed.
func isOurEngineImage(image, binPath string) bool {
	if image == "" || binPath == "" {
		return false
	}
	return strings.EqualFold(normalizeEngineImage(image), normalizeEngineImage(binPath))
}

// isManagedInstallPath reports whether binPath is inside this engine's
// NVPAIR-owned install directory. A matching executable name/path is not, by
// itself, ownership: Detect intentionally recognizes vendor/user installs on
// some platforms. Stop/uninstall must never reclaim those external files.
func isManagedInstallPath(binPath, installDir string) bool {
	if binPath == "" || installDir == "" {
		return false
	}
	binAbs, err := filepath.Abs(normalizeEngineImage(binPath))
	if err != nil {
		return false
	}
	dirAbs, err := filepath.Abs(installDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dirAbs, binAbs)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
