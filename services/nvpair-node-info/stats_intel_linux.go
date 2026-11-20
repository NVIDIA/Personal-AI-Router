// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"encoding/csv"
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
	"time"
)

// The Intel worker publishes completed samples independently of the shared tick.
func (c *statsCollector) runIntel(ctx context.Context) {
	c.intelSources = discoverIntelStatsSources(ctx)
	// An empty successful discovery or an absent tool needs no idle polling.
	if len(c.intelSources) == 0 && (c.intelSources != nil || intelXpuSmiUnavailable.Load()) {
		return
	}
	c.nextIntelDiscovery = time.Now().Add(intelDiscoveryRetryInterval)
	if n := len(c.intelSources); n > 0 {
		slog.Info("Intel GPU stats sources discovered", "count", n)
	}
	ticker := time.NewTicker(statsTickInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		c.refreshIntelSources(ctx, time.Now())
		reading := &intelGPUReading{stats: make(map[string]gpuStat)}
		for _, src := range c.intelSources {
			reading.inventory = append(reading.inventory, src.GPUInfo)
		}
		reading.hasUtilization = sampleIntelStats(ctx, c.intelSources, reading.stats) > 0
		reading.collectedAt = time.Now()
		if ctx.Err() != nil {
			return
		}
		c.latestIntel.Store(reading)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type intelGPUReading struct {
	stats          map[string]gpuStat
	inventory      []GPUInfo
	collectedAt    time.Time
	hasUtilization bool
}

// deviceID joins a dump row to its adapter. Sysfs and render paths are
// optional sources for VRAM usage.
type intelStatsSource struct {
	GPUInfo
	deviceID   int
	sysfsCard  string
	renderNode string
	hasDevID   bool
}

// intelXpuSmiUnavailable latches a missing binary or explicit access refusal.
var intelXpuSmiUnavailable atomic.Bool

// v1 uses -n; v2 uses --number. Remember the working form after a sample.
var intelXpuSmiLegacyCount atomic.Bool

// discoverIntelStatsSources enumerates the Intel adapters the stats
// collector will sample every tick. Combines sysfs (authoritative for
// the card path used by the memory-used fallback) with xpu-smi's
// device_id (when available) so the batched dump can be joined back to
// each adapter without another discovery call per tick.
func discoverIntelStatsSources(ctx context.Context) []intelStatsSource {
	return mergeIntelStatsSources(collectIntelSysfsCards(), collectIntelXpuSmiDevices(ctx))
}

func mergeIntelStatsSources(bySysfs []intelStatsSource, byXpuSmi map[string]intelStatsSource) []intelStatsSource {
	seen := make(map[string]struct{})
	var out []intelStatsSource
	if byXpuSmi != nil {
		// An empty successful discovery needs no retry on a host without Intel.
		out = make([]intelStatsSource, 0, len(bySysfs)+len(byXpuSmi))
	}

	// Prefer the sysfs walk as the source of truth: every card we can
	// read memory-used from ends up in the sample set. Enrich with the
	// xpu-smi device_id when its BDF matches, so utilization joins.
	for _, s := range bySysfs {
		if _, dup := seen[s.statsKey]; dup {
			continue
		}
		if x, ok := byXpuSmi[s.pciAddress]; ok {
			s.GPUInfo = x.GPUInfo
			s.deviceID = x.deviceID
			s.hasDevID = x.hasDevID
			if s.renderNode == "" {
				s.renderNode = x.renderNode
			}
		}
		seen[s.statsKey] = struct{}{}
		out = append(out, s)
	}
	// Keep CLI-only physical adapters as telemetry sources.
	for _, x := range byXpuSmi {
		if _, dup := seen[x.statsKey]; dup {
			continue
		}
		seen[x.statsKey] = struct{}{}
		out = append(out, x)
	}
	// Static inventory and recovery use the same capacity sources.
	for i := range out {
		src := &out[i]
		if src.VramBytes == 0 && src.sysfsCard != "" {
			src.VramBytes = readSysfsVRAMTotal(src.sysfsCard)
		}
		if src.VramBytes == 0 && src.renderNode != "" {
			src.VramBytes, _ = intelVRAMTotalFromDRM(src.renderNode)
		}
	}
	slices.SortFunc(out, func(a, b intelStatsSource) int { return strings.Compare(a.pciAddress, b.pciAddress) })
	return out
}

const intelDiscoveryRetryInterval = time.Minute

// Device IDs may be unavailable while the driver initializes. A failed retry
// keeps existing IDs and sysfs memory sources usable until discovery recovers.
func (c *statsCollector) refreshIntelSources(ctx context.Context, now time.Time) {
	if intelXpuSmiUnavailable.Load() || now.Before(c.nextIntelDiscovery) {
		return
	}
	if c.intelSources != nil && !slices.ContainsFunc(c.intelSources, func(s intelStatsSource) bool { return !s.hasDevID }) {
		return
	}
	c.nextIntelDiscovery = now.Add(intelDiscoveryRetryInterval)
	if devices := collectIntelXpuSmiDevices(ctx); devices != nil {
		c.intelSources = mergeIntelStatsSources(collectIntelSysfsCards(), devices)
	}
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
			GPUInfo:    GPUInfo{Name: "Intel GPU", statsKey: intelStatsKeyPrefix + bdf, pciAddress: bdf},
			sysfsCard:  card,
			renderNode: intelRenderNodeForBDF(bdf),
		})
	}
	return out
}

