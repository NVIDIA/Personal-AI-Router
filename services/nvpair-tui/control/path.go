// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package control is nvpair-tui's local control endpoint: a per-user socket a
// running TUI listens on so a second invocation of the same binary — or any
// script on the box — can drive pairing without a keyboard.
//
// It speaks the same newline-delimited JSON-RPC 2.0 every other PAIR surface
// speaks, and every method it exposes is relayed through the TUI's existing
// broker connection. The broker keeps exactly one client, which is what it is
// built for; this socket does not open a second one.
//
// It is a *local* endpoint and carries no authentication of its own: on
// non-Windows the parent directory is 0700 and the socket 0600, so the file
// permissions are the check, and on Windows the named pipe's default DACL
// grants the creating user. Anything that can read the socket can pair this
// machine with another, which is the same authority the operator sitting at
// the TUI already has.
package control

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"nvpair-shared/ipc"
)

// dirPerm / socketPerm keep the endpoint to its owner. The directory is the
// real guard: net.Listen creates the socket under the process umask, so there
// is a moment before the chmod when the mode is wider than 0600, and a 0700
// parent makes that moment unreachable.
const (
	dirPerm    os.FileMode = 0o700
	socketPerm os.FileMode = 0o600
)

// DefaultPath returns the well-known per-user control endpoint.
//
// On Linux with $XDG_RUNTIME_DIR set that is $XDG_RUNTIME_DIR/nvpair/tui.sock,
// the directory the OS already keeps per-user, 0700 and cleaned at logout.
// Everywhere else it is "run/tui.sock" under the shared per-user data
// directory every PAIR component agrees on (nvpair-shared/appdir). On Windows
// it is a named pipe rather than a filesystem path.
func DefaultPath() (string, error) { return defaultPath() }

// Listen opens the control endpoint at path, creating its parent directory
// 0700 and tightening the socket to 0600.
//
// A socket file left behind by a TUI that did not exit cleanly is removed, but
// only after confirming nothing answers on it: a live socket means another TUI
// is running and this one must not steal its endpoint.
func Listen(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("control socket path is empty")
	}
	if err := prepare(path); err != nil {
		return nil, err
	}
	ln, err := ipc.Listen(path)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket %s: %w", path, err)
	}
	if err := secure(path); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// InUse reports whether something is already answering at path, so a caller
// can tell "no TUI is running" from "the endpoint is taken".
func InUse(path string) bool {
	conn, err := ipc.Dial(path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ensureDir creates the endpoint's parent directory 0700. An existing
// directory keeps whatever mode it has: on Linux $XDG_RUNTIME_DIR is already
// 0700 and owned by the user, and re-chmod'ing a directory PAIR does not own
// is not this component's business.
func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create control socket directory %s: %w", dir, err)
	}
	return nil
}
