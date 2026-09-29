// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// TestLuidKey locks down the PDH instance-name format. If this ever drifts
// from PDH's actual output the per-adapter join performed by
// statsCollector.decodeSnapshot will silently always miss and every GPU
// will report zeroed dynamic fields — a regression that would otherwise
// only surface via a visual comparison against Task Manager. The two test
// cases below cover the zero-high case (the overwhelming majority of real
// adapters) and a non-zero high case, which additionally verifies we use
// bit-reinterpretation for the signed HighPart rather than numeric sign-
// extension.
func TestLuidKey(t *testing.T) {
	cases := []struct {
		name string
		low  uint32
		high int32
		want string
	}{
		{"typical", 0x000054F0, 0x00000000, "luid_0x00000000_0x000054f0_phys_0"},
		{"high-bit-set", 0xDEADBEEF, -1, "luid_0xffffffff_0xdeadbeef_phys_0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := luidKey(c.low, c.high)
			if got != c.want {
				t.Fatalf("luidKey(%#x, %#x) = %q, want %q", c.low, c.high, got, c.want)
			}
		})
	}
}

// TestPDHFmtCounterValueSize pins the scalar pdhFmtCounterValue to the
// C PDH_FMT_COUNTERVALUE size on amd64/arm64 Windows: DWORD CStatus +
// 4 bytes of padding + 8-byte PDH_FMT_COUNTERVALUE union payload = 16.
// decodeCPU hands a pointer to one of these directly to
// PdhGetFormattedCounterValue, so a layout drift would corrupt PDH's
// write into the CPU percentage.
func TestPDHFmtCounterValueSize(t *testing.T) {
	const expected = 16
	if got := unsafe.Sizeof(pdhFmtCounterValue{}); got != expected {
		t.Fatalf("pdhFmtCounterValue size = %d, want %d", got, expected)
	}
}

// TestPDHFmtCounterValueItemSize pins the in-Go layout of
// pdhFmtCounterValueItemW to the C PDH_FMT_COUNTERVALUE_ITEM_W size on
// amd64/arm64 Windows: 8-byte LPWSTR + 16-byte PDH_FMT_COUNTERVALUE = 24.
// If this fires, someone has reordered the fields or we've accidentally
// compiled for 32-bit Windows — either way PDH would be handed garbage
// and every lookup would silently fail.
func TestPDHFmtCounterValueItemSize(t *testing.T) {
	const expected = 24
	if got := unsafe.Sizeof(pdhFmtCounterValueItemW{}); got != expected {
		t.Fatalf("pdhFmtCounterValueItemW size = %d, want %d", got, expected)
	}
}

// TestMemoryStatusExSize pins the in-Go layout of memoryStatusEx to
// the C MEMORYSTATUSEX size on amd64/arm64 Windows:
// 4-byte DWORD + 4-byte DWORD + 7 × 8-byte DWORDLONG = 64.
// A drift here would have GlobalMemoryStatusEx reject our call with
// ERROR_INVALID_PARAMETER because dwLength no longer matches, or
// (worse, if we set dwLength to the wrong size manually) corrupt the
// stack when the OS writes past our struct.
func TestMemoryStatusExSize(t *testing.T) {
	const expected = 64
	if got := unsafe.Sizeof(memoryStatusEx{}); got != expected {
		t.Fatalf("memoryStatusEx size = %d, want %d", got, expected)
	}
}

// testAdapter builds a Windows-shaped GPUInfo: statsKey is the PDH instance
// name for that LUID, which is the key the dynamic counters join on.
func testAdapter(name string, low uint32) GPUInfo {
	return GPUInfo{Name: name, VramBytes: 8 << 30, statsKey: luidKey(low, 0)}
}

// startTestInventoryCollector runs only the inventory loop, at millisecond
// cadence, with an injected detect function — no PDH query, no pollers.
func startTestInventoryCollector(t *testing.T, detect func() []GPUInfo) *statsCollector {
	t.Helper()
	c := newStatsCollector(detect)
	c.inventoryRecoverEvery = time.Millisecond
	c.inventoryRefreshEvery = time.Millisecond
	c.startGPUInventory()
	t.Cleanup(func() {
		close(c.stop)
		c.wg.Wait()
	})
	return c
}

