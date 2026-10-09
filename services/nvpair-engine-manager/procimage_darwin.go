// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"strconv"
	"strings"
)

// lsof -F field output starts each line with a one-letter field identifier.
const (
	lsofFieldFD   = "f"   // a descriptor, e.g. "ftxt"
	lsofFieldName = "n"   // the path of the descriptor before it
	lsofTextFD    = "txt" // the running image and the libraries it loaded
)

// procImage resolves a PID's executable path with lsof.
//
// macOS has no /proc, and this is what decides whether a listener on PAIR's
// managed port is PAIR's own engine binary. An empty result makes that check
// fail closed, so a stop or uninstall of an engine PAIR started but lost the
// handle to is refused.
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
	out := runTool(lsof, "-w", "-p", strconv.Itoa(pid), "-F"+lsofFieldName, "-a", "-d", lsofTextFD)
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
		case strings.HasPrefix(line, lsofFieldFD):
			inText = line == lsofFieldFD+lsofTextFD
		case inText && strings.HasPrefix(line, lsofFieldName):
			// Only a stray CR is stripped. lsof treats space as printable
			// outside the COMMAND column, so a path really ending in a space is
			// reported verbatim; trimming it would let "/dir/engine " compare
			// equal to the managed "/dir/engine" and widen a check that
			// authorizes a kill.
			return strings.TrimSuffix(line[len(lsofFieldName):], "\r")
		}
	}
	return ""
}
