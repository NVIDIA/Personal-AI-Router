// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"encoding/json"
	"errors"
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

// fakeAdapterChanges stands in for the DXGI adapters-changed event, so the
// collector's re-detection logic runs without a GPU or a real adapter change.
type fakeAdapterChanges struct {
	changed chan struct{}
	closed  atomic.Bool
}

func newFakeAdapterChanges() *fakeAdapterChanges {
	return &fakeAdapterChanges{changed: make(chan struct{}, 1)}
}

func (f *fakeAdapterChanges) Changed() <-chan struct{} { return f.changed }
func (f *fakeAdapterChanges) Close()                   { f.closed.Store(true) }

// signal raises one adapters-changed event. Like the real source, a signal
// raised while one is already pending is merged into it.
func (f *fakeAdapterChanges) signal() {
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

const testQuietPeriod = 500 * time.Millisecond

// startTestInventoryCollector runs only inventory watching, with a supplied
// startup list, detector and change source — no performance-counter query or
// stats ticker. This keeps the test's startup and timing conditions explicit.
func startTestInventoryCollector(t *testing.T, startup []GPUInfo, source adapterChangeSource, detect func() []GPUInfo) *statsCollector {
	t.Helper()
	c := newStatsCollector(detect)
	c.quietPeriod = testQuietPeriod
	c.fallbackDelay = time.Minute
	c.openChanges = func() (adapterChangeSource, error) { return source, nil }
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

// Adapter detection at boot can come up empty while the display driver is not
// ready. The collector looks once right after registering for adapter changes,
// then again when DXGI signals that adapters appeared; nothing runs on a timer
// in between. The signal is the only thing that wakes the second detection, so
// it is raised only after the first has found nothing.
func TestWindowsCollector_RecoversEmptyStartupInventoryOnAdapterChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recovered := []GPUInfo{
			testAdapter("Test Adapter A", 0x54f0),
			testAdapter("Test Adapter B", 0x6a10),
		}
		var calls atomic.Int64
		source := newFakeAdapterChanges()
		c := startTestInventoryCollector(t, nil, source, func() []GPUInfo {
			if calls.Add(1) == 1 {
				return nil // the catch-up look after registration
			}
			return recovered
		})
		synctest.Wait()
		require.Equal(t, int64(1), calls.Load(), "one catch-up detection after registering")
		assert.Empty(t, c.Snapshot().GPUInventory, "nothing found yet")

		time.Sleep(time.Hour)
		synctest.Wait()
		require.Equal(t, int64(1), calls.Load(), "an unchanged adapter set must not be detected again")

		source.signal()
		time.Sleep(2 * testQuietPeriod)
		synctest.Wait()
		require.Equal(t, int64(2), calls.Load(), "the adapters-changed signal triggers one detection")
		require.Equal(t, recovered, c.Snapshot().GPUInventory)
	})
}

// A burst of signals (a driver restart removes and re-adds each adapter) is one
// detection after the burst settles, not one per signal and not one mid-burst.
func TestWindowsCollector_CoalescesAdapterChangeBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startup := []GPUInfo{testAdapter("Test Adapter A", 0x54f0)}
		var calls atomic.Int64
		source := newFakeAdapterChanges()
		startTestInventoryCollector(t, startup, source, func() []GPUInfo {
			calls.Add(1)
			return startup
		})
		synctest.Wait()
		require.Zero(t, calls.Load(), "a populated startup inventory needs no catch-up detection")

		for range 3 {
			source.signal()
			time.Sleep(testQuietPeriod / 2)
			synctest.Wait()
			require.Zero(t, calls.Load(), "detection waits for the burst to end")
		}
		time.Sleep(testQuietPeriod)
		synctest.Wait()
		require.Equal(t, int64(1), calls.Load(), "one detection for the whole burst")
	})
}

// Windows reissues adapter identifiers (LUIDs) after a driver update, driver
// restart, or GPU timeout and recovery, and raises the adapters-changed event
// when it does. The re-detected list must replace the startup list: appending it
// would report the same physical card twice, once under each identifier.
func TestWindowsCollector_ReplacesStartupGPUWhenItsIdentifierIsReissued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startup := []GPUInfo{testAdapter("Test Adapter A", 0x54f0)}
		reissued := []GPUInfo{testAdapter("Test Adapter A", 0x7c20)}
		source := newFakeAdapterChanges()
		c := startTestInventoryCollector(t, startup, source, func() []GPUInfo { return reissued })
		synctest.Wait()

		source.signal()
		time.Sleep(2 * testQuietPeriod)
		synctest.Wait()

		snap := c.Snapshot()
		require.Equal(t, reissued, snap.GPUInventory)
		var response NodeInfoResponse
		require.NoError(t, json.Unmarshal(buildResponse(startup, nil, 0, snap, "host-uuid", nil), &response))
		assert.Equal(t, []GPUInfo{{Name: "Test Adapter A", VramBytes: 8 << 30}}, response.GPUs, "one card, not two")
	})
}

