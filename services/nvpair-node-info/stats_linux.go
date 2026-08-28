// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Linux node-stats collection. This collector is the Linux counterpart to
// stats_windows.go: it supplies every dynamic number in /v1/node-info —
// overall CPU busy %, memory-used bytes, and per-GPU VRAM-used + GPU busy %.
// Static identity (CPU name + cores, total RAM, GPU name + total VRAM) is
// detected once at startup elsewhere and merged in by buildResponse.
//
// Data sources, all without cgo:
//
//   - CPU %        : the aggregate "cpu" line of /proc/stat, sampled across
//     two ticks (it reports cumulative jiffies, so a single read is
//     meaningless — we need a delta).
//   - memory-used  : /proc/meminfo, MemTotal - MemAvailable.
//   - GPU          : `nvidia-smi --query-gpu=uuid,utilization.gpu,memory.used`,
//     joined back to the static GPUInfo records by UUID (the statsKey that
//     gpu_linux.go stamps on each adapter). On unified-memory architectures
//     (UMA, e.g. Grace-Blackwell / DGX Spark) nvidia-smi returns [N/A] for
//     memory.used; buildResponse maps the independently sampled system-memory
//     usage onto those statically identified adapters.
//
// Like the Windows collector we keep one background goroutine ticking once a
// second and publish the combined statsSnapshot via an atomic pointer swap;
// HTTP handlers read it lock-free. If nvidia-smi is missing (no NVIDIA driver,
// or an AMD/Intel-only host) GPU stats are simply absent while CPU and memory
// keep working. If /proc reads fail the corresponding field drops out via the
// snapshot's zero value and the downstream omitempty tags.
const statsTickInterval = time.Second

const (
	procStatPath    = "/proc/stat"
	procMeminfoPath = "/proc/meminfo"
)

// cpuTimes holds the cumulative counters from /proc/stat's aggregate "cpu"
// line that we need to derive a utilization percentage across two ticks.
// valid is false when the line couldn't be read or parsed.
type cpuTimes struct {
	idle  uint64 // idle + iowait
	total uint64 // sum of all fields
	valid bool
}

// statsCollector owns the 1 s ticker and the published statsSnapshot. Start it
// once at boot, defer Stop() for clean shutdown, call Snapshot() from the HTTP
// handler. Never nil; the initial snapshot includes system-memory usage so UMA
// responses do not depend on waiting for the first tick.
type statsCollector struct {
	// latest is swapped atomically by the ticker goroutine. Readers copy
	// the pointer; they never see a torn update. The snapshot's GPU map is
	// immutable post-publish — the ticker allocates a fresh map per tick.
	latest atomic.Pointer[statsSnapshot]

	// prevCPU is the previous /proc/stat sample. Only the single ticker
	// goroutine reads or writes it, so no synchronization is needed.
	prevCPU cpuTimes

	// nvidiaUnavailable latches on the first nvidia-smi failure so we don't
	// re-spawn (and re-warn about) a missing binary every tick.
	nvidiaUnavailable atomic.Bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// startStatsCollector synchronously publishes the first system-memory sample,
// primes the CPU baseline, and spins up the tick goroutine. Memory is sampled
// before returning so an immediate UMA response can include vram_used_bytes;
// GPU collection remains asynchronous because nvidia-smi may take up to its
// three-second timeout. Never returns nil.
func startStatsCollector() *statsCollector {
	c := &statsCollector{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	c.latest.Store(initialMemorySnapshot(readMemoryUsed))
	// Prime the CPU baseline so the first tick produces a real delta rather
	// than a spurious reading (with no previous sample, util reports 0).
	c.prevCPU = readCPUTimes()
	go c.run()
	return c
}

// initialMemorySnapshot builds the snapshot published before the ticker starts.
// The reader is injected so success and failure remain unit-testable without
// replacing /proc files. A failed read intentionally leaves memory at zero,
// preserving the existing "unknown means omitted" wire behavior.
func initialMemorySnapshot(readUsed func() (uint64, bool)) *statsSnapshot {
	snap := &statsSnapshot{}
	if used, ok := readUsed(); ok {
		snap.MemUsedBytes = used
	}
	return snap
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

// decodeSnapshot runs one sampling pass across every source. Called only from
// the single-writer ticker goroutine, so the prevCPU update is race-free and
// the atomic pointer swap on c.latest is the only publishing step.
func (c *statsCollector) decodeSnapshot() *statsSnapshot {
	previous := c.Snapshot()
	snap := &statsSnapshot{}

	cur := readCPUTimes()
	snap.CPUUtilPct = cpuUtilization(c.prevCPU, cur)
	if cur.valid {
		c.prevCPU = cur
	}

	if used, ok := readMemoryUsed(); ok {
		snap.MemUsedBytes = used
	}

	gpu := make(map[string]gpuStat)
	sampledAt := time.Time{}
	if c.decodeGPU(gpu) {
		sampledAt = time.Now()
	}
	applyGPUStats(previous, snap, gpu, sampledAt)
	return snap
}

// decodeGPU queries nvidia-smi and folds the per-GPU results into out, keyed
// by UUID. On the first failure it latches nvidiaUnavailable so subsequent
// ticks short-circuit silently. Unified-memory usage remains available through
// the independent /proc/meminfo sample assembled by buildResponse.
func (c *statsCollector) decodeGPU(out map[string]gpuStat) bool {
	if c.nvidiaUnavailable.Load() {
		return false
	}
	csv, err := nvidiaSmiCSV("uuid,utilization.gpu,memory.used")
	if err != nil {
		if c.nvidiaUnavailable.CompareAndSwap(false, true) {
			slog.Warn("nvidia-smi unavailable; GPU utilization / dedicated VRAM-used will not be reported",
				"err", err)
		}
		return false
	}
	parsed, utilizationSamples := parseNvidiaDynamic(csv)
	for k, v := range parsed {
		out[k] = v
	}
	return utilizationSamples > 0
}

// Snapshot returns the latest published statsSnapshot. Safe for concurrent
// callers — the value is immutable post-publish. Returns a zero-value
// snapshot before the first tick, so callers need no nil checks.
func (c *statsCollector) Snapshot() statsSnapshot {
	p := c.latest.Load()
	if p == nil {
		return statsSnapshot{}
	}
	return *p
}

// Stop signals the tick goroutine to exit and waits for it. Safe to call any
// number of times from any number of goroutines via sync.Once.
func (c *statsCollector) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
		<-c.done
	})
}

