// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadstore

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mkIn builds an Incoming with a realistic full-fidelity Info payload.
func mkIn(id, origin, state, scheduledOn string, createdAt int64) Incoming {
	m := map[string]any{
		"id":             id,
		"originatedFrom": origin,
		"state":          state,
		"scheduledOn":    scheduledOn,
		"createdAt":      createdAt,
		"model":          "granite-embedding:latest",
		"engine":         "ollama",
	}
	b, _ := json.Marshal(m)
	return Incoming{
		Origin:      origin,
		ID:          id,
		State:       state,
		ScheduledOn: scheduledOn,
		CreatedAt:   createdAt,
		Info:        b,
	}
}

// TestApplyRejectsRunningAfterTerminal is the core reordering bug: on the
// receiving node the relayed "failed" can arrive before "running" for the same
// workload. The stale running must not resurrect a finished job.
func TestApplyRejectsRunningAfterTerminal(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkIn("9", "laptop", "failed", "pc", 100)), "first sighting (failed) should be accepted")
	require.False(t, s.Apply(mkIn("9", "laptop", "running", "pc", 100)), "running after failed (same generation) must be rejected")
	r, ok := s.Get("laptop", "9")
	require.True(t, ok, "final record (%v)", r)
	require.Equal(t, "failed", r.State, "final record (%v)", r)
	require.True(t, r.Terminal, "final record (%v)", r)
}

// TestApplyRunningThenFailed is the in-order path: running then failed both
// apply and the workload ends terminal.
func TestApplyRunningThenFailed(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkIn("9", "laptop", "running", "pc", 100)), "running should be accepted")
	require.True(t, s.Apply(mkIn("9", "laptop", "failed", "pc", 100)), "failed after running should be accepted (forward progress)")
	r, _ := s.Get("laptop", "9")
	require.Equal(t, "failed", r.State, "final record (%v)", r)
	require.True(t, r.Terminal, "final record (%v)", r)
}

// TestApplyNewerGenerationReplaces is epoch protection: a restarted proxy
// reuses id "5" (createdAt resets forward); its newer-createdAt running must
// replace the prior generation's terminal record, not be mistaken for it.
func TestApplyNewerGenerationReplaces(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkIn("5", "pc", "failed", "pc", 100)), "old-generation terminal should be accepted")
	require.True(t, s.Apply(mkIn("5", "pc", "running", "laptop", 200)), "newer-generation running must replace the old terminal")
	r, _ := s.Get("pc", "5")
	require.Equal(t, "running", r.State, "record (%v)", r)
	require.Equal(t, int64(200), r.CreatedAt, "record (%v)", r)
	require.Equal(t, "laptop", r.ScheduledOn, "record (%v)", r)
	require.False(t, r.Terminal, "record (%v)", r)
}

// TestApplyOlderGenerationRejected: a late event from a prior generation (older
// createdAt) must never touch the current generation's record — even a terminal
// one, which would otherwise look like "forward progress" by state rank.
func TestApplyOlderGenerationRejected(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkIn("5", "pc", "running", "pc", 200)), "current-generation running should be accepted")
	require.False(t, s.Apply(mkIn("5", "pc", "completed", "pc", 100)), "older-generation event must be rejected despite higher state rank")
	r, _ := s.Get("pc", "5")
	require.Equal(t, int64(200), r.CreatedAt, "record (%v)", r)
	require.Equal(t, "running", r.State, "record (%v)", r)
}

// TestApplyEqualRankReemit: an identical re-emit is a no-op, but a same-state
// re-emit that re-points scheduledOn (a failover) is a meaningful change.
func TestApplyEqualRankReemit(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkIn("1", "a", "running", "node1", 100)), "initial running should be accepted")
	require.False(t, s.Apply(mkIn("1", "a", "running", "node1", 100)), "identical re-emit should be a no-op")
	require.True(t, s.Apply(mkIn("1", "a", "running", "node2", 100)), "running re-pointed to a new scheduledOn should be accepted")
	r, _ := s.Get("a", "1")
	require.Equal(t, "node2", r.ScheduledOn, "scheduledOn")
}

// TestApplyCancelledIsTerminal covers the two gates a new terminal state has
// to pass, both of which fail silently and in opposite directions. Omitted from
// rank() it sorts below queued, so applyLocked rejects the event outright and
// the transition never reaches the scheduler or any client. Omitted from
// isTerminal() it is accepted but treated as live forever, so it keeps counting
// toward its node's pending total and stays exposed to the staleness sweeps.
func TestApplyCancelledIsTerminal(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkIn("1", "a", "running", "node1", 100)), "initial running should be accepted")
	// Ranks above running, so the transition is accepted rather than dropped.
	require.True(t, s.Apply(mkIn("1", "a", "cancelled", "node1", 100)), "cancelled after running should be accepted (missing from rank(): sorts below queued and is rejected)")
	// Terminal, so a late running cannot resurrect it.
	require.False(t, s.Apply(mkIn("1", "a", "running", "node1", 100)), "running after cancelled should be rejected as backwards")
	// Terminal, so it drops out of the active set rather than counting as load.
	r, ok := s.Get("a", "1")
	require.True(t, ok, "record should still be stored")
	require.True(t, r.Terminal, "cancelled must be terminal (missing from isTerminal(): counts as pending forever and stays exposed to the staleness sweeps)")
	require.Empty(t, s.ActiveSnapshot(), "a cancelled workload must leave the active snapshot")
}

