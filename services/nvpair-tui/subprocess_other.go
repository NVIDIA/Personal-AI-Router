// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// configureSubprocess places the broker in its own process group so the
// SIGINT a terminal delivers to the TUI's foreground group on Ctrl+C does
// not reach the broker directly. The TUI owns the broker's lifecycle via
// the shutdown RPC + stdin EOF; this mirrors the broker's own handling of
// its workers (see nvpair-ui-broker/subprocess_other.go).
func configureSubprocess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
