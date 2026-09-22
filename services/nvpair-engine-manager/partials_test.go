// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// unreadablePath is a path os.Stat rejects with something other than "does not
// exist". The errors this stands in for — a permission denial on the vendor's
// cache, an I/O error on a failing disk — cannot be provoked portably, and the
// distinction under test is only between "absent" and "could not be read", so
// any non-NotExist error exercises it. Go rejects an interior NUL on every
// platform.
const unreadablePath = "partial\x00name"

// A file that cannot be read is not a file that is gone. Treating the two
// alike let cleanup report success over a partial it never managed to look at,
// which is the one outcome worse than leaving the file: the caller stops
// retrying and the cancel is acknowledged as complete.
func TestQuiescentPathsSeparatesUnreadableFromAbsent(t *testing.T) {
	root := t.TempDir()
	settled := writeBlob(t, root, blobA+"-partial")
	absent := filepath.Join(root, blobB+"-partial")

	stable, busy := quiescentPaths(settledContext(), []string{settled, absent, unreadablePath})

	if !busy {
		t.Error("a path that could not be read was not reported busy, so cleanup would stop retrying")
	}
	if len(stable) != 1 || stable[0].path != settled {
		t.Fatalf("stable = %v, want only %s", stable, filepath.Base(settled))
	}
}

// An absent path on its own settles: there is nothing left to remove, so the
// caller is done rather than retrying to the deadline.
func TestQuiescentPathsIgnoresAnAbsentPath(t *testing.T) {
	root := t.TempDir()
	absent := filepath.Join(root, blobA+"-partial")

	stable, busy := quiescentPaths(settledContext(), []string{absent})

	if busy {
		t.Error("an absent path was reported busy")
	}
	if len(stable) != 0 {
		t.Fatalf("stable = %v, want nothing", stable)
	}
}

// quiescentPaths can only report what was true when it last looked. Both
// vendors coalesce a new download onto whatever partial is already there, so a
// file can be claimed in the gap before the unlink — and on Unix the unlink
// would succeed, leaving the new writer filling an inode with no name. The
// observation is therefore rechecked immediately before the removal.
func TestRemoveIfUnchangedSkipsAFileClaimedSinceItWasObserved(t *testing.T) {
	test := func(name string, claim func(t *testing.T, path string), wantRemoved, wantPresent bool) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := writeBlob(t, root, blobA+"-partial")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("observe %s: %v", path, err)
			}
			claim(t, path)

			removed, err := removeIfUnchanged(stablePath{path: path, info: info})
			if err != nil {
				t.Fatalf("remove: %v", err)
			}
			if removed != wantRemoved {
				t.Errorf("removed = %v, want %v", removed, wantRemoved)
			}
			if _, err := os.Stat(path); (err == nil) != wantPresent {
				t.Errorf("file present = %v, want %v", err == nil, wantPresent)
			}
		})
	}
	test("still matches the observation", func(*testing.T, string) {}, true, false)
	test("another client resumed writing it", growFile, false, true)
	// Already gone is a completed removal, not a failure: something else
	// finished the job and the caller has nothing left to retry.
	test("removed by someone else first", func(t *testing.T, path string) {
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove %s: %v", path, err)
		}
	}, true, false)
}
