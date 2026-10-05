// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lifecycleFrame(id, state, method, origin string) json.RawMessage {
	wl := &Workload{ID: id, Model: "m", Engine: "ollama", State: WorkloadState(state), OriginatedFrom: origin, CreatedAt: 1}
	p, _ := json.Marshal(lifecycleParams{WorkloadInfo: wl})
	return json.RawMessage(p)
}

// TestResyncBypassesReceiverDedup drives the full HTTP receiver path: a normal
// re-broadcast of an already-seen (…,state) frame is deduped and not re-emitted,
// but a re-sync frame (params carry resync:true) bypasses dedup and reaches the
// broker so the store can reconcile.
func TestResyncBypassesReceiverDedup(t *testing.T) {
	var mu sync.Mutex
	var emits int
	self, peer := newPinnedPeerMeshes(t)
	srv := NewServer(0, newDedupIndex(64), self,
		func(*Workload) error { mu.Lock(); emits++; mu.Unlock(); return nil },
		func(string, string) error { return nil })
	post := serveEventsOverMTLS(t, srv, self, peer)
	emitCount := func() int { mu.Lock(); defer mu.Unlock(); return emits }

	wl := &Workload{ID: "1", Model: "m", Engine: "ollama", RunID: "r1", State: StateRunning, OriginatedFrom: "a", CreatedAt: 1}
	wiRaw, _ := json.Marshal(wl)

	// Normal frame: emitted once.
	normal, _ := json.Marshal(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: mustJSON(map[string]json.RawMessage{"workloadInfo": wiRaw})})
	assert.Equal(t, http.StatusOK, post(normal))
	assert.Equal(t, 1, emitCount(), "emits after first post")
	// Same frame again: deduped, not re-emitted.
	assert.Equal(t, http.StatusOK, post(normal))
	assert.Equal(t, 1, emitCount(), "repeat post must dedup")
	// Re-sync frame (same key + state): bypasses dedup, reaches the broker.
	resync, _ := json.Marshal(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: mustJSON(map[string]json.RawMessage{"workloadInfo": wiRaw, "resync": json.RawMessage("true")})})
	assert.Equal(t, http.StatusOK, post(resync))
	assert.Equal(t, 2, emitCount(), "resync must bypass dedup")
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TestActiveSnapshotTracking checks the re-sync set: active workloads are
// retained, a terminal is retained (for a couple of heartbeats) then pruned once
// expired, and a removal drops immediately.
func TestActiveSnapshotTracking(t *testing.T) {
	m := &Manager{activeLocal: make(map[workloadKey]workloadEvent)}
	k1 := workloadKey{origin: "node-a", id: "1"}
	k2 := workloadKey{origin: "node-a", id: "2"}

	m.trackActive(k1, MethodStarted, lifecycleFrame("1", "running", MethodStarted, "node-a"), StateRunning)
	m.trackActive(k2, MethodStarted, lifecycleFrame("2", "running", MethodStarted, "node-a"), StateRunning)
	assert.Len(t, m.activeSnapshot(), 2)

	// A terminal is retained (for re-sync redundancy), not dropped.
	m.trackActive(k1, MethodErrored, lifecycleFrame("1", "failed", MethodErrored, "node-a"), StateFailed)
	assert.Len(t, m.activeSnapshot(), 2, "terminal must be retained")

	// A removal drops immediately (matches on origin+id).
	m.untrackActive("node-a", "2")
	assert.Len(t, m.activeSnapshot(), 1, "removal must drop immediately")

	// Once a terminal's retention expires, the snapshot prunes it.
	m.activeMu.Lock()
	e := m.activeLocal[k1]
	e.expiresAt = time.Now().Add(-time.Minute)
	m.activeLocal[k1] = e
	m.activeMu.Unlock()
	assert.Empty(t, m.activeSnapshot(), "expired terminal must be pruned")
}

// TestActiveSnapshotDistinguishesEngineAndRun: the re-sync set keys on engine +
// runId, so concurrent cross-engine jobs and a reused id after a restart are all
// retained (not collapsed), and a removal drops every composite key sharing
// (origin, id).
func TestActiveSnapshotDistinguishesEngineAndRun(t *testing.T) {
	m := &Manager{activeLocal: make(map[workloadKey]workloadEvent)}

	m.trackActive(workloadKey{origin: "a", engine: "ollama", runID: "r1", id: "1"}, MethodStarted, lifecycleFrame("1", "running", MethodStarted, "a"), StateRunning)
	m.trackActive(workloadKey{origin: "a", engine: "lmstudio", runID: "r2", id: "1"}, MethodStarted, lifecycleFrame("1", "running", MethodStarted, "a"), StateRunning)
	m.trackActive(workloadKey{origin: "a", engine: "ollama", runID: "r3", id: "1"}, MethodStarted, lifecycleFrame("1", "running", MethodStarted, "a"), StateRunning)
	assert.Len(t, m.activeSnapshot(), 3, "engine and runId must keep workloads distinct")

	m.untrackActive("a", "1")
	assert.Empty(t, m.activeSnapshot(), "removal must drop every composite key for the pair")
}

// TestTrackActiveMonotonicTerminal: once an identity is terminal in the re-sync
// set, a later non-terminal event for that same identity is dropped. A stale or
// replayed "running" (e.g. a rehydration/heartbeat frame racing a terminal)
// must not resurrect a finished job, nor strip its expiry so it never ages out.
func TestTrackActiveMonotonicTerminal(t *testing.T) {
	m := &Manager{activeLocal: make(map[workloadKey]workloadEvent)}
	k := workloadKey{origin: "a", engine: "ollama", runID: "r1", id: "1"}

	m.trackActive(k, MethodCompleted, lifecycleFrame("1", "completed", MethodCompleted, "a"), StateCompleted)
	m.trackActive(k, MethodStarted, lifecycleFrame("1", "running", MethodStarted, "a"), StateRunning)

	snap := m.activeSnapshot()
	require.Len(t, snap, 1)
	assert.True(t, snap[0].terminal, "a stale running must not overwrite the terminal")
	assert.False(t, snap[0].expiresAt.IsZero(), "terminal must retain its expiry")
}
