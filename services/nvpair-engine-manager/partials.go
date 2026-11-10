// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// partials.go holds the engine-neutral half of partial-file cleanup: deciding
// whether a file is safe to delete. Which files are candidates in the first
// place is engine-specific and lives with that engine — ollamapull.go for
// Ollama's content-addressed blobs, lmspull.go for LM Studio's repository
// `.part` files.
//
// An engine's cleanup is expected to answer two questions in this order, and
// a new engine's should too:
//
//  1. Attribution — is this file ours? Each pull snapshots the partials
//     present before it starts, and only files that appeared afterwards are
//     candidates. The engine supplies this, as the candidates function it
//     hands removeQuiescentPartials.
//  2. Quiescence — is anyone still writing it? A file this pull created can
//     still be shared, because both vendors coalesce a concurrent download of
//     the same content onto the same file, so one that moves once this pull's
//     transfer has had time to stop is someone else's. That test is the same
//     for every engine, so it lives here.
//
// Deletion requires both. Every ambiguous case resolves toward keeping the
// file: a partial left behind costs disk space the vendor's own cleanup
// reclaims, while one deleted out from under a live download costs its bytes.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Cleanup samples its candidates partialSampleInterval apart, at most
// partialCleanupSamples times (about five seconds), and deletes a file only
// once partialQuietSamples samples in a row (about a second and a half) have
// found it exactly as it was.
//
// No single observation can tell a finished writer from a stalled one: a
// transfer waiting on a slow server, a large flush, or a machine that just woke
// looks exactly like one that has stopped. Requiring a run of still samples
// makes one write anywhere in that run enough to spare the file, which is the
// error to prefer. Attribution does the real work; this is the backstop for the
// one case attribution cannot settle, a file two clients are writing at once.
//
// Engines release their writers asynchronously once a transfer stops, so the
// first partialGraceSamples samples (about a second) belong to the stopped
// transfer: a write then only restarts the file's wait. After that this pull's
// writer is gone, so a candidate that changes or first appears is another
// client's download, kept however still it holds afterwards — a pause longer
// than the quiet period is no sign that it finished. What remains is a shared
// transfer that stalls within the grace and stays stalled through the quiet
// period, which nothing on disk tells apart from one that stopped. A Windows
// sharing violation on the unlink, the engine still holding the file, is
// retried at every sample.
const (
	partialSampleInterval = 250 * time.Millisecond
	partialGraceSamples   = 4
	partialQuietSamples   = 6
	partialCleanupSamples = 20
)

// partialSettleKey addresses the wait override a test installs on a context.
type partialSettleKey struct{}

// waitForNextSample waits out one sample interval. Cleanup runs after the pull's
// own context is already cancelled, so this deliberately does not observe
// ctx.Done: callers hand it a context.WithoutCancel of the pull's, and
// partialCleanupSamples is what limits the total wait.
func waitForNextSample(ctx context.Context) {
	if settle, ok := ctx.Value(partialSettleKey{}).(func()); ok {
		settle()
		return
	}
	time.Sleep(partialSampleInterval)
}

// observation is one stat of a candidate path. A path that could not be read
// is held distinct from one that is absent: "gone" means there is nothing left
// to do, while "unreadable" means we do not know, and the two must not lead to
// the same conclusion.
type observation struct {
	info os.FileInfo
	err  error
}

func observe(path string) observation {
	info, err := os.Stat(path)
	return observation{info: info, err: err}
}

// gone reports whether the path is known to be absent, as opposed to present
// or merely unreadable.
func (o observation) gone() bool { return errors.Is(o.err, fs.ErrNotExist) }

// changedFrom reports whether a file differs from its baseline. An observation
// that could not be read counts as changed: an unreadable file is not a still
// one, and treating it as stable would delete on no evidence.
func (o observation) changedFrom(baseline os.FileInfo) bool {
	return o.err != nil ||
		baseline.Size() != o.info.Size() ||
		!baseline.ModTime().Equal(o.info.ModTime())
}

