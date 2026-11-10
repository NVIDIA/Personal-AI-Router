// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import "golang.org/x/sys/windows"

// diskFreeBytes reports the bytes the current user may still write on the
// volume holding path, honouring any disk quota.
func diskFreeBytes(path string) (uint64, error) {
	dir, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &available, &total, &free); err != nil {
		return 0, err
	}
	return available, nil
}
