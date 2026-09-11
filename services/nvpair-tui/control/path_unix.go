// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package control

import (
	"fmt"
	"os"
	"path/filepath"

	"nvpair-shared/appdir"
)

// maxSocketPath is the shortest sun_path any platform PAIR builds for allows:
// 104 bytes on the BSDs and macOS, 108 on Linux. A path at or over the limit
// is silently truncated by the kernel, which produces a socket at a
// nonsensical path rather than an error, so it is checked up front and
// reported with the flag that fixes it.
const maxSocketPath = 104

// defaultPath prefers the OS's own per-user runtime directory, which exists
// precisely for sockets and is already 0700, and otherwise puts the socket in
// a run/ subdirectory of the shared per-user data directory.
func defaultPath() (string, error) {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return check(filepath.Join(runtimeDir, "nvpair", "tui.sock"))
	}
	path, err := appdir.Path("run", "tui.sock")
	if err != nil {
		return "", fmt.Errorf("resolve the per-user data directory: %w", err)
	}
	return check(path)
}

// check rejects a path the kernel would truncate, naming the escape hatch. A
// truncated sun_path is not an error the kernel reports as one: the bind
// either fails with a bare EINVAL or, worse, succeeds at a path nobody will
// dial, so the length is checked before either can happen.
func check(path string) (string, error) {
	if len(path) >= maxSocketPath {
		return "", fmt.Errorf(
			"the control socket path is %d bytes, over this platform's %d-byte limit (%s); "+
				"pass --control-socket with a shorter path, or --no-control-socket to run without one",
			len(path), maxSocketPath, path)
	}
	return path, nil
}

// prepare makes the endpoint's directory and clears a socket file left by a
// TUI that did not exit cleanly. A file that still answers belongs to a
// running TUI and is never removed.
func prepare(path string) error {
	if _, err := check(path); err != nil {
		return err
	}
	if err := ensureDir(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect control socket %s: %w", path, err)
	}
	if InUse(path) {
		return fmt.Errorf("another nvpair-tui is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove the stale control socket %s: %w", path, err)
	}
	return nil
}

// secure narrows the socket to its owner. net.Listen created it under the
// process umask, which on a default umask is already 0755 — wide enough for
// another local user to connect were the parent directory not 0700.
func secure(path string) error {
	if err := os.Chmod(path, socketPerm); err != nil {
		return fmt.Errorf("restrict the control socket %s to its owner: %w", path, err)
	}
	return nil
}
