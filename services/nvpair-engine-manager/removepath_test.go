// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSafeRemoveUnderRootDeletesNestedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "publisher", "model")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "weights.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := safeRemoveUnderRoot(root, target); err != nil {
		t.Fatalf("safeRemoveUnderRoot: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target still exists after delete: %v", err)
	}
}

func TestSafeRemoveUnderRootRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := safeRemoveUnderRoot(root, filepath.Join(root, "..", filepath.Base(outside))); err == nil {
		t.Fatal("expected traversal escape to fail")
	}
}

func TestSafeRemoveUnderRootRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := safeRemoveUnderRoot(root, link); err == nil {
		t.Fatal("expected symlink escape to fail")
	}
}
