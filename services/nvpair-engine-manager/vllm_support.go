// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type vllmGPUSupport struct {
	Compute string
}

func vllmUnsupportedPlatformReason(goos, arch string) string {
	switch goos {
	case "windows":
		return "Managed vLLM has no native Windows recipe. WSL2 and an explicitly selected user-owned Linux distribution are prerequisites, but this PAIR build does not yet own that WSL child route and never installs or enables WSL; use a qualified Linux amd64 or arm64 PAIR node today"
	case "darwin":
		return "Managed vLLM is unsupported on macOS; use a qualified Linux amd64 or arm64 PAIR node"
	case "linux":
		return fmt.Sprintf("Managed vLLM requires Linux amd64 or arm64; no managed recipe exists for %s/%s", goos, arch)
	default:
		return fmt.Sprintf("Managed vLLM requires a qualified Linux amd64 or arm64 host; no managed recipe exists for %s/%s", goos, arch)
	}
}

func dottedVersionAtLeast(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	a, errA := strconv.Atoi(parts[0])
	b, errB := strconv.Atoi(parts[1])
	return errA == nil && errB == nil && (a > major || a == major && b >= minor)
}

func vllmInstallSupportReason(goos, arch, glibc string, gpus []vllmGPUSupport) string {
	if goos != "linux" || (arch != "amd64" && arch != "arm64") {
		return vllmUnsupportedPlatformReason(goos, arch)
	}
	if !dottedVersionAtLeast(glibc, 2, 28) {
		return "glibc 2.28 or newer is required"
	}
	for _, gpu := range gpus {
		if dottedVersionAtLeast(gpu.Compute, 7, 5) {
			return ""
		}
	}
	return "an NVIDIA GPU with compute capability 7.5 or newer is required"
}

func probeVLLMInstallSupport(parent context.Context) (bool, string) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getconf", "GNU_LIBC_VERSION").Output()
	if err != nil {
		return false, "glibc 2.28 or newer could not be observed"
	}
	glibcFields := strings.Fields(strings.TrimSpace(string(out)))
	glibc := ""
	if len(glibcFields) == 2 && strings.EqualFold(glibcFields[0], "glibc") {
		glibc = glibcFields[1]
	}
	out, err = exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=compute_cap", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return false, "NVIDIA GPU compute capability could not be observed"
	}
	var gpus []vllmGPUSupport
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if compute := strings.TrimSpace(line); compute != "" {
			gpus = append(gpus, vllmGPUSupport{Compute: compute})
		}
	}
	reason := vllmInstallSupportReason(runtime.GOOS, runtime.GOARCH, glibc, gpus)
	return reason == "", reason
}
