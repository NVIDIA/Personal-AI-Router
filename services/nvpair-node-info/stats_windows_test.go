// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
func TestLuidKey_FormatsAdapterIdentity(t *testing.T) {
	test := func(name string, low uint32, high int32, want string) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, luidKey(low, high))
		})
	}
	test("typical", 0x000054F0, 0x00000000, "luid_0x00000000_0x000054f0_phys_0")
	test("high-bit-set", 0xDEADBEEF, -1, "luid_0xffffffff_0xdeadbeef_phys_0")
}

// TestPDHFmtCounterValueSize pins the scalar pdhFmtCounterValue to the
// C PDH_FMT_COUNTERVALUE size on amd64/arm64 Windows: DWORD CStatus +
// 4 bytes of padding + 8-byte PDH_FMT_COUNTERVALUE union payload = 16.
// decodeCPU hands a pointer to one of these directly to
// PdhGetFormattedCounterValue, so a layout drift would corrupt PDH's
// write into the CPU percentage.
func TestPDHFmtCounterValue_MatchesNativeStructureSize(t *testing.T) {
	const expected = 16
	require.Equal(t, uintptr(expected), unsafe.Sizeof(pdhFmtCounterValue{}), "pdhFmtCounterValue size")
}

// TestPDHFmtCounterValueItemSize pins the in-Go layout of
// pdhFmtCounterValueItemW to the C PDH_FMT_COUNTERVALUE_ITEM_W size on
// amd64/arm64 Windows: 8-byte LPWSTR + 16-byte PDH_FMT_COUNTERVALUE = 24.
// If this fires, someone has reordered the fields or we've accidentally
// compiled for 32-bit Windows — either way PDH would be handed garbage
// and every lookup would silently fail.
func TestPDHFmtCounterValueItem_MatchesNativeStructureSize(t *testing.T) {
	const expected = 24
	require.Equal(t, uintptr(expected), unsafe.Sizeof(pdhFmtCounterValueItemW{}), "pdhFmtCounterValueItemW size")
}

// TestMemoryStatusExSize pins the in-Go layout of memoryStatusEx to
// the C MEMORYSTATUSEX size on amd64/arm64 Windows:
// 4-byte DWORD + 4-byte DWORD + 7 × 8-byte DWORDLONG = 64.
// A drift here would have GlobalMemoryStatusEx reject our call with
// ERROR_INVALID_PARAMETER because dwLength no longer matches, or
// (worse, if we set dwLength to the wrong size manually) corrupt the
// stack when the OS writes past our struct.
func TestMemoryStatusEx_MatchesNativeStructureSize(t *testing.T) {
	const expected = 64
	require.Equal(t, uintptr(expected), unsafe.Sizeof(memoryStatusEx{}), "memoryStatusEx size")
}

// testAdapter builds a Windows-shaped GPUInfo: statsKey is the PDH instance
// name for that LUID, which is the key the dynamic counters join on.
func testAdapter(name string, low uint32) GPUInfo {
	return GPUInfo{Name: name, VramBytes: 8 << 30, statsKey: luidKey(low, 0)}
}

// startTestInventoryCollector runs only inventory recovery, with a supplied
// startup list, retry interval, and detector — no performance-counter query or
// stats ticker. This keeps the test's startup and timing conditions explicit.
func startTestInventoryCollector(t *testing.T, startup []GPUInfo, retryEvery time.Duration, detect func() []GPUInfo) *statsCollector {
	t.Helper()
	c := newStatsCollector(detect)
	c.inventoryRecoverEvery = retryEvery
	c.startGPUInventory(startup)
	t.Cleanup(func() { stopTestInventoryCollector(t, c) })
	return c
}

// Bound Stop itself so a shutdown regression cannot hang the test or cleanup.
func stopTestInventoryCollector(t *testing.T, c *statsCollector) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not stop the GPU inventory collector")
	}
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

// Adapter detection at boot can come up empty while the display driver is not
// ready. Previously nothing looked again, so the node served an empty GPU list
// until the service was restarted. Empty detections must be retried until
// adapters appear, with no further detection after the first successful list
// has been published.
func TestWindowsCollector_RetriesUntilGPUInventoryIsAvailable(t *testing.T) {
	const emptyDetections = 3
	recovered := []GPUInfo{
		testAdapter("Test Adapter A", 0x54f0),
		testAdapter("Test Adapter B", 0x6a10),
	}
	var calls atomic.Int64
	c := startTestInventoryCollector(t, nil, time.Millisecond, func() []GPUInfo {
		if calls.Add(1) <= emptyDetections {
			return nil
		}
		return recovered
	})

	// Count only once the loop has exited, so a loop that kept detecting
	// fails here instead of racing the read below.
	waitForInventoryLoopExit(t, c)

	require.Equal(t, int64(emptyDetections+1), calls.Load(), "empty attempts plus the successful detection")
	require.Equal(t, recovered, c.Snapshot().GPUInventory)
}

