// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jaypipes/ghw"
)

// nvidiaSmiTimeout caps how long we wait for a single nvidia-smi invocation.
// The tool is normally well under 100 ms, but on a wedged driver it can hang;
// a hard ceiling keeps both the one-shot startup detect and the per-tick stats
// collector from blocking indefinitely.
const nvidiaSmiTimeout = 3 * time.Second

const amdPCIRoot = "/sys/bus/pci/devices"

// detectGPUs enumerates GPUs on Linux. It prefers nvidia-smi, which yields the
// marketing name, total VRAM, and a stable per-GPU UUID we reuse as the join
// key (statsKey) against the dynamic stats collector's snapshot. When
// nvidia-smi is absent — no NVIDIA driver, or an AMD/Intel-only host — it falls
// back to ghw. AMD adapters receive a PCI-derived join key for dynamic
// utilization, while other adapters list their names without dynamic VRAM or
// utilization (matching the pre-existing non-Windows behavior).
//
// On unified-memory architectures (UMA, e.g. Grace-Blackwell / DGX Spark)
// nvidia-smi reports [N/A] for memory.total because the GPU shares system
// DRAM; in that case VramBytes is filled from detectMemoryTotal() instead.
func detectGPUs() []GPUInfo {
	if out, err := nvidiaSmiCSV("uuid,name,memory.total"); err == nil {
		if gpus, uma := parseNvidiaStatic(out); len(gpus) > 0 {
			if uma {
				if total := detectMemoryTotal(); total > 0 {
					for i := range gpus {
						if gpus[i].usesSystemMemoryUsage {
							gpus[i].VramBytes = total
						}
					}
				}
			}
			// Preserve NVIDIA records and include AMD adapters on mixed hosts.
			for _, gpu := range detectGPUsGHW() {
				if strings.HasPrefix(gpu.statsKey, "amd:") {
					gpus = append(gpus, gpu)
				}
			}
			return gpus
		}
	}
	return detectGPUsGHW()
}

// detectGPUsGHW is the ghw-based fallback, identical in spirit to the
// non-Windows/non-Linux path in gpu_other.go: enumerate display adapters and
// return adapter names. AMD adapters also receive PCI-derived stats keys for
// dynamic utilization; VramBytes stays 0 for every ghw-derived adapter.
func detectGPUsGHW() []GPUInfo {
	gpu, err := ghw.GPU()
	if err != nil {
		log.Printf("GPU detection error: %v", err)
		return nil
	}
	var gpus []GPUInfo
	for _, card := range gpu.GraphicsCards {
		name := "Unknown"
		if card.DeviceInfo != nil && card.DeviceInfo.Product != nil {
			name = card.DeviceInfo.Product.Name
		}
		gpus = append(gpus, GPUInfo{Name: name, statsKey: amdStatsKey(amdPCIRoot, card.Address)})
	}
	return gpus
}

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

// nvidiaSmiCSV runs `nvidia-smi --query-gpu=<fields> --format=csv,noheader,nounits`
// and returns raw stdout. The caller parses the comma-separated rows. A missing
// binary (not on PATH) surfaces as an exec error, which callers treat as "no
// NVIDIA GPU data available" and degrade silently.
func nvidiaSmiCSV(fields string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSmiTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu="+fields,
		"--format=csv,noheader,nounits").Output()
	return string(out), err
}

// isNvidiaSmiNA reports whether an nvidia-smi CSV field is a "not
// applicable" sentinel rather than a numeric value. UMA platforms such as
// DGX Spark return [N/A] or [Not Supported] for GPU memory queries.
func isNvidiaSmiNA(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "[]")
	switch strings.ToLower(s) {
	case "n/a", "not supported":
		return true
	default:
		return false
	}
}

// parseNvidiaStatic decodes the static query (uuid,name,memory.total) into
// GPUInfo records. memory.total is reported in MiB (because of -nounits); we
// convert to bytes. Rows missing a uuid or name, or with an unparseable VRAM
// figure, are skipped rather than emitted with placeholder values. Each
// unified-memory row is marked so response assembly can source its used bytes
// from system memory without depending on dynamic nvidia-smi collection. The
// second return value reports whether any row was unified, allowing the caller
// to fetch the shared system-memory total only when needed.
func parseNvidiaStatic(out string) ([]GPUInfo, bool) {
	var gpus []GPUInfo
	var unifiedMemory bool
	for _, line := range strings.Split(out, "\n") {
		fields := splitCSVRow(line)
		if len(fields) < 3 {
			continue
		}
		uuid, name := fields[0], fields[1]
		if uuid == "" || name == "" {
			continue
		}
		var vramBytes uint64
		usesUnifiedMemory := isNvidiaSmiNA(fields[2])
		if usesUnifiedMemory {
			unifiedMemory = true
		} else if mib, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
			vramBytes = mib * 1024 * 1024
		}
		gpus = append(gpus, GPUInfo{
			Name:                  name,
			VramBytes:             vramBytes,
			statsKey:              uuid,
			usesSystemMemoryUsage: usesUnifiedMemory,
		})
	}
	return gpus, unifiedMemory
}

// splitCSVRow splits one nvidia-smi CSV row on commas and trims surrounding
// whitespace from each field (the tool emits ", " separators). Returns nil for
// a blank line so callers can skip it.
func splitCSVRow(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
