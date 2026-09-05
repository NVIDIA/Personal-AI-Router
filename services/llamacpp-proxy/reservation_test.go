// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"

	"nvpair-shared/schedulerwire"
)

func reservationCandidates(ids ...string) []candidate {
	out := make([]candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, candidate{id: id})
	}
	return out
}

func reservedID(p *Proxy, candidates []candidate) string {
	candidates = append([]candidate(nil), candidates...)
	return p.reserveCandidate(candidates)[0].id
}

func TestReserveCandidate_ConcurrentEqualLoadHasAtMostOneSkew(t *testing.T) {
	p := prProxy(t)
	ids := []string{"a", "b", "c", "d"}
	ranks := make([]schedulerwire.NodeRank, 0, len(ids))
	for i, id := range ids {
		ranks = append(ranks, schedulerwire.NodeRank{ID: id, Rank: i})
	}
	p.SetPrioritySnapshot(schedulerwire.Priority{Nodes: ids, Ranks: ranks})
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
	p.SetPrioritySnapshot(schedulerwire.Priority{
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
	p.SetPrioritySnapshot(schedulerwire.Priority{
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
	p.SetPrioritySnapshot(schedulerwire.Priority{
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
	p.SetPrioritySnapshot(schedulerwire.Priority{
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
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{{ID: "a"}, {ID: "b"}},
	})
	p.SetSelected("b")
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
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"owner-b", "owner-a"},
		Ranks: []schedulerwire.NodeRank{{ID: "owner-b"}, {ID: "owner-a"}},
	})
	p.SetSelected("missing")
	if got := reservedID(p, reservationCandidates("owner-a", "owner-b")); got != "owner-b" {
		t.Fatalf("reservation with ineligible pin = %q, want owner-b", got)
	}
}

func TestReserveCandidate_PreservesFailoverAndSnapshotReset(t *testing.T) {
	p := prProxy(t)
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"b", "a", "c"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "b", Pending: 0},
			{ID: "a", Pending: 5},
			{ID: "c", Pending: 6},
		},
	})
	got := p.reserveCandidate(reservationCandidates("a", "b", "c"))
	want := []string{"b", "a", "c"}
	for i, id := range want {
		if got[i].id != id {
			t.Fatalf("reserved failover order = %v, want %v", candidateIDsFrom(got), want)
		}
	}

	p.SetPrioritySnapshot(schedulerwire.Priority{
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
