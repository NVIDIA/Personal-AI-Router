// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// Merge vendor-specific inventory with generic discovery so detecting one
// GPU does not hide another vendor's adapters on a mixed host.
func detectGPUs() []GPUInfo {
	nvidia := detectNvidiaLinux()
	generic := detectGPUsGHW()
	return mergeLinuxGPUInventory(nvidia, detectIntelGPUs(generic), generic)
}

func mergeLinuxGPUInventory(nvidia, intel, generic []GPUInfo) []GPUInfo {
	missingNvidiaAddress := false
	for _, gpu := range nvidia {
		if canonicalPCIAddress(gpu.pciAddress) == "" {
			missingNvidiaAddress = true
		}
	}
	vendor := append(nvidia, intel...)
	seen := make(map[string]bool, len(vendor))
	for _, gpu := range vendor {
		if address := canonicalPCIAddress(gpu.pciAddress); address != "" {
			seen[address] = true
		}
	}
	for _, gpu := range generic {
		address := canonicalPCIAddress(gpu.pciAddress)
		if strings.EqualFold(gpu.pciVendor, "8086") && isIntelVirtualFunction(address) {
			continue
		}
		if address != "" && seen[address] {
			continue
		}
		// If an NVIDIA address is unavailable, its generic counterpart cannot
		// be identified reliably. Keep the vendor list rather than duplicate it.
		if missingNvidiaAddress && strings.EqualFold(gpu.pciVendor, "10de") {
			continue
		}
		vendor = append(vendor, gpu)
		if address != "" {
			seen[address] = true
		}
	}
	return vendor
}

func isIntelVirtualFunction(address string) bool {
	if address == "" {
		return false
	}
	path := filepath.Join(os.Getenv("GHW_CHROOT"), "/sys/bus/pci/devices", address, "physfn")
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func detectNvidiaLinux() []GPUInfo {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSmiTimeout)
	defer cancel()
	out, err := queryNvidiaSmi(ctx, "uuid,name,memory.total,pci.bus_id")
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) {
		message := strings.ToLower(out + " " + string(exit.Stderr))
		// Retry only if the CLI rejected the field, within the same deadline.
		if strings.Contains(message, "pci.bus_id") &&
			(strings.Contains(message, "not a valid field") || strings.Contains(message, "not supported")) {
			out, err = queryNvidiaSmi(ctx, "uuid,name,memory.total")
		}
	}
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

// detectGPUsGHW supplies display-adapter names and PCI identities. It has no
// VRAM reading. AMD cards get a PCI-derived key for utilization.
func detectGPUsGHW() []GPUInfo {
	gpu, err := ghw.GPU()
	if err != nil {
		log.Printf("GPU detection error: %v", err)
		return nil
	}
	var gpus []GPUInfo
	for _, card := range gpu.GraphicsCards {
		name := "Unknown"
		var vendor string
		if card.DeviceInfo != nil && card.DeviceInfo.Product != nil {
			name = card.DeviceInfo.Product.Name
		}
		if card.DeviceInfo != nil && card.DeviceInfo.Vendor != nil {
			vendor = card.DeviceInfo.Vendor.ID
		}
		gpus = append(gpus, GPUInfo{
			Name: name, statsKey: amdStatsKey(amdPCIRoot, card.Address),
			pciAddress: canonicalPCIAddress(card.Address), pciVendor: vendor,
		})
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
	return queryNvidiaSmi(ctx, fields)
}

func queryNvidiaSmi(ctx context.Context, fields string) (string, error) {
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

// parseNvidiaStatic decodes the static query (uuid,name,memory.total,pci.bus_id) into
// GPUInfo records. memory.total is reported in MiB (because of -nounits); we
// convert to bytes. Rows missing a UUID or name are skipped. An unparseable
// VRAM value leaves capacity unavailable while keeping the adapter. Each
// unified-memory row is marked so response assembly can source its used bytes
// from system memory without depending on dynamic nvidia-smi collection. The
// second return value reports whether any row was unified, allowing the caller
// to fetch the shared system-memory total only when needed.
func parseNvidiaStatic(out string) ([]GPUInfo, bool) {
	var gpus []GPUInfo
	var unifiedMemory bool
	for line := range strings.SplitSeq(out, "\n") {
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
		var pciAddress string
		if len(fields) > 3 {
			pciAddress = canonicalPCIAddress(fields[3])
		}
		gpus = append(gpus, GPUInfo{
			Name:                  name,
			VramBytes:             vramBytes,
			statsKey:              uuid,
			pciAddress:            pciAddress,
			usesSystemMemoryUsage: usesUnifiedMemory,
		})
	}
	return gpus, unifiedMemory
}

var pciAddressPattern = regexp.MustCompile(`^(?:([0-9a-f]{1,8}):)?([0-9a-f]{2}):([0-9a-f]{2})\.([0-7])$`)

// canonicalPCIAddress handles the eight-digit domain reported by nvidia-smi.
func canonicalPCIAddress(address string) string {
	parts := pciAddressPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(address)))
	if parts == nil {
		return ""
	}
	var domain uint64
	if parts[1] != "" {
		var err error
		domain, err = strconv.ParseUint(parts[1], 16, 16)
		if err != nil {
			return ""
		}
	}
	return fmt.Sprintf("%04x:%s:%s.%s", domain, parts[2], parts[3], parts[4])
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
