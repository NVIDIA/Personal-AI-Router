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
	c.startGPUInventory()
	t.Cleanup(c.Stop)
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

// waitForInventoryLoopExit fails the test when the inventory goroutine is still
// running after the deadline, so a loop that never ends fails here instead of
// hanging the package's test run.
func waitForInventoryLoopExit(t *testing.T, c *statsCollector) {
	t.Helper()
	exited := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the GPU inventory loop is still running")
	}
}

// TestWindowsCollectorRetriesEmptyGPUInventory is the defect this loop exists
// for: adapter detection at boot came up empty (the DirectX registry still held
// the previous boot's LUIDs, or the display driver was not ready) and nothing
// ever looked again, so the node served an empty GPU list until it was
// restarted. An empty detection is retried until adapters appear, and no
// detection is spent after the one that found them.
func TestWindowsCollectorRetriesEmptyGPUInventory(t *testing.T) {
	const emptyDetections = 3
	recovered := []GPUInfo{
		testAdapter("Test Adapter A", 0x54f0),
		testAdapter("Test Adapter B", 0x6a10),
	}
	var calls atomic.Int64
	c := startTestInventoryCollector(t, func() []GPUInfo {
		if calls.Add(1) <= emptyDetections {
			return nil
		}
		return recovered
	})

	waitForStats(t, "the recovered inventory", func() bool {
		return len(c.Snapshot().GPUInventory) == len(recovered)
	})
	// Count only once the loop has exited, so a loop that kept detecting
	// fails here instead of racing the read below.
	waitForInventoryLoopExit(t, c)

	if got := calls.Load(); got != emptyDetections+1 {
		t.Fatalf("detect ran %d times, want %d empty detections and the one that found the adapters",
			got, emptyDetections+1)
	}
	inventory := c.Snapshot().GPUInventory
	for i, want := range recovered {
		if inventory[i].Name != want.Name || inventory[i].statsKey != want.statsKey {
			t.Fatalf("GPUInventory[%d] = %+v, want %+v", i, inventory[i], want)
		}
	}
}

// TestWindowsCollectorStopsRetryingWhenStopped covers the other way out of the
// loop: a service shutting down while the inventory is still empty must not
// wait for an adapter that never arrives.
func TestWindowsCollectorStopsRetryingWhenStopped(t *testing.T) {
	var calls atomic.Int64
	c := startTestInventoryCollector(t, func() []GPUInfo {
		calls.Add(1)
		return nil
	})
	waitForStats(t, "a second empty detection", func() bool { return calls.Load() >= 2 })

	c.Stop()
	waitForInventoryLoopExit(t, c)
	if inventory := c.Snapshot().GPUInventory; len(inventory) != 0 {
		t.Fatalf("a loop that never found an adapter published %+v", inventory)
	}
}

// TestWindowsCollectorStopsDetectingOncePublished pins the property that keeps
// one physical card from being listed twice. Windows reissues adapter LUIDs
// mid-session (driver update, restart, GPU timeout and recovery), and
// mergeGPUInventory appends any adapter whose statsKey it has not seen. A loop
// that kept re-detecting after its first publication would therefore add the
// same card again under its new LUID. After the first non-empty detection the
// loop must publish that list once, exit, and never call detect again, even
// though a later enumeration would report a different LUID.
func TestWindowsCollectorStopsDetectingOncePublished(t *testing.T) {
	original := testAdapter("Test Adapter A", 0x54f0)
	reissued := testAdapter("Test Adapter A", 0x7c20) // same card, LUID reissued
	var calls atomic.Int64
	c := startTestInventoryCollector(t, func() []GPUInfo {
		if calls.Add(1) == 1 {
			return []GPUInfo{original}
		}
		return []GPUInfo{reissued}
	})

	waitForInventoryLoopExit(t, c)
	published := c.gpuInventory.Load()

	// Let many more retry intervals pass; the call count must not move.
	time.Sleep(10 * c.inventoryRecoverEvery)
	if got := calls.Load(); got != 1 {
		t.Fatalf("detect ran %d times, want exactly the one that found the adapter", got)
	}
	if c.gpuInventory.Load() != published {
		t.Fatal("the published inventory was replaced after the loop finished")
	}
	inventory := c.Snapshot().GPUInventory
	if len(inventory) != 1 || inventory[0].statsKey != original.statsKey {
		t.Fatalf("GPUInventory = %+v, want only the adapter under its original LUID key %s",
			inventory, original.statsKey)
	}
}

// TestWindowsCollectorRecoveredAdapterPicksUpDynamicStats checks the join the
// recovery relies on: the PDH decoders key every adapter they see by the same
// LUID instance name, whether or not the startup enumeration knew about it, so
// an adapter recovered into the inventory is listed by buildResponse with its
// dynamic numbers.
func TestWindowsCollectorRecoveredAdapterPicksUpDynamicStats(t *testing.T) {
	recovered := []GPUInfo{
		testAdapter("Test Adapter A", 0x54f0),
		testAdapter("Test Adapter B", 0x6a10),
	}
	c := startTestInventoryCollector(t, func() []GPUInfo { return recovered })
	waitForStats(t, "the recovered inventory", func() bool {
		return len(c.Snapshot().GPUInventory) == len(recovered)
	})

	snap := c.Snapshot()
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
}
