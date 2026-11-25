// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

// The half of stop_before that has to refuse: a port still answering after the process
// PAIR started is gone. It is Unix-only because of how a stop works here rather than
// what is being tested — a stop asks the engine's process group to leave and then
// force-kills the group, so the only thing that can still be holding the port is a
// process in no such group, and on Windows the stop is a taskkill /T /F, which ends the
// tree however a child was created. Nothing portable can stand in for a listener PAIR
// has no handle on and no right to kill, so the fake's holder goes in its own group.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// packOnDisk is a pack a filesystem removal can be pointed at: a directory in the
// test's own tree, named by the same cache id the delete passes.
func packOnDisk(t *testing.T) (root, repoID, dir string) {
	t.Helper()

	root = t.TempDir()
	repoID = "quiet-pack"
	dir = filepath.Join(root, repoID)
	require.NoError(t, os.MkdirAll(dir, 0o755), "create the pack to delete")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "weights.bin"), []byte("weights"), 0o644), "fill it")
	return root, repoID, dir
}

// stopLeakedHolder ends the helper that holds the port, whatever a test below does
// with it. It waits for the helper to have taken the port before it kills anything,
// because that is the moment the pid file appears.
func stopLeakedHolder(t *testing.T, pidFile string) {
	t.Helper()

	t.Cleanup(func() {
		for range 50 {
			body, err := os.ReadFile(pidFile)
			if err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(body))); err == nil {
					if proc, err := os.FindProcess(pid); err == nil {
						_ = proc.Kill()
					}
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// TestStopBeforeAction_LeavesTheRemovalForAListenerThatOutlivesTheEngine is the case
// the wait exists for: the process is gone, and the port still answers. Both removal
// kinds a manifest can declare are refused, because neither is safe against a pack a
// server still has mapped, and an error that says nothing happened is the only honest
// report.
func TestStopBeforeAction_LeavesTheRemovalForAListenerThatOutlivesTheEngine(t *testing.T) {
	t.Run("the engine's own removal command", func(t *testing.T) {
		holderPIDFile := filepath.Join(t.TempDir(), "holder.pid")
		stopLeakedHolder(t, holderPIDFile) // the holder is in its own group: nothing else will end it
		seam := newDeleteSeam(t, stoppingFirst(t, cliDelete()), deleteSeamOptions{
			serves:        deleteSeamPack,
			cacheRows:     []listedPack{deleteQuietPack},
			holderPIDFile: holderPIDFile,
		})
		shrinkQuietBound(t, 500*time.Millisecond)
		seam.run(t)

		_, err := seam.delete(t, deleteSeamRepoID)

		require.Error(t, err, "the pack was removed while the engine's port answered")
		assert.Contains(t, err.Error(), "delete_model")
		require.True(t, portIsAnswering(seam.port), "the test's own fixture failed: nothing was holding the port to refuse the removal over")
		assert.NotContains(t, strings.Join(seam.lines(t), "\n"), "remove",
			"the removal ran against a port that never went quiet: %v", seam.lines(t))
	})

	t.Run("a guarded filesystem path", func(t *testing.T) {
		root, repoID, dir := packOnDisk(t)
		holderPIDFile := filepath.Join(t.TempDir(), "holder.pid")
		stopLeakedHolder(t, holderPIDFile) // the holder is in its own group: nothing else will end it
		seam := newDeleteSeam(t, stoppingFirst(t, Action{
			RemovePath: &ActionRemovePath{Root: root, Path: filepath.Join(root, "{model}")},
		}), deleteSeamOptions{
			serves:        dir,
			cacheRows:     []listedPack{{RepoID: repoID, Path: dir}},
			holderPIDFile: holderPIDFile,
		})
		shrinkQuietBound(t, 500*time.Millisecond)
		seam.run(t)

		_, err := seam.delete(t, repoID)

		require.Error(t, err, "the pack's files were removed while the engine's port answered")
		require.True(t, portIsAnswering(seam.port), "the test's own fixture failed: nothing was holding the port")
		_, statErr := os.Stat(dir)
		assert.NoError(t, statErr, "a running server had that pack mapped and its files are gone")
	})
}
