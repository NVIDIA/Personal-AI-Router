// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestOwnerLookupIgnoresPath is the guard for the deployment shape rather than
// the developer one. This worker inherits whatever PATH the desktop app was
// launched with, and nothing between Electron, the broker and here sets one, so
// a narrowed PATH must not disable orphan reclaim and a user-writable directory
// must not be able to shadow the tool that decides which process gets killed.
//
// It exercises pidOnPort, not procImage alone. An earlier version of this test
// checked only the image half and passed while the PID half still went through
// PATH — so it certified a guarantee the system did not have, and the two tests
// that actually cover reclaim skipped instead of failing. Both halves are on the
// same path to a kill; testing one is testing neither.
func TestOwnerLookupIgnoresPath(t *testing.T) {
	// A real listener whose owner is this test process.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	t.Setenv("PATH", "")

	pid, image, ok := pidOnPort(port)
	if !ok {
		t.Fatal("pidOnPort resolved no owner with an empty PATH; the PID lookup must " +
			"not depend on PATH or reclaim stays broken exactly where it was")
	}
	if pid != os.Getpid() {
		t.Errorf("pidOnPort = %d, want this process %d", pid, os.Getpid())
	}
	if image == "" {
		t.Fatal("no image resolved with an empty PATH")
	}

	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !isOurEngineImage(image, want) {
		t.Errorf("image = %q, want the running binary %q", image, want)
	}
}

// TestSystemToolPrefersAnAbsoluteLocation pins the resolution order that keeps
// PATH out of the decision on a normal host.
func TestSystemToolPrefersAnAbsoluteLocation(t *testing.T) {
	got := systemTool("lsof", lsofLocations)
	if !filepath.IsAbs(got) {
		t.Errorf("systemTool resolved %q; macOS ships lsof at an absolute location "+
			"and it should be preferred over a PATH lookup", got)
	}
}

// TestSystemToolFallsBackToTheBareName checks that with no candidate present the
// bare name is returned, so a PATH lookup can still find a distribution that puts
// the tool somewhere else.
func TestSystemToolFallsBackToTheBareName(t *testing.T) {
	if got := systemTool("lsof", []string{"/nonexistent/a", "/nonexistent/b"}); got != "lsof" {
		t.Errorf("systemTool with no candidates = %q, want the bare name", got)
	}
}

// TestProcImageRejectsAPlantedLsof is the other half: even a PATH entry that
// shadows the real tool must not be consulted, because whatever answers here
// decides which PID gets terminated.
func TestProcImageRejectsAPlantedLsof(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "lsof")
	// Prints a well-formed field-output record naming a binary PAIR manages.
	script := "#!/bin/sh\nprintf 'ftxt\\nn/opt/nvpair/ollama\\n'\n"
	if err := os.WriteFile(planted, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got := procImage(os.Getpid())
	if got == "/opt/nvpair/ollama" {
		t.Fatal("procImage used an lsof found on PATH; a hostile PATH entry could " +
			"nominate any process for termination")
	}

	// Sanity: the planted script really would have produced that answer.
	if via := procImageVia(planted, os.Getpid()); via != "/opt/nvpair/ollama" {
		t.Errorf("planted stub returned %q; the test is not exercising what it claims", via)
	}
}

// TestProcImageFailsClosedOnAMissingTool checks the direction that matters for
// safety: if lsof cannot be run, the image must come back empty so the ownership
// check declines rather than matching something.
func TestProcImageFailsClosedOnAMissingTool(t *testing.T) {
	if got := procImageVia("/nonexistent/lsof", os.Getpid()); got != "" {
		t.Errorf("procImageVia with a missing tool = %q, want empty", got)
	}
}

// TestProcImageParsesOnlyTheFirstTextDescriptor pins the parse against output
// shaped like the real thing: a process's txt descriptors include the libraries
// it loaded, and only the leading entry is the executable.
func TestProcImageParsesOnlyTheFirstTextDescriptor(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "lsof-stub")
	script := "#!/bin/sh\n" +
		"printf 'p123\\nftxt\\nn/opt/nvpair/ollama\\nftxt\\nn/usr/lib/dyld\\n" +
		"ftxt\\nn/usr/lib/libSystem.dylib\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := procImageVia(stub, 123); got != "/opt/nvpair/ollama" {
		t.Errorf("procImageVia = %q, want the first txt entry", got)
	}
}

// TestProcImageIgnoresNonTextDescriptors checks the f/n state machine does not
// pick up a path belonging to some other descriptor kind.
func TestProcImageIgnoresNonTextDescriptors(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "lsof-stub")
	script := "#!/bin/sh\n" +
		"printf 'p123\\nfcwd\\nn/some/working/dir\\nftxt\\nn/opt/nvpair/ollama\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := procImageVia(stub, 123); got != "/opt/nvpair/ollama" {
		t.Errorf("procImageVia = %q, want the txt entry, not the cwd", got)
	}
}
