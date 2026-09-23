// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
)

// cleanupRun drives the consecutive passes a candidate must hold still through
// before cleanup will remove it, which is what cleanupOllamaAfterCancel does
// inside its budget, and reports the last pass's busy result.
func cleanupRun(
	t *testing.T,
	ctx context.Context,
	root string,
	digests map[string]bool,
	before ollamaPartialsBefore,
) bool {
	t.Helper()
	steady := make(ollamaSteadyPasses)
	var busy bool
	for pass := range partialCleanupSteadyPasses {
		var err error
		busy, err = cleanupOllamaPartials(ctx, root, digests, before, steady)
		if err != nil {
			t.Fatalf("cleanup pass %d: %v", pass, err)
		}
	}
	return busy
}

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
	if cleanupRun(t, settledContext(), root, digests, before) {
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

	if cleanupRun(t, settledContext(), root, map[string]bool{digestA: true}, before) {
		t.Error("a partial the snapshot excluded was reported busy")
	}
	assertPresent(t, stalled)
}

// Without a snapshot nothing can be attributed to the pull, so cancelling it
// deletes nothing rather than guess at ownership.
func TestOllamaPartialCleanupWithoutASnapshotDeletesNothing(t *testing.T) {
	root := t.TempDir()
	partial := writeBlob(t, root, blobA+"-partial")

	if cleanupRun(t, settledContext(), root, map[string]bool{digestA: true}, nil) {
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

	if !cleanupRun(t, ctx, root, map[string]bool{digestC: true}, before) {
		t.Error("a partial another client was still writing was not reported busy")
	}
	assertPresent(t, shared)
}

// A transfer does not write continuously. The client sharing this partial goes
// quiet for a whole observation window and then resumes, and cleanup runs
// several passes inside its retry budget — so deciding afresh on each pass
// hands a paused writer a new chance to look finished every time, and the file
// is likeliest to be deleted precisely when cleanup tries hardest.
func TestOllamaPartialCleanupKeepsAPartialSharedByAnIntermittentWriter(t *testing.T) {
	root := t.TempDir()
	before, err := ollamaPartialSnapshot(root)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	shared := writeBlob(t, root, blobC+"-partial")

	writing := true
	ctx := withPartialSettle(context.Background(), func() {
		if writing {
			growFile(t, shared)
		}
	})
	steady := make(ollamaSteadyPasses)
	for pass := range 4 {
		writing = pass%2 == 0
		if _, err := cleanupOllamaPartials(ctx, root, map[string]bool{digestC: true}, before, steady); err != nil {
			t.Fatalf("cleanup pass %d: %v", pass, err)
		}
	}
	assertPresent(t, shared)
}
