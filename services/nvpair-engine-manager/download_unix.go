// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func configureDownloadProcess(cmd *exec.Cmd) error {
	configureSysProcAttr(cmd)
	return nil
}

func killDownload(cmd *exec.Cmd) error      { return signalDownload(cmd, syscall.SIGKILL) }
func interruptDownload(cmd *exec.Cmd) error { return signalDownload(cmd, syscall.SIGINT) }
func handleDownloadProcess() bool           { return false }

// signalDownload signals the whole download process group. configureSysProcAttr
// puts the CLI in its own group precisely so a signal reaches the helpers it
// forks; addressing the leader alone leaves the process doing the downloading
// running.
func signalDownload(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		return syscall.Kill(-pgid, sig)
	}
	return cmd.Process.Signal(sig)
}
