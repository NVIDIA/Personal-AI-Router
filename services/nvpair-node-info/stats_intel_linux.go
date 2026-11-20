// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// Intel per-tick GPU stats collection on Linux. Runs alongside the
// nvidia-smi pass in stats_linux.go: both write into the same
// map[statsKey]gpuStat, keyed disjointly (Intel entries use the
// intel-pci- prefix so they can't collide with nvidia-smi UUIDs).
//
// Two-tier preference:
//
//  1. xpu-smi dump — one batched invocation per tick that returns a
//     compact utilization + memory-used snapshot for every Intel adapter
//     the Level Zero stack sees. Parsed defensively because the exact
//     JSON schema has drifted across xpu-smi releases.
//
//  2. sysfs (mem_info_vram_used) — best-effort memory-used fallback
//     when xpu-smi is missing or produced no row for a given adapter.
//     Present on Arc dGPUs; iGPUs share system RAM and do not expose it.
//     Utilization is not read from sysfs — Intel doesn't publish a
//     stable busy-percent counter under DRM, so the field remains
//     "unknown" (zero, dropped by omitempty) on hosts without xpu-smi.
//
// The collector caches the set of Intel adapters at startup so per-tick
// work is bounded. A latch on xpu-smi keeps a missing binary from
// re-warning every second, matching the nvidia-smi treatment.

// intelStatsSource pairs the join key with the sysfs directory the
// sysfs fallback reads from. deviceID is the xpu-smi enumeration index,
// used to correlate a dump row back to this adapter — some xpu-smi
// builds report device_id as the only stable identifier in the dump
// output, others include the BDF; we accept either. renderNode is the
// /dev/dri/renderD* path the DRM_IOCTL_I915_QUERY memory-regions fallback
// opens; empty on adapters where no render node was found (very unusual
// but possible under some virtualization).
type intelStatsSource struct {
	statsKey   string
	bdf        string
	deviceID   int
	sysfsCard  string
	renderNode string
	hasDevID   bool
}

// intelXpuSmiUnavailable latches on the first xpu-smi failure so we
// don't re-spawn (and re-warn about) a missing binary every tick.
var intelXpuSmiUnavailable atomic.Bool

// Remember the working sample-count flag after one successful retry.
var intelXpuSmiLegacyCount atomic.Bool

// discoverIntelStatsSources enumerates the Intel adapters the stats
// collector will sample every tick. Combines sysfs (authoritative for
// the card path used by the memory-used fallback) with xpu-smi's
// device_id (when available) so the batched dump can be joined back to
// each adapter without another discovery call per tick.
func discoverIntelStatsSources() []intelStatsSource {
	bySysfs := collectIntelSysfsCards()
	byXpuSmi := collectIntelXpuSmiDevices()

	seen := make(map[string]struct{})
	var out []intelStatsSource

	// Prefer the sysfs walk as the source of truth: every card we can
	// read memory-used from ends up in the sample set. Enrich with the
	// xpu-smi device_id when its BDF matches, so utilization joins.
	for _, s := range bySysfs {
		if _, dup := seen[s.statsKey]; dup {
			continue
		}
		if x, ok := byXpuSmi[s.bdf]; ok {
			s.deviceID = x.deviceID
			s.hasDevID = x.hasDevID
			if s.renderNode == "" {
				s.renderNode = x.renderNode
			}
		}
		seen[s.statsKey] = struct{}{}
		out = append(out, s)
	}
	// xpu-smi may know about adapters sysfs doesn't (e.g. an SR-IOV
	// setup where /sys/class/drm ordering doesn't line up). Emit those
	// too; they'll have no sysfs card path so the memory-used fallback
	// silently skips them, which matches every other "one source only"
	// path in this collector.
	for _, x := range byXpuSmi {
		if _, dup := seen[x.statsKey]; dup {
			continue
		}
		seen[x.statsKey] = struct{}{}
		out = append(out, x)
	}
	return out
}

