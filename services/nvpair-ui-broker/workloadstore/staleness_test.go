// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadstore

import (
	"testing"
	"time"
)

// clockStore returns a store whose clock the test drives, so staleness can be
// exercised without sleeping. Advance it with the returned pointer.
func clockStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s := New()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

// TestLastSeenCarriesAMonotonicReading guards the reason LastSeen is a time.Time
// rather than epoch ms. Staleness is an elapsed-interval question, and only a
// monotonic reading makes it immune to a wall-clock step — without one, an NTP
// correction or a resume larger than the staleness budget would make every
// remote workload look stale at once and retire live work in a burst.
//
// Stamping LastSeen from any wall-only source (time.UnixMilli, a parsed
// timestamp) would quietly lose the property, so it is asserted against the real
// clock rather than the test seam.
func TestLastSeenCarriesAMonotonicReading(t *testing.T) {
	// Check the detector against a known wall-only value first, so a broken
	// detector cannot let this test pass vacuously.
	if hasMonotonic(time.UnixMilli(1_700_000_000_000)) {
		t.Fatal("detector is wrong: a time.UnixMilli value carries no monotonic reading")
	}

	s := New()
	if !s.Apply(mkIn("1", "peer", "running", "peer", 100)) {
		t.Fatal("first running must be a forward change")
	}
	r, ok := s.Get("peer", "1")
	if !ok {
		t.Fatal("record missing")
	}
	if !hasMonotonic(r.LastSeen) {
		t.Fatal("LastSeen has no monotonic reading; staleness would be vulnerable to a wall-clock step")
	}
}

// hasMonotonic reports whether t carries a monotonic reading. Round(0) strips it,
// and == compares the full struct (unlike Equal, which compares instants only),
// so the two differ exactly when a monotonic reading is present.
func hasMonotonic(t time.Time) bool { return t != t.Round(0) }

// TestLastSeenTracksOriginReassertion is the load-bearing behavior for the
// staleness backstop: the origin's heartbeat re-asserts an unchanged "running"
// every interval, and Apply correctly reports those as no-op merges. LastSeen
// must advance anyway, otherwise "the origin still says running" is
// indistinguishable from "the origin went silent" and the sweep would retire
// live work.
func TestLastSeenTracksOriginReassertion(t *testing.T) {
	s, now := clockStore(t)

	if !s.Apply(mkIn("1", "peer", "running", "peer", 100)) {
		t.Fatal("first running must be a forward change")
	}
	r, _ := s.Get("peer", "1")
	if !r.LastSeen.Equal(*now) {
		t.Fatalf("LastSeen = %v, want %v", r.LastSeen, *now)
	}
	firstUpdated := r.LastUpdated

	*now = now.Add(30 * time.Second)
	if s.Apply(mkIn("1", "peer", "running", "peer", 100)) {
		t.Fatal("an unchanged re-assertion must not report a forward change")
	}
	r, _ = s.Get("peer", "1")
	if !r.LastSeen.Equal(*now) {
		t.Fatalf("LastSeen after re-assertion = %v, want %v (a no-op merge is still a sighting)", r.LastSeen, *now)
	}
	if r.LastUpdated != firstUpdated {
		t.Fatal("LastUpdated must NOT move on a no-op merge; that is why LastSeen exists")
	}
}

// TestLastSeenIgnoresInferredAndStale: only the origin's own current-generation
// assertions count as sightings. A local guess must not make a record look
// freshly confirmed (it would defeat the sweep), and a stale generation is not
// evidence of anything.
func TestLastSeenIgnoresInferredAndStale(t *testing.T) {
	s, now := clockStore(t)
	s.Apply(mkIn("1", "peer", "running", "peer", 200))
	first := *now

	*now = now.Add(time.Minute)
	s.ApplyInferred(mkIn("1", "peer", "failed", "peer", 200))
	r, _ := s.Get("peer", "1")
	if !r.LastSeen.Equal(first) {
		t.Fatalf("LastSeen = %v after an inferred guess, want %v unchanged", r.LastSeen, first)
	}

	// A stale generation for a live record is rejected outright and must not
	// refresh the sighting either.
	s2, now2 := clockStore(t)
	s2.Apply(mkIn("2", "peer", "running", "peer", 500))
	seen := *now2
	*now2 = now2.Add(time.Minute)
	if s2.Apply(mkIn("2", "peer", "running", "peer", 100)) {
		t.Fatal("a stale generation must be rejected")
	}
	r2, _ := s2.Get("peer", "2")
	if !r2.LastSeen.Equal(seen) {
		t.Fatalf("LastSeen = %v after a stale-generation event, want %v unchanged", r2.LastSeen, seen)
	}
}

