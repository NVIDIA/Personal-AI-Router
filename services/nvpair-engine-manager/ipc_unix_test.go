// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nvpair-shared/ipc"
)

// ipcListener creates a Unix-domain-socket listener for the IPC test
// (a short path stays under the socket path-length limit).
func ipcListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("nvpair-em-%d.sock", time.Now().UnixNano()))
	ln, err := ipc.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return ln, path
}
