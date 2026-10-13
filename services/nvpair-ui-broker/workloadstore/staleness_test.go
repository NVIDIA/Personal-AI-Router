// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.False(t, hasMonotonic(time.UnixMilli(1_700_000_000_000)), "detector is wrong: a time.UnixMilli value carries no monotonic reading")

	s := New()
	require.True(t, s.Apply(mkIn("1", "peer", "running", "peer", 100)), "first running must be a forward change")
	r, ok := s.Get("peer", "1")
	require.True(t, ok, "record missing")
	require.True(t, hasMonotonic(r.LastSeen), "LastSeen has no monotonic reading; staleness would be vulnerable to a wall-clock step")
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

	require.True(t, s.Apply(mkIn("1", "peer", "running", "peer", 100)), "first running must be a forward change")
	r, _ := s.Get("peer", "1")
	require.WithinDuration(t, *now, r.LastSeen, 0, "LastSeen")
	firstUpdated := r.LastUpdated

	*now = now.Add(30 * time.Second)
	require.False(t, s.Apply(mkIn("1", "peer", "running", "peer", 100)), "an unchanged re-assertion must not report a forward change")
	r, _ = s.Get("peer", "1")
	require.WithinDuration(t, *now, r.LastSeen, 0, "LastSeen after re-assertion")
	require.True(t, r.LastUpdated == firstUpdated, "LastUpdated must NOT move on a no-op merge; that is why LastSeen exists")
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
	require.WithinDuration(t, first, r.LastSeen, 0, "an inferred guess must not refresh LastSeen")

	// A stale generation for a live record is rejected outright and must not
	// refresh the sighting either.
	s2, now2 := clockStore(t)
	s2.Apply(mkIn("2", "peer", "running", "peer", 500))
	seen := *now2
	*now2 = now2.Add(time.Minute)
	require.False(t, s2.Apply(mkIn("2", "peer", "running", "peer", 100)), "a stale generation must be rejected")
	r2, _ := s2.Get("peer", "2")
	require.WithinDuration(t, seen, r2.LastSeen, 0, "a stale-generation event must not refresh LastSeen")
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
	require.Len(t, stale, 1, "StaleForeign should return exactly [silent]")
	require.Equal(t, "silent", stale[0].ID, "stale id")

	// Once the origin speaks up again the record stops being stale.
	s.Apply(mkIn("silent", "peer", "running", "peer", 100))
	require.Empty(t, s.StaleForeign("self", ttl), "origin re-assertion must clear staleness")
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
	require.Len(t, stale, 1, "expected 1 stale record")
	seenAt := stale[0].LastSeen

	// The origin speaks up after selection but before the guess is applied.
	*now = now.Add(10 * time.Millisecond)
	s.Apply(mkIn("1", "peer", "running", "peer", 100))

	require.False(t, s.ApplyInferredUnchangedSince(mkIn("1", "peer", "failed", "peer", 100), seenAt), "a guess based on an obsolete sighting must not be applied")
	r, _ := s.Get("peer", "1")
	require.Equal(t, "running", r.State, "record (%v)", r)
	require.False(t, r.Inferred, "record (%v)", r)

	// With no intervening heartbeat the same guarded apply does land. The origin
	// has to fall silent again first: its re-assertion above refreshed the
	// sighting, so the record is legitimately not stale until the budget passes.
	*now = now.Add(ttl + time.Millisecond)
	stale = s.StaleForeign("self", ttl)
	require.Len(t, stale, 1, "expected the record to be stale again")
	require.True(t, s.ApplyInferredUnchangedSince(mkIn("1", "peer", "failed", "peer", 100), stale[0].LastSeen), "a guess based on the current sighting must apply")
	r, _ = s.Get("peer", "1")
	require.Equal(t, "failed", r.State, "record (%v)", r)
	require.True(t, r.Inferred, "record (%v)", r)
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
	require.Len(t, stale, 1, "expected 1 stale record")

	// The workload is retired (a peer removal) before the guess lands.
	require.True(t, s.Remove("peer", "1"), "remove should report a deletion")
	require.False(t, s.ApplyInferredUnchangedSince(mkIn("1", "peer", "failed", "peer", 100), stale[0].LastSeen), "a guess about a removed workload must not be applied")
	_, ok := s.Get("peer", "1")
	require.False(t, ok, "the removed workload was resurrected as a synthesized terminal")
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
	assert.Contains(t, got, "here", "a remote-origin workload executing here must be swept: nothing else will ever clear it")
	assert.Contains(t, got, "there", "a remote-origin workload executing elsewhere must be swept")
	assert.NotContains(t, got, "mine", "a local-origin workload must never be swept")
	assert.Len(t, got, 2, "swept")
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
	require.Len(t, stale, 1, "a colliding client key must not be retired")
	require.Equal(t, "solo", stale[0].ID, "a colliding client key must not be retired")

	// Once the collision resolves (one generation reaches a real terminal), the
	// survivor becomes eligible again.
	s.Apply(mkInFull("7", "peer", "lmstudio", "runB", "completed", "peer", 100))
	*now = now.Add(ttl + time.Millisecond)
	assert.Contains(t, replayIDSet(s.StaleForeign("self", ttl)), "7", "with only one live generation left, the silent record must be swept")
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
	require.Len(t, stale, 1, "expected 1 stale record")

	require.True(t, s.ApplyInferred(mkIn("1", "peer", "failed", "peer", 100)), "the sweep's inferred failed must apply to a live record")
	r, _ := s.Get("peer", "1")
	require.True(t, r.Terminal, "record (%v)", r)
	require.True(t, r.Inferred, "record (%v)", r)
	require.Empty(t, s.StaleForeign("self", ttl), "a retired record must not be swept again")

	// The origin comes back and insists it is still running.
	require.True(t, s.Apply(mkIn("1", "peer", "running", "peer", 100)), "an authoritative running must override the inferred failed")
	r, _ = s.Get("peer", "1")
	require.False(t, r.Terminal, "record (%v)", r)
	require.False(t, r.Inferred, "record (%v)", r)
}
