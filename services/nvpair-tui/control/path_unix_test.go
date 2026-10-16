// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package control

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortTempDir gives a temp directory whose path leaves room for a socket
// name. t.TempDir() on macOS sits under /var/folders/... and, once the
// per-user data directory is appended, can pass the 104-byte sun_path limit —
// which is a real constraint of this endpoint, not an artefact of the test.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nvctl")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestDefaultPathPrefersTheRuntimeDirectory(t *testing.T) {
	runtimeDir := shortTempDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := filepath.Join(runtimeDir, "nvpair", "tui.sock")
	if got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathFallsBackToThePerUserDataDirectory(t *testing.T) {
	base := shortTempDir(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", base)

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	// The tail is what matters: every PAIR component agrees on the vendor and
	// product directories, and this endpoint lives in a run/ subdirectory
	// beneath them.
	wantTail := filepath.Join("Nvidia Corporation", "Personal AI Router", "run", "tui.sock")
	if !strings.HasSuffix(got, wantTail) {
		t.Errorf("DefaultPath() = %q, want it to end in %q", got, wantTail)
	}
	if !strings.HasPrefix(got, base) {
		t.Errorf("DefaultPath() = %q, want it under the per-user base %q", got, base)
	}
}

func TestListenRejectsAPathTheKernelWouldTruncate(t *testing.T) {
	long := filepath.Join("/tmp", strings.Repeat("d", maxSocketPath), "tui.sock")
	_, err := Listen(long)
	if err == nil {
		t.Fatal("Listen accepted a path past the sun_path limit")
	}
	for _, want := range []string{"limit", "--control-socket", "--no-control-socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Dir(long)); err == nil {
		t.Error("Listen created a directory for a path it was going to reject")
	}
}

func TestListenCreatesAPrivateEndpoint(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "nested", "tui.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if mode := info.Mode().Perm(); mode != socketPerm {
		t.Errorf("socket mode = %o, want %o: only its owner may drive pairing", mode, socketPerm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat the directory: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != dirPerm {
		t.Errorf("directory mode = %o, want %o", mode, dirPerm)
	}
}

func TestListenRefusesToStealALiveEndpoint(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "tui.sock")
	first, err := Listen(path)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := Listen(path)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second nvpair-tui took over a socket another one is serving")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("error = %q, want it to say another instance holds the endpoint", err)
	}
	// The live endpoint must still be there and still answering.
	if !InUse(path) {
		t.Error("the first instance's endpoint was removed by the second's attempt")
	}
}

func TestListenReclaimsASocketLeftByADeadInstance(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "tui.sock")

	// A TUI that was killed leaves the socket file behind with nothing
	// answering on it — SetUnlinkOnClose(false) reproduces exactly that.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("create the stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close the stale listener: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket file should still exist: %v", err)
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen did not reclaim a stale socket: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if !InUse(path) {
		t.Error("the reclaimed endpoint does not answer")
	}
}

func TestClosingTheListenerRemovesTheSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "tui.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket outlived its listener (stat err = %v)", err)
	}
}

func TestInUseIsFalseForAPathWithNoEndpoint(t *testing.T) {
	if InUse(filepath.Join(shortTempDir(t), "nothing.sock")) {
		t.Error("InUse reported an endpoint where there is no file at all")
	}
}
