// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The OS-neutral entry point for user PATH changes, and the contract each
// platform implements.
//
// Two functions, both build-tagged, one pair per platform:
//
//	persistUserPath(dir, receipt, save) error  — publish dir on the user's PATH
//	removeUserPath(receipt) error              — withdraw what the receipt claims
//
// Windows implements them in userpath_windows.go against HKCU\Environment\Path;
// everything else in userpath_unix.go against the login shell's profiles.
// Both are always called with the user PATH lock held (see lockUserPath), which
// covers the whole read-modify-write, not just the write.
//
// Ownership rules the two share:
//
//   - Record before writing, so a crash or a failed write is retryable.
//   - Never claim an entry PAIR did not put there. Uninstall deletes exactly
//     what the receipt names, so a wrong claim deletes a user's own entry.
//
// Where they legitimately differ: Windows records only when it actually
// appends, because an entry already present is indistinguishable from the
// user's own. Unix records even when the profile already holds a byte-identical
// block, because PAIR's block is self-identifying — it is demonstrably the
// author, and re-adopting is what lets a reinstall clean up after a lost
// receipt.
//
// A third platform implements the two functions and nothing else. The shell
// editing in userpath_shell.go and the Windows entry parsing in
// userpath_windows_entries.go are deliberately untagged so their rules stay
// testable everywhere, not because either is portable.

// Both engines can finish installing together. Serialize user PATH updates
// across engines so one read/modify/write cannot discard the other's entry.
// This covers one process only; lockUserPath extends it across the several
// engine-managers a machine can run at once.
var userPathMu sync.Mutex

// addToUserPath validates the directory before changing the current user's PATH.
// The caller holds the user PATH lock through both ownership and environment writes.
func addToUserPath(dir string, receipt *pathReceipt, save func() error) error {
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\r\n\x00"+string(os.PathListSeparator)) {
		return fmt.Errorf("invalid command-line directory %q", dir)
	}
	return persistUserPath(dir, receipt, save)
}
