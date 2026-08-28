// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestDedupSeenOrAdd(t *testing.T) {
	d := newDedupIndex(8)

	if d.seenOrAdd("a") {
		t.Fatal("first sighting of a should be new")
	}
	if !d.seenOrAdd("a") {
		t.Fatal("second sighting of a should be a duplicate")
	}
	if d.seenOrAdd("b") {
		t.Fatal("first sighting of b should be new")
	}
}

func TestDedupEviction(t *testing.T) {
	d := newDedupIndex(2)

	d.seenOrAdd("a") // {a}
	d.seenOrAdd("b") // {a,b}
	d.seenOrAdd("c") // evicts a -> {b,c}

	if d.seenOrAdd("a") {
		t.Fatal("a should have been evicted and read as new")
	}
}

func TestDedupKeysDistinguishStateAndKind(t *testing.T) {
	d := newDedupIndex(8)

	w := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateQueued}
	wRunning := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateRunning}

	if d.seenOrAdd(keyLifecycle(w)) {
		t.Fatal("queued should be new")
	}
	if d.seenOrAdd(keyLifecycle(wRunning)) {
		t.Fatal("running for same id is a different key, should be new")
	}
	if !d.seenOrAdd(keyLifecycle(w)) {
		t.Fatal("repeat queued should dedup")
	}
	// A removal keyed on the same id must not collide with a lifecycle key.
	if d.seenOrAdd(keyRemove("node-A", "wl-1")) {
		t.Fatal("removal of wl-1 must not collide with lifecycle keys")
	}
}

// TestDedupDistinguishesNodes guards the cross-node collision: Workload.id is
// only unique per node (spec §11), so the same id+state from two different
// nodes must be treated as two distinct workloads, never deduplicated against
// each other.
func TestDedupDistinguishesNodes(t *testing.T) {
	d := newDedupIndex(8)

	nodeA := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateQueued}
	nodeB := &Workload{ID: "wl-1", OriginatedFrom: "node-B", State: StateQueued}

	if d.seenOrAdd(keyLifecycle(nodeA)) {
		t.Fatal("node-A wl-1 should be new")
	}
	if d.seenOrAdd(keyLifecycle(nodeB)) {
		t.Fatal("node-B wl-1 has the same id but a different node, must not dedup against node-A")
	}
	if !d.seenOrAdd(keyLifecycle(nodeA)) {
		t.Fatal("repeat of node-A wl-1 should dedup")
	}
}

// TestDedupDistinguishesEngineAndRun guards the identity fix: id "1" is reused
// by the two engine proxies (each counts from 1) and after a restart (new
// runId). The dedup must treat those as distinct workloads, not collapse them.
func TestDedupDistinguishesEngineAndRun(t *testing.T) {
	d := newDedupIndex(8)
	ollama := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1", State: StateRunning}
	lmstudio := &Workload{ID: "1", OriginatedFrom: "host", Engine: "lmstudio", RunID: "r2", State: StateRunning}
	restarted := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r3", State: StateRunning}

	if d.seenOrAdd(keyLifecycle(ollama)) {
		t.Fatal("ollama host/1 should be new")
	}
	if d.seenOrAdd(keyLifecycle(lmstudio)) {
		t.Fatal("lmstudio host/1 shares the id but a different engine; must not dedup")
	}
	if d.seenOrAdd(keyLifecycle(restarted)) {
		t.Fatal("a reused id from a new run must not dedup against the old run")
	}
	if !d.seenOrAdd(keyLifecycle(ollama)) {
		t.Fatal("repeat of ollama host/1 should dedup")
	}
}

// TestDedupRemovalDistinguishesNodes mirrors TestDedupDistinguishesNodes for
// the removal path: the same workloadId removed on two nodes must be two
// distinct dedup entries.
func TestDedupRemovalDistinguishesNodes(t *testing.T) {
	d := newDedupIndex(8)

	if d.seenOrAdd(keyRemove("node-A", "wl-1")) {
		t.Fatal("removal of node-A wl-1 should be new")
	}
	if d.seenOrAdd(keyRemove("node-B", "wl-1")) {
		t.Fatal("removal of node-B wl-1 must not dedup against node-A")
	}
	if !d.seenOrAdd(keyRemove("node-A", "wl-1")) {
		t.Fatal("repeat removal of node-A wl-1 should dedup")
	}
}
