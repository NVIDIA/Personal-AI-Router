// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// portLookupTimeout bounds each external port-owner lookup (lsof/ss). The
// broker no longer force-kills engine-manager on a timeout, so an unbounded
// lookup that hung would wedge StopAll (and the whole app shutdown).
const portLookupTimeout = 2 * time.Second

// configureSysProcAttr puts the child in its own process group so a
// terminate signals the whole group — engines that fork helper
// processes (model runners, etc.) get cleaned up too.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// gracefulSignal sends SIGTERM to the process group (falling back to the
// process itself). It is the only stop signal engine-manager sends: stop()
// sends this once and waits for the engine to exit, and never escalates to
// SIGKILL. A well-behaved engine (Ollama, and the test fake, whose default
// SIGTERM disposition is to exit) terminates on it.
func gracefulSignal(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		return syscall.Kill(-pgid, syscall.SIGTERM)
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}

// pidOnPort returns the PID listening on the given TCP port and that
// process's executable path. Best-effort on the debug-only Unix targets: it
// shells out to lsof, falling back to ss. ok is false when neither resolves
// an owner, in which case the caller fails closed (declines the stop).
func pidOnPort(port int) (pid int, image string, ok bool) {
	if p, found := lsofPID(port); found {
		return p, procImage(p), true
	}
	if p, found := ssPID(port); found {
		return p, procImage(p), true
	}
	return 0, "", false
}

// lsofPID prints just the PID(s) of the TCP listener on the port (-t = terse).
func lsofPID(port int) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return 0, false
	}
	for _, field := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(field); err == nil && p > 0 {
			return p, true
		}
	}
	return 0, false
}

// ssPID parses the owning PID out of ss's users:(("proc",pid=1234,fd=7)) tail.
func ssPID(port int) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ss", "-ltnp", "sport", "=", ":"+strconv.Itoa(port)).Output()
	if err != nil {
		return 0, false
	}
	s := string(out)
	idx := strings.Index(s, "pid=")
	if idx < 0 {
		return 0, false
	}
	rest := s[idx+len("pid="):]
	end := strings.IndexAny(rest, ",)")
	if end < 0 {
		return 0, false
	}
	if p, err := strconv.Atoi(rest[:end]); err == nil && p > 0 {
		return p, true
	}
	return 0, false
}

// procImage resolves a PID's executable path via /proc (Linux). macOS has no
// /proc, so it returns "" there and the image check is skipped — reclamation
// on macOS relies on the caller declining when the image can't be confirmed.
func procImage(pid int) string {
	if pid <= 0 {
		return ""
	}
	if path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		return path
	}
	return ""
}

// signalPID sends SIGTERM (or SIGKILL when force) to the process group when
// possible. It is the PID-addressed kill used only by the orphan reclaim (a
// process we lost the *exec.Cmd handle to), distinct from the normal
// graceful-only stop() path; force reaches forked helpers (model runners, etc.).
func signalPID(pid int, force bool) error {
	if pid <= 0 {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if pgid, err := syscall.Getpgid(pid); err == nil {
		return syscall.Kill(-pgid, sig)
	}
	return syscall.Kill(pid, sig)
}

// pidAlive reports whether the PID still exists (signal 0 probes existence
// without delivering a signal; EPERM means it exists but isn't ours).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
