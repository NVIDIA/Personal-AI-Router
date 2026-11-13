// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unreadablePath is a path os.Stat rejects with something other than "does not
// exist". The errors this stands in for — a permission denial on the vendor's
// cache, an I/O error on a failing disk — cannot be provoked portably, and the
// distinction under test is only between "absent" and "could not be read", so
// any non-NotExist error exercises it. Go rejects an interior NUL on every
// platform.
const unreadablePath = "partial\x00name"

// withPartialSettle replaces the real wait between cleanup's samples. A case
// where nothing is writing installs a no-op so the samples cost nothing; one
// that models another client still writing appends from the hook, which lands
// between two samples on every run where a background goroutine racing a real
// sleep only usually does.
func withPartialSettle(ctx context.Context, settle func()) context.Context {
	return context.WithValue(ctx, partialSettleKey{}, settle)
}

// settledContext is the context for cleanup that has nothing to wait for.
func settledContext(t *testing.T) context.Context {
	return withPartialSettle(t.Context(), func() {})
}

// listing is a candidates function for removeQuiescentPartials that names the
// same files at every sample, each a group of its own.
func listing(paths ...string) func() ([]partialGroup, error) {
	return func() ([]partialGroup, error) { return eachFile(paths), nil }
}

// group is a candidates function that names the same files at every sample,
// as one group removed in the order given.
func group(files ...string) func() ([]partialGroup, error) {
	return func() ([]partialGroup, error) {
		return []partialGroup{{name: files[len(files)-1], files: files}}, nil
	}
}

func writePartial(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
}

// growFile appends to path, standing in for another client still writing it.
func growFile(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	require.NoError(t, err)
	_, err = file.WriteString("more")
	assert.NoError(t, err)
	assert.NoError(t, file.Close())
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	assert.ErrorIs(t, err, fs.ErrNotExist, "%s should have been removed", filepath.Base(path))
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	assert.NoError(t, err, "%s should have been kept", filepath.Base(path))
}

// blockPartialRemoval makes the unlink of path fail until unblock is called or
// the test ends, standing in for the sharing violations and permission errors
// cleanup meets in the field. The platforms need different levers: Windows
// refuses to delete a file that has an open handle which did not opt into
// FILE_SHARE_DELETE, which is the handle Go's Open returns, while Unix refuses
// the unlink when the parent directory is not writable.
func blockPartialRemoval(t *testing.T, path string) (unblock func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		file, err := os.Open(path)
		require.NoError(t, err)
		unblock = sync.OnceFunc(func() { _ = file.Close() })
		t.Cleanup(unblock)
		return unblock
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the unlink cannot be made to fail")
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0500))
	unblock = sync.OnceFunc(func() { _ = os.Chmod(dir, info.Mode().Perm()) })
	t.Cleanup(unblock)
	return unblock
}

func TestRemoveQuiescentPartials_RemovesFilesThatHoldStill(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.part")
	second := filepath.Join(root, "second.part")
	writePartial(t, first, "data")
	writePartial(t, second, "data")

	require.NoError(t, removeQuiescentPartials(settledContext(t), listing(first, second)))

	assertRemoved(t, first)
	assertRemoved(t, second)
}

// Both vendors coalesce a concurrent download of the same content onto the
// same partial file, so one still growing after this transfer stopped is
// shared. Leaving it is the safe outcome rather than a failed cleanup, and
// cleanup stops watching it once the grace shows the writer is not this pull's.
func TestRemoveQuiescentPartials_KeepsAFileAnotherClientIsWriting(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared.part")
	writePartial(t, shared, "data")
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		growFile(t, shared)
	})

	require.NoError(t, removeQuiescentPartials(ctx, listing(shared)))

	assertPresent(t, shared)
	assert.Equal(t, partialGraceSamples, waits, "cleanup should stop watching a file that moves after the grace")
}

// Engines release their writers asynchronously, so the transfer this pull
// stopped can land a few last writes as cleanup starts. One of those restarts
// the wait rather than sparing the file, which goes once it has held still for
// a full quiet period after it.
func TestRemoveQuiescentPartials_WaitsOutTheStoppedTransfersLastWrites(t *testing.T) {
	partial := filepath.Join(t.TempDir(), "stopped.part")
	writePartial(t, partial, "data")
	waits := 0
	presentAFullQuietPeriodLater := false
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		switch waits {
		case partialGraceSamples - 1:
			// The grace's last sample sees this write.
			growFile(t, partial)
		case partialGraceSamples - 1 + partialQuietSamples:
			// The last wait before that write's quiet period completes.
			_, err := os.Stat(partial)
			presentAFullQuietPeriodLater = err == nil
		}
	})

	require.NoError(t, removeQuiescentPartials(ctx, listing(partial)))

	assert.True(t, presentAFullQuietPeriodLater, "a write during the grace should restart the file's quiet period")
	assertRemoved(t, partial)
}

