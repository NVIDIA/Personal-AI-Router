// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

const statsTickInterval = time.Second

type darwinCPUTimes struct {
	idle  float64
	total float64
	valid bool
}

type darwinGPUReading struct {
	inventory          []GPUInfo
	stats              map[string]gpuStat
	utilizationSamples int
	sampledAt          time.Time
}

type statsCollector struct {
	latest    atomic.Pointer[statsSnapshot]
	latestGPU atomic.Pointer[darwinGPUReading]

	prevCPU    darwinCPUTimes
	readCPU    func() darwinCPUTimes
	readMemory func() (uint64, bool)
	readGPU    func(context.Context) (darwinGPUReading, error)
	interval   time.Duration

	gpuWarning sync.Once
	cancel     context.CancelFunc
	done       sync.WaitGroup
	stopOnce   sync.Once
}

func startStatsCollector() *statsCollector {
	return newDarwinStatsCollector(
		readDarwinCPUTimes,
		readDarwinMemoryUsed,
		readDarwinGPUStats,
		statsTickInterval,
	)
}

func newDarwinStatsCollector(
	readCPU func() darwinCPUTimes,
	readMemory func() (uint64, bool),
	readGPU func(context.Context) (darwinGPUReading, error),
	interval time.Duration,
) *statsCollector {
	ctx, cancel := context.WithCancel(context.Background())
	c := &statsCollector{
		readCPU:    readCPU,
		readMemory: readMemory,
		readGPU:    readGPU,
		interval:   interval,
		cancel:     cancel,
	}
	c.latest.Store(initialDarwinMemorySnapshot(readMemory))
	c.prevCPU = readCPU()
	c.done.Add(2)
	go c.runSystem(ctx)
	go c.runGPU(ctx)
	return c
}

func initialDarwinMemorySnapshot(readMemory func() (uint64, bool)) *statsSnapshot {
	snap := &statsSnapshot{}
	if used, ok := readMemory(); ok {
		snap.MemUsedBytes = used
	}
	return snap
}

func (c *statsCollector) runSystem(ctx context.Context) {
	defer c.done.Done()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.latest.Store(c.decodeSystemSnapshot())
		}
	}
}

func (c *statsCollector) runGPU(ctx context.Context) {
	defer c.done.Done()
	for {
		c.collectGPU(ctx)
		timer := time.NewTimer(c.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *statsCollector) collectGPU(ctx context.Context) {
	reading, err := c.readGPU(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.gpuWarning.Do(func() {
				slog.Warn("ioreg GPU metrics unavailable; retrying", "err", err)
			})
		}
		return
	}
	if len(reading.stats) == 0 {
		return
	}
	if reading.utilizationSamples > 0 {
		reading.sampledAt = time.Now()
		c.latestGPU.Store(&reading)
		return
	}
	previous := c.latestGPU.Load()
	if previous == nil || previous.sampledAt.IsZero() {
		c.latestGPU.Store(&reading)
	}
}

func (c *statsCollector) decodeSystemSnapshot() *statsSnapshot {
	snap := &statsSnapshot{}
	cur := c.readCPU()
	snap.CPUUtilPct = darwinCPUUtilization(c.prevCPU, cur)
	if cur.valid {
		c.prevCPU = cur
	}
	if used, ok := c.readMemory(); ok {
		snap.MemUsedBytes = used
	}
	return snap
}

func (c *statsCollector) Snapshot() statsSnapshot {
	p := c.latest.Load()
	if p == nil {
		return statsSnapshot{}
	}
	snap := *p
	if gpu := c.latestGPU.Load(); gpu != nil {
		snap.GPU = gpu.stats
		snap.GPUInventory = gpu.inventory
		snap.GPUSampledAt = gpu.sampledAt
	}
	return snap
}

func (c *statsCollector) Stop() {
	c.stopOnce.Do(func() {
		c.cancel()
		c.done.Wait()
	})
}

func readDarwinCPUTimes() darwinCPUTimes {
	times, err := cpu.Times(false)
	if err != nil || len(times) != 1 {
		slog.Debug("read Darwin CPU times failed", "err", err)
		return darwinCPUTimes{}
	}
	t := times[0]
	total := t.User + t.System + t.Idle + t.Nice
	if total <= 0 {
		return darwinCPUTimes{}
	}
	return darwinCPUTimes{idle: t.Idle, total: total, valid: true}
}

func darwinCPUUtilization(prev, cur darwinCPUTimes) uint32 {
	if !prev.valid || !cur.valid || cur.total < prev.total || cur.idle < prev.idle {
		return 0
	}
	totalDelta := cur.total - prev.total
	idleDelta := cur.idle - prev.idle
	if totalDelta <= 0 || idleDelta > totalDelta {
		return 0
	}
	return uint32(math.Round((totalDelta - idleDelta) * 100 / totalDelta))
}

func readDarwinMemoryUsed() (uint64, bool) {
	memory, err := mem.VirtualMemory()
	if err != nil {
		slog.Debug("read Darwin memory failed", "err", err)
		return 0, false
	}
	return memory.Used, true
}

func readDarwinGPUStats(ctx context.Context) (darwinGPUReading, error) {
	data, err := readDarwinIORegistryContext(ctx)
	if err != nil {
		return darwinGPUReading{}, err
	}
	var systemMemory uint64
	if memory, memoryErr := mem.VirtualMemory(); memoryErr == nil {
		systemMemory = memory.Total
	}
	return dynamicGPUReadingFromIORegistry(data, systemMemory)
}

func dynamicGPUReadingFromIORegistry(data []byte, systemMemory uint64) (darwinGPUReading, error) {
	records, err := parseIORegistryGPUs(data, systemMemory)
	if err != nil {
		return darwinGPUReading{}, err
	}
	reading := darwinGPUReading{
		inventory: make([]GPUInfo, 0, len(records)),
		stats:     make(map[string]gpuStat, len(records)),
	}
	for _, record := range records {
		reading.inventory = append(reading.inventory, GPUInfo{
			Name:      record.name,
			VramBytes: record.vramTotal,
			statsKey:  record.statsKey,
		})
		if record.utilizationValid {
			reading.utilizationSamples++
		}
		reading.stats[record.statsKey] = gpuStat{
			VRAMUsed:       record.vramUsed,
			UtilizationPct: record.utilizationPct,
		}
	}
	return reading, nil
}
