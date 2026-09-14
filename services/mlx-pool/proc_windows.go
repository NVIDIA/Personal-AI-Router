//go:build windows

// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"syscall"
)

// configureSysProcAttr hides the console window a child would otherwise flash.
// MLX does not run on Windows, so this exists only to keep the package building
// on every platform the rest of the tree builds on.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