// Once the grace is over this pull's writer has stopped, so a write is another
// client resuming the file: both vendors coalesce a new download onto a partial
// already there. Holding still again afterwards proves nothing, since a
// transfer waiting on a slow server pauses for longer than any quiet period.
func TestRemoveQuiescentPartials_KeepsAFileThatChangesAfterTheGrace(t *testing.T) {
	partial := filepath.Join(t.TempDir(), "resumed.part")
	writePartial(t, partial, "data")
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		if waits == partialQuietSamples {
			// The file has matched its baseline at every sample but the one
			// that would have cleared it for removal.
			growFile(t, partial)
		}
	})

	require.NoError(t, removeQuiescentPartials(ctx, listing(partial)))

	assertPresent(t, partial)
}

// A transfer does not write continuously. A client sharing this partial can
// pause for most of a quiet period and then resume, again and again. Restarting
// the wait at each write would leave the file likeliest to be deleted precisely
// when cleanup has been watching it longest.
func TestRemoveQuiescentPartials_KeepsAFileAnIntermittentWriterResumes(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared.part")
	writePartial(t, shared, "data")
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		if waits%(partialQuietSamples-1) == 0 {
			growFile(t, shared)
		}
	})

	require.NoError(t, removeQuiescentPartials(ctx, listing(shared)))

	assertPresent(t, shared)
}

// The candidates are listed again at every sample, so a partial whose first
// bytes are flushed as the transfer stops, within the grace, is still this
// pull's to remove.
func TestRemoveQuiescentPartials_RemovesACandidateThatAppearsLate(t *testing.T) {
	root := t.TempDir()
	early := filepath.Join(root, "early.part")
	late := filepath.Join(root, "late.part")
	writePartial(t, early, "data")
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		if waits == 2 {
			writePartial(t, late, "data")
		}
	})
	candidates := func() ([]partialGroup, error) {
		paths := []string{early}
		if _, err := os.Stat(late); err == nil {
			paths = append(paths, late)
		}
		return eachFile(paths), nil
	}

	require.NoError(t, removeQuiescentPartials(ctx, candidates))

	assertRemoved(t, early)
	assertRemoved(t, late)
}

// Listing no candidates yet proves nothing while the stopped transfer's grace
// lasts: a partial whose first bytes are flushed as the transfer stops is this
// pull's to remove, though nothing else was waiting on cleanup.
func TestRemoveQuiescentPartials_RemovesALatePartialWhenNothingElseIsListed(t *testing.T) {
	late := filepath.Join(t.TempDir(), "late.part")
	flushed := false
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		if waits == partialGraceSamples-2 {
			writePartial(t, late, "data")
			flushed = true
		}
	})
	candidates := func() ([]partialGroup, error) {
		if _, err := os.Stat(late); err == nil {
			return eachFile([]string{late}), nil
		}
		return nil, nil
	}

	require.NoError(t, removeQuiescentPartials(ctx, candidates))

	assert.True(t, flushed, "cleanup should still be looking when the late partial is flushed")
	assertRemoved(t, late)
}

// Once the grace is over this pull's writer is gone, so a partial first
// appearing after it is another client starting the same download rather than
// this pull's late flush.
func TestRemoveQuiescentPartials_KeepsACandidateThatFirstAppearsAfterTheGrace(t *testing.T) {
	root := t.TempDir()
	early := filepath.Join(root, "early.part")
	late := filepath.Join(root, "late.part")
	writePartial(t, early, "data")
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		if waits == partialGraceSamples {
			writePartial(t, late, "data")
		}
	})
	candidates := func() ([]partialGroup, error) {
		paths := []string{early}
		if _, err := os.Stat(late); err == nil {
			paths = append(paths, late)
		}
		return eachFile(paths), nil
	}

	require.NoError(t, removeQuiescentPartials(ctx, candidates))

	assertRemoved(t, early)
	assertPresent(t, late)
}