// TestStaleForeignSelectsOnlySilentRemoteWork pins the selection rules: silent
// remote non-terminal work is stale; a recently re-asserted one is not; a
// terminal one is history, not stale; and this node's own workloads are never
// swept because its proxies are their authority and never re-assert into the
// store.
func TestStaleForeignSelectsOnlySilentRemoteWork(t *testing.T) {
	s, now := clockStore(t)
	const ttl = 5 * time.Minute

	s.Apply(mkIn("silent", "peer", "running", "peer", 100))
	s.Apply(mkIn("done", "peer", "completed", "peer", 100))
	s.Apply(mkIn("mine", "self", "running", "self", 100))

	*now = now.Add(ttl + time.Second)
	// The origin is still asserting this one, right now.
	s.Apply(mkIn("fresh", "peer", "running", "peer", 100))

	stale := s.StaleForeign("self", ttl)
	if len(stale) != 1 {
		ids := make([]string, 0, len(stale))
		for _, r := range stale {
			ids = append(ids, r.ID)
		}
		t.Fatalf("StaleForeign returned %v, want exactly [silent]", ids)
	}
	if stale[0].ID != "silent" {
		t.Fatalf("stale id = %q, want silent", stale[0].ID)
	}

	// Once the origin speaks up again the record stops being stale.
	s.Apply(mkIn("silent", "peer", "running", "peer", 100))
	if got := s.StaleForeign("self", ttl); len(got) != 0 {
		t.Fatalf("StaleForeign returned %d records after the origin re-asserted, want 0", len(got))
	}
}

// TestApplyInferredUnchangedSinceGuardsTheSweepRace: the sweep selects candidates
// and applies them as two steps, so an origin heartbeat can land in between. The
// guarded apply must drop a guess whose sighting is already obsolete, rather than
// marking genuinely running work failed until the next heartbeat.
func TestApplyInferredUnchangedSinceGuardsTheSweepRace(t *testing.T) {
	s, now := clockStore(t)
	const ttl = 5 * time.Minute

	s.Apply(mkIn("1", "peer", "running", "peer", 100))
	*now = now.Add(ttl + time.Millisecond)
	stale := s.StaleForeign("self", ttl)
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale record, got %d", len(stale))
	}
	seenAt := stale[0].LastSeen

	// The origin speaks up after selection but before the guess is applied.
	*now = now.Add(10 * time.Millisecond)
	s.Apply(mkIn("1", "peer", "running", "peer", 100))

	if s.ApplyInferredUnchangedSince(mkIn("1", "peer", "failed", "peer", 100), seenAt) {
		t.Fatal("a guess based on an obsolete sighting must not be applied")
	}
	if r, _ := s.Get("peer", "1"); r.State != "running" || r.Inferred {
		t.Fatalf("record = %+v, want the origin's authoritative running preserved", r)
	}

	// With no intervening heartbeat the same guarded apply does land. The origin
	// has to fall silent again first: its re-assertion above refreshed the
	// sighting, so the record is legitimately not stale until the budget passes.
	*now = now.Add(ttl + time.Millisecond)
	stale = s.StaleForeign("self", ttl)
	if len(stale) != 1 {
		t.Fatalf("expected the record to be stale again, got %d", len(stale))
	}
	if !s.ApplyInferredUnchangedSince(mkIn("1", "peer", "failed", "peer", 100), stale[0].LastSeen) {
		t.Fatal("a guess based on the current sighting must apply")
	}
	if r, _ := s.Get("peer", "1"); r.State != "failed" || !r.Inferred {
		t.Fatalf("record = %+v, want an inferred failed", r)
	}
}

// TestApplyInferredUnchangedSinceWillNotResurrect: a record removed between
// selection and apply must stay removed. Without the presence check the merge
// takes its "new record" branch and re-creates the workload as a synthesized
// terminal, which then reaches clients and the scheduler.
func TestApplyInferredUnchangedSinceWillNotResurrect(t *testing.T) {
	s, now := clockStore(t)
	const ttl = 5 * time.Minute

	s.Apply(mkIn("1", "peer", "running", "peer", 100))
	*now = now.Add(ttl + time.Millisecond)
	stale := s.StaleForeign("self", ttl)
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale record, got %d", len(stale))
	}

	// The workload is retired (a peer removal) before the guess lands.
	if !s.Remove("peer", "1") {
		t.Fatal("remove should report a deletion")
	}
	if s.ApplyInferredUnchangedSince(mkIn("1", "peer", "failed", "peer", 100), stale[0].LastSeen) {
		t.Fatal("a guess about a removed workload must not be applied")
	}
	if _, ok := s.Get("peer", "1"); ok {
		t.Fatal("the removed workload was resurrected as a synthesized terminal")
	}
}

