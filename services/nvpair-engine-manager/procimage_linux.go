// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"strconv"
)

// procImage resolves a PID's executable path from /proc.
//
// The kernel maintains this symlink against the running image, so it is
// unaffected by how the process was invoked and cannot be spoofed by argv.
// Empty on any error (the process exited, or it belongs to another user) so the
// caller's image check fails closed.
func procImage(pid int) string {
	if pid <= 0 {
		return ""
	}
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return path
}
