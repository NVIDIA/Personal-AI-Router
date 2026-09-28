// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestProcImageResolvesThisProcess is the regression guard for a per-OS gap
// that disabled orphan reclamation on macOS entirely.
//
// procImage only ever had a Linux implementation: it read /proc/<pid>/exe, and
// macOS has no /proc, so it returned "" for every PID. The ownership check fails
// closed on an empty image, so a listener on our own managed port could never be
// confirmed as our own engine — an engine PAIR started but lost the handle to
// could not be stopped, and the operator got "running under external management"
// every time.
//
// Checked against this test binary because it is the one process whose real
// executable path is known independently, and it costs nothing to introspect.
func TestProcImageResolvesThisProcess(t *testing.T) {
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	got := procImage(os.Getpid())
	if got == "" {
		t.Fatalf("procImage returned nothing for this process; the ownership check "+
			"fails closed on an empty image, so stop and uninstall are refused on %s",
			runtime.GOOS)
	}
	if normalizeEngineImage(got) != normalizeEngineImage(want) {
		t.Errorf("procImage = %q, want the running binary %q", got, want)
	}
}

// TestProcImageRejectsInvalidPID checks the failure direction, since an image
// that cannot be resolved must never be mistaken for a match.
func TestProcImageRejectsInvalidPID(t *testing.T) {
	test := func(name string, pid int) {
		t.Run(name, func(t *testing.T) {
			if got := procImage(pid); got != "" {
				t.Errorf("procImage(%d) = %q, want empty", pid, got)
			}
		})
	}
	test("zero PID", 0)
	test("negative PID", -1)
}

// TestPathComparisonFollowsSymlinks is the regression guard for the second half
// of the same bug. The two sides of every ownership comparison come from
// different places and only the OS side is resolved: macOS reports
// /private/var/... where the configured path says /var/..., so one file looked
// like two and the stop was declined.
func TestPathComparisonFollowsSymlinks(t *testing.T) {
	realDir := t.TempDir()
	bin := filepath.Join(realDir, "engine")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A second name for the same directory, which is exactly what /var is.
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	viaLink := filepath.Join(linkDir, "engine")

	if !isOurEngineImage(viaLink, bin) {
		t.Error("the same binary reached through a symlinked directory did not match itself")
	}
	if !isManagedInstallPath(viaLink, realDir) {
		t.Error("a binary reached through a symlink was judged outside its own install directory")
	}

	// The guarantee that matters more: a genuinely different binary still fails.
	other := filepath.Join(t.TempDir(), "engine")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isOurEngineImage(other, bin) {
		t.Error("a same-named binary elsewhere matched; this would kill an unrelated process")
	}
	if isManagedInstallPath(other, realDir) {
		t.Error("a binary outside the install directory was judged inside it")
	}
}

// TestManagedInstallPathRejectsAnEscapingLeafSymlink pins the containment
// direction for a symlink that lives inside the install directory but points
// outside it.
//
// Resolving both sides changed this answer: a textual compare saw the link's own
// location and called it managed, while resolution follows it to its target and
// correctly does not. Fail-closed is the right direction here — a stop or
// uninstall declines rather than acting on a file outside the directory PAIR
// owns — but it is worth pinning, because the alternative would let a link
// planted in the install directory nominate an arbitrary file for deletion.
func TestManagedInstallPathRejectsAnEscapingLeafSymlink(t *testing.T) {
	installDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The link sits inside the install dir; its target does not.
	link := filepath.Join(installDir, "engine")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if isManagedInstallPath(link, installDir) {
		t.Error("a symlink inside the install directory pointing outside it was judged " +
			"managed; that would let a planted link nominate an external file")
	}

	// And the same file reached by its real path is still correctly outside.
	if isManagedInstallPath(outside, installDir) {
		t.Error("a file outside the install directory was judged inside it")
	}

	// A real file inside the directory is still managed, so the check has not
	// simply become "always false".
	real := filepath.Join(installDir, "genuine")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isManagedInstallPath(real, installDir) {
		t.Error("a real binary inside the install directory was not judged managed")
	}
}

// TestManagedInstallPathFollowsASymlinkedInstallDir pins the opposite answer one
// level up, so the asymmetry with the leaf rule is a recorded decision.
//
// When the install directory ITSELF is a symlink, its target is treated as the
// managed location: a binary there is managed, and an uninstall removes the real
// files rather than leaving them behind a deleted link. That is what someone who
// points engine-bin at a larger disk needs. A symlink one level down, pointing
// out of the directory, is still refused — the difference is that PAIR owns the
// install directory it was configured with, and does not own whatever a link
// inside it happens to reference.
func TestManagedInstallPathFollowsASymlinkedInstallDir(t *testing.T) {
	target := t.TempDir()
	installDir := filepath.Join(t.TempDir(), "engine-bin")
	if err := os.Symlink(target, installDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	bin := filepath.Join(target, "ollama")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Reached by either name, the binary is inside the managed directory.
	if !isManagedInstallPath(bin, installDir) {
		t.Error("a binary in the target of a symlinked install directory was judged outside it")
	}
	if !isManagedInstallPath(filepath.Join(installDir, "ollama"), installDir) {
		t.Error("a binary reached through the install-directory symlink was judged outside it")
	}

	// And something genuinely elsewhere is still refused.
	outside := filepath.Join(t.TempDir(), "ollama")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isManagedInstallPath(outside, installDir) {
		t.Error("a binary outside the target was judged managed")
	}
}

func TestIsOurEngineImage(t *testing.T) {
	bin := filepath.Join("/opt", "nvpair", "ollama.exe")
	cases := []struct {
		image string
		want  bool
	}{
		{bin, true},
		{`/opt/nvpair/OLLAMA.EXE`, true},
		{bin + ` (deleted)`, true},
		{`/opt/nvpair/other.exe`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := isOurEngineImage(tc.image, bin); got != tc.want {
			t.Errorf("isOurEngineImage(%q, %q) = %v, want %v", tc.image, bin, got, tc.want)
		}
	}
	if isOurEngineImage(bin, ``) {
		t.Fatal("empty binPath must not match")
	}
}

func TestIsManagedInstallPath(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "ollama", "bin", "ollama"+exeExt())
	outside := filepath.Join(t.TempDir(), "ollama"+exeExt())
	if !isManagedInstallPath(inside, filepath.Join(base, "ollama")) {
		t.Fatalf("managed child path %q was rejected", inside)
	}
	if isManagedInstallPath(outside, filepath.Join(base, "ollama")) {
		t.Fatalf("external path %q was accepted", outside)
	}
	if isManagedInstallPath("", filepath.Join(base, "ollama")) {
		t.Fatal("empty binary path must not be managed")
	}
}
