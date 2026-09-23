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

// TestDedupDistinguishesPlacement guards the re-point: a failover or a retry
// moves a workload to another node while its state stays "running", so the two
// events differ on scheduledOn and nothing else. With scheduledOn out of the
// key the second was dropped as a duplicate, and every peer went on showing
// the job on the node it was first sent to for the rest of its life. A repeat
// of the same placement must still dedup, which is what keeps a broadcast
// retry from being re-applied.
func TestDedupDistinguishesPlacement(t *testing.T) {
	d := newDedupIndex(8)

	first := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1", State: StateRunning, ScheduledOn: "node-A"}
	repointed := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1", State: StateRunning, ScheduledOn: "node-B"}

	if d.seenOrAdd(keyLifecycle(first)) {
		t.Fatal("first placement on node-A should be new")
	}
	if d.seenOrAdd(keyLifecycle(repointed)) {
		t.Fatal("a re-point to node-B differs only in scheduledOn and must not dedup against node-A")
	}
	if !d.seenOrAdd(keyLifecycle(first)) {
		t.Fatal("a resent frame for the node-A placement should still dedup")
	}
}

// TestDedupDistinguishesRepeatedPlacements is the reason the key carries the
// producer's event sequence rather than only the workload's current shape.
//
// This index is a permanent set, so any shape-derived key collides as soon as a
// workload revisits a shape it already had — and the retry loop does that
// routinely: queued on A, placement cleared between attempts, then queued on A
// again. Keyed on shape alone the third event matched the first, so every peer
// dropped it and their brokers kept the interim unplaced record while the job
// was really running on A, which took it out of A's pending load.
//
// The redelivery case still has to dedup, because that is what stops an
// out-of-order broadcast retry from reverting a placement.
func TestDedupDistinguishesRepeatedPlacements(t *testing.T) {
	d := newDedupIndex(8)

	base := func(seq int64, node string) *Workload {
		return &Workload{
			ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1",
			State: StateQueued, ScheduledOn: node, Seq: seq,
		}
	}

	onA := base(1, "node-A")
	cleared := base(2, "")
	backOnA := base(3, "node-A")

	if d.seenOrAdd(keyLifecycle(onA)) {
		t.Fatal("first placement on node-A should be new")
	}
	if d.seenOrAdd(keyLifecycle(cleared)) {
		t.Fatal("clearing the placement between attempts should be new")
	}
	if d.seenOrAdd(keyLifecycle(backOnA)) {
		t.Fatal("re-dispatching to node-A repeats an earlier shape and must NOT dedup against it")
	}
	// A redelivery of any of those frames carries its original sequence, so it
	// is still recognised as one.
	if !d.seenOrAdd(keyLifecycle(base(1, "node-A"))) {
		t.Fatal("a resent frame for the first placement should still dedup")
	}
	if !d.seenOrAdd(keyLifecycle(base(3, "node-A"))) {
		t.Fatal("a resent frame for the third event should still dedup")
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
