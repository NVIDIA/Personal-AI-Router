// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"testing"
	"time"
)

// TestStopEscalatesOnIgnoredGracefulSignal guards the stop escalation: an engine
// that ignores SIGTERM must still be stopped. The fake engine here traps SIGTERM
// and keeps running, so only the forced-kill escalation after the grace can end
// it. A stop that returns quickly with the process gone proves the escalation
// fired; the old wait-forever behavior would hang this test past the deadline.
func TestStopEscalatesOnIgnoredGracefulSignal(t *testing.T) {
	// sh ignores SIGTERM (trap … ''), prints its pid, then sleeps. configureSysProcAttr
	// puts it in its own process group, so the group kill reaches it.
	ready := make(chan struct{}, 1)
	proc, err := startManagedProc("sh", []string{"-c", `trap '' TERM; echo ready; sleep 60`}, nil,
		func(_, line string) {
			if line == "ready" {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		})
	if err != nil {
		t.Fatalf("start managed proc: %v", err)
	}

	// Wait until the trap is installed, so the graceful SIGTERM is genuinely
	// ignored and only the escalation can stop the process.
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("fake engine never signaled readiness")
	}

	rt := Runtime{Stop: &StopSpec{GraceS: 1}}
	done := make(chan struct{})
	go func() {
		proc.stop(rt)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not return: escalation never fired on an engine that ignored SIGTERM")
	}

	// The process must actually be gone, not merely abandoned.
	select {
	case <-proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("process still running after stop returned")
	}
}
