// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTermIgnoringProcess(t *testing.T) *managedProc {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	script := `trap '' TERM
: > "$1"
while :; do sleep 1; done`
	proc, err := startManagedProc("/bin/sh", []string{"-c", script, "stubborn-engine", ready}, nil, nil)
	require.NoError(t, err, "start term-ignoring process")
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
			assert.Fail(t, "term-ignoring process did not exit during cleanup")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for !fileExists(ready) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.FileExists(t, ready, "term-ignoring process did not become ready")
	return proc
}

func TestStopHonorsProcessSignalPolicy(t *testing.T) {
	test := func(name string, stop StopSpec, minElapsed, maxElapsed time.Duration) {
		t.Run(name, func(t *testing.T) {
			proc := startTermIgnoringProcess(t)
			manifest := testEngineManifest(fakeEngineBin)
			key := runtime.GOOS + "/" + runtime.GOARCH
			platform := manifest.Platforms[key]
			platform.Runtime.Stop = &stop
			manifest.Platforms[key] = platform
			ex := newTestExecutor(t, manifest)
			state, err := ex.state(manifest.Engine)
			require.NoError(t, err, "resolve engine state")
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
				require.NoError(t, err, "stop engine")
			case <-time.After(maxElapsed + time.Second):
				_ = signalPID(proc.cmd.Process.Pid, true)
				require.FailNowf(t, "stop did not finish", "within %s", maxElapsed+time.Second)
			}

			elapsed := time.Since(started)
			require.GreaterOrEqual(t, elapsed, minElapsed)
			require.LessOrEqual(t, elapsed, maxElapsed)
			select {
			case <-proc.done:
			default:
				require.FailNow(t, "stop returned before the owned process exited")
			}
		})
	}
	test("term escalates after configured grace", StopSpec{Signal: "term", GraceS: 1}, 800*time.Millisecond, 3*time.Second)
	test("kill bypasses configured grace", StopSpec{Signal: "kill", GraceS: 10}, 0, 2*time.Second)
}
