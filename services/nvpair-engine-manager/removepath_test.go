// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafeRemoveUnderRoot_DeletesNestedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "publisher", "model")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, "weights.gguf"), []byte("x"), 0o644))
	require.NoError(t, safeRemoveUnderRoot(root, target), "safeRemoveUnderRoot")
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist, "target still exists after delete")
}

func TestSafeRemoveUnderRoot_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.Error(t, safeRemoveUnderRoot(root, filepath.Join(root, "..", filepath.Base(outside))), "expected traversal escape to fail")
}

func TestSafeRemoveUnderRoot_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	require.NoError(t, os.Symlink(outside, link))
	require.Error(t, safeRemoveUnderRoot(root, link), "expected symlink escape to fail")
}

// An empty model param resolves "{models_dir}/{model}" to the store itself;
// removing it would delete every model at once.
func TestSafeRemoveUnderRootRefusesTheRoot(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "o", "r", "config.json")
	if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{root, root + string(os.PathSeparator), filepath.Join(root, "o", "..")} {
		if err := safeRemoveUnderRoot(root, target); err == nil {
			t.Fatalf("safeRemoveUnderRoot(%q) removed the root", target)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the store lost a model: %v", err)
	}
}
