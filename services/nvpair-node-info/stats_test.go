// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseEngineInstance pins the PDH "GPU Engine" instance-name format.
// If this ever drifts, aggregateUtilization would silently produce an
// empty map for every adapter and every node would report
// utilization_percent=0. The table covers: the typical shape, the vendor-
// specific "Compute_0" suffix (NVIDIA's way of distinguishing async
// compute queues, which PDH just lets through as part of engtype),
// a few malformed variants we must reject, and the edge case of a
// trailing underscore with no engine type.
func TestParseEngineInstance(t *testing.T) {
	test := func(name, in, wantLuid, wantType string, wantOK bool) {
		t.Run(name, func(t *testing.T) {
			luid, et, ok := parseEngineInstance(in)
			require.Equal(t, wantOK, ok)
			require.Equal(t, wantLuid, luid)
			require.Equal(t, wantType, et)
		})
	}
	test("typical 3D engine", "pid_1234_luid_0x00000000_0x000054f0_phys_0_eng_0_engtype_3D", "luid_0x00000000_0x000054f0_phys_0", "3d", true)
	test("compute variant", "pid_99_luid_0x00000000_0x0000abcd_phys_0_eng_2_engtype_Compute_0", "luid_0x00000000_0x0000abcd_phys_0", "compute_0", true)
	test("mixed-case hex normalizes to lower", "pid_1_luid_0x00000000_0x000054F0_phys_0_eng_0_engtype_3D", "luid_0x00000000_0x000054f0_phys_0", "3d", true)
	test("missing luid prefix", "pid_1_eng_0_engtype_3D", "", "", false)
	test("missing eng segment", "pid_1_luid_0x0_0x0_phys_0_engtype_3D", "", "", false)
	test("missing engtype marker", "pid_1_luid_0x0_0x0_phys_0_eng_0", "", "", false)
	test("empty engtype value", "pid_1_luid_0x0_0x0_phys_0_eng_0_engtype_", "", "", false)
	test("unrelated garbage", "completely unrelated string", "", "", false)
}

// TestAggregateUtilization exercises Task Manager's three-step algorithm:
// sum by (luid, engtype), clamp to 100, max across engtypes per luid.
// Each test case targets one specific step so a regression in any of
// them produces a pinpointable failure.
func TestAggregateUtilization(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]float64
		want map[string]uint32
	}{
		{
			name: "empty input",
			in:   map[string]float64{},
			want: map[string]uint32{},
		},
		{
			name: "single engine single process rounds half-up",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 42.5,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 43,
			},
		},
		{
			name: "sum across processes on same engine",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 30,
				"pid_2_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 20,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 50,
			},
		},
		{
			name: "max across engines selects busiest",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D":      10,
				"pid_1_luid_0x0_0x1_phys_0_eng_1_engtype_Compute": 80,
				"pid_1_luid_0x0_0x1_phys_0_eng_2_engtype_Copy":    5,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 80,
			},
		},
		{
			name: "sum exceeding 100 is clamped before max",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 75,
				"pid_2_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 60,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 100,
			},
		},
		{
			name: "multiple GPUs report independently",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 20,
				"pid_1_luid_0x0_0x2_phys_0_eng_0_engtype_3D": 90,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 20,
				"luid_0x0_0x2_phys_0": 90,
			},
		},
		{
			name: "negatives and unparseable instances are dropped",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": -5,
				"garbage": 99,
				"pid_2_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 10,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 10,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, aggregateUtilization(c.in), "aggregateUtilization")
		})
	}
}

func TestApplyGPUStatsRetainsLastUsableSample(t *testing.T) {
	sampledAt := time.Unix(1_700_000_000, 0)
	previous := statsSnapshot{
		GPU: map[string]gpuStat{
			"gpu-a": {VRAMUsed: 4 << 30, UtilizationPct: 73},
		},
		GPUSampledAt: sampledAt,
	}
	next := &statsSnapshot{}

	applyGPUStats(previous, next, map[string]gpuStat{
		"gpu-a": {VRAMUsed: 5 << 30},
	}, time.Time{})

	require.Equal(t, previous.GPU, next.GPU, "failed collection replaced last usable GPU sample:")
	require.WithinDuration(t, sampledAt, next.GPUSampledAt, 0, "failed collection moved sample time")
}