// collectIntelXpuSmiDevices calls xpu-smi discovery once to pull the
// device_id → BDF map. Returns a map keyed by BDF so callers can enrich
// sysfs-discovered adapters in place. A missing executable or explicit access
// refusal is latched; other failures fall back to sysfs for this discovery.
func collectIntelXpuSmiDevices(ctx context.Context) map[string]intelStatsSource {
	if intelXpuSmiUnavailable.Load() {
		return nil
	}
	out, err := runXpuSmi(ctx, "discovery", "--json")
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if xpuSmiUnavailable(err) && intelXpuSmiUnavailable.CompareAndSwap(false, true) {
			slog.Info("xpu-smi unavailable; Intel GPU utilization will not be reported",
				"err", err)
		}
		return nil
	}
	sources, err := parseXpuSmiDiscovery(out)
	if err != nil {
		slog.Debug("invalid xpu-smi discovery response", "err", err)
		return nil
	}
	res := make(map[string]intelStatsSource, len(sources))
	for _, src := range sources {
		if !isIntelVirtualFunction(src.pciAddress) {
			src.renderNode = intelRenderNodeForBDF(src.pciAddress)
			res[src.pciAddress] = src
		}
	}
	return res
}

// sampleIntelStats performs one Intel telemetry pass and folds the
// results into out (keyed by statsKey). Returns the count of adapters
// for which a fresh utilization sample was collected — the stats
// collector uses that to decide whether to advance GPUSampledAt.
//
// If xpu-smi omits memory usage, try the i915 memory query and then the
// driver's sysfs counter. A measured zero from xpu-smi is a valid result.
func sampleIntelStats(ctx context.Context, sources []intelStatsSource, out map[string]gpuStat) int {
	if len(sources) == 0 {
		return 0
	}
	utilizationSamples := 0
	xpuStats := sampleIntelXpuSmi(ctx, sources)
	if ctx.Err() != nil {
		return 0
	}

	for _, src := range sources {
		stat := out[src.statsKey]
		x := xpuStats[src.statsKey]
		if x.hasUtil {
			stat.UtilizationPct = x.util
			utilizationSamples++
		}
		if x.hasMem {
			stat.VRAMUsed = x.mem
		}
		if !x.hasMem && stat.VRAMUsed == 0 && src.renderNode != "" {
			if _, used, ok := intelVRAMFromDRM(src.renderNode); ok {
				stat.VRAMUsed = used
			}
		}
		if !x.hasMem && stat.VRAMUsed == 0 && src.sysfsCard != "" {
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
func sampleIntelXpuSmi(ctx context.Context, sources []intelStatsSource) map[string]intelDynamicStat {
	if intelXpuSmiUnavailable.Load() || !slices.ContainsFunc(sources, func(s intelStatsSource) bool { return s.hasDevID }) {
		return nil
	}
	countFlag := "--number"
	if intelXpuSmiLegacyCount.Load() {
		countFlag = "-n"
	}
	out, err := runXpuSmi(ctx, "dump", "--device", "-1", "--metrics", "0,18", countFlag, "1")
	// Only an explicit CLI rejection of --number establishes the legacy
	// spelling. Driver, authorization and timeout errors are not retries.
	if err != nil && ctx.Err() == nil && !xpuSmiUnavailable(err) && countFlag == "--number" && xpuSmiRejectsNumber(err) {
		out, err = runXpuSmi(ctx, "dump", "--device", "-1", "--metrics", "0,18", "-n", "1")
		if err == nil {
			intelXpuSmiLegacyCount.Store(true)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if xpuSmiUnavailable(err) && intelXpuSmiUnavailable.CompareAndSwap(false, true) {
			slog.Info("xpu-smi dump unavailable; using Intel driver memory fallback",
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
