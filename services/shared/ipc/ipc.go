// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

// Package ipc is the shared transport for the optional --ipc endpoint: a Unix
// domain socket on non-Windows, a Windows named pipe on Windows. It
// single-sources the dialIPC that was copy-pasted across every subprocess,
// plus a Listen helper for tests and the (future) discovery daemon.
package ipc

import (
	"io"
	"net"
)

// Dial connects to the IPC endpoint at path (a Unix domain socket).
func Dial(path string) (io.ReadWriteCloser, error) {
	return net.Dial("unix", path)
}

// Listen creates an IPC listener at path (a Unix domain socket).
func Listen(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}
