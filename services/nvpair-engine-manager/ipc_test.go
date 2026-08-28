// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()

	frames := make(chan frame, 64)
	go readFrames(conn, frames)

	waitNotify(t, frames, "engine:ready", 5*time.Second)
	send(t, conn, 1, "engine:get-installed", nil)
	if r := waitResult(t, frames, "1", 5*time.Second); !strings.Contains(string(r), "engines") {
		t.Fatalf("get-installed over IPC returned: %s", r)
	}
	send(t, conn, 2, "shutdown", nil)
	waitResult(t, frames, "2", 5*time.Second)
}