// A change signal that leaves the adapter set as it was publishes nothing, and a
// detection that finds no adapters (a driver mid-restart) keeps the last list
// instead of blanking the node's GPUs.
func TestWindowsCollector_IgnoresUnchangedAndEmptyDetections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startup := []GPUInfo{testAdapter("Test Adapter A", 0x54f0)}
		var detected atomic.Pointer[[]GPUInfo]
		detected.Store(&startup)
		source := newFakeAdapterChanges()
		c := startTestInventoryCollector(t, startup, source, func() []GPUInfo { return *detected.Load() })
		synctest.Wait()

		source.signal()
		time.Sleep(2 * testQuietPeriod)
		synctest.Wait()
		assert.Empty(t, c.Snapshot().GPUInventory, "the same adapters are not a change")

		none := []GPUInfo(nil)
		detected.Store(&none)
		source.signal()
		time.Sleep(2 * testQuietPeriod)
		synctest.Wait()
		assert.Empty(t, c.Snapshot().GPUInventory, "an empty detection must not replace the startup list")
	})
}

// Stop releases the adapter-change registration, including while the loop is
// idle waiting for a signal that never comes.
func TestWindowsCollector_StopClosesAdapterChangeSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newFakeAdapterChanges()
		c := startTestInventoryCollector(t, []GPUInfo{testAdapter("Test Adapter A", 0x54f0)}, source,
			func() []GPUInfo { return nil })
		synctest.Wait()
		require.False(t, source.closed.Load(), "the registration lives as long as the collector")

		stopTestInventoryCollector(t, c)
		assert.True(t, source.closed.Load(), "Stop must unregister the adapters-changed event")
	})
}

// Before Windows 10 1803 there is no adapters-changed event. The collector then
// re-detects exactly once after a delay, which still rescues a display driver
// that was not ready at boot, and never polls: hours later it has not looked
// again.
func TestWindowsCollector_FallsBackToOneDeferredDetectionWithoutAdapterEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recovered := []GPUInfo{testAdapter("Test Adapter A", 0x54f0)}
		var calls atomic.Int64
		c := newStatsCollector(func() []GPUInfo {
			calls.Add(1)
			return recovered
		})
		c.fallbackDelay = time.Minute
		c.openChanges = func() (adapterChangeSource, error) { return nil, errors.New("IDXGIFactory7 unavailable") }
		c.startGPUInventory(nil)
		t.Cleanup(func() { stopTestInventoryCollector(t, c) })

		synctest.Wait()
		require.Zero(t, calls.Load(), "the fallback waits before looking")
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Equal(t, int64(1), calls.Load())
		require.Equal(t, recovered, c.Snapshot().GPUInventory)

		time.Sleep(24 * time.Hour)
		synctest.Wait()
		require.Equal(t, int64(1), calls.Load(), "the fallback is a single re-detection, not a poll")
	})
}

// Shutdown must not wait out the fallback delay.
func TestWindowsCollector_StopInterruptsFallbackWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		c := newStatsCollector(func() []GPUInfo {
			calls.Add(1)
			return nil
		})
		c.fallbackDelay = time.Hour
		c.openChanges = func() (adapterChangeSource, error) { return nil, errors.New("IDXGIFactory7 unavailable") }
		c.startGPUInventory(nil)
		synctest.Wait()

		stopTestInventoryCollector(t, c)
		assert.Zero(t, calls.Load())
		assert.Empty(t, c.Snapshot().GPUInventory)
	})
}

// Re-detected adapters and dynamic readings are published independently.
// Windows performance counters key GPU readings by the same adapter identifier
// used in the inventory, so the response builder can attach readings to a
// re-detected GPU. Snapshot must supply both publications together without
// losing GPU readings, their collection timestamp, or CPU and memory readings.
// Seed the collector's stats before taking the snapshot so this checks the
// combination itself. The response builder's matching of readings to adapters is
// covered in stats_test.go.
func TestWindowsCollectorSnapshot_PreservesStatsWhenReplacingInventory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recovered := []GPUInfo{
			testAdapter("Test Adapter A", 0x54f0),
			testAdapter("Test Adapter B", 0x6a10),
		}
		c := startTestInventoryCollector(t, nil, newFakeAdapterChanges(), func() []GPUInfo { return recovered })
		latest := statsSnapshot{
			GPU: map[string]gpuStat{
				recovered[1].statsKey: {VRAMUsed: 1 << 30, UtilizationPct: 42},
			},
			GPUSampledAt: time.Time{}.Add(time.Second),
			CPUUtilPct:   17,
			MemUsedBytes: 6 << 30,
		}
		c.latest.Store(&latest)
		synctest.Wait()

		want := latest
		want.GPUInventory = recovered
		want.GPUInventoryReplaces = true
		require.Equal(t, want, c.Snapshot())
	})
}
