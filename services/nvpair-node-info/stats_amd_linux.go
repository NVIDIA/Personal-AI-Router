// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"time"
)

// Per-tick amdgpu sampling, the dynamic half of the AMD support whose
// inventory half lives in gpu_amd_linux.go.
//
// Every figure is one small sysfs read, so this runs inline on the collector's
// 1 s tick rather than in a sampler goroutine of its own (the accelerator
// sampler in accel_linux.go needs a goroutine because its utilization is a
// derivative of an interrupt counter observed at 100 ms; amdgpu keeps the busy
// percentage itself).
//
// The three numbers:
//
//   - utilization: gpu_busy_percent, already the 0..100 figure nvidia-smi's
//     utilization.gpu reports. A value outside that range, or an unreadable
//     file, is dropped rather than published as 0 - a driver that cannot say
//     must not look idle.
//   - VRAM used: mem_info_vram_used, plus mem_info_gtt_used on a unified part,
//     matching the capacity rule so used never exceeds total.
//
// A host without amdgpu costs one os.ReadDir of /sys/class/drm per tick and
// logs nothing at all; a host without /sys/class/drm logs a single Debug line
// for the process lifetime (listAMDCards).

// amdHwmonName is the hwmon driver name amdgpu registers its sensors under.
// Sensors from a differently named hwmon on the same device are still usable,
// they are just considered after amdgpu's own.
const amdHwmonName = "amdgpu"

// amdSampleAt folds one amdgpu sampling pass into the tick's GPU map and
// reports the sample time the snapshot should carry. A usable utilization
// reading is fresh GPU telemetry exactly as an nvidia-smi one is, so it
// advances sampledAt (and with it TelemetryValid); anything less leaves the
// caller's value untouched. This is the single line stats_linux.go adds.
func amdSampleAt(out map[string]gpuStat, sampledAt time.Time) time.Time {
	if decodeAMD(drmClassDir, out) {
		return time.Now()
	}
	return sampledAt
}

// decodeAMD samples every AMD card under drmRoot into out, keyed by the same
// statsKey the inventory stamped on the matching GPUInfo. sampled reports
// whether any card produced a valid utilization reading - the condition that
// marks the snapshot freshly sampled. A card that yields no figure at all is
// left out of the map entirely, so a transient sysfs failure keeps the row's
// previous values rather than publishing zeros over them.
func decodeAMD(drmRoot string, out map[string]gpuStat) bool {
	sampled := false
	for _, c := range listAMDCards(drmRoot) {
		var stat gpuStat
		any := false
		if pct, ok := amdBusyPercent(c.deviceDir); ok {
			stat.UtilizationPct = pct
			sampled = true
			any = true
		}
		if used, ok := amdUsedBytes(c); ok {
			stat.VRAMUsed = used
			any = true
		}
		if any {
			out[c.statsKey] = stat
		}
	}
	return sampled
}

// amdBusyPercent reads gpu_busy_percent. Out-of-range values are rejected:
// the attribute is documented as 0..100 and anything else means we are not
// reading what we think we are.
func amdBusyPercent(deviceDir string) (uint32, bool) {
	pct, ok := amdSysfsUint(deviceDir, "gpu_busy_percent")
	if !ok || pct > 100 {
		return 0, false
	}
	return uint32(pct), true
}

// amdUsedBytes is the card's in-use memory: driver-reported VRAM, plus the
// GTT the driver has mapped when the row's capacity included the GTT
// aperture. ok is false when the driver reports no VRAM usage at all.
func amdUsedBytes(c amdCard) (uint64, bool) {
	used, ok := amdSysfsUint(c.deviceDir, "mem_info_vram_used")
	if !ok {
		return 0, false
	}
	if c.unifiedPool {
		if gtt, okGTT := amdSysfsUint(c.deviceDir, "mem_info_gtt_used"); okGTT {
			used += gtt
		}
	}
	return used, true
}
