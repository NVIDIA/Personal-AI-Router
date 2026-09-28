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
//     candidates. This is what the engine supplies.
//  2. Quiescence — is anyone still writing it? A file this pull created can
//     still be shared, because both vendors coalesce a concurrent download of
//     the same content onto the same file. That test is the same for every
//     engine, so it lives here.
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

// A partial file is deleted only after it has matched its first observation at
// every one of partialSettleSamples checks, partialSettleInterval apart.
//
// No single observation can tell a finished writer from a stalled one: a
// transfer waiting on a slow server, a large flush, or a machine that just woke
// looks exactly like one that has stopped. Requiring several in a row makes one
// write anywhere in the window enough to spare the file, which is the error to
// prefer. Attribution does the real work; this is the backstop for the one case
// attribution cannot settle, a file two clients are writing at once.
const (
	partialSettleInterval = 250 * time.Millisecond
	partialSettleSamples  = 3
)

// partialSettleKey addresses the settle override a test installs on a context.
type partialSettleKey struct{}

// settlePartials waits out one observation window. Cleanup runs after the pull's
// own context is already cancelled, so this deliberately does not observe
// ctx.Done: callers hand it a context.WithoutCancel of the pull's, and the
// bounded retry in retryPartialCleanup is what limits the total wait.
func settlePartials(ctx context.Context) {
	if settle, ok := ctx.Value(partialSettleKey{}).(func()); ok {
		settle()
		return
	}
	time.Sleep(partialSettleInterval)
}

// observation is one stat of a candidate path. A path that could not be read
// is held distinct from one that is absent: "gone" means there is nothing left
// to do, while "unreadable" means we do not know, and the two must not lead to
// the same conclusion.
type observation struct {
	info os.FileInfo
	err  error
}

// gone reports whether the path is known to be absent, as opposed to present
// or merely unreadable.
func (o observation) gone() bool { return errors.Is(o.err, fs.ErrNotExist) }

// moved reports whether a file differs from when it was first observed. An
// observation that could not be read counts as moved: an unreadable file is
// not a still one, and treating it as stable would delete on no evidence.
func (o observation) moved(first os.FileInfo) bool {
	return o.err != nil ||
		first.Size() != o.info.Size() ||
		!first.ModTime().Equal(o.info.ModTime())
}

// statPaths observes every path, recording why each could not be read rather
// than dropping it. An entry is returned for every requested path.
func statPaths(paths []string) map[string]observation {
	observed := make(map[string]observation, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		observed[path] = observation{info: info, err: err}
	}
	return observed
}

// stablePath is a candidate that held still, carried with the observation it
// settled on so the removal can confirm nothing has changed since.
type stablePath struct {
	path string
	info os.FileInfo
}

// quiescentPaths splits paths into those whose size and modification time held
// still across every observation, and reports whether any of the rest is still
// unfinished business. A path that moved, or appeared mid-window, is another
// client's and the caller can look again; one that is gone is already handled;
// one that could not be read at all is unknown, which is reported as busy
// rather than quietly treated as success.
func quiescentPaths(ctx context.Context, paths []string) (stable []stablePath, busy bool) {
	watching := make(map[string]os.FileInfo, len(paths))
	for path, first := range statPaths(paths) {
		if first.err == nil {
			watching[path] = first.info
		}
	}
	for sample := 0; sample < partialSettleSamples && len(watching) > 0; sample++ {
		settlePartials(ctx)
		now := statPaths(paths)
		for path, first := range watching {
			if now[path].moved(first) {
				delete(watching, path)
			}
		}
	}
	final := statPaths(paths)
	for _, path := range paths {
		if info, quiet := watching[path]; quiet {
			stable = append(stable, stablePath{path: path, info: info})
			continue
		}
		if !final[path].gone() {
			busy = true
		}
	}
	return stable, busy
}

