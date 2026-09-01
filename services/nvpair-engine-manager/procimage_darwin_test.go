// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProcImageIgnoresPath is the guard for the deployment shape rather than the
// developer one. This worker inherits whatever PATH the desktop app was launched
// with, and nothing between Electron, the broker and here sets one. Resolving
// lsof through PATH would mean a narrowed PATH silently returns the original bug
// — orphan reclaim refused, with the same misleading "external management"
// message — and a hostile PATH entry could choose which process PAIR kills.
func TestProcImageIgnoresPath(t *testing.T) {
	if _, err := os.Stat(lsofPath); err != nil {
		t.Fatalf("macOS is expected to ship lsof at %s: %v", lsofPath, err)
	}
	t.Setenv("PATH", "")

	got := procImage(os.Getpid())
	if got == "" {
		t.Fatal("procImage found nothing with an empty PATH; lsof must be resolved absolutely")
	}

	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if normalizeEngineImage(got) != normalizeEngineImage(want) {
		t.Errorf("procImage = %q, want the running binary %q", got, want)
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
	if isOurEngineImage("", "/opt/nvpair/ollama") {
		t.Error("an unresolvable image matched; the check must fail closed")
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
