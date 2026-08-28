// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
)

// workload is the subset of the workload-manager Workload object (WM spec §6)
// the scheduler reads. Other fields are ignored.
type workload struct {
	ID             string `json:"id"`
	Engine         string `json:"engine"`
	RunID          string `json:"runId"`
	State          string `json:"state"`
	OriginatedFrom string `json:"originatedFrom"`
	ScheduledOn    string `json:"scheduledOn"`
}

// workloadParams is the envelope for workloads:upsert (a single workloadInfo).
type workloadParams struct {
	WorkloadInfo workload `json:"workloadInfo"`
}

// removeParams is the envelope for workloads:remove.
type removeParams struct {
	WorkloadID     string `json:"workloadId"`
	OriginatedFrom string `json:"originatedFrom"`
}

// availableNode mirrors the broker's discovery:* node shape. Ranking keys on
// the stable per-host UUID (hostUuid) so a node's identity — and the
// scheduledOn attribution matched against it — survives a PC rename.
// Every node the broker publishes carries a hostUuid (manual nodes learn theirs
// from node-info), so there is no id fallback.
type availableNode struct {
	ID       string `json:"id"`
	HostUUID string `json:"hostUuid"`
}

// key is the node's operational identity: its hostUuid, matching the value
// proxies stamp as scheduledOn (see subscribedToNode).
func (n availableNode) key() string {
	return n.HostUUID
}

// wlKey uniquely identifies a workload across the cluster — the same global
// identity the broker's store and the workload-manager use. The bare
// (originatedFrom, id) isn't unique: each engine's request counter starts at 1
// and resets on a proxy restart, so two concurrent cross-engine jobs (and
// same-id jobs across restarts) would collide and drop one from the pending
// counts. engine + the proxy's per-process runId keep them distinct.
type wlKey struct {
	origin string
	engine string
	runID  string
	id     string
}

// applyNodesChanged replaces the node universe with the broker's latest
// discovery snapshot (a bare array of nodes). It reports whether the set
// changed, so callers can skip no-op recomputes.
func (m *Manager) applyNodesChanged(params json.RawMessage) bool {
	var nodes []availableNode
	if err := json.Unmarshal(params, &nodes); err != nil {
		slog.Warn("bad discovery:nodes-changed payload", "err", err)
		return false
	}
	set := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if k := n.key(); k != "" {
			set[k] = true
		}
	}
	m.mu.Lock()
	changed := !sameNodeSet(m.nodes, set)
	m.nodes = set
	for hostUUID := range m.telemetry {
		if !set[hostUUID] {
			delete(m.telemetry, hostUUID)
		}
	}
	m.mu.Unlock()
	slog.Debug("node universe updated", "count", len(set))
	return changed
}

// applyUpsert records active work and removes terminal work. It reports whether
// the node-wide pending counts changed; queued->running on the same node is a
// catalog update but not a scheduling change.
func (m *Manager) applyUpsert(params json.RawMessage) bool {
	var p workloadParams
	if err := json.Unmarshal(params, &p); err != nil {
		slog.Warn("bad workloads:upsert payload", "err", err)
		return false
	}
	w := p.WorkloadInfo
	if w.ID == "" {
		return false
	}
	key := wlKey{origin: w.OriginatedFrom, engine: w.Engine, runID: w.RunID, id: w.ID}

	m.mu.Lock()
	defer m.mu.Unlock()

	previous, existed := m.catalog[key]
	previousNode := pendingNode(previous)
	if !isPending(w) {
		delete(m.catalog, key)
		return existed && previousNode != ""
	}

	m.catalog[key] = w
	return previousNode != w.ScheduledOn
}

// applyRemove drops a workload from the catalog. The removal wire carries only
// (workloadId, originatedFrom) — no engine/runId — so it drops every composite
// key matching that pair (mirroring the broker store's Remove). It reports
// whether any removed workload contributed to pending load.
func (m *Manager) applyRemove(params json.RawMessage) bool {
	var p removeParams
	if err := json.Unmarshal(params, &p); err != nil {
		slog.Warn("bad workloads:remove payload", "err", err)
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	changed := false
	for k := range m.catalog {
		if k.origin == p.OriginatedFrom && k.id == p.WorkloadID {
			if pendingNode(m.catalog[k]) != "" {
				changed = true
			}
			delete(m.catalog, k)
		}
	}
	return changed
}

func isPending(w workload) bool {
	return w.State == "queued" || w.State == "running"
}

func pendingNode(w workload) string {
	if !isPending(w) {
		return ""
	}
	return w.ScheduledOn
}

func sameNodeSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}
