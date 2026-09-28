// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errRenameHeld = errors.New("held by another process")

func stubRename(t *testing.T, failFirst int, transient func(error) bool) *int {
	t.Helper()
	oldRename, oldTransient, oldEvery, oldWindow := renameDir, isTransientRenameErr, renameRetryEvery, renameRetryWindow
	t.Cleanup(func() {
		renameDir, isTransientRenameErr, renameRetryEvery, renameRetryWindow = oldRename, oldTransient, oldEvery, oldWindow
	})
	renameRetryEvery, renameRetryWindow = time.Millisecond, 2*time.Second
	calls := 0
	renameDir = func(oldpath, newpath string) error {
		calls++
		if calls <= failFirst {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errRenameHeld}
		}
		return os.Rename(oldpath, newpath)
	}
	isTransientRenameErr = transient
	return &calls
}

// TestPromoteLlamaRuntimeRetriesTransientRename: a promotion denied while the
// staged executables are still held (Windows Defender on a lab node, twice)
// succeeds once the hold lifts instead of failing the whole install.
func TestPromoteLlamaRuntimeRetriesTransientRename(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, llamaExecutable()), []byte("bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := stubRename(t, 3, func(err error) bool { return errors.Is(err, errRenameHeld) })
	if err := promoteLlamaRuntime(root, stage); err != nil {
		t.Fatalf("promotion did not survive a transient hold: %v", err)
	}
	if *calls != 4 {
		t.Fatalf("rename attempts = %d, want 4 (three held, one success)", *calls)
	}
	if _, err := os.Stat(filepath.Join(root, "runtime", llamaExecutable())); err != nil {
		t.Fatalf("runtime was not promoted: %v", err)
	}
}

// A permanent error is returned at once: no retry loop hides a real failure.
func TestRenameWithRetryReturnsPermanentErrorAtOnce(t *testing.T) {
	calls := stubRename(t, 100, func(error) bool { return false })
	err := renameWithRetry(filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b"))
	if !errors.Is(err, errRenameHeld) || *calls != 1 {
		t.Fatalf("err=%v attempts=%d, want the original error after one attempt", err, *calls)
	}
}

// A hold that never lifts still fails, bounded by the retry window.
func TestRenameWithRetryGivesUpAfterTheWindow(t *testing.T) {
	calls := stubRename(t, 1_000_000, func(err error) bool { return errors.Is(err, errRenameHeld) })
	renameRetryWindow = 20 * time.Millisecond
	start := time.Now()
	err := renameWithRetry(filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b"))
	if !errors.Is(err, errRenameHeld) || *calls < 2 || time.Since(start) > time.Second {
		t.Fatalf("err=%v attempts=%d elapsed=%s, want the held error after a bounded retry", err, *calls, time.Since(start))
	}
}
