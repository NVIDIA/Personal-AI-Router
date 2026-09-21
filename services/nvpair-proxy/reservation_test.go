// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"

	"nvpair-shared/schedulerwire"
)

// applySnapshot delivers a ranking the way the broker does: with a fresh
// generation, always greater than the last applied.
//
// Generation 0 is rejected rather than defaulted, because an unversioned
// snapshot that cleared reservations without advancing the epoch would let a
// reservation taken before it release one taken after. Tests that care about a
// specific generation set it themselves and this leaves it alone.
func applySnapshot(p *Proxy, priority schedulerwire.Priority) int {
	if priority.Generation == 0 {
		p.priorityMu.RLock()
		priority.Generation = p.appliedPriorityGeneration + 1
		p.priorityMu.RUnlock()
	}
	return p.SetPrioritySnapshot(priority)
}

func reservationCandidates(ids ...string) []candidate {
	out := make([]candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, candidate{id: id})
	}
	return out
}

func reservedID(p *Proxy, candidates []candidate) string {
	candidates = append([]candidate(nil), candidates...)
	ordered, _ := p.reserveCandidate(p.soleFacade(), candidates)
	return ordered[0].id
}

func TestReserveCandidate_ConcurrentEqualLoadHasAtMostOneSkew(t *testing.T) {
	p := prProxy(t)
	ids := []string{"a", "b", "c", "d"}
	ranks := make([]schedulerwire.NodeRank, 0, len(ids))
	for i, id := range ids {
		ranks = append(ranks, schedulerwire.NodeRank{ID: id, Rank: i})
	}
	applySnapshot(p, schedulerwire.Priority{Nodes: ids, Ranks: ranks})
	candidates := reservationCandidates(ids...)

	const requests = 100
	chosen := make(chan string, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chosen <- reservedID(p, candidates)
		}()
	}
	wg.Wait()
	close(chosen)

	counts := make(map[string]int, len(ids))
	for id := range chosen {
		counts[id]++
	}
	min, max := requests, 0
	for _, id := range ids {
		if counts[id] < min {
			min = counts[id]
		}
		if counts[id] > max {
			max = counts[id]
		}
	}
	if max-min > 1 {
		t.Fatalf("100 equal-load reservations are imbalanced: %v", counts)
	}
}

func TestReserveCandidate_ConvergesUnequalPendingDepths(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"a", "b", "c"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "a", Pending: 0, Rank: 0},
			{ID: "b", Pending: 2, Rank: 1},
			{ID: "c", Pending: 4, Rank: 2},
		},
	})
	candidates := reservationCandidates("a", "b", "c")
	assigned := map[string]int{}
	for range 6 {
		assigned[reservedID(p, candidates)]++
	}

	total := map[string]int{
		"a": assigned["a"],
		"b": 2 + assigned["b"],
		"c": 4 + assigned["c"],
	}
	if total["a"] != 4 || total["b"] != 4 || total["c"] != 4 {
		t.Fatalf("unequal depths did not converge: assigned=%v total=%v", assigned, total)
	}
}

func TestReserveCandidate_CombinesPendingPressureAndReservations(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"a", "b", "c"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "a", Pending: 0, GPUPressure: 3},
			{ID: "b", Pending: 1, GPUPressure: 0},
			{ID: "c", Pending: 0, GPUPressure: 2},
		},
	})
	candidates := reservationCandidates("a", "b", "c")
	got := []string{
		reservedID(p, candidates),
		reservedID(p, candidates),
		reservedID(p, candidates),
	}
	want := []string{"b", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GPU-aware reservations = %v, want %v", got, want)
		}
	}
}

func TestSetPrioritySnapshotClampsGPUPressure(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"low", "high"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "low", GPUPressure: -1},
			{ID: "high", GPUPressure: schedulerwire.MaxGPUPressure + 1},
		},
	})
	p.priorityMu.RLock()
	low := p.priorityGPUPressure["low"]
	high := p.priorityGPUPressure["high"]
	p.priorityMu.RUnlock()
	if low != 0 || high != schedulerwire.MaxGPUPressure {
		t.Fatalf("clamped GPU pressure = low:%d high:%d", low, high)
	}
}

