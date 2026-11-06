// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsOurEngineImage_RecognizesManagedExecutable(t *testing.T) {
	bin := filepath.Join("/opt", "nvpair", "ollama.exe")
	test := func(name, image string, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, isOurEngineImage(image, bin))
		})
	}
	test("matching binary", bin, true)
	test("case insensitive", `/opt/nvpair/OLLAMA.EXE`, true)
	test("deleted binary", bin+` (deleted)`, true)
	test("other binary", `/opt/nvpair/other.exe`, false)
	test("empty image", ``, false)
	require.False(t, isOurEngineImage(bin, ``), "empty binPath must not match")
}

func TestIsManagedInstallPath_RejectsPathsOutsideManagedRoot(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "ollama", "bin", "ollama"+exeExt())
	outside := filepath.Join(t.TempDir(), "ollama"+exeExt())
	require.True(t, isManagedInstallPath(inside, filepath.Join(base, "ollama")), "managed child path (%v)", inside)
	require.False(t, isManagedInstallPath(outside, filepath.Join(base, "ollama")), "external path (%v)", outside)
	require.False(t, isManagedInstallPath("", filepath.Join(base, "ollama")), "empty binary path must not be managed")
}

// processGone reports whether pid is gone. A killed process whose parent has
// not reaped it yet is a zombie, which is as dead as a process gets.
func processGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// "pid (comm) S ..." — comm may contain spaces and parentheses, so cut after the last ")".
	fields := strings.Fields(string(data[strings.LastIndex(string(data), ")")+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processGone(pid)
}

// vLLM's main process can exit on a polite stop while the helper it forked
// (EngineCore, holding the GPU) is still alive, or never gets to answer one.
// Stopping an engine has to take the helpers with it: they are in the
// engine's process group and no one else will ever clean them up.
func TestManagedProcStop_KillsHelpersTheMainProcessLeftBehind(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process groups and /proc")
	}

	// The main process exits on SIGTERM; its helper ignores it and has closed the
	// engine's output pipes, so the stop sees the main process end and nothing
	// else would ever notice the helper.
	script := `trap 'exit 0' TERM; (trap '' TERM; exec sleep 300 >/dev/null 2>&1) & echo $!; while true; do sleep 0.1; done`
	childPID := make(chan int, 1)
	proc, err := startManagedProc("sh", []string{"-c", script}, nil, func(stream, line string) {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && stream == "stdout" {
			select {
			case childPID <- pid:
			default:
			}
		}
	})
	require.NoError(t, err)
	var helper int
	select {
	case helper = <-childPID:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the helper process never started")
	}
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })
	require.False(t, processGone(helper), "the helper died before the stop")

	started := time.Now()
	proc.stop(5 * time.Second)

	// The main process answered the polite signal, so this must not have waited
	// out the grace: what is under test is the cleanup after it exits.
	assert.LessOrEqual(t, time.Since(started), 2*time.Second, "the main process should have exited on the graceful signal")
	assert.True(t, waitGone(helper, 3*time.Second), "helper process %d survived the engine being stopped", helper)
}

// A process that does not answer the polite signal is force-killed after the
// grace, together with its helpers.
func TestManagedProcStop_ForceKillsAProcessThatIgnoresTheGracefulSignal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process groups and /proc")
	}

	script := `trap '' TERM; (trap '' TERM; exec sleep 300) & echo $!; while true; do sleep 1; done`
	childPID := make(chan int, 1)
	proc, err := startManagedProc("sh", []string{"-c", script}, nil, func(stream, line string) {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && stream == "stdout" {
			select {
			case childPID <- pid:
			default:
			}
		}
	})
	require.NoError(t, err)
	helper := <-childPID
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })

	started := time.Now()
	proc.stop(500 * time.Millisecond)

	assert.LessOrEqual(t, time.Since(started), 3*time.Second, "the stop must not wait much past the grace")
	assert.True(t, waitGone(proc.cmd.Process.Pid, time.Second), "the process survived the forced stop")
	assert.True(t, waitGone(helper, 3*time.Second), "the helper survived the forced stop")
}
