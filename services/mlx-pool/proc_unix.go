//go:build !windows

// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"syscall"
)

// configureSysProcAttr puts each child in its own process group so a signal
// reaches the whole tree. mlx_lm.server is a console entry point that may hold
// helper processes; signalling only the leader can leave weights resident.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate asks the child's whole group to exit. SIGTERM, not SIGKILL: mlx-lm
// releases Metal buffers on a clean exit, and an escalation path exists in the
// caller for a child that ignores it.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
}
