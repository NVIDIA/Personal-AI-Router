// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"

	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIPCTransport exercises the --ipc transport end-to-end: a listener
// (a named pipe on Windows, a Unix socket elsewhere) accepts the
// manager's dial, then we drive the JSON-RPC handshake over that
// connection. Confirms dialIPC + the codec work off stdio.
func TestIPCTransport(t *testing.T) {
	ln, path := ipcListener(t)
	defer ln.Close()

	cmd := exec.Command(managerBin, "--ipc", path)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	conn, err := ln.Accept()
	require.NoError(t, err, "accept")
	defer conn.Close()

	frames := make(chan frame, 64)
	go readFrames(conn, frames)

	waitNotify(t, frames, "engine:ready", 5*time.Second)
	send(t, conn, 1, "engine:get-installed", nil)
	require.Contains(t, string(waitResult(t, frames, "1", 5*time.Second)), "engines", "get-installed over IPC")
	send(t, conn, 2, "shutdown", nil)
	waitResult(t, frames, "2", 5*time.Second)
}
