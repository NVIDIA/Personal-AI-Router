// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows node-stats collection via the Performance Data Helper (PDH)
// API and a couple of direct kernel32 syscalls.
//
// This collector is responsible for every dynamic number in
// /v1/node-info: per-adapter VRAM used and GPU busy %, overall CPU
// busy %, and memory-used bytes. Static identity (GPU name, total
// VRAM, CPU name + cores, total RAM) is detected elsewhere once at
// startup and passed to buildResponse.
//
// GPU counters we use:
//
//   - \GPU Adapter Memory(*)\Dedicated Usage   (instantaneous gauge, int64)
//   - \GPU Engine(*)\Utilization Percentage    (rate counter, float64)
//
// CPU counter:
//
//   - \Processor(_Total)\% Processor Time      (scalar rate counter, float64)
//
// All three of these are PERF_100NSEC_TIMER rate counters, meaning a
// valid reading requires TWO successive PdhCollectQueryData calls with
// enough elapsed time between them — the first call is just a
// baseline. Opening and priming a query per HTTP request would add ~1
// s of cold-cache latency that's incompatible with the UI's 2 s poll,
// so we keep one persistent query warm for the life of the service:
//
//   - Open the query at startup.
//   - Add every counter the host supports to it.
//   - Issue one priming collect immediately.
//   - Run a background goroutine that ticks once per second, collects
//     fresh data, samples memory via GlobalMemoryStatusEx in the same
//     loop, and publishes the combined statsSnapshot via an atomic
//     pointer swap.
//   - HTTP handlers read the atomic pointer with no lock and no sleep.
//
// Memory-used doesn't go through PDH at all — GlobalMemoryStatusEx is
// a single cheap syscall that returns total + available in one shot.
// Folding it into the per-tick snapshot (rather than reading it at
// HTTP time) means the JSON handler reads every dynamic number from
// one atomic load, so subsystems can't drift across a publish
// boundary.
//
// Why PdhAddEnglishCounterW instead of PdhAddCounterW: counter display
// names are localized on non-English Windows. The English variant
// resolves the path regardless of system locale.
//
// Availability:
//   - GPU counters require Windows 10 1709 or newer.
//   - Processor counter exists on every supported Windows version.
//
// If a specific counter is missing (stripped SKU, ancient build) we
// latch a per-counter unavailable flag so future process-lifetime
// retries short-circuit silently. A host where every counter is
// absent still gets a non-nil collector whose Snapshot() returns an
// empty statsSnapshot, which drops every dynamic field from JSON via
// the per-field omitempty tags.

const (
	pdhCounterPathDedicated = `\GPU Adapter Memory(*)\Dedicated Usage`
	pdhCounterPathEngine    = `\GPU Engine(*)\Utilization Percentage`
	pdhCounterPathCPU       = `\Processor(_Total)\% Processor Time`

	// PDH format flags (dwFormat). LARGE returns a signed 64-bit int in the
	// counter-value union; DOUBLE returns an IEEE-754 float64 in the same
	// 8-byte slot. We request the format appropriate to each counter when
	// reading its per-instance values.
	pdhFmtLarge  = 0x00000400
	pdhFmtDouble = 0x00000200

	// PDH status codes we care about.
	pdhCStatusValidData = 0
	pdhMoreData         = 0x800007D2
	pdhCStatusNoObject  = 0xC0000BB8

	// statsTickInterval is the cadence at which we call
	// PdhCollectQueryData and GlobalMemoryStatusEx. 1 s matches the
	// natural sampling interval of the rate counters (shorter intervals
	// produce noisier readings; longer ones delay detection of load
	// spikes) and keeps the memory sample fresh enough for a 2 s UI
	// poll without being wastefully tight.
	statsTickInterval = time.Second
)

