// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"errors"
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
	return procImageVia(systemTool("lsof", lsofLocations), pid)
}

// procImageVia runs one lsof and extracts the executable path. Split out so the
// parsing can be tested against a stub, and so a missing tool has a test.
func procImageVia(lsof string, pid int) string {
	if pid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()

	// -w suppresses warnings, because lsof's exit status is not a statement
	// about our PID: it returns 1 if it hit *any* error anywhere, including a
	// filesystem it could not stat, while still printing correct records for the
	// process asked about. Hosts with network mounts or FUSE volumes hit that
	// routinely. So the output is parsed whenever there is any, and the exit
	// status only decides the empty case — otherwise a warning about an
	// unrelated mount would blank the image and silently refuse a legitimate
	// stop, which is the bug this file exists to fix.
	out, err := exec.CommandContext(ctx, lsof,
		"-w", "-p", strconv.Itoa(pid), "-Fn", "-a", "-d", "txt").Output()

	// A non-zero exit is tolerated; anything else is not. Failing to start the
	// tool means it is missing, and a deadline means the answer may be truncated
	// mid-record, so neither can be parsed.
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		return ""
	}
	if ctx.Err() != nil {
		return ""
	}
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
