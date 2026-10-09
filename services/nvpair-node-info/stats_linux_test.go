// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseProcStat pins the /proc/stat aggregate-line parse. The collector
// derives CPU % from the delta of two of these samples, so a regression here
// (e.g. picking up a per-core "cpuN" line, or miscounting idle) would make
// every Linux node report a wrong or zero utilization.
func TestParseProcStat(t *testing.T) {
	test := func(name, in string, wantIdle, wantTotal uint64, wantValid bool) {
		t.Run(name, func(t *testing.T) {
			got := parseProcStat(in)
			require.Equal(t, wantValid, got.valid)
			if !wantValid {
				return
			}
			require.Equal(t, wantIdle, got.idle)
			require.Equal(t, wantTotal, got.total)
		})
	}
	// Idle includes both idle and iowait ticks.
	test("typical aggregate line, idle = idle+iowait", "cpu  100 0 50 800 40 0 10 0 0 0\ncpu0 50 0 25 400 20 0 5 0 0 0\n", 840, 1000, true)
	test("ignores cpuN lines, only aggregate counts", "cpu0 50 0 25 400 20 0 5 0 0 0\ncpu  10 0 10 70 10 0 0 0 0 0\n", 80, 100, true)
	test("no aggregate cpu line", "intr 12345\nctxt 6789\n", 0, 0, false)
	test("too few fields", "cpu 1 2\n", 0, 0, false)
	test("all-zero totals are invalid", "cpu  0 0 0 0 0 0 0 0 0 0\n", 0, 0, false)
}

// TestCPUUtilization exercises the delta math and every guard: invalid
// samples, counter resets, zero elapsed time, and rounding.
func TestCPUUtilization(t *testing.T) {
	valid := func(idle, total uint64) cpuTimes {
		return cpuTimes{idle: idle, total: total, valid: true}
	}
	test := func(name string, prev, cur cpuTimes, want uint32) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, cpuUtilization(prev, cur))
		})
	}
	test("50 percent busy", valid(100, 200), valid(200, 400), 50)
	test("fully busy", valid(100, 200), valid(100, 300), 100)
	test("fully idle", valid(100, 200), valid(200, 300), 0)
	test("rounds half up", valid(0, 0), valid(425, 1000), 58) // 57.5% busy
	test("invalid prev -> 0", cpuTimes{}, valid(1, 2), 0)
	test("counter reset (total went backwards) -> 0", valid(100, 500), valid(50, 200), 0)
	test("no elapsed jiffies -> 0", valid(100, 200), valid(100, 200), 0)
}

// TestInitialMemorySnapshot verifies that startup publishes a usable memory
// sample before the first ticker event while preserving omission semantics
// when /proc/meminfo cannot be read.
func TestInitialMemorySnapshot(t *testing.T) {
	test := func(name string, used uint64, ok bool, wantUsed uint64) {
		t.Run(name, func(t *testing.T) {
			snap := initialMemorySnapshot(func() (uint64, bool) { return used, ok })
			require.Equal(t, wantUsed, snap.MemUsedBytes)
		})
	}
	test("successful startup read", 48<<30, true, 48<<30)
	test("failed startup read remains unknown", 123, false, 0)
}

// TestParseMeminfoUsed pins the MemTotal-MemAvailable computation, the
// MemFree fallback for pre-3.14 kernels, the kB->bytes conversion, and the
// failure cases (missing total, underflow).
func TestParseMeminfoUsed(t *testing.T) {
	test := func(name, in string, wantUsed uint64, wantOK bool) {
		t.Run(name, func(t *testing.T) {
			used, ok := parseMeminfoUsed(in)
			require.Equal(t, wantOK, ok)
			if ok {
				require.Equal(t, wantUsed, used)
			}
		})
	}
	test("MemAvailable present", "MemTotal:       1000 kB\nMemFree:         200 kB\nMemAvailable:    400 kB\n", 600*1024, true)                       // (1000 - 400) kB
	test("MemAvailable absent, falls back to MemFree", "MemTotal:       1000 kB\nMemFree:         200 kB\nBuffers:          50 kB\n", 800*1024, true) // (1000 - 200) kB
	test("missing MemTotal", "MemFree:         200 kB\nMemAvailable:    400 kB\n", 0, false)
	test("available exceeds total and no usable free -> fail", "MemTotal:        100 kB\nMemAvailable:    200 kB\n", 0, false)
}

