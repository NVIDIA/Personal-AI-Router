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

const amdPCIRoot = "/sys/bus/pci/devices"

// amdStatsKey identifies AMD adapters by PCI address, independently of DRM
// card numbering. The root argument permits tests without real hardware.
func amdStatsKey(root, address string) string {
	if !strings.Contains(address, ":") || filepath.Base(address) != address {
		return ""
	}
	if strings.Count(address, ":") == 1 {
		address = "0000:" + address
	}
	data, err := os.ReadFile(filepath.Join(root, address, "vendor"))
	if err != nil || strings.TrimSpace(string(data)) != "0x1002" {
		return ""
	}
	return "amd:" + address
}

// decodeAMDUtilization reads the amdgpu driver counter without a ROCm library,
// subprocess or elevated privilege. Missing or invalid counters stay absent.
// Memory is deliberately not mapped to VRAM: Strix Halo's GTT, reserved VRAM,
// HSA pool and MemAvailable have different semantics and overlap.
func decodeAMDUtilization(root string, out map[string]gpuStat) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	valid := false
	for _, entry := range entries {
		key := amdStatsKey(root, entry.Name())
		if key == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "gpu_busy_percent"))
		if err != nil {
			continue
		}
		pct, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
		if err != nil || pct > 100 {
			continue
		}
		out[key] = gpuStat{UtilizationPct: uint32(pct)}
		valid = true
	}
	return valid
}