func TestApplyGPUStatsPublishesIdleAndPreSampleFields(t *testing.T) {
	partial := map[string]gpuStat{"gpu-a": {VRAMUsed: 2 << 30}}
	beforeFirstSample := &statsSnapshot{}
	applyGPUStats(statsSnapshot{}, beforeFirstSample, partial, time.Time{})
	require.Equal(t, partial, beforeFirstSample.GPU, "pre-sample fields (%v)", beforeFirstSample)
	require.True(t, beforeFirstSample.GPUSampledAt.IsZero(), "pre-sample fields (%v)", beforeFirstSample)

	sampledAt := time.Unix(1_700_000_000, 123)
	idle := map[string]gpuStat{"gpu-a": {UtilizationPct: 0}}
	usable := &statsSnapshot{}
	applyGPUStats(statsSnapshot{}, usable, idle, sampledAt)
	require.Equal(t, idle, usable.GPU, "idle sample (%v, %v)", usable, sampledAt)
	require.WithinDuration(t, sampledAt, usable.GPUSampledAt, 0, "idle sample (%v)", usable)
}

func TestBuildResponseTelemetryFreshness(t *testing.T) {
	now := time.Unix(1_700_000_000, 500_000_000)
	test := func(name string, sampledAt time.Time, wantValid bool, wantAge int64) {
		t.Run(name, func(t *testing.T) {
			body := buildResponseAt(
				[]GPUInfo{{Name: "GPU 0", statsKey: "gpu-a"}},
				nil,
				0,
				statsSnapshot{
					GPU:          map[string]gpuStat{"gpu-a": {UtilizationPct: 0}},
					GPUSampledAt: sampledAt,
				},
				"",
				nil,
				now,
			)
			var typed NodeInfoResponse
			require.NoError(t, json.Unmarshal(body, &typed), "decode response")
			require.Equal(t, wantValid, typed.TelemetryValid)
			require.Equal(t, wantAge, typed.MSSince)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(body, &raw), "decode raw response")
			require.Contains(t, raw, "telemetryValid", "telemetryValid missing from response")
			require.Contains(t, raw, "msSince", "msSince missing from response")
		})
	}
	test("no sample", time.Time{}, false, 0)
	test("valid idle sample", now.Add(-137*time.Millisecond), true, 137)
	test("future sample clamps age", now.Add(time.Millisecond), true, 0)
}

// buildResponseDecode is a test helper that marshals through the real
// JSON path so omitempty behavior is part of the assertion — e.g. a
// GPU whose stats are zero must produce a response with the field
// absent, not `"vram_used_bytes":0`; a nil CPUInfo must produce JSON
// with no "cpu" key at all, not `"cpu":null`.
func buildResponseDecode(t *testing.T, static []GPUInfo, cpu *CPUInfo, memTotal uint64, snap statsSnapshot) (NodeInfoResponse, map[string]any) {
	t.Helper()
	body := buildResponse(static, cpu, memTotal, snap, "", nil)
	var typed NodeInfoResponse
	require.NoError(t, json.Unmarshal(body, &typed), "typed decode")
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw), "raw decode")
	return typed, raw
}

// TestBuildResponseMerge verifies the happy path: matched LUIDs get
// dynamic fields from the stats map, unmatched LUIDs stay zero.
func TestBuildResponseMerge(t *testing.T) {
	static := []GPUInfo{
		{Name: "GPU 0", VramBytes: 16 << 30, statsKey: "luid_a"},
		{Name: "GPU 1", VramBytes: 8 << 30, statsKey: "luid_b"},
	}
	snap := statsSnapshot{
		GPU: map[string]gpuStat{
			"luid_a": {VRAMUsed: 4 << 30, UtilizationPct: 27},
			// luid_b intentionally absent — must stay zero.
		},
	}
	typed, _ := buildResponseDecode(t, static, nil, 0, snap)

	require.Len(t, typed.GPUs, 2)
	assert.Equal(t, uint64(4<<30), typed.GPUs[0].VramUsedBytes, "gpu 0 VramUsedBytes")
	assert.Equal(t, uint32(27), typed.GPUs[0].UtilizationPercent, "gpu 0 UtilizationPercent")
	assert.Equal(t, uint64(0), typed.GPUs[1].VramUsedBytes, "gpu 1 VramUsedBytes")
	assert.Equal(t, uint32(0), typed.GPUs[1].UtilizationPercent, "gpu 1 UtilizationPercent")
}

