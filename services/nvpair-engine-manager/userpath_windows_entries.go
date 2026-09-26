// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "strings"

// Windows PATH string algebra: splitting HKCU\Environment\Path on ';' and
// deciding what one entry means. Deliberately untagged, like userpath_shell.go,
// so the parsing rules are testable on every platform rather than only on the
// one CI happens to run. userpath_windows.go holds the registry access these
// feed, and that file is the Windows-only half.

// Remove only the entry we wrote; differently spelled or expanded user entries
// are preserved. Comparison matches the one appendWindowsPath used to decide
// the entry was missing, so an external tool that re-cased the entry or gave it
// a trailing separator cannot make it unremovable.
//
// Unlike appendWindowsPath, no expansion is applied: the receipt records the
// literal entry PAIR wrote, and expanding a user's %VAR% here could match an
// entry PAIR never added.
//
// The search runs from the end because appendWindowsPath appends. The receipt
// records a directory, not a position, so when something else has since added
// the same directory the last match is the better guess at PAIR's own — and
// installers conventionally prepend. Exactly one goes, leaving whatever the
// other tool wanted where it wanted it.
func removeWindowsPath(current, owned string) string {
	if owned == "" {
		return current
	}
	entries := strings.Split(current, ";")
	for i := len(entries) - 1; i >= 0; i-- {
		if strings.EqualFold(normalizeWindowsPathEntry(entries[i]), normalizeWindowsPathEntry(owned)) {
			return strings.Join(append(entries[:i:i], entries[i+1:]...), ";")
		}
	}
	return current
}

// addWindowsPath records newly appended entries but never claims existing ones.
// Ownership is recorded only when the PATH actually changed, which is the
// Windows half of the rule stated in userpath.go.
func addWindowsPath(current, dir string, expand func(string) string, receipt *pathReceipt, save func() error) (string, error) {
	next := appendWindowsPath(current, dir, expand)
	if next != current {
		receipt.WindowsEntry = dir
		if err := save(); err != nil {
			return current, err
		}
	}
	return next, nil
}

// appendWindowsPath preserves existing entries and their order, including
// expandable environment references. Windows directory comparisons ignore case.
//
// A trailing separator is dropped rather than carried through: appending after
// one turns a harmless trailing empty element into an interior one, and Windows
// executable and DLL search has historically resolved an empty PATH element
// against the current directory.
func appendWindowsPath(current, dir string, expand func(string) string) string {
	for _, entry := range strings.Split(current, ";") {
		if strings.EqualFold(normalizeWindowsPathEntry(expand(entry)), normalizeWindowsPathEntry(dir)) {
			return current
		}
	}
	current = strings.TrimRight(current, ";")
	if current == "" {
		return dir
	}
	return current + ";" + dir
}

// normalizeWindowsPathEntry makes two spellings of one directory comparable:
// surrounding quotes and spaces, slash direction, and a trailing separator are
// all cosmetic on Windows.
func normalizeWindowsPathEntry(value string) string {
	return strings.TrimRight(strings.ReplaceAll(strings.Trim(value, " \""), "/", `\`), `\`)
}
