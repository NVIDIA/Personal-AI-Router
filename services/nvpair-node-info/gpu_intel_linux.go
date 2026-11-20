// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// Cap each xpu-smi call in case the driver stalls.
	xpuSmiTimeout = 3 * time.Second

	intelPCIVendorID    = "0x8086"
	intelStatsKeyPrefix = "intel-pci-"
)

// xpuSmiDiscoveryDevice mirrors the subset of xpu-smi's `discovery --json`
// entries we consume. Only the fields relevant to inventory are pulled;
// extra keys the tool may add in future versions are ignored by
// encoding/json.
//
// memory_physical_size_byte is present on discrete Intel GPUs (Arc,
// Data Center) and absent (or zero) on iGPUs that share system memory.
// It arrives as either a numeric or string on different xpu-smi builds,
// so we parse it defensively in intelMemoryBytes.
type xpuSmiDiscoveryDevice struct {
	DeviceID               *int            `json:"device_id"`
	DeviceName             string          `json:"device_name"`
	DeviceType             string          `json:"device_type"`
	DeviceFunctionType     string          `json:"device_function_type"`
	PCIBDFAddress          string          `json:"pci_bdf_address"`
	MemoryPhysicalSizeByte json.RawMessage `json:"memory_physical_size_byte"`
}

type xpuSmiDiscoveryEnvelope struct {
	DeviceList []xpuSmiDiscoveryDevice `json:"device_list"`
}

// detectIntelGPUs prefers xpu-smi's name and capacity for each PCI address.
// A card missing from Level Zero still contributes its DRM identity and counters.
func detectIntelGPUs(generic []GPUInfo) []GPUInfo {
	gpus := detectIntelViaXpuSmi()
	byAddress := make(map[string]int, len(gpus))
	for i, gpu := range gpus {
		byAddress[gpu.pciAddress] = i
	}
	for _, card := range collectIntelSysfsCards() {
		index, ok := byAddress[card.bdf]
		if !ok {
			index = len(gpus)
			byAddress[card.bdf] = index
			gpus = append(gpus, GPUInfo{Name: "Intel GPU", statsKey: card.statsKey, pciAddress: card.bdf})
		}
		gpu := &gpus[index]
		if gpu.VramBytes == 0 {
			gpu.VramBytes = readSysfsVRAMTotal(card.sysfsCard)
		}
	}
	for i := range gpus {
		gpu := &gpus[i]
		if gpu.Name == "Intel GPU" {
			for _, adapter := range generic {
				if adapter.pciAddress == gpu.pciAddress && adapter.Name != "" && adapter.Name != "Unknown" {
					gpu.Name = adapter.Name
					break
				}
			}
		}
		if gpu.VramBytes == 0 {
			if node := intelRenderNodeForBDF(gpu.pciAddress); node != "" {
				gpu.VramBytes, _ = intelVRAMTotalFromDRM(node)
			}
		}
	}
	return gpus
}

// detectIntelViaXpuSmi runs `xpu-smi discovery --json` and folds the
// device list into GPUInfo. An unavailable or empty result leaves sysfs
// as the inventory source.
func detectIntelViaXpuSmi() []GPUInfo {
	out, err := xpuSmiJSON(context.Background(), "discovery", "--json")
	if err != nil {
		slog.Debug("xpu-smi discovery unavailable", "err", err)
		return nil
	}
	gpus, err := parseXpuSmiDiscovery(out)
	if err != nil {
		slog.Debug("xpu-smi discovery JSON parse failed", "err", err)
		return nil
	}
	physical := gpus[:0]
	for _, gpu := range gpus {
		if !isIntelVirtualFunction(gpu.pciAddress) {
			physical = append(physical, gpu)
		}
	}
	return physical
}

func parseXpuSmiDiscovery(out []byte) ([]GPUInfo, error) {
	var env xpuSmiDiscoveryEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, err
	}
	gpus := make([]GPUInfo, 0, len(env.DeviceList))
	for _, d := range env.DeviceList {
		if !isPhysicalIntelGPU(d) {
			continue
		}
		address := canonicalPCIAddress(d.PCIBDFAddress)
		if address == "" {
			continue
		}
		name := strings.TrimSpace(d.DeviceName)
		if name == "" {
			name = "Intel GPU"
		}
		gpus = append(gpus, GPUInfo{
			Name:       name,
			VramBytes:  intelMemoryBytes(d.MemoryPhysicalSizeByte),
			statsKey:   intelStatsKeyPrefix + address,
			pciAddress: address,
		})
	}
	return gpus, nil
}

// isPhysicalIntelGPU filters the xpu-smi device list to physical GPU
// entries. device_type distinguishes GPU from other Intel accelerators;
// device_function_type separates physical adapters from SR-IOV virtual
// functions ("virtual"). A missing device_function_type is treated as
// physical for compatibility with xpu-smi builds that don't emit it.
func isPhysicalIntelGPU(d xpuSmiDiscoveryDevice) bool {
	if !strings.EqualFold(strings.TrimSpace(d.DeviceType), "GPU") &&
		d.DeviceType != "" {
		return false
	}
	fn := strings.ToLower(strings.TrimSpace(d.DeviceFunctionType))
	switch fn {
	case "", "physical":
		return true
	default:
		return false
	}
}

// intelMemoryBytes decodes memory_physical_size_byte. xpu-smi has
// emitted this field as a number ("1699966976") on some builds and as
// a string ("\"1699966976\"") on others; a missing value or zero is
// treated as "unknown" (VramBytes stays 0, which the omitempty tag
// drops from the wire).
func intelMemoryBytes(raw json.RawMessage) uint64 {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0
	}
	trimmed = strings.Trim(trimmed, `"`)
	v, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// intelStatsKeyFromBDF normalizes a PCI BDF address to the statsKey the
// stats collector joins on. xpu-smi and sysfs both spell the address
// lowercase already, but we normalize defensively so a version that
// reports uppercase (or omits the domain prefix) still joins.
func intelStatsKeyFromBDF(bdf string) string {
	address := canonicalPCIAddress(bdf)
	if address == "" {
		return ""
	}
	return intelStatsKeyPrefix + address
}

// xpuSmiJSON runs the xpu-smi CLI with the supplied argv and returns
// stdout. A missing binary (not on PATH) surfaces as an exec error,
// which callers treat as "no xpu-smi telemetry available" and degrade
// silently to sysfs.
func xpuSmiJSON(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, xpuSmiTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "xpu-smi", args...).Output()
}

// readSysfsBDF resolves the PCI BDF address a DRM card is attached to.
// The `device` entry under /sys/class/drm/cardN is a symlink into
// /sys/devices/pci*/..., whose final component is the BDF.
func readSysfsBDF(cardPath string) (string, error) {
	target, err := os.Readlink(filepath.Join(cardPath, "device"))
	if err != nil {
		return "", err
	}
	return canonicalPCIAddress(filepath.Base(target)), nil
}

// readSysfsVRAMTotal returns the DRM-reported total VRAM in bytes, or
// zero when the driver doesn't expose it (iGPU) or the read fails.
func readSysfsVRAMTotal(cardPath string) uint64 {
	data, err := os.ReadFile(filepath.Join(cardPath, "device", "mem_info_vram_total"))
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// readSysfsVRAMUsed returns the DRM-reported bytes of VRAM in use, or
// (0, false) when the driver doesn't expose it or the read fails. This
// is the sysfs counterpart to xpu-smi's dynamic stats and is consumed
// from stats_linux.go.
func readSysfsVRAMUsed(cardPath string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(cardPath, "device", "mem_info_vram_used"))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
