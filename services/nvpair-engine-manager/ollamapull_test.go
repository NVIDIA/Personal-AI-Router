// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
)

// Cancelling a download removes the partial blobs that download created and
// nothing else. A completed blob can be a layer of an installed model, and a
// partial for a digest this pull never reported belongs to another transfer.
func TestOllamaPartialCleanupRemovesOnlyThisPullsPartials(t *testing.T) {
	root := t.TempDir()
	expectKeep := []string{
		writeBlob(t, root, blobA),
		writeBlob(t, root, blobB+"-partial"),
	}
	before, err := ollamaPartialSnapshot(root)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	expectDelete := []string{
		writeBlob(t, root, blobA+"-partial"),
		writeBlob(t, root, blobA+"-partial-0"),
	}

	// "../escape" stands for a digest the engine never could have reported; it
	// must not be turned into a path.
	digests := map[string]bool{digestA: true, "../escape": true}
	busy, err := cleanupOllamaPartials(settledContext(), root, digests, before)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if busy {
		t.Error("settled partials were reported busy")
	}
	for _, path := range expectDelete {
		assertRemoved(t, path)
	}
	for _, path := range expectKeep {
		assertPresent(t, path)
	}
}

// A partial already on disk when the download started is not that download's to
// delete. Blobs are content-addressed, so it is either another client's
// transfer — which can sit still for far longer than any settle window while it
// waits on a slow server — or one PAIR deliberately kept when an earlier
// attempt's context died without a cancel request. Only the files a pull
// creates are unambiguously its own.
func TestOllamaPartialCleanupPreservesPartialsOlderThanThePull(t *testing.T) {
	root := t.TempDir()
	stalled := writeBlob(t, root, blobA+"-partial")
	before, err := ollamaPartialSnapshot(root)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}

	busy, err := cleanupOllamaPartials(settledContext(), root, map[string]bool{digestA: true}, before)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if busy {
		t.Error("a partial the snapshot excluded was reported busy")
	}
	assertPresent(t, stalled)
}

// Without a snapshot nothing can be attributed to the pull, so cancelling it
// deletes nothing rather than guess at ownership.
func TestOllamaPartialCleanupWithoutASnapshotDeletesNothing(t *testing.T) {
	root := t.TempDir()
	partial := writeBlob(t, root, blobA+"-partial")

	busy, err := cleanupOllamaPartials(settledContext(), root, map[string]bool{digestA: true}, nil)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if busy {
		t.Error("cleanup that ran on no snapshot reported busy")
	}
	assertPresent(t, partial)
}

// Ollama coalesces a concurrent pull of the same layer — from its CLI, its
// desktop app, the TUI's own engine-manager, or any other client on the daemon
// — onto the very same partial file, including one this pull created. A file
// still growing after this transfer stopped is therefore shared, so it survives
// and is reported busy for the caller to look at again.
func TestOllamaPartialCleanupPreservesAPartialAnotherClientIsWriting(t *testing.T) {
	root := t.TempDir()
	before, err := ollamaPartialSnapshot(root)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	shared := writeBlob(t, root, blobC+"-partial")
	ctx := withPartialSettle(context.Background(), func() { growFile(t, shared) })

	busy, err := cleanupOllamaPartials(ctx, root, map[string]bool{digestC: true}, before)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !busy {
		t.Error("a partial another client was still writing was not reported busy")
	}
	assertPresent(t, shared)
}
