// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"strconv"
	"strings"
)

// ss is iproute2, which is Linux-only; its location moves by distribution.
var ssLocations = []string{"/usr/sbin/ss", "/sbin/ss", "/usr/bin/ss"}

// ssPID parses the owning PID out of ss's users:(("proc",pid=1234,fd=7)) tail.
func ssPID(port int) (int, bool) {
	out := runTool(systemTool("ss", ssLocations),
		"-ltnp", "sport", "=", ":"+strconv.Itoa(port))
	if len(out) == 0 {
		return 0, false
	}
	s := string(out)
	idx := strings.Index(s, "pid=")
	if idx < 0 {
		return 0, false
	}
	rest := s[idx+len("pid="):]
	end := strings.IndexAny(rest, ",)")
	if end < 0 {
		return 0, false
	}
	if p, err := strconv.Atoi(rest[:end]); err == nil && p > 0 {
		return p, true
	}
	return 0, false
}
