// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"nvpair-shared/appdir"
)

// entriesFile is the on-disk file name of the service-owned durable
// manual-node list, inside the shared app data dir (appdir).
const entriesFile = "manual-nodes.json"

// entriesFileShape is the on-disk shape of the durable list.
type entriesFileShape struct {
	Entries []ManualEntry `json:"entries"`
}

func entriesPath() (string, error) { return appdir.Path(entriesFile) }

// loadEntries returns the persisted entries. A missing file reports nil, nil
// (first run); a corrupt file reports an error the caller logs and ignores
// (the in-memory list stays authoritative for this run).
func loadEntries() ([]ManualEntry, error) {
	path, err := entriesPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f entriesFileShape
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return f.Entries, nil
}

// saveEntries atomically writes the full entry set (tmp + rename) so a crash
// mid-write can't leave a truncated file behind.
func saveEntries(entries []ManualEntry) error {
	path, err := entriesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entriesFileShape{Entries: entries})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// currentEntries returns the tracked entries sorted by node id so the
// persisted file is deterministic across saves.
func (m *Manager) currentEntries() []ManualEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ManualEntry, 0, len(m.nodes))
	for _, tn := range m.nodes {
		out = append(out, tn.entry)
	}
	sort.Slice(out, func(i, j int) bool { return nodeID(out[i]) < nodeID(out[j]) })
	return out
}

// loadPersistedEntries restores the durable list into memory at startup, so
// manual nodes added through any frontend survive a restart of this service.
// Each restored entry is probed and announced exactly like a fresh node/add.
func (m *Manager) loadPersistedEntries() {
	entries, err := loadEntries()
	if err != nil {
		slog.Warn("failed to load persisted manual entries", "err", err)
		return
	}
	for _, e := range entries {
		m.addNode(e)
	}
	if len(entries) > 0 {
		slog.Info("manual entries restored", "count", len(entries))
	}
}

// saveNow persists the current entry set; a failed save means an entry won't
// survive the next restart, which the operator can re-add — log, don't fail.
func (m *Manager) saveNow() {
	if err := saveEntries(m.currentEntries()); err != nil {
		slog.Warn("failed to save manual entries", "err", err)
	}
}
