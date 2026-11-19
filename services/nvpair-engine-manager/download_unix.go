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

// pinDownloadProcess has nothing to hold on Unix: signalDownload addresses the
// process group, whose ID is not reissued while any member of it remains.
func pinDownloadProcess(*exec.Cmd) (func(), error) { return func() {}, nil }

// signalDownload signals the whole download process group. configureSysProcAttr
// puts the CLI in its own group precisely so a signal reaches the helpers it
// forks; addressing the leader alone leaves the process doing the downloading
// running.
//
// Setpgid makes the CLI the group's leader, so the group ID is its PID. It is
// not looked up from that PID, because once the CLI is reaped the PID can name
// a stranger whose group would be signalled instead.
func signalDownload(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
