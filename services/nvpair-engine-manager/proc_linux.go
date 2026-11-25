// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procImage resolves a PID's executable path by reading /proc/<pid>/exe. An
// unknown or exited PID answers "", and the caller fails closed.
func procImage(pid int) string {
	if pid <= 0 {
		return ""
	}
	if path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		return path
	}
	return ""
}

// procRunsScript reports whether pid was started as an interpreter running the
// engine script at binPath. An engine installed as a Python package (vLLM's
// venv/bin/vllm) is a script, so the process's executable is the interpreter and
// procImage never matches binPath; what does match is the script named in the
// command line, as one of its first arguments ("python /path/venv/bin/vllm
// serve ..."). Only Linux exposes a trustworthy command line through /proc:
// the darwin build answers false, which keeps script-run engines failing
// closed.
func procRunsScript(pid int, binPath string) bool {
	if pid <= 0 || binPath == "" {
		return false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	want := filepath.Clean(binPath)
	if !filepath.IsAbs(want) {
		return false // an engine path is absolute; a bare word could match any argument
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	for i := 0; i < len(args) && i < 3; i++ {
		if args[i] != "" && filepath.Clean(args[i]) == want {
			return true
		}
	}
	return false
}
