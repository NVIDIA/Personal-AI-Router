// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.LessOrEqual(t, max-min, 1, "100 equal-load reservations are imbalanced (%v)", counts)
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
	require.Equal(t, map[string]int{"a": 4, "b": 4, "c": 4}, total, "unequal depths did not converge: assigned %v", assigned)
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
	require.Equal(t, []string{"b", "b", "c"}, got, "GPU-aware reservations")
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
	require.Equal(t, 0, low, "clamped GPU pressure = low (%v, %v)", low, high)
	require.Equal(t, schedulerwire.MaxGPUPressure, high, "clamped GPU pressure = low (%v, %v)", low, high)
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
	require.Equal(t, 1, kept, "reservation on (%v, %v)", first, kept)

	// An older generation is stale and must not roll the baseline back either.
	applySnapshot(p, schedulerwire.Priority{Generation: 6, Nodes: []string{"only-stale"}})
	p.priorityMu.RLock()
	order := append([]string(nil), p.priority...)
	p.priorityMu.RUnlock()
	require.Len(t, order, 2, "priority after a stale generation")
	require.Equal(t, "a", order[0], "priority after a stale generation (%v)", order)

	// A newer generation applies and clears, which is the behavior the
	// idempotency must not have broken.
	applySnapshot(p, schedulerwire.Priority{
		Generation: 8,
		Nodes:      []string{"a", "b"},
		Ranks:      []schedulerwire.NodeRank{{ID: "a", Pending: 0}, {ID: "b", Pending: 0}},
	})
	p.priorityMu.RLock()
	assert.Empty(t, p.priorityReservations, "reservations after a newer generation")
	p.priorityMu.RUnlock()
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
		require.Equal(t, n, counts[id], "legacy nodes-only assignments (%v, %v)", counts, want)
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
		require.Contains(t, []string{"owner-a", "owner-b"}, got, "reservation escaped eligible candidates")
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
	{
		got := reservedID(p, candidates)
		require.Equal(t, "b", got, "manual pin resolved to")
	}
	p.priorityMu.RLock()
	defer p.priorityMu.RUnlock()
	require.Empty(t, p.priorityReservations, "manual pin created optimistic reservations")
}

func TestReserveCandidate_IneligibleManualPinDoesNotBypassReservations(t *testing.T) {
	p := prProxy(t)
	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"owner-b", "owner-a"},
		Ranks: []schedulerwire.NodeRank{{ID: "owner-b"}, {ID: "owner-a"}},
	})
	p.soleFacade().SetSelected("missing")
	{
		got := reservedID(p, reservationCandidates("owner-a", "owner-b"))
		require.Equal(t, "owner-b", got, "reservation with ineligible pin")
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
	require.Equal(t, []string{"b", "a", "c"}, candidateIDsFrom(got), "reserved failover order")

	applySnapshot(p, schedulerwire.Priority{
		Nodes: []string{"a", "b", "c"},
		Ranks: []schedulerwire.NodeRank{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	})
	{
		next := reservedID(p, reservationCandidates("a", "b", "c"))
		require.Equal(t, "a", next, "new snapshot did not reset reservations: next")
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
	require.True(t, held.held, "no reservation was taken on a flat snapshot")
	{
		got := reservationCount(p, held.nodeID)
		require.Equal(t, 1, got, "reservation count for")
	}

	p.releaseReservation(held)
	{
		got := reservationCount(p, held.nodeID)
		require.Equal(t, 0, got, "reservation count for")
	}
	// The entry is deleted rather than left at zero, so the map cannot grow one
	// key per node ever dispatched to.
	p.priorityMu.RLock()
	assert.NotContains(t, p.priorityReservations, held.nodeID, "released reservation left a zero entry for")
	p.priorityMu.RUnlock()
}

// A snapshot supersedes every reservation taken before it, because its pending
// counts already include that work. A late release must therefore be dropped:
// applying it would double-count the completion and leave the node reading as
// permanently idle.
func TestReleaseFromBeforeASnapshotIsIgnored(t *testing.T) {
	p := prProxy(t)
	flatPriority(p, 1, "a", "b")

	_, stale := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))
	require.True(t, stale.held, "no reservation was taken")

	// A new snapshot arrives, resetting reservations, and another request
	// reserves against it.
	flatPriority(p, 2, "a", "b")
	_, fresh := p.reserveCandidate(p.soleFacade(), reservationCandidates("a", "b"))
	freshCount := reservationCount(p, fresh.nodeID)

	// The first request now finishes.
	p.releaseReservation(stale)

	{
		got := reservationCount(p, fresh.nodeID)
		require.Equal(t, freshCount, got, "a release from generation (%v, %v)", got, freshCount)
	}
	require.GreaterOrEqual(t, reservationCount(p, "a"), 0, "reservation count must not be negative")
	require.GreaterOrEqual(t, reservationCount(p, "b"), 0, "reservation count must not be negative")
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
	require.Equal(t, to, moved.nodeID, "moved reservation node (%v)", to)
	{
		got := reservationCount(p, from)
		assert.Equal(t, 0, got, "refusing node (%v, %v)", from, got)
	}
	{
		got := reservationCount(p, to)
		assert.Equal(t, 1, got, "serving node (%v, %v)", to, got)
	}

	// Releasing the moved token clears the serving node, not the original.
	p.releaseReservation(moved)
	{
		got := reservationCount(p, to)
		assert.Equal(t, 0, got, "serving node (%v, %v)", to, got)
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

	require.NotEqual(t, second.nodeID, first.nodeID, "both dispatches chose")
	assert.Equal(t, 1, reservationCount(p, first.nodeID), "node")
	assert.Equal(t, 1, reservationCount(p, second.nodeID), "node")
}