var (
	modPDH                           = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW                = modPDH.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW        = modPDH.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData          = modPDH.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterValue  = modPDH.NewProc("PdhGetFormattedCounterValue")
	procPdhGetFormattedCounterArrayW = modPDH.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery                = modPDH.NewProc("PdhCloseQuery")

	modKernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")

	// Per-counter "unavailable" latches. Set once on first
	// PDH_CSTATUS_NO_OBJECT and never cleared — if the counter set
	// isn't present now, it isn't coming back without a reboot, and we
	// don't want to log-spam on every retry attempt during startup.
	pdhVRAMUnavailable   atomic.Bool
	pdhEngineUnavailable atomic.Bool
	pdhCPUUnavailable    atomic.Bool
)

// pdhFmtCounterValue mirrors PDH_FMT_COUNTERVALUE. The 8-byte payload is a
// C union; we store it as a uint64 and reinterpret per counter format:
//
//	PDH_FMT_LARGE  → int64(Value)
//	PDH_FMT_DOUBLE → math.Float64frombits(Value)
//
// A test pins this struct at 16 bytes so the C union layout stays honest.
// DWORD CStatus + 4 bytes of padding + 8-byte union = 16.
type pdhFmtCounterValue struct {
	CStatus uint32
	_       uint32
	Value   uint64
}

// pdhFmtCounterValueItemW mirrors PDH_FMT_COUNTERVALUE_ITEM_W. On amd64/
// arm64: 8-byte LPWSTR + 16-byte PDH_FMT_COUNTERVALUE = 24.
type pdhFmtCounterValueItemW struct {
	Name  *uint16
	Value pdhFmtCounterValue
}

// memoryStatusEx mirrors the C MEMORYSTATUSEX struct passed to
// GlobalMemoryStatusEx. Every field is a ULONGLONG except the leading
// DWORD length (which the caller must set to sizeof) and the memory-
// load percentage. Only TotalPhys and AvailPhys matter to us; the
// others are kept so the struct size matches what the API validates
// against via the dwLength field.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// luidKey formats a Windows LUID (low / high DWORDs, as reported by
// DXGI_ADAPTER_DESC1.AdapterLuid) into the same string PDH writes for
// the "GPU Adapter Memory" instance name. Keeping the format in lock-
// step lets the VRAM decoder's map lookup and the engine decoder's
// parseEngineInstance output land at the same key. The high half is
// cast via bit-reinterpretation so a negative HighPart renders the same
// hex digits both sides write.
func luidKey(low uint32, high int32) string {
	return fmt.Sprintf("luid_0x%08x_0x%08x_phys_0", uint32(high), low)
}

