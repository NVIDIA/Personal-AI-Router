// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// procImage resolves a PID's executable path with lsof.
//
// macOS has no /proc, and this is the check that decides whether a listener on
// our managed port is our own engine binary. Returning "" unconditionally — as
// this did before there was a macOS implementation — made that check fail closed
// forever: an engine PAIR started but lost the handle to could never be
// reclaimed, so the operator's stop was refused every time with "running under
// external management", and uninstall failed with it.
//
// The `txt` descriptor is the running image as the kernel records it, not argv,
// so it is right even for a process launched through a PATH lookup and cannot be
// spoofed by the process renaming itself. `ps -o comm=` is not a substitute: it
// reports a bare name in exactly that case, which would not match the absolute
// path being compared against.
//
// Empty on any error so the caller's image check fails closed.
func procImage(pid int) string {
	return procImageVia(lsofPath(), pid)
}

// macOSLsof is where macOS ships lsof. Named because a bare "lsof" resolves
// through PATH, and this process inherits whatever PATH the desktop app was
// launched with — nothing in the chain from Electron through the broker sets
// one. A GUI launch normally yields a PATH containing /usr/sbin, but a user or
// launcher that narrows it would silently reinstate the exact bug this file
// exists to fix, with the same misleading "external management" message and no
// signal that a tool was missing.
//
// nvpair-node-info already addresses ioreg by absolute path for the same reason.
const macOSLsof = "/usr/sbin/lsof"

// lsofPath prefers the known macOS location and otherwise leaves resolution to
// PATH, which is what the other BSDs this file also builds for need — FreeBSD
// installs lsof from ports, under a different prefix.
func lsofPath() string {
	if _, err := os.Stat(macOSLsof); err == nil {
		return macOSLsof
	}
	return "lsof"
}

// procImageVia runs one lsof and extracts the executable path, so the parsing
// can be tested without depending on where the tool lives.
func procImageVia(lsof string, pid int) string {
	if pid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		lsof, "-p", strconv.Itoa(pid), "-Fn", "-a", "-d", "txt").Output()
	if err != nil {
		return ""
	}
	// Field output: an `f` line names the descriptor, and the `n` line that
	// follows it is that descriptor's path. Only txt descriptors were requested,
	// so the first such path is the executable.
	inText := false
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "f"):
			inText = line == "ftxt"
		case inText && strings.HasPrefix(line, "n"):
			return strings.TrimSpace(line[1:])
		}
	}
	return ""
}
