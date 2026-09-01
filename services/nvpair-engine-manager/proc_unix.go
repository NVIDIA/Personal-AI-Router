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

// Absolute locations of the tools this file shells out to, tried in order
// before falling back to a PATH lookup.
//
// PATH is not ours to trust: this worker inherits whatever the desktop app was
// launched with, and nothing between Electron, the broker and here sets one, so
// a user-writable directory such as /opt/homebrew/bin can shadow a system tool.
// What comes back decides which PID gets terminated.
//
// A single list covers every Unix because a path that does not exist is simply
// skipped; macOS ships lsof in /usr/sbin, most Linux distributions in /usr/bin,
// and ss moves around by distribution.
var lsofLocations = []string{"/usr/sbin/lsof", "/usr/bin/lsof"}

// systemTool is the first usable location, or name for a PATH lookup.
//
// Executability is checked, not just existence: a directory or a non-executable
// file at a candidate path would otherwise short-circuit the search and take the
// later candidates out of play.
//
// The fallback exists because Linux distributions disagree on where these live,
// not as a convenience. Reaching it means none of the known locations exist, in
// which case a PATH lookup is the only remaining chance of resolving the owner
// at all — and failing to resolve declines the stop rather than widening it.
func systemTool(name string, locations []string) string {
	for _, path := range locations {
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return path
	}
	return name
}

// runTool executes a resolved tool and returns its stdout.
//
// A non-zero exit is tolerated when there is still output, because lsof reports
// failure if it hit *any* error anywhere — a filesystem it could not stat, which
// is routine with network or FUSE mounts — while printing correct records for
// what was asked about. Treating that as total failure is not a harmless
// conservatism here: it drops the owner lookup through to the next mechanism,
// and on macOS that means resolving `ss` (which does not exist) through PATH.
//
// A failure to start the tool, or a deadline (the answer may be truncated
// mid-record), yields nothing so the caller fails closed.
func runTool(tool string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, tool, args...).Output()
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	return out
}

// lsofPID prints just the PID(s) of the TCP listener on the port (-t = terse).
func lsofPID(port int) (int, bool) {
	// -w suppresses the warnings that make lsof exit non-zero; see runTool.
	out := runTool(systemTool("lsof", lsofLocations),
		"-w", "-nP", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN")
	if len(out) == 0 {
		return 0, false
	}
	for _, field := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(field); err == nil && p > 0 {
			return p, true
		}
	}
	return 0, false
}

// signalPID sends SIGTERM (or SIGKILL when force) to the orphan being reclaimed.
// It is the PID-addressed kill used only by the orphan reclaim (a process we lost
// the *exec.Cmd handle to), distinct from the normal graceful-only stop() path.
//
// The group is signalled only when the target leads it, so force still reaches
// forked helpers (model runners and the like) of anything PAIR started —
// configureSysProcAttr sets Setpgid, which makes every such process a group
// leader. An adopted engine PAIR did not start is a different case: it may have
// been launched from a shell wrapper or a pipeline, which puts unrelated
// processes in its group, and the ownership check that authorized this kill
// examined one PID. Signalling the group there would extend a single-process
// decision across processes nothing verified.
func signalPID(pid int, force bool) error {
	if pid <= 0 {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
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
