// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// tombstonesFile is the durable record of cluster-wide removals. Persisting it
// keeps a removed node from being resurrected by a stale roster gossip after a
// restart (the add/remove race in an eventually-consistent set).
const tombstonesFile = "tombstones.json"

func (m *Manager) tombstonesPath() string {
	return filepath.Join(m.clusterDir, tombstonesFile)
}

// putTombstone records t if it is newer than any tombstone we already hold for
// the same node, returning whether our state changed. Newest-wins keeps the
// merge idempotent across re-delivery.
func (m *Manager) putTombstone(t Tombstone) bool {
	m.tsMu.Lock()
	defer m.tsMu.Unlock()
	old, hadOld := m.tombstones[t.NodeUUID]
	if cur, ok := m.tombstones[t.NodeUUID]; ok {
		if cur.AdmissionEpoch != 0 && t.AdmissionEpoch != 0 {
			if cur.AdmissionEpoch >= t.AdmissionEpoch {
				return false
			}
		} else if cur.RemovedAt >= t.RemovedAt {
			return false
		}
	}
	m.tombstones[t.NodeUUID] = t
	if err := m.persistTombstonesLocked(); err != nil {
		if hadOld {
			m.tombstones[t.NodeUUID] = old
		} else {
			delete(m.tombstones, t.NodeUUID)
		}
		log.Printf("persist tombstone for %s: %v", t.NodeUUID, err)
		return false
	}
	return true
}

// tombstoneFor returns the current tombstone for a node, if any.
func (m *Manager) tombstoneFor(uuid string) (Tombstone, bool) {
	m.tsMu.Lock()
	defer m.tsMu.Unlock()
	t, ok := m.tombstones[uuid]
	return t, ok
}

// clearTombstone drops a tombstone (e.g. a node was legitimately re-invited with
// a newer add than the recorded removal).
func (m *Manager) clearTombstone(uuid string) {
	m.tsMu.Lock()
	defer m.tsMu.Unlock()
	if old, ok := m.tombstones[uuid]; ok {
		delete(m.tombstones, uuid)
		if err := m.persistTombstonesLocked(); err != nil {
			m.tombstones[uuid] = old
			log.Printf("clear tombstone for %s: %v", uuid, err)
		}
	}
}

// snapshotTombstones returns a stable copy for legacy roster compatibility.
// Admission-aware proof retention is supersession-based, never wall-clock TTL.
func (m *Manager) snapshotTombstones() []Tombstone {
	m.tsMu.Lock()
	defer m.tsMu.Unlock()
	out := make([]Tombstone, 0, len(m.tombstones))
	for _, t := range m.tombstones {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeUUID < out[j].NodeUUID })
	return out
}

// clearAllTombstones drops the entire tombstone set (used when leaving a
// cluster — the old cluster's removals are no longer this node's concern).
func (m *Manager) clearAllTombstones() error {
	m.tsMu.Lock()
	defer m.tsMu.Unlock()
	if len(m.tombstones) == 0 {
		return nil
	}
	old := m.tombstones
	m.tombstones = make(map[string]Tombstone)
	if err := m.persistTombstonesLocked(); err != nil {
		m.tombstones = old
		return err
	}
	return nil
}

// gcTombstones is retained as a no-op call site for compatibility. Removal
// evidence is deleted only when a newer authenticated admission supersedes it.
func (m *Manager) gcTombstones() {
}

// persistTombstonesLocked atomically writes the tombstone set. Caller holds tsMu.
func (m *Manager) persistTombstonesLocked() error {
	out := make([]Tombstone, 0, len(m.tombstones))
	for _, t := range m.tombstones {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeUUID < out[j].NodeUUID })
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(m.tombstonesPath(), data, 0o600); err != nil {
		return err
	}
	return nil
}

// loadTombstones restores the legacy tombstone set on startup. A missing file is
// the normal first-run state.
func (m *Manager) loadTombstones() {
	data, err := os.ReadFile(m.tombstonesPath())
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		log.Printf("load tombstones: %v", err)
		return
	}
	var list []Tombstone
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("load tombstones: parse: %v", err)
		return
	}
	m.tsMu.Lock()
	defer m.tsMu.Unlock()
	for _, t := range list {
		m.tombstones[t.NodeUUID] = t
	}
}
