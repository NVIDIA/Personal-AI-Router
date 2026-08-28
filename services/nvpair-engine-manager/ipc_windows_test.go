// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"fmt"
	"net"
	"testing"
	"time"

	"nvpair-shared/ipc"
)

// ipcListener creates a Windows named-pipe listener for the IPC test.
func ipcListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	path := fmt.Sprintf(`\\.\pipe\nvpair-em-test-%d`, time.Now().UnixNano())
	ln, err := ipc.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	return ln, path
}
