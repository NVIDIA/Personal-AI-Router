// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDarwinCPUUtilization(t *testing.T) {
	valid := func(idle, total float64) darwinCPUTimes {
		return darwinCPUTimes{idle: idle, total: total, valid: true}
	}
	cases := []struct {
		name string
		prev darwinCPUTimes
		cur  darwinCPUTimes
		want uint32
	}{
		{"busy delta", valid(40, 100), valid(60, 200), 80},
		{"idle delta", valid(40, 100), valid(140, 200), 0},
		{"invalid baseline", darwinCPUTimes{}, valid(60, 200), 0},
		{"counter reset", valid(40, 100), valid(20, 50), 0},
		{"no elapsed ticks", valid(40, 100), valid(40, 100), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			{
				got := darwinCPUUtilization(c.prev, c.cur)
				require.True(t, got == c.want, "darwinCPUUtilization() (%v)", got)
			}
		})
	}
}

func TestInitialDarwinMemorySnapshot(t *testing.T) {
	snap := initialDarwinMemorySnapshot(func() (uint64, bool) {
		return 12 << 30, true
	})
	require.True(t, snap.MemUsedBytes == 12<<30, "MemUsedBytes")

	snap = initialDarwinMemorySnapshot(func() (uint64, bool) {
		return 0, false
	})
	require.Equal(t, uint64(0), snap.MemUsedBytes, "failed read published")
}

func TestDarwinCollectorPublishesAndStops(t *testing.T) {
	var cpuReads atomic.Uint64
	readCPU := func() darwinCPUTimes {
		n := float64(cpuReads.Add(1))
		return darwinCPUTimes{idle: n * 20, total: n * 100, valid: true}
	}
	var memoryReads atomic.Uint64
	readMemory := func() (uint64, bool) {
		return memoryReads.Add(1) << 30, true
	}
	readGPU := func(context.Context) (darwinGPUReading, error) {
		return darwinGPUReading{
			inventory: []GPUInfo{{Name: "Apple M3 Max", statsKey: "ioreg:2a"}},
			stats: map[string]gpuStat{
				"ioreg:2a": {VRAMUsed: 8 << 30, UtilizationPct: 42},
			},
			utilizationSamples: 1,
		}, nil
	}

	c := newDarwinStatsCollector(readCPU, readMemory, readGPU, 2*time.Millisecond)
	deadline := time.Now().Add(250 * time.Millisecond)
	for (c.Snapshot().CPUUtilPct != 80 ||
		c.Snapshot().GPU["ioreg:2a"].UtilizationPct != 42) &&
		time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	snap := c.Snapshot()
	require.Equal(t, uint32(80), snap.CPUUtilPct)
	require.GreaterOrEqual(t, snap.MemUsedBytes, uint64(2<<30))
	require.Equal(t, uint32(42), snap.GPU["ioreg:2a"].UtilizationPct, "GPU snapshot")
	require.Len(t, snap.GPUInventory, 1, "GPU inventory")
	require.Equal(t, "Apple M3 Max", snap.GPUInventory[0].Name, "GPU inventory")

	c.Stop()
	c.Stop()
}

func TestDarwinCollectorKeepsMemoryOnCPUFailure(t *testing.T) {
	c := &statsCollector{
		prevCPU: darwinCPUTimes{idle: 20, total: 100, valid: true},
		readCPU: func() darwinCPUTimes {
			return darwinCPUTimes{}
		},
		readMemory: func() (uint64, bool) {
			return 8 << 30, true
		},
	}
	snap := c.decodeSystemSnapshot()
	require.Equal(t, uint32(0), snap.CPUUtilPct, "unexpected partial snapshot (%v)", snap)
	require.True(t, snap.MemUsedBytes == 8<<30, "unexpected partial snapshot (%v)", snap)
}

func TestDarwinCollectorRetriesGPUAfterFailure(t *testing.T) {
	var calls atomic.Uint64
	c := &statsCollector{
		prevCPU: darwinCPUTimes{idle: 20, total: 100, valid: true},
		readCPU: func() darwinCPUTimes {
			return darwinCPUTimes{idle: 40, total: 200, valid: true}
		},
		readMemory: func() (uint64, bool) {
			return 8 << 30, true
		},
		readGPU: func(context.Context) (darwinGPUReading, error) {
			if calls.Add(1) == 1 {
				return darwinGPUReading{}, errors.New("temporary ioreg failure")
			}
			return darwinGPUReading{
				inventory:          []GPUInfo{{Name: "Apple M3 Max", statsKey: "ioreg:2a"}},
				stats:              map[string]gpuStat{"ioreg:2a": {UtilizationPct: 77}},
				utilizationSamples: 1,
			}, nil
		},
	}
	c.latest.Store(&statsSnapshot{})

	c.collectGPU(context.Background())
	{
		got := c.Snapshot().GPU
		require.Len(t, got, 0, "failed GPU read published")
	}
	c.collectGPU(context.Background())
	{
		got := c.Snapshot().GPU["ioreg:2a"].UtilizationPct
		require.Equal(t, uint32(77), got, "retry utilization")
	}
}