func waitForStats(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWindowsCollectorRecoversEmptyGPUInventory is the defect: adapter
// detection at boot came up empty (the DirectX registry still held the
// previous boot's LUIDs) and nothing ever looked again, so the node served an
// empty GPU list until it was restarted. The collector now re-detects until
// adapters appear, publishes them through the snapshot, and buildResponse
// lists them with the dynamic numbers joined on their statsKey — and a
// re-detection that finds the same set republishes nothing.
func TestWindowsCollectorRecoversEmptyGPUInventory(t *testing.T) {
	recovered := []GPUInfo{
		testAdapter("Test Adapter A", 0x54f0),
		testAdapter("Test Adapter B", 0x6a10),
	}
	var calls atomic.Int64
	c := startTestInventoryCollector(t, func() []GPUInfo {
		if calls.Add(1) == 1 {
			return nil // the startup enumeration that came up empty
		}
		return recovered
	})

	waitForStats(t, "the recovered inventory", func() bool {
		return len(c.Snapshot().GPUInventory) == 2
	})

	snap := c.Snapshot()
	if snap.GPUInventory[0].Name != "Test Adapter A" || snap.GPUInventory[1].Name != "Test Adapter B" {
		t.Fatalf("GPUInventory = %+v", snap.GPUInventory)
	}

	// The PDH decoders key every adapter they see by the same LUID instance
	// name, whether or not the inventory knew about it at startup.
	snap.GPU = map[string]gpuStat{
		recovered[1].statsKey: {VRAMUsed: 1 << 30, UtilizationPct: 42},
	}
	var got struct {
		GPUs []GPUInfo `json:"GPUs"`
	}
	if err := json.Unmarshal(buildResponse(nil, nil, 0, snap, "host-uuid", nil), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.GPUs) != 2 {
		t.Fatalf("response listed %d GPUs: %+v", len(got.GPUs), got.GPUs)
	}
	if got.GPUs[0].Name != "Test Adapter A" || got.GPUs[0].VramBytes != 8<<30 {
		t.Fatalf("first GPU = %+v", got.GPUs[0])
	}
	if got.GPUs[1].VramUsedBytes != 1<<30 ||
		got.GPUs[1].UtilizationPercent != 42 {
		t.Fatalf("recovered adapter did not pick up its dynamic stats: %+v", got.GPUs[1])
	}

	// Steady state: further detections return the same statsKeys, so the
	// published inventory pointer must stay exactly as it is.
	published := c.gpuInventory.Load()
	settled := calls.Load() + 3
	waitForStats(t, "three more detections", func() bool { return calls.Load() >= settled })
	if c.gpuInventory.Load() != published {
		t.Fatal("an unchanged adapter set was republished")
	}
}

// TestWindowsCollectorRepublishesChangedGPUInventory covers the other half of
// the refresh: a set that really changed (an adapter hot-plugged after
// startup, or a clone a remote session brought in) is published without a
// restart.
func TestWindowsCollectorRepublishesChangedGPUInventory(t *testing.T) {
	first := []GPUInfo{testAdapter("Test Adapter A", 0x54f0)}
	second := append(append([]GPUInfo{}, first...), testAdapter("Test Adapter B", 0x6a10))
	var plugged atomic.Bool
	c := startTestInventoryCollector(t, func() []GPUInfo {
		if plugged.Load() {
			return second
		}
		return first
	})

	waitForStats(t, "the first inventory", func() bool {
		return len(c.Snapshot().GPUInventory) == 1
	})
	published := c.gpuInventory.Load()

	plugged.Store(true)
	waitForStats(t, "the changed inventory", func() bool {
		return len(c.Snapshot().GPUInventory) == 2
	})
	if c.gpuInventory.Load() == published {
		t.Fatal("the changed adapter set reused the previous publication")
	}
	if keys := gpuStatsKeys(c.Snapshot().GPUInventory); len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("statsKeys = %v", keys)
	}
}