// readCPUTimes reads the aggregate "cpu" line from /proc/stat. Returns an
// invalid cpuTimes on any read/parse failure, which cpuUtilization treats as
// "unknown" (0 %).
func readCPUTimes() cpuTimes {
	data, err := os.ReadFile(procStatPath)
	if err != nil {
		slog.Debug("read /proc/stat failed", "err", err)
		return cpuTimes{}
	}
	return parseProcStat(string(data))
}

// parseProcStat extracts the aggregate "cpu" line (not the per-core "cpuN"
// lines). The fields after the label are, in order: user nice system idle
// iowait irq softirq steal guest guest_nice. idle time is idle+iowait; total
// is the sum of every field.
func parseProcStat(s string) cpuTimes {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var total, idle uint64
		for i, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				continue
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		if total == 0 {
			return cpuTimes{}
		}
		return cpuTimes{idle: idle, total: total, valid: true}
	}
	return cpuTimes{}
}

// cpuUtilization derives a 0..100 busy percentage from two cumulative samples.
// Returns 0 when either sample is invalid, the counters went backwards (a
// counter reset), or no jiffies elapsed between samples — all of which read as
// "idle/unknown", matching the omitempty convention used downstream.
func cpuUtilization(prev, cur cpuTimes) uint32 {
	if !prev.valid || !cur.valid {
		return 0
	}
	if cur.total < prev.total || cur.idle < prev.idle {
		return 0
	}
	totalDelta := cur.total - prev.total
	idleDelta := cur.idle - prev.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		return 0
	}
	busy := float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
	return uint32(math.Round(busy))
}

// readMemoryUsed returns physical-memory bytes in use, or false on failure.
func readMemoryUsed() (uint64, bool) {
	data, err := os.ReadFile(procMeminfoPath)
	if err != nil {
		slog.Debug("read /proc/meminfo failed", "err", err)
		return 0, false
	}
	return parseMeminfoUsed(string(data))
}

// parseMeminfoUsed computes used = MemTotal - MemAvailable. When MemAvailable
// is absent (kernels < 3.14) it falls back to MemFree, which overstates "used"
// by reclaimable buffers/cache but is the best estimate on such kernels.
// /proc/meminfo values are in kB; the result is in bytes. Returns false when
// MemTotal is missing or the subtraction would underflow.
func parseMeminfoUsed(s string) (uint64, bool) {
	var total, avail, free uint64
	var haveTotal, haveAvail, haveFree bool
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = v, true
		case "MemAvailable:":
			avail, haveAvail = v, true
		case "MemFree:":
			free, haveFree = v, true
		}
	}
	if !haveTotal {
		return 0, false
	}
	switch {
	case haveAvail && avail <= total:
		return (total - avail) * 1024, true
	case haveFree && free <= total:
		return (total - free) * 1024, true
	default:
		return 0, false
	}
}

// parseNvidiaDynamic decodes the dynamic query (uuid,utilization.gpu,
// memory.used) into the per-GPU snapshot map keyed by UUID — the same statsKey
// gpu_linux.go stamped on each static GPUInfo. memory.used is in MiB
// (-nounits) and converted to bytes; utilization.gpu is an integer percent,
// clamped to 100. The returned sample count includes a parsed 0 % reading but
// excludes [N/A] and malformed utilization, so callers do not mistake
// memory-only rows for fresh utilization. Rows with a missing uuid are skipped.
// A memory.used value of [N/A] remains zero here; buildResponse supplies
// system-memory usage for GPUs that static detection identified as
// unified-memory adapters.
func parseNvidiaDynamic(out string) (map[string]gpuStat, int) {
	res := map[string]gpuStat{}
	utilizationSamples := 0
	for _, line := range strings.Split(out, "\n") {
		fields := splitCSVRow(line)
		if len(fields) < 3 {
			continue
		}
		uuid := fields[0]
		if uuid == "" {
			continue
		}
		var stat gpuStat
		if pct, err := strconv.ParseUint(fields[1], 10, 32); err == nil {
			if pct > 100 {
				pct = 100
			}
			stat.UtilizationPct = uint32(pct)
			utilizationSamples++
		}
		if mib, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
			stat.VRAMUsed = mib * 1024 * 1024
		}
		res[uuid] = stat
	}
	return res, utilizationSamples
}