// quietFile is a candidate's baseline — how it looked when it last changed —
// and how many samples in a row have found it exactly so. It is kept for the
// whole cleanup rather than taken afresh at intervals, so a write landing
// between any two samples is seen at the next one: within the grace it
// restarts the count, and after it marks the file as another client's, rather
// than handing a writer that paused a new chance to look finished.
type quietFile struct {
	baseline os.FileInfo
	still    int
}

// removeQuiescentPartials deletes the files candidates lists once each has held
// still for partialQuietSamples samples in a row, and leaves the rest in place:
// a file that changes or first appears after the stopped transfer's grace at
// once, and any other when partialCleanupSamples run out.
//
// candidates is the engine's attribution: the partial files this pull created.
// It is asked again at every sample, because a candidate can appear late (this
// pull's writer flushing its first bytes as the transfer stops) or vanish (the
// engine discarding its own partial).
//
// Only a failure is an error: a removal that kept failing, or candidates that
// could not be listed, at the last sample. A file another client is writing,
// or one still unreadable when the samples run out, is left in place as the
// safe outcome rather than a failed cleanup. One candidate that cannot be
// removed does not strand the others; each is tried at every sample.
func removeQuiescentPartials(ctx context.Context, candidates func() ([]string, error)) error {
	watched := make(map[string]*quietFile)
	// shared holds the candidates seen changing, or first seen, after the
	// grace: other clients' downloads, left to them.
	shared := make(map[string]bool)
	var failure error
	for sample := range partialCleanupSamples {
		if sample > 0 {
			waitForNextSample(ctx)
		}
		paths, err := candidates()
		if err != nil {
			failure = err
			continue
		}
		failure = nil
		pending := false
		listed := make(map[string]bool, len(paths))
		for _, path := range paths {
			listed[path] = true
			if shared[path] {
				continue
			}
			now := observe(path)
			watch, seen := watched[path]
			switch {
			case now.gone():
				delete(watched, path)
				continue
			case now.err != nil:
				// Unknown is not absent. Look again rather than report the
				// file handled.
				delete(watched, path)
				pending = true
				continue
			case !seen || now.changedFrom(watch.baseline):
				if sample >= partialGraceSamples {
					// This pull's writer has had its grace, so whoever wrote
					// this is still downloading it.
					delete(watched, path)
					shared[path] = true
					continue
				}
				watched[path] = &quietFile{baseline: now.info}
				pending = true
				continue
			}
			watch.still++
			if watch.still < partialQuietSamples {
				pending = true
				continue
			}
			removed, err := removeIfUnchanged(path, watch.baseline)
			if err != nil {
				if failure == nil {
					failure = err
				}
				pending = true
				continue
			}
			if !removed {
				// Claimed between the last sample and the unlink.
				delete(watched, path)
				pending = true
			}
		}
		// A file that stopped being a candidate starts over if it returns.
		for path := range watched {
			if !listed[path] {
				delete(watched, path)
			}
		}
		if !pending {
			return nil
		}
	}
	if failure != nil {
		return fmt.Errorf("download stopped, but partial-file cleanup failed: %w", failure)
	}
	return nil
}

// removeIfUnchanged deletes path only if it still matches the baseline cleanup
// last saw it hold, reporting whether the file is now gone.
//
// This narrows a race it cannot close. Between the last sample and the unlink,
// another client can open the file and resume writing it — both vendors
// coalesce a new download of the same content onto whatever partial is
// already there. Re-checking immediately before the unlink cuts the exposure
// from the sample interval to the gap between two adjacent syscalls. Closing
// it outright would need a lock protocol neither vendor participates in.
//
// The platforms fail differently, and only one fails safely. On Windows an
// open handle without FILE_SHARE_DELETE makes Remove return a sharing
// violation, so the file survives on its own. On Unix the unlink succeeds and
// a writer holding a descriptor goes on filling an inode with no name, losing
// its download silently — so on that side this check is the only guard there
// is.
func removeIfUnchanged(path string, baseline os.FileInfo) (removed bool, err error) {
	now := observe(path)
	switch {
	case now.gone():
		return true, nil
	case now.changedFrom(baseline):
		// Unreadable, or claimed since we looked. Either way it is not ours to
		// delete on this sample.
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}