// A file that cannot be read is not a file that is gone. Treating the two
// alike let cleanup report success over a partial it never managed to look at
// and stop at once, where an unknown file is worth looking at again for as long
// as the budget allows. Neither is a failed cleanup. An absent file is looked
// for until the grace is over, since the stopped transfer may yet flush it.
func TestRemoveQuiescentPartials_SamplesUntilEveryCandidateIsKnownToBeGone(t *testing.T) {
	test := func(name, candidate string, wantWaits int) {
		t.Run(name, func(t *testing.T) {
			waits := 0
			ctx := withPartialSettle(t.Context(), func() { waits++ })

			require.NoError(t, removeQuiescentPartials(ctx, listing(candidate)))

			assert.Equal(t, wantWaits, waits)
		})
	}
	test("an absent file is looked for until the grace is over", filepath.Join(t.TempDir(), "absent.part"), partialGraceSamples-1)
	test("an unreadable file is sampled until the budget runs out", unreadablePath, partialCleanupSamples-1)
}

// One partial that cannot be removed must not strand the others behind it, and
// a removal still failing when the samples run out is reported.
func TestRemoveQuiescentPartials_ContinuesPastAFailedRemoval(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked.part")
	free := filepath.Join(t.TempDir(), "free.part")
	writePartial(t, blocked, "data")
	writePartial(t, free, "data")
	blockPartialRemoval(t, blocked)

	err := removeQuiescentPartials(settledContext(t), listing(blocked, free))

	assert.ErrorContains(t, err, "partial-file cleanup failed")
	assertPresent(t, blocked)
	assertRemoved(t, free)
}

// A group's files go in the order listed, and a removal that fails stops
// there. Ollama lists resume files before the data file: a data file left on
// its own is a fresh download to Ollama, while resume files left without their
// data file describe bytes that are gone.
func TestRemoveQuiescentPartials_StopsAGroupsRemovalAtAFileThatWillNotGo(t *testing.T) {
	resume := filepath.Join(t.TempDir(), "blob-partial-0")
	data := filepath.Join(t.TempDir(), "blob-partial")
	writePartial(t, resume, "chunk")
	writePartial(t, data, "data")
	blockPartialRemoval(t, resume)

	err := removeQuiescentPartials(settledContext(t), group(resume, data))

	assert.ErrorContains(t, err, "partial-file cleanup failed")
	assertPresent(t, resume)
	assertPresent(t, data)
}

// The members a failed removal left behind are retried at the next sample, and
// cleanup's own removals before the failure do not read as another client
// changing the group.
func TestRemoveQuiescentPartials_FinishesAGroupWhoseRemovalFailedPartWay(t *testing.T) {
	first := filepath.Join(t.TempDir(), "blob-partial-0")
	second := filepath.Join(t.TempDir(), "blob-partial-1")
	data := filepath.Join(t.TempDir(), "blob-partial")
	for _, path := range []string{first, second, data} {
		writePartial(t, path, "data")
	}
	unblock := blockPartialRemoval(t, second)
	waits := 0
	ctx := withPartialSettle(t.Context(), func() {
		waits++
		if waits == partialQuietSamples+1 {
			unblock()
		}
	})

	require.NoError(t, removeQuiescentPartials(ctx, group(first, second, data)))

	assert.Equal(t, partialQuietSamples+1, waits, "the retry should come at the sample after the failed removal")
	assertRemoved(t, first)
	assertRemoved(t, second)
	assertRemoved(t, data)
}

// Cleanup can only report what was true when it last looked. Both vendors
// coalesce a new download onto whatever partial is already there, so a file can
// be claimed in the gap before the unlink — and on Unix the unlink would
// succeed, leaving the new writer filling an inode with no name. The baseline
// is therefore rechecked immediately before the removal.
func TestRemoveIfUnchanged_SkipsAFileClaimedSinceItWasObserved(t *testing.T) {
	test := func(name string, claim func(t *testing.T, path string), wantRemoved, wantPresent bool) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "partial.part")
			writePartial(t, path, "data")
			baseline, err := os.Stat(path)
			require.NoError(t, err)
			claim(t, path)

			removed, err := removeIfUnchanged(path, baseline)

			require.NoError(t, err)
			assert.Equal(t, wantRemoved, removed)
			_, err = os.Stat(path)
			assert.Equal(t, wantPresent, err == nil, "file present")
		})
	}
	test("removes a file that still matches its baseline", func(*testing.T, string) {}, true, false)
	test("keeps a file another client resumed writing", growFile, false, true)
	// Already gone is a completed removal, not a failure: something else
	// finished the job and the caller has nothing left to retry.
	test("counts a file someone else removed first as removed", func(t *testing.T, path string) {
		require.NoError(t, os.Remove(path))
	}, true, false)
}