// TestApplyCrossNodeIsolation: the same numeric id from two origins is two
// distinct workloads and must never merge against each other.
func TestApplyCrossNodeIsolation(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkIn("1", "A", "failed", "A", 100)), "A/1 should be accepted")
	require.True(t, s.Apply(mkIn("1", "B", "running", "B", 100)), "B/1 has the same id but a different origin; must be independent")
	require.Equal(t, 2, s.Len())
	a, _ := s.Get("A", "1")
	require.Equal(t, "failed", a.State, "A/1 state")
	b, _ := s.Get("B", "1")
	require.Equal(t, "running", b.State, "B/1 state")
}

func TestApplyRejectsMalformed(t *testing.T) {
	s := New()
	require.False(t, s.Apply(Incoming{ID: "", Origin: "a", State: "running"}), "missing id must be rejected")
	require.False(t, s.Apply(Incoming{ID: "1", Origin: "", State: "running"}), "missing origin must be rejected")
	require.Equal(t, 0, s.Len())
}

func TestRemove(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	require.True(t, s.Remove("a", "1"), "Remove of present entry should report true")
	require.False(t, s.Remove("a", "1"), "Remove of absent entry should report false")
	_, ok := s.Get("a", "1")
	require.False(t, ok, "entry should be gone after Remove")
}

func TestSnapshotOrderedByCreatedAt(t *testing.T) {
	s := New()
	s.Apply(mkIn("2", "a", "running", "a", 300))
	s.Apply(mkIn("1", "a", "running", "a", 100))
	s.Apply(mkIn("1", "b", "running", "b", 200))

	snap := s.Snapshot()
	require.Len(t, snap, 3, "snapshot len")
	var got []int64
	for _, raw := range snap {
		var hdr struct {
			CreatedAt int64 `json:"createdAt"`
		}
		require.NoError(t, json.Unmarshal(raw, &hdr), "bad snapshot entry")
		got = append(got, hdr.CreatedAt)
	}
	require.Equal(t, []int64{100, 200, 300}, got, "snapshot order")
}

func TestActiveSnapshotExcludesTerminalHistory(t *testing.T) {
	s := New()
	s.Apply(mkIn("completed", "a", "completed", "a", 100))
	s.Apply(mkIn("queued", "a", "queued", "b", 200))
	s.Apply(mkIn("running", "b", "running", "a", 300))
	s.Apply(mkIn("failed", "b", "failed", "b", 400))

	snap := s.ActiveSnapshot()
	require.Len(t, snap, 2, "active snapshot len")
	var ids []string
	for _, raw := range snap {
		var hdr struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}
		require.NoError(t, json.Unmarshal(raw, &hdr), "bad active snapshot entry")
		require.Contains(t, []string{"queued", "running"}, hdr.State, "active snapshot included terminal state")
		ids = append(ids, hdr.ID)
	}
	require.ElementsMatch(t, []string{"queued", "running"}, ids, "active snapshot ids")
}

func TestActiveForNode(t *testing.T) {
	s := New()
	// Non-terminal, executes on pc (origin laptop) — should match "pc".
	s.Apply(mkIn("1", "laptop", "running", "pc", 100))
	// Terminal on pc — should NOT match (node-loss sweep only touches live).
	s.Apply(mkIn("2", "laptop", "failed", "pc", 100))
	// Non-terminal elsewhere — should not match "pc".
	s.Apply(mkIn("3", "laptop", "running", "other", 100))
	// Non-terminal originating on pc — should match "pc".
	s.Apply(mkIn("4", "pc", "running", "laptop", 100))

	active := s.ActiveForNode("pc")
	require.Len(t, active, 2, "ActiveForNode(pc) len")
	for _, r := range active {
		require.False(t, r.Terminal, "ActiveForNode returned a terminal record (%v)", r)
		require.Contains(t, []string{r.Origin, r.ScheduledOn}, "pc", "ActiveForNode returned an unrelated record (%v)", r)
	}
}

