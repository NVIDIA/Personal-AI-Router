// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// managedProc is a spawned engine process with its stdout/stderr
// captured line-by-line and a `done` channel that closes when it
// exits. The OS-specific primitives it relies on (console hiding,
// graceful signal, force kill) live in proc_windows.go / proc_unix.go
// so the same logic here runs on every platform.
type managedProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// startManagedProc launches bin with args and the extra env (merged
// over the current environment), hides any console window, and streams
// stdout/stderr lines to onLine. The returned proc's done channel
// closes once the process exits.
func startManagedProc(bin string, args []string, env map[string]string, onLine func(stream, line string)) (*managedProc, error) {
	cmd := exec.Command(bin, args...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
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

	mp := &managedProc{cmd: cmd, done: make(chan struct{})}
	var scanWg sync.WaitGroup
	scanWg.Add(2)
	go func() { defer scanWg.Done(); scanLines(stdout, "stdout", onLine) }()
	go func() { defer scanWg.Done(); scanLines(stderr, "stderr", onLine) }()
	go func() {
		// Drain stdout/stderr fully before Wait: Wait closes the pipes on
		// exit, which would otherwise truncate the final captured lines.
		scanWg.Wait()
		_ = cmd.Wait()
		close(mp.done)
	}()
	return mp, nil
}

func scanLines(r io.Reader, stream string, onLine func(stream, line string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		if onLine != nil {
			onLine(stream, sc.Text())
		}
	}
}

// stop stops the process and waits for it to exit, with no timeout.
//
// It sends one platform-appropriate stop signal (see gracefulSignal) and then
// blocks until the process is gone:
//   - Unix: SIGTERM to the process group — a graceful ask, with no escalation
//     to SIGKILL. A well-behaved engine (Ollama, and the test fake) exits on it.
//   - Windows: taskkill /T /F. Our engines run windowless, and a windowless
//     process can't receive a graceful (non-/F) close, so /F is the only signal
//     that actually stops it — never force-killing there would leave the engine
//     running forever.
//
// There is deliberately no timeout: a stop is complete only when the engine has
// actually exited. On Unix an engine that ignored SIGTERM would not be stopped
// and this would wait for it; in practice engines exit on SIGTERM.
func (mp *managedProc) stop() {
	if mp == nil || mp.cmd == nil || mp.cmd.Process == nil {
		return
	}
	select {
	case <-mp.done:
		return // already exited
	default:
	}
	_ = gracefulSignal(mp.cmd)
	<-mp.done
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
