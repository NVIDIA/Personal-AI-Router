// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

// How the fake's port holder escapes the process tree PAIR terminates. The holder
// stands in for a listener that outlives the engine process, which is only a thing
// that can happen when the process holding the port is not one PAIR started: a stop
// asks the engine's whole group to exit and then force-kills what is left of it.

import (
	"os/exec"
	"syscall"
)

// detachFromProcessGroup gives the helper its own process group, so neither the
// graceful signal nor the forced kill that follows it reaches it.
func detachFromProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
