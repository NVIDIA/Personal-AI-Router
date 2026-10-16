// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nvpair-shared/schedulerwire"
)

// snapshotProxy returns a proxy whose discovery holds ids and whose scheduler
// snapshot ranks them in the order given, so the least-loaded pick is
// deterministic. Each test builds its own: reserveCandidate increments a
// reservation, so reusing one proxy across assertions would let accumulated load
// decide the second answer and the test would pass for the wrong reason.
func snapshotProxy(t *testing.T, ids ...string) *Proxy {
	t.Helper()
	p := prProxy(t, ids...)
	ranks := make([]schedulerwire.NodeRank, 0, len(ids))
	for i, id := range ids {
		ranks = append(ranks, schedulerwire.NodeRank{ID: id, Rank: i})
	}
	applySnapshot(p, schedulerwire.Priority{Nodes: ids, Ranks: ranks})
	return p
}

// TestResolveCandidates_CoolingGoesLast: a node that spent a full first-byte
// deadline without producing content is ordered behind the rest, overriding the
// priority list it would otherwise head.
func TestResolveCandidates_CoolingGoesLast(t *testing.T) {
	p := prProxy(t, "a", "b", "c")
	p.SetPriority([]string{"a", "b", "c"})
	assertOrder(t, candidateIDs(p), []string{"a", "b", "c"})

	p.noteNoFirstByte("a", time.Now())
	assertOrder(t, candidateIDs(p), []string{"b", "c", "a"})
}

// TestResolveCandidates_CoolingKeepsRelativeOrder: demotion is a stable
// partition, not a sort. The scheduler's ranking still decides within each group,
// so two cooling nodes stay in their own relative order at the back.
func TestResolveCandidates_CoolingKeepsRelativeOrder(t *testing.T) {
	p := prProxy(t, "a", "b", "c", "d")
	p.SetPriority([]string{"a", "b", "c", "d"})

	now := time.Now()
	p.noteNoFirstByte("a", now)
	p.noteNoFirstByte("c", now)
	assertOrder(t, candidateIDs(p), []string{"b", "d", "a", "c"})
}

// TestResolveCandidates_SoleCoolingCandidateStays: demotion never becomes
// exclusion. With one candidate the back is also the front, so a single-node
// cluster is unaffected by its own cooldown.
func TestResolveCandidates_SoleCoolingCandidateStays(t *testing.T) {
	p := prProxy(t, "a")
	p.noteNoFirstByte("a", time.Now())
	assertOrder(t, candidateIDs(p), []string{"a"})
}

// TestResolveCandidates_CooldownExpiresOnTime: the stamp lapses on its own, with
// no success required. A peer that went quiet and was never dispatched to again
// must not be held at the back forever, which is what would happen if only a
// first byte could clear it.
func TestResolveCandidates_CooldownExpiresOnTime(t *testing.T) {
	setForTest(t, &candidateCooldown, 50*time.Millisecond)

	p := prProxy(t, "a", "b")
	p.SetPriority([]string{"a", "b"})
	p.noteNoFirstByte("a", time.Now())
	assertOrder(t, candidateIDs(p), []string{"b", "a"})

	time.Sleep(100 * time.Millisecond)
	assertOrder(t, candidateIDs(p), []string{"a", "b"})
}

// TestResolveCandidates_FirstByteClearsCooldownEarly: a node that serves again
// is restored immediately rather than sitting out the rest of its window. A byte
// from the node is direct evidence about that node.
func TestResolveCandidates_FirstByteClearsCooldownEarly(t *testing.T) {
	p := prProxy(t, "a", "b")
	p.SetPriority([]string{"a", "b"})
	p.noteNoFirstByte("a", time.Now())
	assertOrder(t, candidateIDs(p), []string{"b", "a"})

	p.noteFirstByte("a")
	assertOrder(t, candidateIDs(p), []string{"a", "b"})
}