// TestBuildResponseUnifiedMemoryUsesSystemSnapshot verifies that UMA memory
// usage comes from the independently collected system-memory snapshot. It must
// remain available when nvidia-smi produced no dynamic GPU row, and a matching
// row may contribute utilization but must not replace the shared-memory value
// with a dedicated-VRAM counter.
func TestBuildResponseUnifiedMemoryUsesSystemSnapshot(t *testing.T) {
	const sysMemUsed uint64 = 48 << 30
	static := []GPUInfo{{
		Name:                  "NVIDIA GB10",
		VramBytes:             128 << 30,
		statsKey:              "GPU-spark",
		usesSystemMemoryUsage: true,
	}}
	test := func(name string, gpuStats map[string]gpuStat, wantUtil uint32) {
		t.Run(name, func(t *testing.T) {
			snap := statsSnapshot{GPU: gpuStats, MemUsedBytes: sysMemUsed}
			typed, raw := buildResponseDecode(t, static, nil, 0, snap)
			assert.Equal(t, sysMemUsed, typed.GPUs[0].VramUsedBytes)
			assert.Equal(t, wantUtil, typed.GPUs[0].UtilizationPercent)

			gpus, ok := raw["GPUs"].([]any)
			require.True(t, ok, "unexpected GPUs payload")
			require.Len(t, gpus, 1, "unexpected GPUs payload")
			obj, ok := gpus[0].(map[string]any)
			require.True(t, ok, "gpu 0 not an object")
			assert.Contains(t, obj, "vram_used_bytes", "vram_used_bytes should be present for unified memory")
		})
	}
	test("without dynamic nvidia-smi row", nil, 0)
	test("with dynamic utilization row", map[string]gpuStat{"GPU-spark": {VRAMUsed: 1 << 30, UtilizationPct: 42}}, 42)
}

// Apple Silicon shares physical memory but ioreg reports the GPU-specific
// mapped allocation. It must flow through the normal per-adapter stats path,
// not be replaced with whole-system memory usage.
func TestBuildResponseAppleUnifiedMemoryUsesGPUAllocation(t *testing.T) {
	static := []GPUInfo{{
		Name:      "Apple M3 Max",
		VramBytes: 36 << 30,
		statsKey:  "ioreg:2a",
	}}
	snap := statsSnapshot{
		GPU: map[string]gpuStat{
			"ioreg:2a": {VRAMUsed: 8 << 30, UtilizationPct: 73},
		},
		MemUsedBytes: 24 << 30,
	}
	typed, _ := buildResponseDecode(t, static, nil, 0, snap)
	gpu := typed.GPUs[0]
	require.Equal(t, uint64(8<<30), gpu.VramUsedBytes, "unexpected Apple GPU metrics (%v)", gpu)
	require.Equal(t, uint32(73), gpu.UtilizationPercent, "unexpected Apple GPU metrics (%v)", gpu)
}

func TestBuildResponseRecoversDarwinGPUInventory(t *testing.T) {
	snap := statsSnapshot{
		GPU: map[string]gpuStat{
			"ioreg:2a": {VRAMUsed: 8 << 30, UtilizationPct: 73},
		},
		GPUInventory: []GPUInfo{{
			Name:      "Apple M3 Max",
			VramBytes: 36 << 30,
			statsKey:  "ioreg:2a",
		}},
	}
	typed, _ := buildResponseDecode(t, nil, nil, 0, snap)
	require.Len(t, typed.GPUs, 1, "recovered GPU count")
	gpu := typed.GPUs[0]
	require.Equal(t, "Apple M3 Max", gpu.Name, "unexpected recovered GPU (%v)", gpu)
	require.Equal(t, uint64(36<<30), gpu.VramBytes, "unexpected recovered GPU (%v)", gpu)
	require.Equal(t, uint64(8<<30), gpu.VramUsedBytes, "unexpected recovered GPU (%v)", gpu)
	require.Equal(t, uint32(73), gpu.UtilizationPercent, "unexpected recovered GPU (%v)", gpu)

	static := []GPUInfo{{Name: "Apple M3 Max", statsKey: "ioreg:2a"}}
	merged := mergeGPUInventory(static, snap.GPUInventory)
	require.Len(t, merged, 1, "matching recovered GPU did not enrich in place")
	require.Equal(t, uint64(36<<30), merged[0].VramBytes, "matching recovered GPU did not enrich in place (%v)", merged)
}

