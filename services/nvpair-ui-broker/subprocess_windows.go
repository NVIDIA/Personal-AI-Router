// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// configureSubprocess sets platform-specific attributes on a worker the
// broker spawns. On Windows it hides the console window and starts the
// child in a NEW process group (CREATE_NEW_PROCESS_GROUP) so a console
// Ctrl+C delivered to the broker's group is not propagated to the
// children. That gives the same guarantee Setpgid provides on Unix: the
// broker is the sole owner of each child's lifecycle (workers exit when
// the broker closes their stdin), so an orderly shutdown is never
// misclassified as a crash to restart.
func configureSubprocess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		// CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP
		CreationFlags: 0x08000000 | 0x00000200,
	}
}

// terminateIsKill reports whether signalTerminate is really a kill. On Windows it
// is: there is no graceful step available, so a worker that must not be
// interrupted part-way through its own teardown has to be left alone instead.
const terminateIsKill = true

// signalTerminate asks a worker that ignored its stdin EOF to exit. Windows has
// no signal an arbitrary process is obliged to honour — os.Process.Signal accepts
// only Kill here — so the caller's terminate-then-kill escalation necessarily
// collapses into a single step on this platform.
func signalTerminate(p *os.Process) error {
	return p.Kill()
}