// TestReplayForNode covers the rehydration set the broker hands a (re)started
// workload-manager: every active local-origin record (always), plus terminal
// local-origin records within the window — never a peer's, never an inferred
// guess, and never a terminal that has aged out. Including recent terminals is
// what keeps the manager's two-interval terminal re-sync window alive across a
// restart.
func TestReplayForNode(t *testing.T) {
	s := New()
	clock := int64(1_000_000)
	s.now = func() time.Time { return time.UnixMilli(clock) }
	const window = int64(60_000)

	s.Apply(mkIn("1", "host", "running", "peer", 1))        // active local-origin → always
	s.Apply(mkIn("2", "host", "failed", "host", 1))         // recent terminal local-origin → replay
	s.Apply(mkIn("3", "peer", "running", "host", 1))        // peer-origin (scheduled here) → never
	s.Apply(mkIn("4", "host", "completed", "host", 1))      // recent terminal local-origin → replay
	s.ApplyInferred(mkIn("5", "host", "failed", "gone", 1)) // inferred terminal local-origin → never

	require.ElementsMatch(t, []string{"1", "2", "4"}, replayIDSet(s.ReplayForNode("host", window)), "replay active and recent terminal records, excluding peers and inferred records")

	// Age everything past the window: terminals drop; the still-running record
	// (non-terminal) is always replayed regardless of age.
	clock += window + 1
	require.ElementsMatch(t, []string{"1"}, replayIDSet(s.ReplayForNode("host", window)), "post-window replay includes only the still-active record")
}

// TestReplayForNodeAfterLoadUsesCompletionTime is the load-to-replay
// regression: Load stamps LastUpdated=now on every restored terminal, so if
// ReplayForNode keyed its window on LastUpdated it would treat all persisted
// history as just-finished and rebroadcast it on the first post-restart
// respawn. Freshness must come from the persisted completedAt instead, so only
// a genuinely recent terminal is replayed.
func TestReplayForNodeAfterLoadUsesCompletionTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	const window = int64(60_000)

	// Persist two terminals: one that finished ~1000s ago, one 30s ago.
	writer := newStoreAt(path, testNow)
	writer.Apply(mkTerm("old-history", "host", testNow-1_001_000, testNow-1_000_000))
	writer.Apply(mkTerm("recent", "host", testNow-31_000, testNow-30_000))
	require.NoError(t, writer.Flush(), "flush")

	// A fresh store loads at the same wall clock. Load stamps LastUpdated=now on
	// BOTH restored records, but completedAt is preserved in Info — so keying the
	// replay window on completion time (not LastUpdated) keeps the 1000s-old
	// terminal out while still replaying the 30s-old one.
	s := newStoreAt(path, testNow)
	require.NoError(t, s.Load(), "load")

	require.ElementsMatch(t, []string{"recent"}, replayIDSet(s.ReplayForNode("host", window)), "old history must not look recent after Load")
}

func replayIDSet(recs []Record) []string {
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestInferredMarksRunningFailed: the node-loss sweep may fail an authoritative
// running job.
func TestInferredMarksRunningFailed(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	require.True(t, s.ApplyInferred(mkIn("1", "a", "failed", "a", 100)), "inferred failed should mark a running job failed")
	r, _ := s.Get("a", "1")
	require.Equal(t, "failed", r.State, "record (%v)", r)
	require.True(t, r.Terminal, "record (%v)", r)
	require.True(t, r.Inferred, "record (%v)", r)
}

// TestAuthoritativeOverridesInferred is the blip-and-return case: a node
// wrongly inferred failed, then the origin re-asserts running (same generation).
// The authoritative event must win.
func TestAuthoritativeOverridesInferred(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	s.ApplyInferred(mkIn("1", "a", "failed", "a", 100)) // node-loss guess
	require.True(t, s.Apply(mkIn("1", "a", "running", "a", 100)), "authoritative running must override an inferred failed")
	r, _ := s.Get("a", "1")
	require.Equal(t, "running", r.State, "record (%v)", r)
	require.False(t, r.Terminal, "record (%v)", r)
	require.False(t, r.Inferred, "record (%v)", r)
}

// TestInferredCannotOverrideAuthoritativeTerminal: a guess must never overwrite
// the origin's real terminal.
func TestInferredCannotOverrideAuthoritativeTerminal(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "completed", "a", 100))
	require.False(t, s.ApplyInferred(mkIn("1", "a", "failed", "a", 100)), "inferred failed must not override an authoritative terminal")
	r, _ := s.Get("a", "1")
	require.Equal(t, "completed", r.State, "record (%v)", r)
	require.False(t, r.Inferred, "record (%v)", r)
}

// TestAuthoritativeTerminalOverridesInferred: the origin's real terminal
// replaces a prior inferred one.
func TestAuthoritativeTerminalOverridesInferred(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	s.ApplyInferred(mkIn("1", "a", "failed", "a", 100))
	require.True(t, s.Apply(mkIn("1", "a", "completed", "a", 100)), "origin's authoritative terminal must override an inferred one")
	r, _ := s.Get("a", "1")
	require.Equal(t, "completed", r.State, "record (%v)", r)
	require.False(t, r.Inferred, "record (%v)", r)
}

