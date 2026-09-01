// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
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
	return procImageVia(systemTool("lsof", lsofLocations), pid)
}

// procImageVia runs one lsof and extracts the executable path. Split out so the
// parsing can be tested against a stub, and so a missing tool has a test.
func procImageVia(lsof string, pid int) string {
	if pid <= 0 {
		return ""
	}
	// -w suppresses the warnings that make lsof exit non-zero; runTool owns the
	// rule about when a non-zero exit still carries a usable answer, so the two
	// lookups on the path to a kill cannot drift apart on it.
	out := runTool(lsof, "-w", "-p", strconv.Itoa(pid), "-Fn", "-a", "-d", "txt")
	// No records: the process is not ours, or it has already gone. Fail closed.
	if len(out) == 0 {
		return ""
	}
	// Field output: an `f` line names the descriptor, and the `n` line that
	// follows it is that descriptor's path. Only txt descriptors were requested,
	// and the kernel lists the executable before the libraries it pulled in, so
	// the first such path is the image.
	inText := false
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "f"):
			inText = line == "ftxt"
		case inText && strings.HasPrefix(line, "n"):
			// Only a stray CR is stripped. lsof treats space as printable
			// outside the COMMAND column, so a path really ending in a space is
			// reported verbatim; trimming it would let "/dir/engine " compare
			// equal to the managed "/dir/engine" and widen a check that
			// authorizes a kill.
			return strings.TrimSuffix(line[1:], "\r")
		}
	}
	return ""
}