// removeIfUnchanged deletes path only if it still matches the observation
// quiescentPaths settled on, reporting whether the file is now gone.
//
// This narrows a race it cannot close. Between the final observation and the
// unlink, another client can open the file and resume writing it — both
// vendors coalesce a new download of the same content onto whatever partial is
// already there. Re-checking immediately before the unlink cuts the exposure
// from the settle window to the gap between two adjacent syscalls. Closing it
// outright would need a lock protocol neither vendor participates in.
//
// The platforms fail differently, and only one fails safely. On Windows an
// open handle without FILE_SHARE_DELETE makes Remove return a sharing
// violation, so the file survives on its own. On Unix the unlink succeeds and
// a writer holding a descriptor goes on filling an inode with no name, losing
// its download silently — so on that side this check is the only guard there
// is.
func removeIfUnchanged(candidate stablePath) (removed bool, err error) {
	now := statPaths([]string{candidate.path})[candidate.path]
	switch {
	case now.gone():
		return true, nil
	case now.moved(candidate.info):
		// Unreadable, or claimed since we looked. Either way it is not ours to
		// delete on this pass.
		return false, nil
	}
	if err := os.Remove(candidate.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// partialSteadyPasses counts, per path, how many cleanup passes in a row have
// found the file holding still. One cancellation's retry loop keeps one of
// these; it must not be nil, since a pass records into it.
type partialSteadyPasses map[string]int

// partialCleanupSteadyPasses is how many consecutive passes must find a
// candidate unchanged before it is deleted.
//
// Each pass observes its own window, so every extra pass is another chance for
// a writer that merely paused to look finished. Requiring a run means any
// movement resets the count, so a file has to hold still across the whole run
// to qualify.
const partialCleanupSteadyPasses = 2

// removeSettledPartials runs one cleanup pass over the files a pull created,
// deleting those that have now held still for partialCleanupSteadyPasses in a
// row. busy reports that something is left for another pass to look at. A
// failed removal does not stop the pass: the remaining candidates are still
// tried, and the first error is returned once they have been.
func removeSettledPartials(ctx context.Context, created []string, steady partialSteadyPasses) (busy bool, err error) {
	stable, busy := quiescentPaths(ctx, created)
	held := make(map[string]bool, len(stable))
	for _, candidate := range stable {
		held[candidate.path] = true
	}
	// Anything that moved, vanished, or stopped being a candidate this pass
	// starts its run over.
	for path := range steady {
		if !held[path] {
			delete(steady, path)
		}
	}
	for _, candidate := range stable {
		steady[candidate.path]++
		if steady[candidate.path] < partialCleanupSteadyPasses {
			busy = true
			continue
		}
		removed, removeErr := removeIfUnchanged(candidate)
		if removeErr != nil {
			busy = true
			if err == nil {
				err = removeErr
			}
			continue
		}
		if !removed {
			// Claimed between the last observation and the unlink.
			delete(steady, candidate.path)
			busy = true
		}
	}
	return busy, err
}

// partialCleanupRetryInterval spaces the cleanup passes after a cancellation
// and partialCleanupBudget bounds them. Engines release their writers
// asynchronously once a transfer stops, so a Windows sharing violation and a
// file that has not stopped moving yet both tend to resolve within a few
// passes. The budget holds several full settle windows, and a file still busy
// when it runs out belongs to someone else.
const (
	partialCleanupRetryInterval = 100 * time.Millisecond
	partialCleanupBudget        = 5 * time.Second
)

// retryPartialCleanup repeats pass until nothing is left busy or the budget
// runs out. A file still busy at the deadline is left in place; that is the
// only safe outcome, and is not a cleanup failure. A removal error still
// standing at the deadline is.
func retryPartialCleanup(pass func(partialSteadyPasses) (busy bool, err error)) error {
	steady := make(partialSteadyPasses)
	deadline := time.Now().Add(partialCleanupBudget)
	for {
		time.Sleep(partialCleanupRetryInterval)
		busy, err := pass(steady)
		if err == nil && !busy {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				return nil
			}
			return fmt.Errorf("download stopped, but partial-file cleanup failed: %w", err)
		}
	}
}