// statsCollector owns the persistent PDH query and the 1 s ticker that
// updates the published statsSnapshot. Start it once at service boot,
// defer Stop() for clean shutdown, call Snapshot() from the HTTP
// handler. Even if PDH initialization fails the returned value is
// non-nil and Snapshot() returns a zero-valued statsSnapshot, so
// callers can use it unconditionally without nil-checks.
type statsCollector struct {
	query         uintptr
	vramCounter   uintptr
	engineCounter uintptr
	cpuCounter    uintptr
	hasVRAM       bool
	hasEngine     bool
	hasCPU        bool

	// latest is swapped atomically by the ticker goroutine. Readers
	// copy the pointer and read the snapshot; they never see a torn
	// update. The snapshot's GPU map is immutable post-publish — the
	// ticker allocates a fresh map every second rather than mutating
	// the previous one.
	latest atomic.Pointer[statsSnapshot]

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// startStatsCollector opens the query, adds whichever counters the
// host supports, issues the priming collect that kicks off the rate-
// counter delta math, and spins up the 1 s tick goroutine. Never
// returns nil; on any error path the collector is a well-behaved
// no-op whose Snapshot() yields a zero statsSnapshot forever (and
// memory-used still gets published every tick regardless of PDH
// state, since it's a pure syscall).
func startStatsCollector() *statsCollector {
	c := &statsCollector{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	c.latest.Store(&statsSnapshot{})

	pdhOK := c.open() == nil

	// Priming collect. The VRAM gauge is already valid after one call,
	// but the engine and CPU rate counters only yield a meaningful
	// reading on the NEXT collect — that's the first tick of c.run().
	if pdhOK {
		if r, _, _ := procPdhCollectQueryData.Call(c.query); r != 0 {
			slog.Warn("PdhCollectQueryData (prime) failed",
				"status", fmt.Sprintf("0x%08x", uint32(r)))
		}
	} else {
		slog.Warn("PDH stats collection disabled",
			"effect", "GPU and CPU utilization / VRAM-used will not be reported; memory-used still works")
	}

	go c.run()
	return c
}

func (c *statsCollector) open() error {
	r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&c.query)))
	if r != 0 {
		return fmt.Errorf("PdhOpenQueryW: 0x%08x", uint32(r))
	}

	if ctr, ok := c.addCounter(pdhCounterPathDedicated, &pdhVRAMUnavailable); ok {
		c.vramCounter = ctr
		c.hasVRAM = true
	}
	if ctr, ok := c.addCounter(pdhCounterPathEngine, &pdhEngineUnavailable); ok {
		c.engineCounter = ctr
		c.hasEngine = true
	}
	if ctr, ok := c.addCounter(pdhCounterPathCPU, &pdhCPUUnavailable); ok {
		c.cpuCounter = ctr
		c.hasCPU = true
	}

	if !c.hasVRAM && !c.hasEngine && !c.hasCPU {
		procPdhCloseQuery.Call(c.query)
		c.query = 0
		return fmt.Errorf("no performance counters available on this host")
	}
	return nil
}

// addCounter tries to add `path` to the open query. Returns the counter
// handle and ok=true on success, 0/false on any failure. PDH_CSTATUS_NO_OBJECT
// is treated specially: it means the counter set is missing on this host
// (pre-1709 Windows, or a stripped SKU), so we latch unavailFlag to keep
// future retries silent.
func (c *statsCollector) addCounter(path string, unavailFlag *atomic.Bool) (uintptr, bool) {
	if unavailFlag.Load() {
		return 0, false
	}
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		slog.Warn("PDH path encode failed", "path", path, "err", err)
		return 0, false
	}
	var counter uintptr
	r, _, _ := procPdhAddEnglishCounterW.Call(
		c.query,
		uintptr(unsafe.Pointer(pathW)),
		0,
		uintptr(unsafe.Pointer(&counter)),
	)
	if r != 0 {
		if uint32(r) == pdhCStatusNoObject {
			if unavailFlag.CompareAndSwap(false, true) {
				slog.Warn("PDH counter not available on this host",
					"path", path)
			}
			return 0, false
		}
		slog.Warn("PdhAddEnglishCounterW failed",
			"path", path,
			"status", fmt.Sprintf("0x%08x", uint32(r)))
		return 0, false
	}
	return counter, true
}

func (c *statsCollector) run() {
	defer close(c.done)
	ticker := time.NewTicker(statsTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.latest.Store(c.decodeSnapshot())
		}
	}
}

// decodeSnapshot runs one sampling pass across every data source and
// returns the pointer-friendly snapshot for atomic publication. Called
// under the single-writer ticker goroutine, so no mutex is needed —
// the atomic pointer swap on c.latest is the only publishing step.
//
// A per-tick PDH collect is done here rather than in the caller so
// failures (e.g. a PDH query in an error state) don't blank out the
// memory sample, which comes from an independent syscall.
func (c *statsCollector) decodeSnapshot() *statsSnapshot {
	previous := c.Snapshot()
	snap := &statsSnapshot{}

	if c.query != 0 {
		if r, _, _ := procPdhCollectQueryData.Call(c.query); r != 0 {
			slog.Debug("PdhCollectQueryData failed",
				"status", fmt.Sprintf("0x%08x", uint32(r)))
		} else {
			gpu, valid := c.decodeGPU()
			sampledAt := time.Time{}
			if valid {
				sampledAt = time.Now()
			}
			applyGPUStats(previous, snap, gpu, sampledAt)
			snap.CPUUtilPct = c.decodeCPU()
		}
	}
	if snap.GPU == nil {
		applyGPUStats(previous, snap, nil, time.Time{})
	}

	if used, ok := readMemoryUsed(); ok {
		snap.MemUsedBytes = used
	}

	return snap
}

