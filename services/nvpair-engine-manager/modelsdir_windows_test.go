// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// junction creates link as a directory junction to dir. Unlike a symlink, a
// junction needs no privilege, which is what makes it the way a standard user
// would aim the elevated uninstaller somewhere else.
func junction(t *testing.T, link, dir string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput()
	require.NoError(t, err, "mklink /J %s %s: %s", link, dir, out)
}

// victimDir makes a directory with one file in it and returns the file, which
// must survive whatever removal a junction pointed at the directory took part in.
func victimDir(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	keep := filepath.Join(dir, "keep-me.txt")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))
	return keep
}

// TestRemoveTreePreservingUnlinksJunctionedTarget is the guard for steering the
// elevated uninstaller through a junction. Go reports a junction as an irregular
// file, not a symlink, so a check for symlinks alone let the removal follow one
// into the directory it names and delete what was there.
func TestRemoveTreePreservingUnlinksJunctionedTarget(t *testing.T) {
	root := t.TempDir()
	keep := victimDir(t, filepath.Join(root, "elsewhere"))
	link := filepath.Join(root, ".lmstudio")
	junction(t, link, filepath.Dir(keep))

	require.NoError(t, removeTreePreserving(link, filepath.Join(link, "models")), "removeTreePreserving")

	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "the junction survived")
	assert.FileExists(t, keep, "followed the junction and deleted the target")
}

// TestRemoveTreePreservingUnlinksJunctionOnTheWayToTheStore checks the same for
// a junction met while descending toward the model store, where the removal
// recurses rather than starting.
func TestRemoveTreePreservingUnlinksJunctionOnTheWayToTheStore(t *testing.T) {
	root := t.TempDir()
	keep := victimDir(t, filepath.Join(root, "elsewhere"))
	engineRoot := filepath.Join(root, ".lmstudio")
	require.NoError(t, os.MkdirAll(engineRoot, 0o755))
	link := filepath.Join(engineRoot, "data")
	junction(t, link, filepath.Dir(keep))

	require.NoError(t, removeTreePreserving(engineRoot, filepath.Join(link, "models")), "removeTreePreserving")

	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "the junction survived")
	assert.FileExists(t, keep, "followed the junction and deleted the target")
}