// A node/set-priority call can time out after the proxy already applied it, so
// the broker may redeliver a generation. Applying a snapshot clears the
// optimistic reservations — that is its purpose, since the new pending counts
// already include those dispatches — so a redelivery must not clear them a
// second time, or it discards precisely the dispatches the scheduler has not
// seen yet and two concurrent bursts converge on one node.
func TestSetPrioritySnapshot_RedeliveredGenerationKeepsReservations(t *testing.T) {
	p := prProxy(t)
	snapshot := schedulerwire.Priority{
		Generation: 7,
		Nodes:      []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "a", Pending: 0},
			{ID: "b", Pending: 0},
		},
	}
	p.SetPrioritySnapshot(snapshot)

	candidates := reservationCandidates("a", "b")
	first := reservedID(p, candidates)

	// The same generation arriving again is an acknowledged no-op.
	p.SetPrioritySnapshot(snapshot)
	p.priorityMu.RLock()
	kept := p.priorityReservations[first]
	p.priorityMu.RUnlock()
	if kept != 1 {
		t.Fatalf("reservation on %q after redelivery = %d, want 1 (redelivery cleared it)", first, kept)
	}

	// An older generation is stale and must not roll the baseline back either.
	applySnapshot(p, schedulerwire.Priority{Generation: 6, Nodes: []string{"only-stale"}})
	p.priorityMu.RLock()
	order := append([]string(nil), p.priority...)
	p.priorityMu.RUnlock()
	if len(order) != 2 || order[0] != "a" {
		t.Fatalf("priority after a stale generation = %v, want the generation-7 order", order)
	}

	// A newer generation applies and clears, which is the behavior the
	// idempotency must not have broken.
	applySnapshot(p, schedulerwire.Priority{
		Generation: 8,
		Nodes:      []string{"a", "b"},
		Ranks:      []schedulerwire.NodeRank{{ID: "a", Pending: 0}, {ID: "b", Pending: 0}},
	})
	p.priorityMu.RLock()
	cleared := len(p.priorityReservations)
	p.priorityMu.RUnlock()
	if cleared != 0 {
		t.Fatalf("reservations after a newer generation = %d, want 0", cleared)
	}
}

func TestReserveCandidate_LegacyNodesOnlyUsesZeroBaseline(t *testing.T) {
	p := prProxy(t)
	p.SetPriority([]string{"a", "b", "c"})
	candidates := reservationCandidates("a", "b", "c")
	counts := map[string]int{}
	for range 5 {
		counts[reservedID(p, candidates)]++
	}
	want := map[string]int{"a": 2, "b": 2, "c": 1}
	for id, n := range want {
		if counts[id] != n {
			t.Fatalf("legacy nodes-only assignments = %v, want %v", counts, want)
		}
	}
}

func TestReserveCandidate_UsesEligibleCandidates(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"missing", "owner-a", "owner-b", "unknown"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "missing", Pending: 0},
			{ID: "owner-a", Pending: 4},
			{ID: "owner-b", Pending: 5},
			{ID: "unknown", Pending: 0},
		},
	})
	candidates := reservationCandidates("owner-a", "owner-b")

	for range 8 {
		got := reservedID(p, candidates)
		if got != "owner-a" && got != "owner-b" {
			t.Fatalf("reservation escaped eligible candidates to %q", got)
		}
	}
}

func TestReserveCandidate_ManualPinBypassesReservations(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{{ID: "a"}, {ID: "b"}},
	})
	p.soleFacade().SetSelected("b")
	candidates := reservationCandidates("b", "a") // resolveCandidates puts the pin first
	if got := reservedID(p, candidates); got != "b" {
		t.Fatalf("manual pin resolved to %q, want b", got)
	}
	p.priorityMu.RLock()
	defer p.priorityMu.RUnlock()
	if len(p.priorityReservations) != 0 {
		t.Fatalf("manual pin created optimistic reservations: %v", p.priorityReservations)
	}
}

func TestReserveCandidate_IneligibleManualPinDoesNotBypassReservations(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"owner-b", "owner-a"},
		Ranks: []schedulerwire.NodeRank{{ID: "owner-b"}, {ID: "owner-a"}},
	})
	p.soleFacade().SetSelected("missing")
	if got := reservedID(p, reservationCandidates("owner-a", "owner-b")); got != "owner-b" {
		t.Fatalf("reservation with ineligible pin = %q, want owner-b", got)
	}
}

func TestReserveCandidate_PreservesFailoverAndSnapshotReset(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"b", "a", "c"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "b", Pending: 0},
			{ID: "a", Pending: 5},
			{ID: "c", Pending: 6},
		},
	})
	got, _ := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b", "c"))
	want := []string{"b", "a", "c"}
	for i, id := range want {
		if got[i].id != id {
			t.Fatalf("reserved failover order = %v, want %v", candidateIDsFrom(got), want)
		}
	}

	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"a", "b", "c"},
		Ranks: []schedulerwire.NodeRank{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	})
	if next := reservedID(p, reservationCandidates("a", "b", "c")); next != "a" {
		t.Fatalf("new snapshot did not reset reservations: next = %q, want a", next)
	}
}

func candidateIDsFrom(candidates []candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.id)
	}
	return out
}

