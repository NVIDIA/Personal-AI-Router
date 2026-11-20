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
		index, ok := byAddress[card.pciAddress]
		if !ok {
			index = len(gpus)
			byAddress[card.pciAddress] = index
			gpus = append(gpus, card.GPUInfo)
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
	out, err := runXpuSmi(context.Background(), "discovery", "--json")
	if err != nil {
		slog.Debug("xpu-smi discovery unavailable", "err", err)
		return nil
	}
	sources, err := parseXpuSmiDiscovery(out)
	if err != nil {
		slog.Debug("xpu-smi discovery JSON parse failed", "err", err)
		return nil
	}
	physical := make([]GPUInfo, 0, len(sources))
	for _, src := range sources {
		if !isIntelVirtualFunction(src.pciAddress) {
			physical = append(physical, src.GPUInfo)
		}
	}
	return physical
}

func parseXpuSmiDiscovery(out []byte) ([]intelStatsSource, error) {
	var env xpuSmiDiscoveryEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, err
	}
	sources := make([]intelStatsSource, 0, len(env.DeviceList))
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
		src := intelStatsSource{GPUInfo: GPUInfo{
			Name:       name,
			VramBytes:  intelMemoryBytes(d.MemoryPhysicalSizeByte),
			statsKey:   intelStatsKeyPrefix + address,
			pciAddress: address,
		}}
		if d.DeviceID != nil && *d.DeviceID >= 0 {
			src.deviceID, src.hasDevID = *d.DeviceID, true
		}
		sources = append(sources, src)
	}
	return sources, nil
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

func runXpuSmi(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, xpuSmiTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xpu-smi", args...)
	// A helper may still hold an output pipe after cancellation.
	cmd.WaitDelay = statsTickInterval
	return cmd.Output()
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
