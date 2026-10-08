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

func TestSafeRemoveUnderRootDeletesNestedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "publisher", "model")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, "weights.gguf"), []byte("x"), 0o644))
	require.NoError(t, safeRemoveUnderRoot(root, target), "safeRemoveUnderRoot")
	{
		_, err := os.Stat(target)
		require.ErrorIs(t, err, os.ErrNotExist, "target still exists after delete")
	}
}

func TestSafeRemoveUnderRootRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.Error(t, safeRemoveUnderRoot(root, filepath.Join(root, "..", filepath.Base(outside))), "expected traversal escape to fail")
}

func TestSafeRemoveUnderRootRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	require.NoError(t, os.Symlink(outside, link))
	require.Error(t, safeRemoveUnderRoot(root, link), "expected symlink escape to fail")
}