func (c *statsCollector) decodeGPU() (map[string]gpuStat, bool) {
	out := make(map[string]gpuStat)
	if c.hasVRAM {
		for name, v := range readCounterLarge(c.vramCounter) {
			if v < 0 {
				continue
			}
			// PDH can return mixed-case hex for the LUID depending on
			// the Windows build; normalize to lowercase to match the
			// %08x format used by luidKey() on the DXGI side.
			lname := strings.ToLower(name)
			if !strings.HasPrefix(lname, "luid_") {
				continue
			}
			s := out[lname]
			s.VRAMUsed = uint64(v)
			out[lname] = s
		}
	}
	utilizationSamples := 0
	if c.hasEngine {
		util := aggregateUtilization(readCounterDouble(c.engineCounter))
		utilizationSamples = len(util)
		for luid, pct := range util {
			s := out[luid]
			s.UtilizationPct = pct
			out[luid] = s
		}
	}
	return out, utilizationSamples > 0
}

// decodeCPU reads the scalar \Processor(_Total)\% Processor Time
// counter. Uses PdhGetFormattedCounterValue (scalar) rather than the
// array API because there's exactly one instance (_Total) — the array
// API would still work but adds an allocation and two syscalls for no
// gain. Returns 0 when the counter is disabled or the reading is in
// an error state; 0 propagates through buildResponse as an absent
// utilization_percent field via omitempty.
func (c *statsCollector) decodeCPU() uint32 {
	if !c.hasCPU {
		return 0
	}
	var v pdhFmtCounterValue
	r, _, _ := procPdhGetFormattedCounterValue.Call(
		c.cpuCounter,
		uintptr(pdhFmtDouble),
		0,
		uintptr(unsafe.Pointer(&v)),
	)
	if r != 0 {
		slog.Debug("PdhGetFormattedCounterValue (CPU) failed",
			"status", fmt.Sprintf("0x%08x", uint32(r)))
		return 0
	}
	if v.CStatus != pdhCStatusValidData {
		return 0
	}
	pct := math.Float64frombits(v.Value)
	if math.IsNaN(pct) || pct < 0 {
		return 0
	}
	if pct > 100 {
		pct = 100
	}
	return uint32(math.Round(pct))
}

// readMemoryUsed returns physical-memory bytes in use (total - available)
// via one GlobalMemoryStatusEx syscall. The second return is false only
// if the syscall itself failed, which is effectively impossible on any
// supported Windows version — we return it anyway so callers can
// differentiate "zero used" (an unrealistic but valid reading) from
// "we don't know" in a future refactor.
func readMemoryUsed() (uint64, bool) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		slog.Debug("GlobalMemoryStatusEx failed", "err", err)
		return 0, false
	}
	if ms.TotalPhys == 0 || ms.AvailPhys > ms.TotalPhys {
		return 0, false
	}
	return ms.TotalPhys - ms.AvailPhys, true
}

// readCounterLarge pulls the PDH_FMT_LARGE payload for each instance of
// a counter. The returned map is keyed by the raw instance name — the
// VRAM decoder lowercases these, the engine decoder parses out the LUID
// portion via parseEngineInstance, so neither cares about the raw form
// beyond "it's whatever PDH wrote".
func readCounterLarge(counter uintptr) map[string]int64 {
	items, ok := fetchCounterArray(counter, pdhFmtLarge)
	if !ok {
		return nil
	}
	out := make(map[string]int64, len(items))
	for i := range items {
		it := &items[i]
		if it.Value.CStatus != pdhCStatusValidData || it.Name == nil {
			continue
		}
		out[windows.UTF16PtrToString(it.Name)] = int64(it.Value.Value)
	}
	return out
}

