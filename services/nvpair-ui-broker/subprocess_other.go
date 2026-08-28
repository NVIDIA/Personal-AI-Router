// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// configureSubprocess sets platform-specific attributes on a worker the
// broker spawns. On Unix it places the child in its own process group
// (Setpgid) so a signal sent to the broker's process group — most
// importantly the SIGINT a terminal delivers to the whole foreground
// group on Ctrl+C — does NOT reach the children directly.
//
// That keeps the broker the sole owner of each child's lifecycle: a worker
// exits only when the broker stops it (by closing its stdin), so the
// supervisor never sees a shutdown-time death with its stop-channel still
// open and never misclassifies an orderly shutdown as a crash to restart.
// Workers still shut down if the broker dies abruptly, because they exit on
// stdin EOF when the broker's pipe closes.
func configureSubprocess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateIsKill reports whether signalTerminate is really a kill. On Unix it is
// not: SIGTERM is a genuine graceful step, so the escalation has something to try
// before force-killing.
const terminateIsKill = false

// signalTerminate asks a worker that ignored its stdin EOF to exit. SIGTERM is
// worth a step of its own before a kill: a Go worker parked in a blocking write
// still runs its signal handler and can exit on its own terms.
//
// The signal goes to the process rather than its group. configureSubprocess puts
// each worker in its own group so terminal signals don't reach it, and signalling
// the group here would also hit anything that worker spawned, which is the
// engine-orphaning behaviour the shutdown path is built to avoid.
func signalTerminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
