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

// openDownloadLauncher returns an open handle to pid when it still names a
// launcher this worker started, established by its image being this same
// executable. A cancel racing the CLI's exit could otherwise attach to a
// stranger's console — and GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0) reaches
// everything sharing it.
//
// The handle is the guard, not the image check. Windows reissues a PID as soon
// as the last handle to the exited process closes, so verifying the image and
// then closing the handle proves only what was true a moment ago: the PID can
// be recycled before the caller attaches. Holding this handle keeps the
// process object, and therefore the PID, from being reused — so the caller
// must not close it until it has attached to the console.
func openDownloadLauncher(pid uint32) (windows.Handle, bool) {
	self, err := os.Executable()
	if err != nil {
		return 0, false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, false
	}
	image := make([]uint16, 32768)
	size := uint32(len(image))
	if err := windows.QueryFullProcessImageName(handle, 0, &image[0], &size); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, false
	}
	if !strings.EqualFold(windows.UTF16ToString(image[:size]), self) {
		_ = windows.CloseHandle(handle)
		return 0, false
	}
	return handle, true
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
	launcher, ok := openDownloadLauncher(uint32(pid))
	if !ok {
		fmt.Fprintln(os.Stderr, "download launcher is gone")
		os.Exit(1)
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	attach := kernel.NewProc("AttachConsole")
	free := kernel.NewProc("FreeConsole")
	ignore := kernel.NewProc("SetConsoleCtrlHandler")
	generate := kernel.NewProc("GenerateConsoleCtrlEvent")
	free.Call()
	attached, _, attachErr := attach.Call(uintptr(pid))
	// The handle has held the PID against reuse up to here, which is the point
	// the console stops being addressed by number, so it has done its job.
	// os.Exit below would skip a defer, so close it explicitly.
	_ = windows.CloseHandle(launcher)
	if attached == 0 {
		fmt.Fprintln(os.Stderr, attachErr)
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