// readCounterDouble is the float64 twin of readCounterLarge. The 8-byte
// union slot is reinterpreted via math.Float64frombits so we don't need
// a parallel pdhFmtCounterValueDouble struct type — one PDH_FMT_COUNTERVALUE
// shape, two reader flavors.
func readCounterDouble(counter uintptr) map[string]float64 {
	items, ok := fetchCounterArray(counter, pdhFmtDouble)
	if !ok {
		return nil
	}
	out := make(map[string]float64, len(items))
	for i := range items {
		it := &items[i]
		if it.Value.CStatus != pdhCStatusValidData || it.Name == nil {
			continue
		}
		out[windows.UTF16PtrToString(it.Name)] = math.Float64frombits(it.Value.Value)
	}
	return out
}

// fetchCounterArray runs the two-call PdhGetFormattedCounterArrayW dance
// and returns a Go slice header over the returned buffer. Both VRAM and
// engine counters share this boilerplate — only the format flag differs.
//
// PDH lays the item structs at the start of the buffer and writes the
// NUL-terminated instance-name strings into the tail of the same buffer;
// each item's Name pointer refers back into that buffer. The returned
// slice's data pointer references the buffer's first byte, so Go's GC
// keeps the whole allocation alive for as long as the caller holds the
// slice — including the strings the Name pointers resolve to.
func fetchCounterArray(counter uintptr, formatFlag uint32) ([]pdhFmtCounterValueItemW, bool) {
	var bufSize, itemCount uint32
	r, _, _ := procPdhGetFormattedCounterArrayW.Call(
		counter,
		uintptr(formatFlag),
		uintptr(unsafe.Pointer(&bufSize)),
		uintptr(unsafe.Pointer(&itemCount)),
		0,
	)
	if uint32(r) != pdhMoreData {
		slog.Debug("PdhGetFormattedCounterArrayW (sizing) unexpected status",
			"status", fmt.Sprintf("0x%08x", uint32(r)))
		return nil, false
	}
	if itemCount == 0 || bufSize == 0 {
		return nil, true
	}
	buf := make([]byte, bufSize)
	r, _, _ = procPdhGetFormattedCounterArrayW.Call(
		counter,
		uintptr(formatFlag),
		uintptr(unsafe.Pointer(&bufSize)),
		uintptr(unsafe.Pointer(&itemCount)),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if r != 0 {
		slog.Debug("PdhGetFormattedCounterArrayW failed",
			"status", fmt.Sprintf("0x%08x", uint32(r)))
		return nil, false
	}
	return unsafe.Slice((*pdhFmtCounterValueItemW)(unsafe.Pointer(&buf[0])), itemCount), true
}

// Snapshot returns the latest published statsSnapshot. Safe to call
// concurrently from many HTTP request goroutines — the snapshot value
// is immutable post-publish. Returns a zero-value snapshot (empty GPU
// map, zero CPUUtilPct, zero MemUsedBytes) before the first tick
// completes, so callers don't need nil checks.
func (c *statsCollector) Snapshot() statsSnapshot {
	p := c.latest.Load()
	if p == nil {
		return statsSnapshot{}
	}
	return *p
}

// Stop signals the tick goroutine to exit, waits for it, and closes the
// PDH query. Safe to call any number of times from any number of
// goroutines — sync.Once guarantees the shutdown body runs exactly
// once. (A naive `select { case <-c.stop: default: }` guard would race
// two concurrent callers past the guard before either ran the close,
// double-closing the channel and panicking. The current single caller
// is `defer collector.Stop()` in main, so the panic is unreachable
// today, but the Once costs nothing and keeps the docstring honest if
// a future supervisor or signal handler ever joins in.)
func (c *statsCollector) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
		<-c.done
		if c.query != 0 {
			procPdhCloseQuery.Call(c.query)
			c.query = 0
		}
	})
}
