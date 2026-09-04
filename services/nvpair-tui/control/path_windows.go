// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package control

import (
	"fmt"
	"os/user"
	"strings"
)

// defaultPath is a named pipe rather than a file. The pipe namespace is flat
// and machine-wide, so the name carries the user's SID to keep two signed-in
// users from colliding — the same reason the Unix path is per-user.
func defaultPath() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve the current user: %w", err)
	}
	// A SID ("S-1-5-21-...") is already pipe-name safe; a username may not be,
	// so anything unexpected is reduced to characters a pipe name accepts.
	return `\\.\pipe\nvpair-tui-` + sanitize(u.Uid), nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// prepare has nothing to clean up: a named pipe exists only while its server
// process does, so there is no stale endpoint to remove. A pipe still held by
// a running TUI makes the listen itself fail, which is the check.
func prepare(path string) error {
	if !strings.HasPrefix(path, `\\.\pipe\`) {
		// An operator who passed a filesystem path with --control-socket gets
		// a Unix-socket-shaped endpoint; it still needs its directory.
		return ensureDir(path)
	}
	return nil
}

// secure is a no-op for a named pipe: go-winio creates it with the default
// DACL, which grants the creating user and denies everyone else. A
// filesystem path passed with --control-socket has no mode to set on Windows.
func secure(path string) error { return nil }
