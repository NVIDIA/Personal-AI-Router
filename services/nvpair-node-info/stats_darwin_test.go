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
			if got := darwinCPUUtilization(c.prev, c.cur); got != c.want {
				t.Fatalf("darwinCPUUtilization() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestInitialDarwinMemorySnapshot(t *testing.T) {
	snap := initialDarwinMemorySnapshot(func() (uint64, bool) {
		return 12 << 30, true
	})
	if snap.MemUsedBytes != 12<<30 {
		t.Fatalf("MemUsedBytes = %d, want %d", snap.MemUsedBytes, uint64(12<<30))
	}

	snap = initialDarwinMemorySnapshot(func() (uint64, bool) {
		return 0, false
	})
	if snap.MemUsedBytes != 0 {
		t.Fatalf("failed read published %d bytes", snap.MemUsedBytes)
	}
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
	if snap.CPUUtilPct != 80 {
		t.Fatalf("CPUUtilPct = %d, want 80", snap.CPUUtilPct)
	}
	if snap.MemUsedBytes < 2<<30 {
		t.Fatalf("MemUsedBytes = %d, want a refreshed sample", snap.MemUsedBytes)
	}
	if snap.GPU["ioreg:2a"].UtilizationPct != 42 {
		t.Fatalf("GPU snapshot = %+v", snap.GPU)
	}
	if len(snap.GPUInventory) != 1 || snap.GPUInventory[0].Name != "Apple M3 Max" {
		t.Fatalf("GPU inventory = %+v", snap.GPUInventory)
	}

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
	if snap.CPUUtilPct != 0 || snap.MemUsedBytes != 8<<30 {
		t.Fatalf("unexpected partial snapshot: %+v", snap)
	}
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
	if got := c.Snapshot().GPU; len(got) != 0 {
		t.Fatalf("failed GPU read published %+v", got)
	}
	c.collectGPU(context.Background())
	if got := c.Snapshot().GPU["ioreg:2a"].UtilizationPct; got != 77 {
		t.Fatalf("retry utilization = %d, want 77", got)
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
	if partial.GPU["ioreg:2a"].VRAMUsed != 2<<30 || !partial.GPUSampledAt.IsZero() {
		t.Fatalf("pre-utilization reading = %+v, want display data without sample time", partial)
	}

	c.collectGPU(context.Background())
	idle := c.Snapshot()
	if idle.GPU["ioreg:2a"].VRAMUsed != 3<<30 || idle.GPU["ioreg:2a"].UtilizationPct != 0 ||
		idle.GPUSampledAt.IsZero() {
		t.Fatalf("valid idle reading = %+v, want fresh 0%% utilization", idle)
	}

	c.collectGPU(context.Background())
	retained := c.Snapshot()
	if retained.GPU["ioreg:2a"].VRAMUsed != 3<<30 ||
		!retained.GPUSampledAt.Equal(idle.GPUSampledAt) {
		t.Fatalf("missing utilization replaced last valid reading: got %+v want %+v", retained, idle)
	}
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
	if got := c.Snapshot().CPUUtilPct; got != 80 {
		t.Fatalf("CPUUtilPct = %d while GPU reader blocked, want 80", got)
	}

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Stop did not cancel blocked GPU reader")
	}
}

func TestDynamicGPUReadingFromIORegistry(t *testing.T) {
	reading, err := dynamicGPUReadingFromIORegistry([]byte(ioRegistryGPUFixture), 36<<30)
	if err != nil {
		t.Fatalf("dynamicGPUReadingFromIORegistry() error = %v", err)
	}
	apple := reading.stats["ioreg:2a"]
	if apple.VRAMUsed != 8<<30 || apple.UtilizationPct != 100 {
		t.Fatalf("unexpected Apple stats: %+v", apple)
	}
	discrete := reading.stats["ioreg:63"]
	if discrete.VRAMUsed != 2<<30 || discrete.UtilizationPct != 25 {
		t.Fatalf("unexpected discrete stats: %+v", discrete)
	}
	if reading.inventory[0].VramBytes != 36<<30 {
		t.Fatalf("unexpected Apple inventory: %+v", reading.inventory[0])
	}
	if reading.utilizationSamples != 2 {
		t.Fatalf("utilization samples = %d, want 2", reading.utilizationSamples)
	}
}
