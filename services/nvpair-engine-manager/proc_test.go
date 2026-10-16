// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestProcImageResolvesThisProcess checks that procImage returns the executable
// behind a live PID. The ownership check fails closed on an empty image, so
// without it a listener on PAIR's own managed port can never be confirmed as
// PAIR's engine. The test binary is the process whose executable is known
// independently.
func TestProcImageResolvesThisProcess(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skipf("procImage has no implementation on %s", runtime.GOOS)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	got := procImage(os.Getpid())
	if got == "" {
		t.Fatalf("procImage returned nothing for this process on %s; the ownership "+
			"check fails closed on an empty image, so stop and uninstall are refused",
			runtime.GOOS)
	}
	if !isOurEngineImage(got, want) {
		t.Errorf("procImage = %q, want the running binary %q", got, want)
	}
}

// TestProcImageRejectsInvalidPID checks that an unresolvable PID yields no image,
// so it can never be mistaken for a match.
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

// writeFakeBinary creates an executable file named name in dir and returns its
// path.
func writeFakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// symlinkedEngine writes an engine binary and returns it twice: canonically, as
// the OS reports a running process, and through a symlinked directory, the shape
// a configured /var/... path has against the kernel's /private/var/... on macOS.
func symlinkedEngine(t *testing.T) (canonical, viaLink string) {
	t.Helper()
	realDir := t.TempDir()
	bin := writeFakeBinary(t, realDir, "engine")
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatalf("resolve engine path: %v", err)
	}
	return canonical, filepath.Join(linkDir, "engine")
}

// symlinkedInstallDir returns an install directory that is itself a symlink,
// and the directory it points to.
func symlinkedInstallDir(t *testing.T) (installDir, target string) {
	t.Helper()
	target = t.TempDir()
	installDir = filepath.Join(t.TempDir(), "engine-bin")
	if err := os.Symlink(target, installDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return installDir, target
}

// TestEngineImageMatchesABinaryConfiguredThroughASymlink checks that the
// canonical image the OS reports matches the same binary configured through a
// symlinked directory.
func TestEngineImageMatchesABinaryConfiguredThroughASymlink(t *testing.T) {
	canonical, viaLink := symlinkedEngine(t)
	if !isOurEngineImage(canonical, viaLink) {
		t.Errorf("reported image %q did not match the same binary configured as %q", canonical, viaLink)
	}
}

// TestEngineImageRejectsASameNamedBinaryElsewhere checks that resolving the
// configured path does not widen the match to a same-named binary in another
// directory, which would authorize killing an unrelated process.
func TestEngineImageRejectsASameNamedBinaryElsewhere(t *testing.T) {
	_, viaLink := symlinkedEngine(t)
	other := writeFakeBinary(t, t.TempDir(), "engine")
	if isOurEngineImage(other, viaLink) {
		t.Errorf("image %q matched configured binary %q", other, viaLink)
	}
}

// TestInstallPathContainsABinaryConfiguredThroughASymlink checks containment
// when the configured binary path goes through a symlinked directory.
func TestInstallPathContainsABinaryConfiguredThroughASymlink(t *testing.T) {
	canonical, viaLink := symlinkedEngine(t)
	installDir := filepath.Dir(canonical)
	if !isManagedInstallPath(viaLink, installDir) {
		t.Errorf("binary %q was judged outside its install directory %q", viaLink, installDir)
	}
}

// TestInstallPathRejectsAnEscapingLeafSymlink checks that a symlink inside the
// install directory that points outside it is judged outside, so a planted link
// cannot nominate an external file for a stop or an uninstall.
func TestInstallPathRejectsAnEscapingLeafSymlink(t *testing.T) {
	installDir := t.TempDir()
	outside := writeFakeBinary(t, t.TempDir(), "victim")
	link := filepath.Join(installDir, "engine")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if isManagedInstallPath(link, installDir) {
		t.Errorf("symlink %q to %q was judged inside %q", link, outside, installDir)
	}
}

// TestInstallPathFollowsASymlinkedInstallDir checks that when the install
// directory is itself a symlink, a binary in its target is managed. PAIR owns the
// directory it was configured with, wherever that points; it does not own what a
// link inside that directory points to.
func TestInstallPathFollowsASymlinkedInstallDir(t *testing.T) {
	installDir, target := symlinkedInstallDir(t)
	bin := writeFakeBinary(t, target, "ollama")
	if !isManagedInstallPath(bin, installDir) {
		t.Errorf("binary %q in the target of install directory %q was judged outside it", bin, installDir)
	}
}

// TestInstallPathContainsABinaryNamedThroughASymlinkedInstallDir checks the same
// binary reached by the install directory's own name.
func TestInstallPathContainsABinaryNamedThroughASymlinkedInstallDir(t *testing.T) {
	installDir, target := symlinkedInstallDir(t)
	writeFakeBinary(t, target, "ollama")
	viaLink := filepath.Join(installDir, "ollama")
	if !isManagedInstallPath(viaLink, installDir) {
		t.Errorf("binary %q was judged outside install directory %q", viaLink, installDir)
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
