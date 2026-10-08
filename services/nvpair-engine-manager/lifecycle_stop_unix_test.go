// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func startTermIgnoringProcess(t *testing.T) *managedProc {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	script := `trap '' TERM
: > "$1"
while :; do sleep 1; done`
	proc, err := startManagedProc("/bin/sh", []string{"-c", script, "stubborn-engine", ready}, nil, nil)
	if err != nil {
		t.Fatalf("start term-ignoring process: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-proc.done:
			return
		default:
		}
		_ = signalPID(proc.cmd.Process.Pid, true)
		select {
		case <-proc.done:
		case <-time.After(2 * time.Second):
			t.Errorf("term-ignoring process did not exit during cleanup")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for !fileExists(ready) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !fileExists(ready) {
		t.Fatal("term-ignoring process did not become ready")
	}
	return proc
}

func TestStopHonorsProcessSignalPolicy(t *testing.T) {
	test := func(t *testing.T, stop StopSpec, minElapsed, maxElapsed time.Duration) {
		t.Helper()
		proc := startTermIgnoringProcess(t)
		manifest := testEngineManifest(fakeEngineBin)
		key := runtime.GOOS + "/" + runtime.GOARCH
		platform := manifest.Platforms[key]
		platform.Runtime.Stop = &stop
		manifest.Platforms[key] = platform
		ex := newTestExecutor(t, manifest)
		state, err := ex.state(manifest.Engine)
		if err != nil {
			t.Fatalf("resolve engine state: %v", err)
		}
		state.mu.Lock()
		state.running = true
		state.healthy = true
		state.proc = proc
		state.mu.Unlock()

		started := time.Now()
		result := make(chan error, 1)
		go func() {
			result <- ex.Stop(manifest.Engine)
		}()
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("stop engine: %v", err)
			}
		case <-time.After(maxElapsed + time.Second):
			_ = signalPID(proc.cmd.Process.Pid, true)
			t.Fatalf("stop did not finish within %s", maxElapsed+time.Second)
		}

		elapsed := time.Since(started)
		if elapsed < minElapsed || elapsed > maxElapsed {
			t.Fatalf("stop elapsed %s, want between %s and %s", elapsed, minElapsed, maxElapsed)
		}
		select {
		case <-proc.done:
		default:
			t.Fatal("stop returned before the owned process exited")
		}
	}

	t.Run("term escalates after configured grace", func(t *testing.T) {
		test(t, StopSpec{Signal: "term", GraceS: 1}, 800*time.Millisecond, 3*time.Second)
	})
	t.Run("kill bypasses configured grace", func(t *testing.T) {
		test(t, StopSpec{Signal: "kill", GraceS: 10}, 0, 2*time.Second)
	})
}
