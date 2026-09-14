// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"log"
	"os/exec"
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

// detectGPUs enumerates GPUs on Linux. Each vendor-specific detector runs
// independently so a mixed host (e.g. NVIDIA + Intel Arc) reports every
// adapter, not just the first one whose driver stack is installed.
//
// Preferences:
//
//   - NVIDIA: nvidia-smi yields the marketing name, total VRAM, and a stable
//     per-GPU UUID reused as the join key (statsKey) against the dynamic stats
//     collector's snapshot. On unified-memory architectures (UMA, e.g.
//     Grace-Blackwell / DGX Spark) nvidia-smi reports [N/A] for memory.total
//     because the GPU shares system DRAM; VramBytes is filled from
//     detectMemoryTotal() instead.
//
//   - Intel: xpu-smi (Intel oneAPI) yields the marketing name, total VRAM
//     (dGPU only), and a per-adapter PCI BDF used as the join key. When
//     xpu-smi is not installed the sysfs walk under /sys/class/drm/card*
//     still identifies every Intel adapter by vendor ID and pulls VRAM
//     from mem_info_vram_total when the driver exposes it.
//
// When neither vendor-specific detector produces anything (no NVIDIA driver,
// no Intel adapter, or a pure AMD host) we fall back to ghw. ghw reports
// adapter names but no VRAM and no join key, so those hosts list their GPUs
// without dynamic VRAM/utilization — matching the pre-existing non-Windows
// behavior for unsupported vendors.
func detectGPUs() []GPUInfo {
	var gpus []GPUInfo
	gpus = append(gpus, detectNvidiaLinux()...)
	gpus = append(gpus, detectIntelGPUs()...)
	if len(gpus) == 0 {
		return detectGPUsGHW()
	}
	return gpus
}

// detectNvidiaLinux is the NVIDIA half of detectGPUs, extracted so the
// vendor-specific detectors compose without early-returning past each other.
// Returns nil when nvidia-smi is absent, times out, or produces an empty
// static row set.
func detectNvidiaLinux() []GPUInfo {
	out, err := nvidiaSmiCSV("uuid,name,memory.total")
	if err != nil {
		return nil
	}
	gpus, uma := parseNvidiaStatic(out)
	if len(gpus) == 0 {
		return nil
	}
	if uma {
		if total := detectMemoryTotal(); total > 0 {
			for i := range gpus {
				if gpus[i].usesSystemMemoryUsage {
					gpus[i].VramBytes = total
				}
			}
		}
	}
	return gpus
}

// detectGPUsGHW is the ghw-based fallback, identical in spirit to the
// non-Windows/non-Linux path in gpu_other.go: enumerate display adapters and
// return names only (VramBytes stays 0, statsKey stays empty).
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
		gpus = append(gpus, GPUInfo{Name: name})
	}
	return gpus
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
