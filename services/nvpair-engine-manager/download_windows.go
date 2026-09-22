// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func configureDownloadProcess(cmd *exec.Cmd) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// The broker disables Ctrl+C for its workers; that setting survives even
	// CREATE_NEW_CONSOLE. A launcher clears it before the CLI inherits it.
	cmd.Args = append([]string{executable, "--run-download", cmd.Path}, cmd.Args[1:]...)
	cmd.Path = executable
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_CONSOLE}
	return nil
}

func killDownload(cmd *exec.Cmd) error { return taskkill(cmd, true) }

// runDownloadProcess owns the CLI in a private hidden console. Reset only this
// launcher's inherited Ctrl+C setting, leaving the broker and worker untouched.
func runDownloadProcess(argv []string) int {
	ignore := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	if ok, _, err := ignore.Call(0, 0); ok == 0 {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// The child has inherited enabled Ctrl+C. Ignore it in the launcher only,
	// so the launcher stays alive to relay the CLI's exit and close its pipes.
	if ok, _, err := ignore.Call(0, 1); ok == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// isDownloadLauncher reports whether pid still names a launcher this worker
// started, by checking that its image is this same executable. Windows reissues
// a PID as soon as the last handle to the exited process closes, so a cancel
// racing the CLI's exit could otherwise attach to a stranger's console — and
// GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0) reaches everything sharing it.
func isDownloadLauncher(pid uint32) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	image := make([]uint16, 32768)
	size := uint32(len(image))
	if err := windows.QueryFullProcessImageName(handle, 0, &image[0], &size); err != nil {
		return false
	}
	return strings.EqualFold(windows.UTF16ToString(image[:size]), self)
}

func interruptDownload(cmd *exec.Cmd) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	helper := exec.CommandContext(ctx, executable, "--interrupt-download", strconv.Itoa(cmd.Process.Pid))
	configureSysProcAttr(helper)
	return helper.Run()
}

// Download launch and console attachment run in helpers so engine-manager's
// stdio and signal handlers are not affected.
func handleDownloadProcess() bool {
	if len(os.Args) >= 3 && os.Args[1] == "--run-download" {
		os.Exit(runDownloadProcess(os.Args[2:]))
	}
	if len(os.Args) != 3 || os.Args[1] != "--interrupt-download" {
		return false
	}
	pid, err := strconv.ParseUint(os.Args[2], 10, 32)
	if err != nil || pid == 0 {
		os.Exit(1)
	}
	if !isDownloadLauncher(uint32(pid)) {
		fmt.Fprintln(os.Stderr, "download launcher is gone")
		os.Exit(1)
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	attach := kernel.NewProc("AttachConsole")
	free := kernel.NewProc("FreeConsole")
	ignore := kernel.NewProc("SetConsoleCtrlHandler")
	generate := kernel.NewProc("GenerateConsoleCtrlEvent")
	free.Call()
	if ok, _, err := attach.Call(uintptr(pid)); ok == 0 {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if ok, _, err := ignore.Call(0, 1); ok == 0 {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if ok, _, err := generate.Call(0, 0); ok == 0 {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Control-event delivery is asynchronous.
	time.Sleep(100 * time.Millisecond)
	free.Call()
	return true
}
