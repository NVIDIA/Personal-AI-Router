// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// configureSubprocess hides the broker's console window and starts it in a
// new process group so a console Ctrl+C delivered to the TUI is not
// propagated to the broker. The TUI owns the broker's lifecycle via the
// shutdown RPC + stdin EOF; this mirrors the broker's own handling of its
// workers (see nvpair-ui-broker/subprocess_windows.go).
func configureSubprocess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		// CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP
		CreationFlags: 0x08000000 | 0x00000200,
	}
}
