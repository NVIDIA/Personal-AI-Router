// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
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
// output, others include the BDF; we accept either.
type intelStatsSource struct {
	statsKey  string
	bdf       string
	deviceID  int
	sysfsCard string
	hasDevID  bool
}

// intelXpuSmiUnavailable latches on the first xpu-smi failure so we
// don't re-spawn (and re-warn about) a missing binary every tick.
var intelXpuSmiUnavailable atomic.Bool

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
	cards, err := filepath.Glob("/sys/class/drm/card*")
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
		if err != nil || bdf == "" {
			continue
		}
		out = append(out, intelStatsSource{
			statsKey:  intelStatsKeyPrefix + bdf,
			bdf:       bdf,
			sysfsCard: card,
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
		res[bdf] = intelStatsSource{
			statsKey: key,
			bdf:      bdf,
			deviceID: d.DeviceID,
			hasDevID: true,
		}
	}
	return res
}

// sampleIntelStats performs one Intel telemetry pass and folds the
// results into out (keyed by statsKey). Returns the count of adapters
// for which a fresh utilization sample was collected — the stats
// collector uses that to decide whether to advance GPUSampledAt.
//
// The xpu-smi path runs first (one batched invocation for every
// adapter), then the sysfs fallback fills any adapter whose VRAM-used
// slot is still zero. Neither path is mandatory: on a host with no
// xpu-smi and iGPUs only, the map is left untouched and the Intel
// adapters still appear in the inventory without dynamic stats.
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
// Metric IDs: 0 = GPU utilization %, 5 = GPU memory used (bytes). These
// are the two IDs xpu-smi has kept stable across every 1.x release; the
// dump JSON reports them under human-readable column names that vary
// per build ("GPU Utilization (%)", "XPUM_STATS_GPU_UTILIZATION", etc.),
// so we normalize the header lookup.
func sampleIntelXpuSmi(sources []intelStatsSource) map[string]intelDynamicStat {
	if intelXpuSmiUnavailable.Load() {
		return nil
	}
	// -d -1 selects all devices; -n 1 requests exactly one snapshot; -m
	// 0,5 picks utilization and memory-used. -j asks for JSON. Ancient
	// xpu-smi releases reject -j — an unknown flag surfaces as a
	// nonzero exit which the latch below then silences.
	out, err := xpuSmiJSON(context.Background(),
		"dump", "-d", "-1", "-m", "0,5", "-n", "1", "-j")
	if err != nil {
		if intelXpuSmiUnavailable.CompareAndSwap(false, true) {
			slog.Info("xpu-smi dump unavailable; Intel GPU utilization / memory-used will not be reported",
				"err", err)
		}
		return nil
	}
	return parseIntelXpuSmiDump(out, sources)
}

// parseIntelXpuSmiDump decodes an `xpu-smi dump -j` payload. The tool
// emits either a top-level JSON array of row objects or an object with
// a "data" / "device_list" array; we accept both. Each row has a
// device-id field and free-form column keys — we match utilization and
// memory-used by keyword rather than by exact name so a header rename
// in a future xpu-smi build still parses.
func parseIntelXpuSmiDump(out []byte, sources []intelStatsSource) map[string]intelDynamicStat {
	rows := extractIntelDumpRows(out)
	if len(rows) == 0 {
		return nil
	}
	byDeviceID := make(map[int]string, len(sources))
	byBDF := make(map[string]string, len(sources))
	for _, s := range sources {
		if s.hasDevID {
			byDeviceID[s.deviceID] = s.statsKey
		}
		if s.bdf != "" {
			byBDF[s.bdf] = s.statsKey
		}
	}
	res := make(map[string]intelDynamicStat, len(rows))
	for _, row := range rows {
		key := matchIntelDumpRow(row, byDeviceID, byBDF)
		if key == "" {
			continue
		}
		stat := intelDynamicStat{}
		for k, v := range row {
			lk := strings.ToLower(k)
			switch {
			case strings.Contains(lk, "util"):
				if pct, ok := parseFloatishPercent(v); ok {
					stat.util = pct
					stat.hasUtil = true
				}
			case strings.Contains(lk, "mem"):
				if bytesUsed, ok := parseIntelMemoryValue(k, v); ok {
					stat.mem = bytesUsed
					stat.hasMem = true
				}
			}
		}
		if stat.hasUtil || stat.hasMem {
			res[key] = stat
		}
	}
	return res
}