// TestResolveCandidates_ExplicitSelectionIsAlsoDemoted documents a deliberate
// choice rather than an accident: the demotion runs after all three ordering
// passes, so it also applies to an explicitly selected node.
//
// The selection is "this node first", not "this node only" — the rest of the
// list is still appended — so a demoted selection is still dispatched to once
// the healthy candidates are exhausted.
func TestResolveCandidates_ExplicitSelectionIsAlsoDemoted(t *testing.T) {
	p := prProxy(t, "a", "b")
	p.soleFacade().SetSelected("a")
	assertOrder(t, candidateIDs(p), []string{"a", "b"})

	p.noteNoFirstByte("a", time.Now())
	assertOrder(t, candidateIDs(p), []string{"b", "a"})
}

// TestReserveCandidate_UnchangedWithoutCooling pins the fast path: with nothing
// cooling, the least-loaded pick is exactly what it was. The balance invariants
// in reservation_test.go depend on this.
func TestReserveCandidate_UnchangedWithoutCooling(t *testing.T) {
	p := snapshotProxy(t, "a", "b")
	if got := reservedID(p, reservationCandidates("a", "b")); got != "a" {
		t.Fatalf("reserved %q, want the top-ranked %q with nothing cooling", got, "a")
	}
}

// TestReserveCandidate_SkipsCooling is why the demotion needs a second hook. The
// reserved head is chosen by load, not by list position, so the ordering in
// resolveCandidates does not reach it: without this skip a cooling node is put
// straight back at the front of the round it was just demoted from.
func TestReserveCandidate_SkipsCooling(t *testing.T) {
	p := snapshotProxy(t, "a", "b")
	p.noteNoFirstByte("a", time.Now())
	if got := reservedID(p, reservationCandidates("a", "b")); got != "b" {
		t.Fatalf("reserved %q, want %q: the cooling node was chosen as the head anyway", got, "b")
	}
}

// TestReserveCandidate_AllCoolingStillReserves: when every listed candidate is
// cooling there is nothing to prefer, and leaving the round unreserved would
// turn demotion into exclusion. The ordinary least-loaded pick takes over.
func TestReserveCandidate_AllCoolingStillReserves(t *testing.T) {
	p := snapshotProxy(t, "a", "b")
	now := time.Now()
	p.noteNoFirstByte("a", now)
	p.noteNoFirstByte("b", now)
	if got := reservedID(p, reservationCandidates("a", "b")); got != "a" {
		t.Fatalf("reserved %q, want the top-ranked %q when everything is cooling", got, "a")
	}
}

// TestHandleHTTP_StalledNodeIsNotPaidForTwice is the point of the whole change,
// and the assertion is the dispatch count rather than the candidate order: a
// reordering that still sent the next request into the same stall would pass an
// order-only test while costing the caller a second full first-byte deadline.
//
// The first request pays the stall once and fails over. The second must not
// reach the staller at all, so its hit count stays at one.
func TestHandleHTTP_StalledNodeIsNotPaidForTwice(t *testing.T) {
	setForTest(t, &firstBodyTimeout, testFirstBodyTimeout)

	forEachEngine(t, func(t *testing.T, tc engineCase) {
		stalled := newStalledEngine(t)
		var goodHits int
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			goodHits++
			_, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"done":true}`)
		}))
		defer good.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "stalled", stalled.url, tc.advertisedModel))
		disc.AddManual(nodeForModel(t, "good", good.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		// Priority rather than an explicit selection: this test is about the
		// cooldown, not about how a pin interacts with it.
		p.SetPriority([]string{"stalled", "good"})

		first := httptest.NewRecorder()
		p.soleFacade().handleHTTP(first, tc.inferenceRequest())
		if first.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200 after failing over", first.Code)
		}
		if stalled.hits() != 1 {
			t.Fatalf("staller hits after the first request = %d, want 1", stalled.hits())
		}
		if got := candidateIDs(p); got[len(got)-1] != "stalled" {
			t.Fatalf("candidate order after the stall = %v, want the staller last", got)
		}

		second := httptest.NewRecorder()
		p.soleFacade().handleHTTP(second, tc.inferenceRequest())
		if second.Code != http.StatusOK {
			t.Fatalf("second request status = %d, want 200", second.Code)
		}
		if stalled.hits() != 1 {
			t.Errorf("staller hits after the second request = %d, want 1: the cooldown did not hold and the caller paid the stall twice", stalled.hits())
		}
		if goodHits != 2 {
			t.Errorf("healthy node served %d requests, want 2", goodHits)
		}
	})
}