func TestDarwinCollectorRequiresUtilizationSample(t *testing.T) {
	readings := []darwinGPUReading{
		{
			inventory: []GPUInfo{{Name: "Apple M3 Max", statsKey: "ioreg:2a"}},
			stats:     map[string]gpuStat{"ioreg:2a": {VRAMUsed: 2 << 30}},
		},
		{
			inventory:          []GPUInfo{{Name: "Apple M3 Max", statsKey: "ioreg:2a"}},
			stats:              map[string]gpuStat{"ioreg:2a": {VRAMUsed: 3 << 30, UtilizationPct: 0}},
			utilizationSamples: 1,
		},
		{
			inventory: []GPUInfo{{Name: "Apple M3 Max", statsKey: "ioreg:2a"}},
			stats:     map[string]gpuStat{"ioreg:2a": {VRAMUsed: 4 << 30}},
		},
	}
	call := 0
	c := &statsCollector{
		readGPU: func(context.Context) (darwinGPUReading, error) {
			reading := readings[call]
			call++
			return reading, nil
		},
	}
	c.latest.Store(&statsSnapshot{})

	c.collectGPU(context.Background())
	partial := c.Snapshot()
	require.True(t, partial.GPU["ioreg:2a"].VRAMUsed == 2<<30, "pre-utilization reading (%v)", partial)
	require.True(t, partial.GPUSampledAt.IsZero(), "pre-utilization reading (%v)", partial)

	c.collectGPU(context.Background())
	idle := c.Snapshot()
	require.True(t, idle.GPU["ioreg:2a"].VRAMUsed == 3<<30, "valid idle reading (%v)", idle)
	require.Equal(t, uint32(0), idle.GPU["ioreg:2a"].UtilizationPct, "valid idle reading (%v)", idle)
	require.False(t, idle.GPUSampledAt.IsZero(), "valid idle reading (%v)", idle)

	c.collectGPU(context.Background())
	retained := c.Snapshot()
	require.True(t, retained.GPU["ioreg:2a"].VRAMUsed == 3<<30, "missing utilization replaced last valid reading: (%v, %v)", retained, idle)
	require.True(t, retained.GPUSampledAt.Equal(idle.GPUSampledAt), "missing utilization replaced last valid reading: (%v, %v)", retained, idle)
}

func TestDarwinCollectorPublishesSystemStatsWhileGPUBlocks(t *testing.T) {
	started := make(chan struct{})
	readGPU := func(ctx context.Context) (darwinGPUReading, error) {
		close(started)
		<-ctx.Done()
		return darwinGPUReading{}, ctx.Err()
	}
	var cpuReads atomic.Uint64
	c := newDarwinStatsCollector(
		func() darwinCPUTimes {
			n := float64(cpuReads.Add(1))
			return darwinCPUTimes{idle: n * 20, total: n * 100, valid: true}
		},
		func() (uint64, bool) { return 8 << 30, true },
		readGPU,
		2*time.Millisecond,
	)
	<-started

	deadline := time.Now().Add(250 * time.Millisecond)
	for c.Snapshot().CPUUtilPct != 80 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	{
		got := c.Snapshot().CPUUtilPct
		require.Equal(t, uint32(80), got, "CPUUtilPct")
	}

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(250 * time.Millisecond):
		require.FailNow(t, "test expectation failed", "Stop did not cancel blocked GPU reader")
	}
}

func TestDynamicGPUReadingFromIORegistry(t *testing.T) {
	reading, err := dynamicGPUReadingFromIORegistry([]byte(ioRegistryGPUFixture), 36<<30)
	require.NoError(t, err, "dynamicGPUReadingFromIORegistry() error")
	apple := reading.stats["ioreg:2a"]
	require.True(t, apple.VRAMUsed == 8<<30, "unexpected Apple stats (%v)", apple)
	require.Equal(t, uint32(100), apple.UtilizationPct, "unexpected Apple stats (%v)", apple)
	discrete := reading.stats["ioreg:63"]
	require.True(t, discrete.VRAMUsed == 2<<30, "unexpected discrete stats (%v)", discrete)
	require.Equal(t, uint32(25), discrete.UtilizationPct, "unexpected discrete stats (%v)", discrete)
	require.True(t, reading.inventory[0].VramBytes == 36<<30, "unexpected Apple inventory")
	require.Equal(t, 2, reading.utilizationSamples, "utilization samples")
}
