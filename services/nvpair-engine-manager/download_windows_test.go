// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// A cancel racing the CLI's exit can reach interruptDownload after the PID has
// been reissued, and the helper's GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)
// reaches every process sharing that console. The image check is what keeps the
// signal inside the app, and the returned handle is what keeps the PID from
// being recycled between that check and the attach.
func TestOpenDownloadLauncher_AcceptsOnlyThisAppsOwnImage(t *testing.T) {
	t.Run("accepts this test binary", func(t *testing.T) {
		handle, ok := openDownloadLauncher(uint32(os.Getpid()))
		require.True(t, ok, "this test binary was not recognized as its own image")
		assert.NotZero(t, handle, "an accepted launcher came back without a handle, so nothing holds its PID")
		assert.NoError(t, windows.CloseHandle(handle))
	})
	reject := func(name string, pid uint32) {
		t.Run(name, func(t *testing.T) {
			handle, ok := openDownloadLauncher(pid)
			if !assert.False(t, ok, "pid %d was accepted as a download launcher", pid) {
				_ = windows.CloseHandle(handle)
				return
			}
			// A rejection must not leak the handle it opened to look.
			assert.Zero(t, handle)
		})
	}
	reject("rejects the Windows System process", 4)
	// A PID no process holds is a reissue candidate too.
	reject("rejects a pid no process holds", 0xFFFFFFF0)
}

// The broker launches engine-manager in a new process group, which disables
// Ctrl+C and passes that setting to descendants, including a download CLI. The
// interrupt has to reach the CLI anyway, so the cancellation test is run again
// in a child process started the way the broker starts engine-manager.
func TestRunLMSDownloadCommand_InterruptsTheCLIUnderTheBrokersProcessFlags(t *testing.T) {
	const cancellation = "TestRunLMSDownloadCommand_InterruptsTheCLIWhenCancelled"
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+cancellation+"$", "-test.v")
	configureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP

	output, err := cmd.CombinedOutput()

	require.NoError(t, err, "download cancellation under the broker's process flags:\n%s", output)
	// A pattern that matches no test also exits cleanly.
	assert.Contains(t, string(output), "--- PASS: "+cancellation, "the cancellation test did not run in the child")
}
