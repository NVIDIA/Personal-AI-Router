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
//     hands removeQuiescentPartials. Files that one transfer needs together,
//     such as Ollama's data file and the resume files describing its chunks,
//     are handed over as one group, and a group with any member that predates
//     the pull is left out whole.
//  2. Quiescence — is anyone still writing it? A file this pull created can
//     still be shared, because both vendors coalesce a concurrent download of
//     the same content onto the same file, so one that moves once this pull's
//     transfer has had time to stop is someone else's. A group moves when any
//     member does. That test is the same for every engine, so it lives here.
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
// transfer: a write then only restarts the file's wait, and cleanup keeps
// sampling through them even with nothing listed yet. After that this pull's
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

// partialGroup is partial files that cleanup keeps or removes together,
// because removing only some of them breaks the transfer they belong to. name
// identifies the group from one sample to the next while members come and go,
// and files are removed in the order listed.
type partialGroup struct {
	name  string
	files []string
}

// eachFile makes every path a group of its own, for an engine whose partial
// files do not depend on one another.
func eachFile(paths []string) []partialGroup {
	groups := make([]partialGroup, len(paths))
	for i, path := range paths {
		groups[i] = partialGroup{name: path, files: []string{path}}
	}
	return groups
}

// groupObservation is one sample of a group: each member present and how it
// looked, or the error that kept a member from being read.
type groupObservation struct {
	present map[string]os.FileInfo
	err     error
}

func observeGroup(files []string) groupObservation {
	present := make(map[string]os.FileInfo, len(files))
	for _, path := range files {
		now := observe(path)
		switch {
		case now.gone():
		case now.err != nil:
			return groupObservation{err: now.err}
		default:
			present[path] = now.info
		}
	}
	return groupObservation{present: present}
}

// gone reports whether every member is known to be absent.
func (o groupObservation) gone() bool { return o.err == nil && len(o.present) == 0 }

// changedFrom reports whether a member appeared, vanished, or moved since the
// baseline. An unreadable member counts as a change, as it does for one file.
func (o groupObservation) changedFrom(baseline map[string]os.FileInfo) bool {
	if o.err != nil || len(o.present) != len(baseline) {
		return true
	}
	for path, info := range o.present {
		was, ok := baseline[path]
		if !ok || (observation{info: info}).changedFrom(was) {
			return true
		}
	}
	return false
}

// quietGroup is a candidate group's baseline — how each member looked when the
// group last changed — and how many samples in a row have found it exactly
// so. It is kept for the whole cleanup rather than taken afresh at intervals,
// so a write landing between any two samples is seen at the next one: within
// the grace it restarts the count, and after it marks the group as another
// client's, rather than handing a writer that paused a new chance to look
// finished.
type quietGroup struct {
	baseline map[string]os.FileInfo
	still    int
}

// removeQuiescentPartials deletes the groups candidates lists once each has
// held still for partialQuietSamples samples in a row, and leaves the rest in
// place: a group that changes or first appears after the stopped transfer's
// grace at once, and any other when partialCleanupSamples run out.
//
// candidates is the engine's attribution: the partial files this pull created,
// grouped by the transfer that needs them. It is asked again at every sample,
// because a candidate can appear late (this pull's writer flushing its first
// bytes as the transfer stops) or vanish (the engine discarding its own
// partial). Cleanup therefore never finishes inside the grace, even when
// nothing is listed.
//
// Only a failure is an error: a removal that kept failing, or candidates that
// could not be listed, at the last sample. A group another client is writing,
// or one still unreadable when the samples run out, is left in place as the
// safe outcome rather than a failed cleanup. One group that cannot be removed
// does not strand the others; each is tried at every sample.
func removeQuiescentPartials(ctx context.Context, candidates func() ([]partialGroup, error)) error {
	watched := make(map[string]*quietGroup)
	// shared holds the groups seen changing, or first seen, after the grace:
	// other clients' downloads, left to them.
	shared := make(map[string]bool)
	var failure error
	for sample := range partialCleanupSamples {
		if sample > 0 {
			waitForNextSample(ctx)
		}
		groups, err := candidates()
		if err != nil {
			failure = err
			continue
		}
		failure = nil
		pending := false
		listed := make(map[string]bool, len(groups))
		for _, group := range groups {
			listed[group.name] = true
			if shared[group.name] {
				continue
			}
			now := observeGroup(group.files)
			watch, seen := watched[group.name]
			switch {
			case now.gone():
				delete(watched, group.name)
				continue
			case now.err != nil:
				// Unknown is not absent. Look again rather than report the
				// group handled.
				delete(watched, group.name)
				pending = true
				continue
			case !seen || now.changedFrom(watch.baseline):
				if sample >= partialGraceSamples {
					// This pull's writer has had its grace, so whoever changed
					// this is still downloading it.
					delete(watched, group.name)
					shared[group.name] = true
					continue
				}
				watched[group.name] = &quietGroup{baseline: now.present}
				pending = true
				continue
			}
			watch.still++
			if watch.still < partialQuietSamples {
				pending = true
				continue
			}
			removed, err := watch.removeIfUnchanged(group.files)
			if err != nil {
				if failure == nil {
					failure = err
				}
				pending = true
				continue
			}
			if !removed {
				// Claimed between the last sample and the unlink.
				delete(watched, group.name)
				pending = true
			}
		}
		// A group that stopped being a candidate starts over if it returns.
		for name := range watched {
			if !listed[name] {
				delete(watched, name)
			}
		}
		// Having nothing to watch settles nothing while the grace lasts: the
		// stopped transfer can still flush a partial that is this pull's.
		if !pending && sample >= partialGraceSamples-1 {
			return nil
		}
	}
	if failure != nil {
		return fmt.Errorf("download stopped, but partial-file cleanup failed: %w", failure)
	}
	return nil
}

// removeIfUnchanged deletes a group's members in the order files lists them,
// provided the group still matches the baseline cleanup last saw it hold,
// reporting whether every member is now gone. A removal that fails stops
// there, leaving the members after it for the next sample, and each member
// removed comes out of the baseline so that retry does not read this
// cleanup's own removals as the group changing.
func (g *quietGroup) removeIfUnchanged(files []string) (removed bool, err error) {
	now := observeGroup(files)
	switch {
	case now.gone():
		return true, nil
	case now.changedFrom(g.baseline):
		return false, nil
	}
	for _, path := range files {
		baseline, ok := g.baseline[path]
		if !ok {
			continue
		}
		removed, err := removeIfUnchanged(path, baseline)
		if err != nil || !removed {
			return false, err
		}
		delete(g.baseline, path)
	}
	return true, nil
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
