// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"nvpair-shared/schedulerwire"
)

// schedulerEngines is the fixed set of engine-specific output contracts. Both
// receive the same node-wide ranking because their work shares node resources.
var schedulerEngines = []string{"ollama", "lmstudio"}

// NodeRank is retained as the scheduler's public status type while the wire
// definition is shared with the broker and proxies.
type NodeRank = schedulerwire.NodeRank

// schedulePriorityParams preserves the local name used by existing scheduler
// tests while the payload itself is now shared across processes.
type schedulePriorityParams = schedulerwire.EnginePriority

// EngineSchedule is the per-engine view surfaced by scheduler:get-status.
type EngineSchedule struct {
	Engine        string     `json:"engine"`
	Emitted       []NodeRank `json:"emitted"`
	LastEmittedAt int64      `json:"lastEmittedAt"`
}

// engineState is the last rank snapshot emitted for an engine, kept so the
// scheduler emits only when its order, pending counts, or GPU pressure changes.
type engineState struct {
	ranks         []NodeRank
	lastEmittedAt int64
}

// scheduleLoop provides periodic reconciliation until ctx is cancelled. Workload
// and discovery events recompute directly; this loop also services forced ticks.
// It reads the mutable interval each iteration so scheduler:set-interval takes
// effect on the next periodic pass.
func (m *Manager) scheduleLoop(ctx context.Context) {
	for {
		m.mu.Lock()
		d := m.interval
		m.mu.Unlock()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.tickCh:
			timer.Stop()
			m.recomputeAll(true)
		case <-timer.C:
			m.recomputeAll(false)
		}
	}
}

func (m *Manager) recomputeAll(force bool) {
	m.recomputeMu.Lock()
	defer m.recomputeMu.Unlock()

	order, ranks := m.rank()
	for _, e := range schedulerEngines {
		m.emitIfChanged(e, order, ranks, force)
	}
}

// rank computes the workload and GPU-pressure node-wide order (spec §7.2).
func (m *Manager) rank() ([]string, []NodeRank) {
	return m.rankAt(time.Now())
}

// rankAt exists so freshness behavior can be tested without wall-clock sleeps.
// Pending includes queued+running workloads from every engine scheduledOn a
// discovered node. Nodes sort by pending+pressure, then pressure, then stable id.
func (m *Manager) rankAt(now time.Time) ([]string, []NodeRank) {
	m.mu.Lock()
	pending := make(map[string]int, len(m.nodes))
	for id := range m.nodes {
		pending[id] = 0
	}
	for _, w := range m.catalog {
		if !isPending(w) {
			continue
		}
		if w.ScheduledOn == "" {
			continue // unplaced work counts toward no node
		}
		if _, ok := m.nodes[w.ScheduledOn]; ok {
			pending[w.ScheduledOn]++
		}
	}
	ranks := make([]NodeRank, 0, len(m.nodes))
	for id := range m.nodes {
		state, ok := m.telemetry[id]
		ranks = append(ranks, NodeRank{
			ID:          id,
			Pending:     pending[id],
			GPUPressure: effectiveGPUPressure(state, ok, now),
		})
	}
	m.mu.Unlock()

	sort.Slice(ranks, func(i, j int) bool {
		leftLoad := ranks[i].Pending + ranks[i].GPUPressure
		rightLoad := ranks[j].Pending + ranks[j].GPUPressure
		if leftLoad != rightLoad {
			return leftLoad < rightLoad
		}
		if ranks[i].GPUPressure != ranks[j].GPUPressure {
			return ranks[i].GPUPressure < ranks[j].GPUPressure
		}
		return ranks[i].ID < ranks[j].ID
	})
	order := make([]string, len(ranks))
	for i := range ranks {
		ranks[i].Rank = i
		order[i] = ranks[i].ID
	}
	return order, ranks
}

// emitIfChanged publishes a shared ranking through one engine output only when
// its order, pending counts, or pressure differ from the last emission (or when
// forced).
// The notify happens outside the state lock so a slow write never stalls
// ingestion.
func (m *Manager) emitIfChanged(engine string, order []string, ranks []NodeRank, force bool) {
	m.mu.Lock()
	prev := m.emitted[engine]
	changed := force || !equalRanks(prev.ranks, ranks)
	if changed {
		m.emitted[engine] = engineState{ranks: ranks, lastEmittedAt: time.Now().UnixMilli()}
	}
	m.mu.Unlock()

	if !changed {
		return
	}
	payload := schedulerwire.EnginePriority{Engine: engine, Nodes: order, Ranks: ranks}
	if err := m.codec.Notify("schedule:priority", payload); err != nil {
		slog.Warn("emit schedule:priority failed", "engine", engine, "err", err)
		return
	}
	slog.Info("emitted priority", "engine", engine, "nodes", order)
}

// status snapshots the scheduler state for scheduler:get-status.
func (m *Manager) status() statusResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	engines := make(map[string]EngineSchedule, len(schedulerEngines))
	for _, e := range schedulerEngines {
		st := m.emitted[e]
		emitted := st.ranks
		if emitted == nil {
			emitted = []NodeRank{}
		}
		engines[e] = EngineSchedule{Engine: e, Emitted: emitted, LastEmittedAt: st.lastEmittedAt}
	}
	return statusResult{IntervalMs: m.interval.Milliseconds(), Engines: engines}
}

// statusResult is the scheduler:get-status response shape.
type statusResult struct {
	IntervalMs int64                     `json:"interval_ms"`
	Engines    map[string]EngineSchedule `json:"engines"`
}

func equalRanks(a, b []NodeRank) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