// mkInFull builds an Incoming with explicit engine + runId (and no completedAt).
func mkInFull(id, origin, engine, runID, state, scheduledOn string, createdAt int64) Incoming {
	m := map[string]any{
		"id": id, "originatedFrom": origin, "engine": engine, "runId": runID,
		"state": state, "scheduledOn": scheduledOn, "createdAt": createdAt, "model": "m",
	}
	b, _ := json.Marshal(m)
	in, _ := ParseIncoming(b)
	return in
}

// mkTermFull builds a terminal (failed) Incoming with explicit engine + runId.
func mkTermFull(id, origin, engine, runID string, createdAt, completedAt int64) Incoming {
	m := map[string]any{
		"id": id, "originatedFrom": origin, "engine": engine, "runId": runID,
		"state": "failed", "scheduledOn": origin, "createdAt": createdAt,
		"completedAt": completedAt, "model": "m",
	}
	b, _ := json.Marshal(m)
	in, _ := ParseIncoming(b)
	return in
}

// TestCrossEngineConcurrentDistinct: two concurrent jobs from one host that
// reuse id "1" (Ollama + LM Studio, separate counters) are distinct workloads
// and must not collide in the store.
func TestCrossEngineConcurrentDistinct(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkInFull("1", "host", "ollama", "ro", "running", "host", 100)), "ollama/1 should be accepted")
	require.True(t, s.Apply(mkInFull("1", "host", "lmstudio", "rl", "running", "host", 200)), "lmstudio/1 shares the numeric id but a different engine; must be distinct")
	require.Equal(t, 2, s.Len())
}

// TestRestartReuseKeepsHistory: after a proxy restart reuses id "1" (new runId),
// the prior generation's terminal history must be preserved, not overwritten.
func TestRestartReuseKeepsHistory(t *testing.T) {
	s := New()
	require.True(t, s.Apply(mkTermFull("1", "host", "ollama", "run1", 100, 150)), "gen-1 terminal should be accepted")
	require.True(t, s.Apply(mkInFull("1", "host", "ollama", "run2", "running", "host", 200)), "gen-2 reused id (new run) should be a distinct workload")
	require.Equal(t, 2, s.Len())
}

// TestGenerationBeforeProvenance: a stale generation must be rejected before
// provenance is considered, in both interleavings (same identity key, differing
// createdAt).
func TestGenerationBeforeProvenance(t *testing.T) {
	// (a) A stale authoritative event must not replace a newer inferred record.
	s := New()
	s.Apply(mkInFull("1", "a", "ollama", "r", "running", "a", 200))
	s.ApplyInferred(mkInFull("1", "a", "ollama", "r", "failed", "a", 200)) // inferred at gen 200
	require.False(t, s.Apply(mkInFull("1", "a", "ollama", "r", "running", "a", 100)), "stale authoritative gen-100 must not replace the gen-200 record")
	r, _ := s.Get("a", "1")
	require.Equal(t, int64(200), r.CreatedAt)

	// (b) A stale inferred failure must not replace a newer authoritative running.
	s.Apply(mkInFull("2", "a", "ollama", "r", "running", "a", 200))
	require.False(t, s.ApplyInferred(mkInFull("2", "a", "ollama", "r", "failed", "a", 100)), "stale inferred gen-100 must not fail a gen-200 running")
	r, _ = s.Get("a", "2")
	require.Equal(t, "running", r.State, "record (%v)", r)
	require.Equal(t, int64(200), r.CreatedAt, "record (%v)", r)
}

func TestParseIncoming(t *testing.T) {
	info := mkIn("7", "laptop", "running", "pc", 4242).Info
	in, ok := ParseIncoming(info)
	require.True(t, ok, "valid workloadInfo should parse")
	require.Equal(t, "7", in.ID, "parsed (%v)", in)
	require.Equal(t, "laptop", in.Origin, "parsed (%v)", in)
	require.Equal(t, "running", in.State, "parsed (%v)", in)
	require.Equal(t, "pc", in.ScheduledOn, "parsed (%v)", in)
	require.Equal(t, int64(4242), in.CreatedAt, "parsed (%v)", in)

	_, ok = ParseIncoming([]byte(`{"originatedFrom":"laptop","state":"running"}`))
	require.False(t, ok, "missing id should not parse")
	_, ok = ParseIncoming([]byte(`{"id":"1","state":"running"}`))
	require.False(t, ok, "missing originatedFrom should not parse")
	_, ok = ParseIncoming([]byte(`not json`))
	require.False(t, ok, "invalid JSON should not parse")
}
