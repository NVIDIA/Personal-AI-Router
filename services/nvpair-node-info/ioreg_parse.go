// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"howett.net/plist"
)

type ioRegistryPerformance struct {
	DeviceUtilization   *uint64 `plist:"Device Utilization %"`
	GPUActivity         *uint64 `plist:"GPU Activity(%)"`
	AllocSystemMemory   uint64  `plist:"Alloc system memory"`
	InUseSystemMemory   uint64  `plist:"In use system memory"`
	VRAMUsedBytes       *uint64 `plist:"vramUsedBytes"`
	InUseVidMemoryBytes *uint64 `plist:"inUseVidMemoryBytes"`
	VRAMFreeBytes       *uint64 `plist:"vramFreeBytes"`
}

type ioRegistryEntry struct {
	EntryID               uint64                `plist:"IORegistryEntryID"`
	EntryName             string                `plist:"IORegistryEntryName"`
	ObjectClass           string                `plist:"IOObjectClass"`
	Model                 string                `plist:"model"`
	VRAMTotalMB           uint64                `plist:"VRAM,totalMB"`
	Performance           ioRegistryPerformance `plist:"PerformanceStatistics"`
	RegistryEntryChildren []ioRegistryEntry     `plist:"IORegistryEntryChildren"`
}

type darwinGPURecord struct {
	statsKey         string
	name             string
	vramTotal        uint64
	vramUsed         uint64
	utilizationPct   uint32
	utilizationValid bool
}

func parseIORegistryGPUs(data []byte, systemMemory uint64) ([]darwinGPURecord, error) {
	var roots []ioRegistryEntry
	if _, err := plist.Unmarshal(data, &roots); err != nil {
		return nil, fmt.Errorf("decode ioreg plist: %w", err)
	}

	records := make([]darwinGPURecord, 0, len(roots))
	for _, root := range roots {
		appendIORegistryGPUs(root, systemMemory, &records)
	}
	return records, nil
}

func appendIORegistryGPUs(entry ioRegistryEntry, systemMemory uint64, records *[]darwinGPURecord) {
	if entry.EntryID != 0 {
		name := strings.TrimSpace(entry.Model)
		if name == "" {
			name = strings.TrimSpace(entry.EntryName)
		}
		if name == "" {
			name = strings.TrimSpace(entry.ObjectClass)
		}
		if name != "" {
			*records = append(*records, normalizeIORegistryGPU(entry, name, systemMemory))
		}
	}
	for _, child := range entry.RegistryEntryChildren {
		appendIORegistryGPUs(child, systemMemory, records)
	}
}

func normalizeIORegistryGPU(entry ioRegistryEntry, name string, systemMemory uint64) darwinGPURecord {
	isApple := strings.HasPrefix(entry.ObjectClass, "AGXAccelerator") ||
		strings.HasPrefix(name, "Apple ")
	record := darwinGPURecord{
		statsKey: fmt.Sprintf("ioreg:%x", entry.EntryID),
		name:     name,
	}
	if isApple {
		record.vramTotal = systemMemory
		record.vramUsed = entry.Performance.AllocSystemMemory
	} else {
		record.vramTotal = entry.VRAMTotalMB * 1024 * 1024
		dedicatedUsed := entry.Performance.VRAMUsedBytes
		if dedicatedUsed == nil {
			dedicatedUsed = entry.Performance.InUseVidMemoryBytes
		}
		if dedicatedUsed != nil {
			record.vramUsed = *dedicatedUsed
		} else {
			record.vramUsed = entry.Performance.AllocSystemMemory
		}
		if record.vramTotal == 0 && dedicatedUsed != nil &&
			entry.Performance.VRAMFreeBytes != nil {
			record.vramTotal = *dedicatedUsed + *entry.Performance.VRAMFreeBytes
		}
	}
	var utilization uint64
	if entry.Performance.DeviceUtilization != nil {
		utilization = *entry.Performance.DeviceUtilization
		record.utilizationValid = true
	} else if entry.Performance.GPUActivity != nil {
		utilization = *entry.Performance.GPUActivity
		record.utilizationValid = true
	}
	if utilization > 100 {
		utilization = 100
	}
	record.utilizationPct = uint32(utilization)
	return record
}
