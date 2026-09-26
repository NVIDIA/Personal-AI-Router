// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os/exec"
	"testing"
	"time"
)

// An engine that ignores SIGTERM, or leaves a child behind, must not outlive a
// stop: after the grace period the whole process group is force-killed.
func TestStopEscalatesToSIGKILL(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs a POSIX shell")
	}
	restore := stopEscalateAfter
	stopEscalateAfter = 300 * time.Millisecond
	t.Cleanup(func() { stopEscalateAfter = restore })
	// The parent traps TERM and spawns a child in the same process group that
	// also traps it; only SIGKILL to the group ends both.
	mp, err := startManagedProc("sh", []string{"-c", `trap "" TERM; sh -c 'trap "" TERM; sleep 60' & wait`}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pid := mp.cmd.Process.Pid
	done := make(chan struct{})
	go func() { mp.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not escalate past an ignored SIGTERM")
	}
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatal("process group survived the forced stop")
	}
}
