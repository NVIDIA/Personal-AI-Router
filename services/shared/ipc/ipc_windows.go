// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package ipc

import (
	"io"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

func isPipePath(path string) bool {
	return strings.HasPrefix(path, `\\.\pipe\`) || strings.HasPrefix(path, `\\.\Pipe\`)
}

// Dial connects to the IPC endpoint at path (a named pipe when path is a pipe
// path, otherwise a Unix domain socket).
func Dial(path string) (io.ReadWriteCloser, error) {
	if isPipePath(path) {
		return winio.DialPipe(path, nil)
	}
	return net.Dial("unix", path)
}

// Listen creates an IPC listener at path (a named pipe when path is a pipe
// path, otherwise a Unix domain socket).
func Listen(path string) (net.Listener, error) {
	if isPipePath(path) {
		return winio.ListenPipe(path, nil)
	}
	return net.Listen("unix", path)
}
