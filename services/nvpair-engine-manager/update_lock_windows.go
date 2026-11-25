// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func tryEngineInstallLock(guard *os.File) error {
	return windows.LockFileEx(windows.Handle(guard.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
