// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"syscall"
)

func tryEngineInstallLock(guard *os.File) error {
	return syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
