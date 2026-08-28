// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
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
	if code := post(normal); code != http.StatusOK {
		t.Fatalf("normal post = %d, want 200", code)
	}
	if emitCount() != 1 {
		t.Fatalf("emits after first = %d, want 1", emitCount())
	}
	// Same frame again: deduped, not re-emitted.
	if code := post(normal); code != http.StatusOK || emitCount() != 1 {
		t.Fatalf("repeat post: code=%d emits=%d, want 200/1 (deduped)", code, emitCount())
	}
	// Re-sync frame (same key + state): bypasses dedup, reaches the broker.
	resync, _ := json.Marshal(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: mustJSON(map[string]json.RawMessage{"workloadInfo": wiRaw, "resync": json.RawMessage("true")})})
	if code := post(resync); code != http.StatusOK {
		t.Fatalf("resync post = %d, want 200", code)
	}
	if emitCount() != 2 {
		t.Fatalf("emits after resync = %d, want 2 (resync must bypass dedup)", emitCount())
	}
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
	if got := len(m.activeSnapshot()); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}

	// A terminal is retained (for re-sync redundancy), not dropped.
	m.trackActive(k1, MethodErrored, lifecycleFrame("1", "failed", MethodErrored, "node-a"), StateFailed)
	if got := len(m.activeSnapshot()); got != 2 {
		t.Fatalf("count after terminal = %d, want 2 (terminal retained)", got)
	}

	// A removal drops immediately (matches on origin+id).
	m.untrackActive("node-a", "2")
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("count after removal = %d, want 1", got)
	}

	// Once a terminal's retention expires, the snapshot prunes it.
	m.activeMu.Lock()
	e := m.activeLocal[k1]
	e.expiresAt = time.Now().Add(-time.Minute)
	m.activeLocal[k1] = e
	m.activeMu.Unlock()
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("count after terminal expiry = %d, want 0", got)
	}
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
	if got := len(m.activeSnapshot()); got != 3 {
		t.Fatalf("count = %d, want 3 (engine + runId keep same-id workloads distinct)", got)
	}

	m.untrackActive("a", "1")
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("count after removal = %d, want 0 (removal drops every composite key for the pair)", got)
	}
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
	if len(snap) != 1 {
		t.Fatalf("count = %d, want 1", len(snap))
	}
	if !snap[0].terminal {
		t.Fatal("a stale running overwrote the terminal in the re-sync set")
	}
	if snap[0].expiresAt.IsZero() {
		t.Fatal("terminal event lost its expiry (a running with no expiry overwrote it)")
	}
}