// TestParseNvidiaStatic pins the static enumeration parse: name + total VRAM
// (MiB->bytes) keyed by UUID, with skips for malformed rows, plus UMA [N/A]
// detection.
func TestParseNvidiaStatic(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []GPUInfo
		wantUMA bool
	}{
		{
			name: "discrete VRAM",
			in: "GPU-aaa, NVIDIA GeForce RTX 4090, 24564\n" +
				"GPU-bbb, NVIDIA A100, 81920\n" +
				", MissingUUID, 4096\n" + // skipped: empty uuid
				"GPU-ccc, , 4096\n" + // skipped: empty name
				"GPU-ddd, Bad VRAM, notanumber\n", // kept, vram 0
			want: []GPUInfo{
				{Name: "NVIDIA GeForce RTX 4090", VramBytes: 24564 * 1024 * 1024, statsKey: "GPU-aaa"},
				{Name: "NVIDIA A100", VramBytes: 81920 * 1024 * 1024, statsKey: "GPU-bbb"},
				{Name: "Bad VRAM", VramBytes: 0, statsKey: "GPU-ddd"},
			},
		},
		{
			name: "unified memory [N/A]",
			in:   "GPU-spark, NVIDIA GB10, [N/A]\n",
			want: []GPUInfo{
				{Name: "NVIDIA GB10", VramBytes: 0, statsKey: "GPU-spark", usesSystemMemoryUsage: true},
			},
			wantUMA: true,
		},
		{
			name: "unified memory Not Supported",
			in:   "GPU-spark, NVIDIA GB10, [Not Supported]\n",
			want: []GPUInfo{
				{Name: "NVIDIA GB10", VramBytes: 0, statsKey: "GPU-spark", usesSystemMemoryUsage: true},
			},
			wantUMA: true,
		},
		{
			name: "mixed discrete and unified memory",
			in: "GPU-aaa, NVIDIA GeForce RTX 4090, 24564\n" +
				"GPU-spark, NVIDIA GB10, [N/A]\n",
			want: []GPUInfo{
				{Name: "NVIDIA GeForce RTX 4090", VramBytes: 24564 * 1024 * 1024, statsKey: "GPU-aaa"},
				{Name: "NVIDIA GB10", VramBytes: 0, statsKey: "GPU-spark", usesSystemMemoryUsage: true},
			},
			wantUMA: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, uma := parseNvidiaStatic(c.in)
			require.Equal(t, c.wantUMA, uma, "unifiedMemory (%v)", uma)
			require.Equal(t, c.want, got, "parseNvidiaStatic()")
		})
	}
}

// TestParseNvidiaDynamic pins the dynamic-stats parse: per-UUID gpuStat with
// utilization clamped to 100 and memory.used converted MiB->bytes, plus a
// separate count of successfully parsed utilization samples. A numeric 0 is a
// valid idle sample; [N/A] and malformed values leave display data intact but
// must not make node-wide telemetry valid.
func TestParseNvidiaDynamic(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		want        map[string]gpuStat
		wantSamples int
	}{
		{
			name: "discrete VRAM",
			in: "GPU-aaa, 37, 8192\n" +
				"GPU-bbb, 150, 1024\n" + // util clamped to 100
				", 50, 2048\n" + // skipped: empty uuid
				"GPU-ccc, bad, alsobad\n", // kept, both zero
			want: map[string]gpuStat{
				"GPU-aaa": {UtilizationPct: 37, VRAMUsed: 8192 * 1024 * 1024},
				"GPU-bbb": {UtilizationPct: 100, VRAMUsed: 1024 * 1024 * 1024},
				"GPU-ccc": {UtilizationPct: 0, VRAMUsed: 0},
			},
			wantSamples: 2,
		},
		{
			name: "unified memory [N/A] leaves memory for response assembly",
			in:   "GPU-spark, 42, [N/A]\n",
			want: map[string]gpuStat{
				"GPU-spark": {UtilizationPct: 42},
			},
			wantSamples: 1,
		},
		{
			name: "all unavailable utilization remains invalid",
			in: "GPU-spark, [N/A], [N/A]\n" +
				"GPU-memory, N/A, 2048\n",
			want: map[string]gpuStat{
				"GPU-spark":  {},
				"GPU-memory": {VRAMUsed: 2048 * 1024 * 1024},
			},
			wantSamples: 0,
		},
		{
			name: "valid idle sample survives mixed unavailable rows",
			in: "GPU-idle, 0, [N/A]\n" +
				"GPU-unknown, [N/A], 1024\n",
			want: map[string]gpuStat{
				"GPU-idle":    {UtilizationPct: 0},
				"GPU-unknown": {VRAMUsed: 1024 * 1024 * 1024},
			},
			wantSamples: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, samples := parseNvidiaDynamic(c.in)
			require.Equal(t, c.want, got, "parseNvidiaDynamic()")
			require.Equal(t, c.wantSamples, samples, "parseNvidiaDynamic() samples (%v)", samples)
		})
	}
}

// TestIsNvidiaSmiNA pins recognition of nvidia-smi not-applicable sentinels.
func TestIsNvidiaSmiNA(t *testing.T) {
	test := func(name, in string, want bool) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, isNvidiaSmiNA(in))
		})
	}
	test("bracketed N/A", "[N/A]", true)
	test("N/A", "N/A", true)
	test("bracketed unsupported", "[Not Supported]", true)
	test("unsupported", "Not Supported", true)
	test("number", "8192", false)
	test("malformed number", "notanumber", false)
	test("empty", "", false)
}