// TestStaleForeignSweepsWorkThisNodeIsExecuting: a remote-origin workload routed
// to this node's own engine must still be swept when its origin goes quiet.
//
// It is tempting to exempt it on the grounds that our engine is running it, but
// lifecycle events come from the ORIGINATING proxy, never from the destination
// engine — so the executing node has no independent signal that would ever clear
// the record. Exempting it converts a bounded staleness window into a permanent
// phantom on the one node least able to notice.
func TestStaleForeignSweepsWorkThisNodeIsExecuting(t *testing.T) {
	s, now := clockStore(t)
	const ttl = 5 * time.Minute

	s.Apply(mkIn("here", "peer", "running", "self", 100))  // routed to us
	s.Apply(mkIn("there", "peer", "running", "peer", 100)) // routed elsewhere
	s.Apply(mkIn("mine", "self", "running", "self", 100))  // ours: never swept
	*now = now.Add(ttl + time.Millisecond)

	got := map[string]bool{}
	for _, r := range s.StaleForeign("self", ttl) {
		got[r.ID] = true
	}
	if !got["here"] {
		t.Error("a remote-origin workload executing here must be swept: nothing else will ever clear it")
	}
	if !got["there"] {
		t.Error("a remote-origin workload executing elsewhere must be swept")
	}
	if got["mine"] {
		t.Error("a local-origin workload must never be swept")
	}
	if len(got) != 2 {
		t.Errorf("swept %v, want exactly here+there", got)
	}
}

// TestStaleForeignSkipsCollidingClientKeys: the broker identifies a workload by
// (origin, engine, runId, id) but the desktop keys only on (origin, id), and both
// engines number their requests from 1 and reset on restart, so that coarse key
// collides in practice. Emitting a synthesized terminal for one generation would
// land on whichever generation currently holds the key — possibly a live job — so
// an ambiguous candidate is left alone.
func TestStaleForeignSkipsCollidingClientKeys(t *testing.T) {
	s, now := clockStore(t)
	const ttl = 5 * time.Minute

	// Same origin and id, different engines: two distinct workloads to the broker,
	// one key to the desktop.
	s.Apply(mkInFull("7", "peer", "ollama", "runA", "running", "peer", 100))
	s.Apply(mkInFull("7", "peer", "lmstudio", "runB", "running", "peer", 100))
	// An uncontested record, to prove the sweep is still working at all.
	s.Apply(mkIn("solo", "peer", "running", "peer", 100))
	*now = now.Add(ttl + time.Millisecond)

	stale := s.StaleForeign("self", ttl)
	if len(stale) != 1 || stale[0].ID != "solo" {
		ids := make([]string, 0, len(stale))
		for _, r := range stale {
			ids = append(ids, r.ID)
		}
		t.Fatalf("StaleForeign returned %v, want exactly [solo] — a colliding client key must not be retired", ids)
	}

	// Once the collision resolves (one generation reaches a real terminal), the
	// survivor becomes eligible again.
	s.Apply(mkInFull("7", "peer", "lmstudio", "runB", "completed", "peer", 100))
	*now = now.Add(ttl + time.Millisecond)
	found := false
	for _, r := range s.StaleForeign("self", ttl) {
		if r.ID == "7" {
			found = true
		}
	}
	if !found {
		t.Error("with only one live generation left, the silent record must be swept")
	}
}

// TestStaleForeignInferredFailIsReconcilable closes the loop: retiring a stale
// record uses the inferred path, so an origin that was merely unable to reach us
// un-sticks its own workload with its next authoritative event.
func TestStaleForeignInferredFailIsReconcilable(t *testing.T) {
	s, now := clockStore(t)
	const ttl = 5 * time.Minute

	s.Apply(mkIn("1", "peer", "running", "peer", 100))
	*now = now.Add(ttl + time.Millisecond)
	stale := s.StaleForeign("self", ttl)
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale record, got %d", len(stale))
	}

	if !s.ApplyInferred(mkIn("1", "peer", "failed", "peer", 100)) {
		t.Fatal("the sweep's inferred failed must apply to a live record")
	}
	if r, _ := s.Get("peer", "1"); !r.Terminal || !r.Inferred {
		t.Fatalf("record = %+v, want an inferred terminal", r)
	}
	if got := s.StaleForeign("self", ttl); len(got) != 0 {
		t.Fatalf("a retired record must not be swept again, got %d", len(got))
	}

	// The origin comes back and insists it is still running.
	if !s.Apply(mkIn("1", "peer", "running", "peer", 100)) {
		t.Fatal("an authoritative running must override the inferred failed")
	}
	if r, _ := s.Get("peer", "1"); r.Terminal || r.Inferred {
		t.Fatalf("record = %+v, want authoritative running restored", r)
	}
}
