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

	"golang.org/x/sys/windows"
)

// A cancel racing the CLI's exit can reach interruptDownload after the PID has
// been reissued, and the helper's GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)
// reaches every process sharing that console. The image check is what keeps the
// signal inside the app, and the returned handle is what keeps the PID from
// being recycled between that check and the attach.
func TestOpenDownloadLauncherRejectsForeignProcesses(t *testing.T) {
	handle, ok := openDownloadLauncher(uint32(os.Getpid()))
	if !ok {
		t.Fatal("this test binary was not recognized as its own image")
	}
	if handle == 0 {
		t.Error("accepted launcher came back without a handle, so nothing holds its PID")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Errorf("close launcher handle: %v", err)
	}
	// PID 4 is the Windows System process; a PID no process holds is also a
	// reissue candidate.
	for _, pid := range []uint32{4, 0xFFFFFFF0} {
		handle, ok := openDownloadLauncher(pid)
		if ok {
			t.Errorf("pid %d was accepted as a download launcher", pid)
			_ = windows.CloseHandle(handle)
			continue
		}
		// A rejection must not leak the handle it opened to look.
		if handle != 0 {
			t.Errorf("pid %d was rejected but returned handle %v", pid, handle)
		}
	}
}

// The broker launches engine-manager in a new process group, which disables
// Ctrl+C and passes that setting to descendants, including a download CLI.
func TestLMSDownloadInBrokerProcessGroup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLMSDownloadInterruptsChild$", "-test.v")
	configureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("download cancellation under broker process flags: %v\n%s", err, output)
	}
}
