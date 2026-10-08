// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAtomicWriteCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.json")

	require.NoError(t, atomicWrite(path, []byte("first\n"), 0o600), "create")
	got, err := os.ReadFile(path)
	require.NoError(t, err, "read")
	require.Equal(t, "first\n", string(got), "got (%v)", got)

	require.NoError(t, atomicWrite(path, []byte("second\n"), 0o600), "replace")
	got, err = os.ReadFile(path)
	require.NoError(t, err, "read after replace")
	require.Equal(t, "second\n", string(got), "got (%v)", got)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "readdir")
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"), "leftover temp file")
	}
}

func TestAtomicWriteRetriesTransientRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.json")
	require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o600))

	var attempts atomic.Int32
	orig := renameFile
	renameFile = func(oldpath, newpath string) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("rename: Access is denied.")
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { renameFile = orig })

	require.NoError(t, atomicWrite(path, []byte("new\n"), 0o600), "atomicWrite")
	{
		got := attempts.Load()
		require.Equal(t, int32(3), got, "rename attempts")
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new\n", string(got), "got (%v)", got)
}

func TestAtomicWriteNonTransientRenameFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.json")

	var attempts atomic.Int32
	orig := renameFile
	renameFile = func(oldpath, newpath string) error {
		attempts.Add(1)
		return errors.New("rename: no such file or directory")
	}
	t.Cleanup(func() { renameFile = orig })

	err := atomicWrite(path, []byte("x\n"), 0o600)
	require.Error(t, err, "expected error")
	{
		got := attempts.Load()
		require.Equal(t, int32(1), got, "rename attempts")
	}
	require.ErrorContains(t, err, "no such file or directory")
}

func TestIsTransientReplaceError(t *testing.T) {
	require.True(t, isTransientReplaceError(errors.New("Access is denied.")), "expected Access is denied to be transient")
	require.True(t, isTransientReplaceError(errors.New("The process cannot access the file because it is being used by another process.")), "expected sharing text to be transient")
	require.False(t, isTransientReplaceError(errors.New("no such file or directory")), "ENOENT must not be treated as transient")
	require.False(t, isTransientReplaceError(nil), "nil must not be transient")
}
