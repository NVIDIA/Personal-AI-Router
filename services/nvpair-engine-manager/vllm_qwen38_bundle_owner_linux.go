//go:build linux

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"syscall"
)

func qwen38BundleOwnedMode(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	want := os.FileMode(0o600)
	if directory {
		want = 0o700
	}
	return ok && stat.Uid == uint32(os.Getuid()) && info.Mode().Perm() == want && (directory || stat.Nlink == 1)
}