// flatPriority makes every listed node equally loaded, so reservations are the
// only thing that can break the tie and dispatch is observable.
func flatPriority(p *Proxy, generation uint64, ids ...string) {
	ranks := make([]schedulerwire.NodeRank, 0, len(ids))
	for _, id := range ids {
		ranks = append(ranks, schedulerwire.NodeRank{ID: id})
	}
	applySnapshot(p, schedulerwire.Priority{
		Generation: generation,
		Nodes:      ids,
		Ranks:      ranks,
	})
}

func reservationCount(p *Proxy, id string) int {
	p.priorityMu.RLock()
	defer p.priorityMu.RUnlock()
	return p.priorityReservations[id]
}

// A reservation is a claim on capacity for one in-flight request. Releasing it
// when the request ends is what keeps a node from looking loaded until the next
// snapshot: without it a steady trickle of short requests drives dispatch away
// from a node that is actually idle.
func TestReleasedReservationStopsCountingAsLoad(t *testing.T) {
	p := prProxy(t)
	flatPriority(p, 1, "a", "b")

	_, held := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))
	if !held.held {
		t.Fatal("no reservation was taken on a flat snapshot")
	}
	if got := reservationCount(p, held.nodeID); got != 1 {
		t.Fatalf("reservation count for %q = %d, want 1", held.nodeID, got)
	}

	p.releaseReservation(held)
	if got := reservationCount(p, held.nodeID); got != 0 {
		t.Fatalf("reservation count for %q after release = %d, want 0", held.nodeID, got)
	}
	// The entry is deleted rather than left at zero, so the map cannot grow one
	// key per node ever dispatched to.
	p.priorityMu.RLock()
	_, present := p.priorityReservations[held.nodeID]
	p.priorityMu.RUnlock()
	if present {
		t.Errorf("released reservation left a zero entry for %q", held.nodeID)
	}
}

// A snapshot supersedes every reservation taken before it, because its pending
// counts already include that work. A late release must therefore be dropped:
// applying it would double-count the completion and leave the node reading as
// permanently idle.
func TestReleaseFromBeforeASnapshotIsIgnored(t *testing.T) {
	p := prProxy(t)
	flatPriority(p, 1, "a", "b")

	_, stale := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))
	if !stale.held {
		t.Fatal("no reservation was taken")
	}

	// A new snapshot arrives, resetting reservations, and another request
	// reserves against it.
	flatPriority(p, 2, "a", "b")
	_, fresh := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))
	freshCount := reservationCount(p, fresh.nodeID)

	// The first request now finishes.
	p.releaseReservation(stale)

	if got := reservationCount(p, fresh.nodeID); got != freshCount {
		t.Fatalf("a release from generation %d changed the generation-%d count for %q: %d, want %d",
			stale.generation, fresh.generation, fresh.nodeID, got, freshCount)
	}
	for _, id := range []string{"a", "b"} {
		if got := reservationCount(p, id); got < 0 {
			t.Fatalf("reservation count for %q went negative: %d", id, got)
		}
	}
}

// Failover moves the claim to the node that actually served. The node that
// refused the request is not doing the work, so leaving its reservation in
// place would steer later dispatch away from it for no reason.
func TestFailoverMovesTheReservationToTheServingNode(t *testing.T) {
	p := prProxy(t)
	flatPriority(p, 1, "a", "b")

	_, held := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))
	from := held.nodeID
	to := "a"
	if from == to {
		to = "b"
	}

	moved := p.moveReservation(held, to)
	if moved.nodeID != to {
		t.Fatalf("moved reservation node = %q, want %q", moved.nodeID, to)
	}
	if got := reservationCount(p, from); got != 0 {
		t.Errorf("refusing node %q still holds %d reservations", from, got)
	}
	if got := reservationCount(p, to); got != 1 {
		t.Errorf("serving node %q holds %d reservations, want 1", to, got)
	}

	// Releasing the moved token clears the serving node, not the original.
	p.releaseReservation(moved)
	if got := reservationCount(p, to); got != 0 {
		t.Errorf("serving node %q still holds %d reservations after release", to, got)
	}
}

// Two facades in one process compete for the same GPU, so a dispatch through
// either has to be visible to the other. That is the whole reason the map is
// process-wide rather than per facade.
func TestReservationsAreSharedAcrossFacades(t *testing.T) {
	p := prProxy(t)
	flatPriority(p, 1, "a", "b")

	// Same host, so this stands in for a second facade's request.
	_, first := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))
	_, second := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))

	if first.nodeID == second.nodeID {
		t.Fatalf("both dispatches chose %q; the second did not see the first's reservation", first.nodeID)
	}
	for _, r := range []reservation{first, second} {
		if got := reservationCount(p, r.nodeID); got != 1 {
			t.Errorf("node %q holds %d reservations, want 1", r.nodeID, got)
		}
	}
}