// collectIntelSysfsCards walks /sys/class/drm/card* and returns one
// entry per Intel adapter, keyed by the same statsKey static discovery
// stamps in GPUInfo. Errors during scan return an empty slice — the
// stats collector treats "no Intel sources" as "nothing to sample".
func collectIntelSysfsCards() []intelStatsSource {
	cards, err := filepath.Glob(filepath.Join(os.Getenv("GHW_CHROOT"), "/sys/class/drm/card*"))
	if err != nil {
		return nil
	}
	var out []intelStatsSource
	for _, card := range cards {
		base := filepath.Base(card)
		if strings.Contains(base, "-") {
			continue
		}
		vendor, err := os.ReadFile(filepath.Join(card, "device", "vendor"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(vendor)) != intelPCIVendorID {
			continue
		}
		bdf, err := readSysfsBDF(card)
		if err != nil || bdf == "" || isIntelVirtualFunction(bdf) {
			continue
		}
		out = append(out, intelStatsSource{
			statsKey:   intelStatsKeyPrefix + bdf,
			bdf:        bdf,
			sysfsCard:  card,
			renderNode: intelRenderNode(card),
		})
	}
	return out
}

// collectIntelXpuSmiDevices calls xpu-smi discovery once to pull the
// device_id → BDF map. Returns a map keyed by BDF so callers can enrich
// sysfs-discovered adapters in place. On any failure the latch is set
// and the map is empty; the stats collector then falls back to sysfs
// alone.
func collectIntelXpuSmiDevices() map[string]intelStatsSource {
	if intelXpuSmiUnavailable.Load() {
		return nil
	}
	out, err := xpuSmiJSON(context.Background(), "discovery", "--json")
	if err != nil {
		if intelXpuSmiUnavailable.CompareAndSwap(false, true) {
			slog.Info("xpu-smi unavailable; Intel GPU utilization will not be reported",
				"err", err)
		}
		return nil
	}
	var env xpuSmiDiscoveryEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil
	}
	res := make(map[string]intelStatsSource, len(env.DeviceList))
	for _, d := range env.DeviceList {
		if !isPhysicalIntelGPU(d) {
			continue
		}
		key := intelStatsKeyFromBDF(d.PCIBDFAddress)
		if key == "" {
			continue
		}
		bdf := strings.TrimPrefix(key, intelStatsKeyPrefix)
		if isIntelVirtualFunction(bdf) {
			continue
		}
		source := intelStatsSource{statsKey: key, bdf: bdf, renderNode: intelRenderNodeForBDF(bdf)}
		if d.DeviceID != nil && *d.DeviceID >= 0 {
			source.deviceID, source.hasDevID = *d.DeviceID, true
		}
		res[bdf] = source
	}
	return res
}

// sampleIntelStats performs one Intel telemetry pass and folds the
// results into out (keyed by statsKey). Returns the count of adapters
// for which a fresh utilization sample was collected — the stats
// collector uses that to decide whether to advance GPUSampledAt.
//
// The xpu-smi path runs first (one batched invocation for every
// adapter), then two fallbacks fill any adapter whose VRAM-used slot
// is still zero: the DRM_IOCTL_I915_QUERY memory-regions ioctl (works
// on every stock i915 kernel — same source intel_gpu_top uses), and
// finally the DRM mem_info_vram_used sysfs attribute (only present on
// newer i915 / xe builds). Neither utilization nor memory-used is
// mandatory: on a host with iGPUs only and no xpu-smi, the map is
// left untouched and the Intel adapters still appear in the inventory
// without dynamic stats.
func sampleIntelStats(sources []intelStatsSource, out map[string]gpuStat) int {
	if len(sources) == 0 {
		return 0
	}
	utilizationSamples := 0
	xpuStats := sampleIntelXpuSmi(sources)

	for _, src := range sources {
		stat := out[src.statsKey]
		if x, ok := xpuStats[src.statsKey]; ok {
			if x.hasUtil {
				stat.UtilizationPct = x.util
				utilizationSamples++
			}
			if x.hasMem {
				stat.VRAMUsed = x.mem
			}
		}
		if stat.VRAMUsed == 0 && src.renderNode != "" {
			if _, used, ok := intelVRAMFromDRM(src.renderNode); ok {
				stat.VRAMUsed = used
			}
		}
		if stat.VRAMUsed == 0 && src.sysfsCard != "" {
			if used, ok := readSysfsVRAMUsed(src.sysfsCard); ok {
				stat.VRAMUsed = used
			}
		}
		// Only publish an entry if we actually have a field to report;
		// otherwise leave the map alone so an omitempty consumer can't
		// misread "we tried and got nothing" as "zero busy".
		if stat.UtilizationPct != 0 || stat.VRAMUsed != 0 {
			out[src.statsKey] = stat
		}
	}
	return utilizationSamples
}

