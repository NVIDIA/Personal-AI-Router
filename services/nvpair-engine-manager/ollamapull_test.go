// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Digests and blob names are written out in full rather than built with
// strings.Repeat, so a failure message can be grepped straight back to the case
// that produced it.
const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	blobA   = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	blobB   = "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	blobC   = "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// writeBlob seeds a file named like an Ollama blob in root.
func writeBlob(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	writePartial(t, path, "data")
	return path
}

// Cancelling a download removes the partial blobs that download created and
// nothing else. A completed blob can be a layer of an installed model, and a
// partial for a digest this pull never reported belongs to another transfer.
func TestCleanupOllamaAfterCancel_RemovesOnlyPartialsThisPullCreated(t *testing.T) {
	root := t.TempDir()
	completedLayer := writeBlob(t, root, blobA)
	otherTransfer := writeBlob(t, root, blobB+"-partial")
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)
	created := writeBlob(t, root, blobA+"-partial")
	createdChunk := writeBlob(t, root, blobA+"-partial-0")
	// "../escape" stands for a digest the engine never could have reported; it
	// must not be turned into a path.
	digests := map[string]bool{digestA: true, "../escape": true}

	require.NoError(t, cleanupOllamaAfterCancel(settledContext(), root, digests, before))

	assertRemoved(t, created)
	assertRemoved(t, createdChunk)
	assertPresent(t, completedLayer)
	assertPresent(t, otherTransfer)
}

// A partial already on disk when the download started is not that download's to
// delete. Blobs are content-addressed, so it is either another client's
// transfer — which can sit still for far longer than any quiet period while it
// waits on a slow server — or one PAIR deliberately kept when an earlier
// attempt's context died without a cancel request. Only the files a pull
// creates are unambiguously its own.
func TestCleanupOllamaAfterCancel_KeepsPartialsOlderThanThePull(t *testing.T) {
	root := t.TempDir()
	stalled := writeBlob(t, root, blobA+"-partial")
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)

	require.NoError(t, cleanupOllamaAfterCancel(settledContext(), root, map[string]bool{digestA: true}, before))

	assertPresent(t, stalled)
}

// Without a snapshot nothing can be attributed to the pull, so cancelling it
// deletes nothing rather than guess at ownership.
func TestCleanupOllamaAfterCancel_DeletesNothingWithoutASnapshot(t *testing.T) {
	root := t.TempDir()
	partial := writeBlob(t, root, blobA+"-partial")

	require.NoError(t, cleanupOllamaAfterCancel(settledContext(), root, map[string]bool{digestA: true}, nil))

	assertPresent(t, partial)
}

// Ollama coalesces a concurrent pull of the same layer — from its CLI, its
// desktop app, the TUI's own engine-manager, or any other client on the daemon
// — onto the very same partial file, including one this pull created. A file
// still growing after this transfer stopped is therefore shared, and survives.
func TestCleanupOllamaAfterCancel_KeepsAPartialAnotherClientIsWriting(t *testing.T) {
	root := t.TempDir()
	before, err := ollamaPartialSnapshot(root)
	require.NoError(t, err)
	shared := writeBlob(t, root, blobC+"-partial")
	ctx := withPartialSettle(context.Background(), func() { growFile(t, shared) })

	require.NoError(t, cleanupOllamaAfterCancel(ctx, root, map[string]bool{digestC: true}, before))

	assertPresent(t, shared)
}