// extractIntelDumpRows unpacks the top-level shape variations xpu-smi
// dump has emitted across releases: a bare array of row objects, or an
// object with a rows/data/device_list array of the same. Returns nil
// when neither shape matches.
func extractIntelDumpRows(out []byte) []map[string]any {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []map[string]any
		if err := json.Unmarshal(out, &arr); err == nil {
			return arr
		}
		return nil
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		return nil
	}
	for _, key := range []string{"data", "rows", "device_list", "metrics"} {
		if v, ok := env[key]; ok {
			if arr, ok := v.([]any); ok {
				rows := make([]map[string]any, 0, len(arr))
				for _, r := range arr {
					if m, ok := r.(map[string]any); ok {
						rows = append(rows, m)
					}
				}
				return rows
			}
		}
	}
	return nil
}

// matchIntelDumpRow resolves a dump row back to one of the adapters we
// know about. Prefers device_id (an integer) when the row exposes it,
// then falls back to matching on PCI BDF if the row carries one. A row
// that can't be matched is skipped — a version of xpu-smi that reports
// an adapter we didn't discover during startup is safely ignored.
func matchIntelDumpRow(row map[string]any, byDeviceID map[int]string, byBDF map[string]string) string {
	for k, v := range row {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "deviceid") || lk == "device_id" || lk == "device" {
			if id, ok := parseInt(v); ok {
				if key, ok := byDeviceID[id]; ok {
					return key
				}
			}
		}
		if strings.Contains(lk, "bdf") || strings.Contains(lk, "pci") {
			if s, ok := v.(string); ok {
				bdf := strings.ToLower(strings.TrimSpace(s))
				if strings.Count(bdf, ":") == 1 {
					bdf = "0000:" + bdf
				}
				if key, ok := byBDF[bdf]; ok {
					return key
				}
			}
		}
	}
	return ""
}

// parseFloatishPercent accepts the integer, float, or string forms
// xpu-smi has emitted for a percent column and returns a 0..100 uint32.
// Values above 100 are clamped, matching the nvidia-smi treatment.
func parseFloatishPercent(v any) (uint32, bool) {
	f, ok := parseFloat(v)
	if !ok {
		return 0, false
	}
	if f < 0 {
		return 0, false
	}
	if f > 100 {
		f = 100
	}
	return uint32(f + 0.5), true
}

// parseIntelMemoryValue interprets the value of a memory column. xpu-smi
// dump reports either raw bytes or MiB depending on the column heading;
// we detect MiB from the header text and convert. Unknown units default
// to bytes.
func parseIntelMemoryValue(header string, v any) (uint64, bool) {
	f, ok := parseFloat(v)
	if !ok || f < 0 {
		return 0, false
	}
	lh := strings.ToLower(header)
	switch {
	case strings.Contains(lh, "mib") || strings.Contains(lh, "(mib)"):
		return uint64(f * 1024 * 1024), true
	case strings.Contains(lh, "gib") || strings.Contains(lh, "(gib)"):
		return uint64(f * 1024 * 1024 * 1024), true
	default:
		return uint64(f), true
	}
}

func parseFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f, true
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" || strings.EqualFold(s, "n/a") {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func parseInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
	case string:
		s := strings.TrimSpace(x)
		if i, err := strconv.Atoi(s); err == nil {
			return i, true
		}
	}
	return 0, false
}