// intelDynamicStat is the parsed per-adapter sample returned by
// sampleIntelXpuSmi. Booleans distinguish "we have this field" from
// "the field is legitimately zero" — a fully-idle utilization reading
// is still a valid sample and must count toward the tick's freshness.
type intelDynamicStat struct {
	util    uint32
	mem     uint64
	hasUtil bool
	hasMem  bool
}

// sampleIntelXpuSmi runs one batched xpu-smi dump invocation for every
// adapter in sources and returns a map keyed by statsKey. On any
// failure (binary missing, timeout, unexpected schema) it returns nil
// and the caller falls through to sysfs.
//
// Metric 0 is GPU utilization (%); metric 18 is memory used (MiB).
// Both supported CLI families return CSV on a non-terminal stdout.
func sampleIntelXpuSmi(sources []intelStatsSource) map[string]intelDynamicStat {
	if intelXpuSmiUnavailable.Load() || !slices.ContainsFunc(sources, func(s intelStatsSource) bool { return s.hasDevID }) {
		return nil
	}
	countFlag := "--number"
	if intelXpuSmiLegacyCount.Load() {
		countFlag = "-n"
	}
	out, err := xpuSmiJSON(context.Background(), "dump", "--device", "-1", "--metrics", "0,18", countFlag, "1")
	// Only an explicit CLI rejection of --number establishes the legacy
	// spelling. Driver, authorization and timeout errors are not retries.
	if err != nil && !xpuSmiUnavailable(err) && countFlag == "--number" && xpuSmiRejectsNumber(err) {
		out, err = xpuSmiJSON(context.Background(), "dump", "--device", "-1", "--metrics", "0,18", "-n", "1")
		if err == nil {
			intelXpuSmiLegacyCount.Store(true)
		}
	}
	if err != nil {
		if xpuSmiUnavailable(err) && intelXpuSmiUnavailable.CompareAndSwap(false, true) {
			slog.Info("xpu-smi dump unavailable; Intel GPU utilization / memory-used will not be reported",
				"err", err)
		} else {
			slog.Debug("xpu-smi dump failed; retaining Intel memory fallbacks", "err", err)
		}
		return nil
	}
	return parseIntelXpuSmiCSV(out, sources)
}

func xpuSmiRejectsNumber(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	message := strings.ToLower(string(exit.Stderr))
	return strings.Contains(message, "--number") &&
		(strings.Contains(message, "not expected") || strings.Contains(message, "unknown option") || strings.Contains(message, "unrecognized option"))
}

func xpuSmiUnavailable(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrPermission) {
		return true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		message := strings.ToLower(string(exit.Stderr))
		return strings.Contains(message, "permission denied") || strings.Contains(message, "access denied")
	}
	return false
}

// parseIntelXpuSmiCSV recognizes only the documented columns, so memory
// utilization cannot be mistaken for GPU utilization or bytes used.
func parseIntelXpuSmiCSV(out []byte, sources []intelStatsSource) map[string]intelDynamicStat {
	r := csv.NewReader(strings.NewReader(string(out)))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	headers, err := r.Read()
	if err != nil {
		return nil
	}
	device, util, memory := -1, -1, -1
	for i, header := range headers {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "deviceid", "device_id":
			device = i
		case "average % utilization of all gpu engines", "gpu utilization (%)", "utilization.gpu (%)":
			util = i
		case "gpu memory used (mib)", "memory.used (mib)":
			memory = i
		}
	}
	if device < 0 || (util < 0 && memory < 0) {
		return nil
	}
	ids := make(map[int]string, len(sources))
	for _, src := range sources {
		if src.hasDevID {
			ids[src.deviceID] = src.statsKey
		}
	}
	result := make(map[string]intelDynamicStat)
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil
		}
		if device >= len(row) {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(row[device]))
		key, ok := ids[id]
		if err != nil || !ok {
			continue
		}
		var stat intelDynamicStat
		if util >= 0 && util < len(row) {
			if n, err := strconv.ParseFloat(strings.TrimSpace(row[util]), 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 100 {
				stat.util, stat.hasUtil = uint32(n+0.5), true
			}
		}
		if memory >= 0 && memory < len(row) {
			if n, err := strconv.ParseFloat(strings.TrimSpace(row[memory]), 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n < math.Exp2(44) {
				stat.mem, stat.hasMem = uint64(n*1024*1024), true
			}
		}
		if stat.hasUtil || stat.hasMem {
			result[key] = stat
		}
	}
	return result
}