// Shutdown is the other way out of the retry loop: a service stopping while
// the inventory is still empty must not wait for an adapter that never arrives.
// A long retry interval makes waiting for the next attempt exceed the shutdown
// deadline. synctest's fake clock and Wait put the loop at its pending timer
// before Stop is called, without spending real time waiting for that timer.
func TestWindowsCollector_InterruptsInventoryRetryWaitWhenStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		c := startTestInventoryCollector(t, nil, time.Hour, func() []GPUInfo {
			calls.Add(1)
			return nil
		})
		synctest.Wait()
		require.Equal(t, int64(1), calls.Load(), "the collector should be waiting for its next attempt")

		stopTestInventoryCollector(t, c)
		assert.Empty(t, c.Snapshot().GPUInventory, "a loop that never found an adapter must not publish inventory")
	})
}

// main detects the startup inventory before starting the stats collector. If
// that detection succeeded, recovery must not enumerate again: a driver reset
// between the two calls could otherwise report the same physical card with a
// new identifier, which the response builder would append as a second GPU.
func TestWindowsCollector_KeepsStartupGPUWithoutDetectingItsReissuedIdentifier(t *testing.T) {
	startup := []GPUInfo{testAdapter("Test Adapter A", 0x54f0)}
	reissued := testAdapter("Test Adapter A", 0x7c20)
	var calls atomic.Int64
	c := startTestInventoryCollector(t, startup, time.Millisecond, func() []GPUInfo {
		calls.Add(1)
		return []GPUInfo{reissued}
	})
	waitForInventoryLoopExit(t, c)

	assert.Zero(t, calls.Load(), "a populated startup inventory should prevent further detection")
	var response NodeInfoResponse
	require.NoError(t, json.Unmarshal(buildResponse(startup, nil, 0, c.Snapshot(), "host-uuid", nil), &response))
	assert.Equal(t, []GPUInfo{{Name: "Test Adapter A", VramBytes: 8 << 30}}, response.GPUs)
}

// Stopping after the first publication keeps one physical card from being
// listed twice. Windows can reissue adapter identifiers (LUIDs) during a driver
// update, driver restart, or GPU timeout and recovery. mergeGPUInventory appends
// any adapter whose statsKey it has not seen, so detecting again could add the
// same card under its new identifier. The loop must publish once and exit even
// though a later enumeration would report that new identifier.
func TestWindowsCollector_StopsDetectingAfterFirstPublication(t *testing.T) {
	original := testAdapter("Test Adapter A", 0x54f0)
	reissued := testAdapter("Test Adapter A", 0x7c20) // same card, LUID reissued
	var calls atomic.Int64
	c := startTestInventoryCollector(t, nil, time.Millisecond, func() []GPUInfo {
		if calls.Add(1) == 1 {
			return []GPUInfo{original}
		}
		return []GPUInfo{reissued}
	})

	waitForInventoryLoopExit(t, c)
	require.Equal(t, int64(1), calls.Load(), "only the first detection should run")
	require.Equal(t, []GPUInfo{original}, c.Snapshot().GPUInventory)
}

// Recovered adapters and dynamic readings are published independently. Windows
// performance counters key GPU readings by the same adapter identifier used in
// the inventory, so the response builder can attach readings to a recovered GPU.
// Snapshot must supply both publications together without losing GPU readings,
// their collection timestamp, or CPU and memory readings. Seed the collector's
// stats before taking the snapshot so this checks the combination itself. The
// response builder's matching of readings to adapters is covered in stats_test.go.
func TestWindowsCollectorSnapshot_PreservesStatsWhenAddingRecoveredInventory(t *testing.T) {
	recovered := []GPUInfo{
		testAdapter("Test Adapter A", 0x54f0),
		testAdapter("Test Adapter B", 0x6a10),
	}
	c := startTestInventoryCollector(t, nil, time.Millisecond, func() []GPUInfo { return recovered })
	latest := statsSnapshot{
		GPU: map[string]gpuStat{
			recovered[1].statsKey: {VRAMUsed: 1 << 30, UtilizationPct: 42},
		},
		GPUSampledAt: time.Time{}.Add(time.Second),
		CPUUtilPct:   17,
		MemUsedBytes: 6 << 30,
	}
	c.latest.Store(&latest)
	waitForInventoryLoopExit(t, c)

	want := latest
	want.GPUInventory = recovered
	require.Equal(t, want, c.Snapshot())
}