// TestBuildResponseOmitsZero confirms that a GPU with no stats match
// produces JSON without the dynamic fields, not with literal zeros.
// Clients depend on this to distinguish "unknown / unavailable" from
// "actually idle" for vram_used_bytes.
func TestBuildResponseOmitsZero(t *testing.T) {
	static := []GPUInfo{{Name: "GPU 0", VramBytes: 4 << 30, statsKey: "luid_a"}}
	_, raw := buildResponseDecode(t, static, nil, 0, statsSnapshot{})

	gpus, ok := raw["GPUs"].([]any)
	require.True(t, ok, "unexpected GPUs payload")
	require.Len(t, gpus, 1, "unexpected GPUs payload")
	obj, ok := gpus[0].(map[string]any)
	require.True(t, ok, "gpu 0 not an object")
	assert.NotContains(t, obj, "vram_used_bytes", "vram_used_bytes should be absent when unknown")
	assert.NotContains(t, obj, "utilization_percent", "utilization_percent should be absent when unknown")
}

// TestBuildResponseCPUMemoryMatrix exercises the four combinations of
// cpu/memory presence and absence so the pointer-omitempty behavior is
// pinned down: a nil *CPUInfo or a zero memTotal must drop the entire
// top-level object, not produce an empty shell like `"cpu":{}` or
// `"memory":{"total_bytes":0}`. Clients distinguish "host can't
// introspect this subsystem" from "subsystem reports zero" exactly the
// way they already do for GPU dynamic fields.
func TestBuildResponseCPUMemoryMatrix(t *testing.T) {
	cpu := &CPUInfo{Name: "Intel Core i9-13900K", Cores: 24}
	const memTotal uint64 = 32 << 30
	snap := statsSnapshot{
		GPU:          map[string]gpuStat{},
		CPUUtilPct:   42,
		MemUsedBytes: 10 << 30,
	}

	cases := []struct {
		name         string
		cpu          *CPUInfo
		memTotal     uint64
		snap         statsSnapshot
		wantCPUKey   bool
		wantMemKey   bool
		wantCPUUtil  uint32
		wantCPUName  string
		wantCPUCores uint32
		wantMemTotal uint64
		wantMemUsed  uint64
	}{
		{
			name:         "cpu and memory both present",
			cpu:          cpu,
			memTotal:     memTotal,
			snap:         snap,
			wantCPUKey:   true,
			wantMemKey:   true,
			wantCPUUtil:  42,
			wantCPUName:  cpu.Name,
			wantCPUCores: 24,
			wantMemTotal: memTotal,
			wantMemUsed:  10 << 30,
		},
		{
			name:         "cpu only",
			cpu:          cpu,
			memTotal:     0,
			snap:         statsSnapshot{CPUUtilPct: 42},
			wantCPUKey:   true,
			wantMemKey:   false,
			wantCPUUtil:  42,
			wantCPUName:  cpu.Name,
			wantCPUCores: 24,
		},
		{
			name:         "memory only",
			cpu:          nil,
			memTotal:     memTotal,
			snap:         statsSnapshot{MemUsedBytes: 10 << 30},
			wantCPUKey:   false,
			wantMemKey:   true,
			wantMemTotal: memTotal,
			wantMemUsed:  10 << 30,
		},
		{
			name:       "neither",
			cpu:        nil,
			memTotal:   0,
			snap:       statsSnapshot{},
			wantCPUKey: false,
			wantMemKey: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			typed, raw := buildResponseDecode(t, nil, c.cpu, c.memTotal, c.snap)

			if c.wantCPUKey {
				assert.Contains(t, raw, "cpu")
				require.NotNil(t, typed.CPU, "typed.CPU is nil but cpu key was present in raw JSON")
				assert.Equal(t, c.wantCPUName, typed.CPU.Name, "cpu.Name")
				assert.Equal(t, c.wantCPUCores, typed.CPU.Cores, "cpu.Cores")
				assert.Equal(t, c.wantCPUUtil, typed.CPU.UtilizationPercent, "cpu.UtilizationPercent")
			} else {
				assert.NotContains(t, raw, "cpu")
				assert.Nil(t, typed.CPU)
			}

			if c.wantMemKey {
				assert.Contains(t, raw, "memory")
				require.NotNil(t, typed.Memory, "typed.Memory is nil but memory key was present in raw JSON")
				assert.Equal(t, c.wantMemTotal, typed.Memory.TotalBytes, "memory.TotalBytes")
				assert.Equal(t, c.wantMemUsed, typed.Memory.UsedBytes, "memory.UsedBytes")
			} else {
				assert.NotContains(t, raw, "memory")
				assert.Nil(t, typed.Memory)
			}
		})
	}
}
