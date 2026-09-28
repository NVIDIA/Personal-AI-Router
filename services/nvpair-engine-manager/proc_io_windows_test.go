// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatchProcessIOSeesTransfer: a child that moves bytes without growing the
// watched staging tree (PowerShell buffering a download) still counts as
// progress through its own I/O counters.
func TestWatchProcessIOSeesTransfer(t *testing.T) {
	target := filepath.Join(t.TempDir(), "sink.bin")
	script := "1..6 | ForEach-Object { [System.IO.File]::AppendAllText('" + target + "', ('x' * 65536)); Start-Sleep -Milliseconds 300 }"
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("powershell unavailable: %v", err)
	}
	var progress atomic.Int32
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchProcessIO(context.Background(), cmd.Process.Pid, 100*time.Millisecond, func() { progress.Add(1) }, stop)
	}()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	close(stop)
	<-done
	if info, err := os.Stat(target); err != nil || info.Size() == 0 {
		t.Fatalf("child wrote nothing: %v", err)
	}
	if progress.Load() < 2 {
		t.Fatalf("I/O watcher reported %d progress ticks over a 1.8 s transfer", progress.Load())
	}
}
