// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupLMSAfterCancel_RemovesThePartialButKeepsCompletedShards(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owner", "repo")
	completeShard := filepath.Join(target, "model-00001-of-00002.gguf")
	writePartial(t, completeShard, "data")
	const model = "https://huggingface.co/owner/repo@Q4_K_M"
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	partial := filepath.Join(target, "downloading_model-Q4_K_M.gguf.part")
	writePartial(t, partial, "data")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, nil))

	assertRemoved(t, partial)
	assertPresent(t, completeShard)
}

func TestCleanupLMSAfterCancel_KeepsPartialsOlderThanThePull(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owner", "repo")
	olderPartial := filepath.Join(target, "downloading_other.gguf.part")
	writePartial(t, olderPartial, "other download")
	before, err := lmsPartialFiles(root, "owner/repo")
	require.NoError(t, err)
	newPartial := filepath.Join(target, "downloading_selected.gguf.part")
	writePartial(t, newPartial, "new download")
	named := map[string]bool{"selected.gguf": true, "other.gguf": true}

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, "owner/repo", before, named))

	assertPresent(t, olderPartial)
	assertRemoved(t, newPartial)
}

// An unpinned request lets the CLI choose the quantization, so a partial that
// appeared since the snapshot is only this download's when the CLI named its
// file. One it did not name can be the LM Studio app fetching another
// quantization of the same repository, even though it is new and holding still.
func TestCleanupLMSAfterCancel_UnpinnedRequestRemovesOnlyFilesTheCLINamed(t *testing.T) {
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	test := func(name string, named map[string]bool, wantOursRemoved bool) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
			require.NoError(t, os.MkdirAll(target, 0700))
			before, err := lmsPartialFiles(root, model)
			require.NoError(t, err)
			ours := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
			theirs := filepath.Join(target, "downloading_Qwen3-8B-Q8_0.gguf.part")
			writePartial(t, ours, "our download")
			writePartial(t, theirs, "the app's download, stalled")

			require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, named))

			if wantOursRemoved {
				assertRemoved(t, ours)
			} else {
				assertPresent(t, ours)
			}
			assertPresent(t, theirs)
		})
	}
	test("removes the named file and keeps the other", map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true}, true)
	test("deletes nothing when the CLI named no file", nil, false)
}

// The LM Studio app downloads into the same repository directory PAIR does, so
// cancelling a @Q4_K_M pull must not remove the @Q8_0 the app is fetching —
// neither the copy that grew since the snapshot nor one created after it.
func TestCleanupLMSAfterCancel_KeepsOtherQuantizations(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	growing := filepath.Join(target, "downloading_Qwen3-8B-Q8_0.gguf.part")
	writePartial(t, growing, "app download")
	const model = "lmstudio-community/Qwen3-8B-GGUF@Q4_K_M"
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	require.Empty(t, before, "another quantization was captured as this pull's")
	appeared := filepath.Join(target, "downloading_Qwen3-8B-Q6_K.gguf.part")
	ours := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, growing, "app download, now longer")
	writePartial(t, appeared, "app started this one after our snapshot")
	writePartial(t, ours, "our download")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, nil))

	assertPresent(t, growing)
	assertPresent(t, appeared)
	assertRemoved(t, ours)
}

// The app can also be fetching the very quantization PAIR asked for, into the
// same file. Carrying the requested quantization in its name is what makes a
// partial a candidate, not what makes it this download's, so one still growing
// after this transfer stopped has to survive too.
func TestCleanupLMSAfterCancel_KeepsTheRequestedQuantizationWhileAnotherClientWritesIt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF@Q4_K_M"
	require.NoError(t, os.MkdirAll(target, 0700))
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	shared := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, shared, "shared download")
	ctx := withPartialSettle(context.Background(), func() { growFile(t, shared) })

	require.NoError(t, cleanupLMSAfterCancel(ctx, root, model, before, nil))

	assertPresent(t, shared)
}

// Growing since the snapshot is not evidence of ownership. A writer that paused
// looks exactly like one that finished, so a partial already present when this
// pull started stays another client's however many bytes it gained meanwhile.
//
// The model hub pulls unpinned ids, so no quantization filter excludes
// anything — every partial in the repository reaches this rule, which is why it
// has to be the strict one.
func TestCleanupLMSAfterCancel_KeepsAPreexistingPartialThatGrew(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	theirs := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, theirs, "app download")
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	require.Len(t, before, 1, "the snapshot should hold the partial already present")
	// The app writes more while our download runs, then stalls, so the file is
	// perfectly still by the time cleanup observes it.
	growFile(t, theirs)

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true}))

	assertPresent(t, theirs)
}

// `lms get` resumes in place, so the same rule means a cancelled resume leaves
// its own partial behind. That is deliberate: nothing on disk tells a resume
// target apart from another client's file, the bytes stay useful to the next
// attempt, and the alternative is deleting a live download whenever the guess
// goes the other way. Files this pull actually created are still removed.
func TestCleanupLMSAfterCancel_LeavesAResumedDownloadsPartial(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	resuming := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, resuming, "bytes the attempt being resumed left behind")
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	growFile(t, resuming)
	fresh := filepath.Join(target, "downloading_Qwen3-8B-Q6_K.gguf.part")
	writePartial(t, fresh, "a shard this pull started from nothing")
	named := map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true, "Qwen3-8B-Q6_K.gguf": true}

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, named))

	assertPresent(t, resuming)
	assertRemoved(t, fresh)
}

// A nil snapshot means the inspection failed, not that the directory was empty.
// Attribution is impossible without one, so cleanup deletes nothing — the rule
// cleanupOllamaAfterCancel applies too.
func TestCleanupLMSAfterCancel_DeletesNothingWithoutASnapshot(t *testing.T) {
	root := t.TempDir()
	partial := filepath.Join(root, "owner", "repo", "downloading_model-Q4_K_M.gguf.part")
	writePartial(t, partial, "data")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, "owner/repo@Q4_K_M", nil, nil))

	assertPresent(t, partial)
}

// The models directory is declared by the engine's manifest. Without one there
// is no cache to attribute files in, and resolving the repository anyway would
// look for partials relative to the engine manager's working directory.
func TestLMSPartialFiles_RefusesAnUndeclaredModelsDirectory(t *testing.T) {
	files, err := lmsPartialFiles("", "owner/repo@Q4_K_M")

	assert.ErrorContains(t, err, "no models directory")
	assert.Nil(t, files, "a listing that could not be made must stay distinct from an empty one")
}
