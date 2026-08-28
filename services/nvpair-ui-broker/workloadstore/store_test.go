// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadstore

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
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
	if !s.Apply(mkIn("9", "laptop", "failed", "pc", 100)) {
		t.Fatal("first sighting (failed) should be accepted")
	}
	if s.Apply(mkIn("9", "laptop", "running", "pc", 100)) {
		t.Fatal("running after failed (same generation) must be rejected")
	}
	r, ok := s.Get("laptop", "9")
	if !ok || r.State != "failed" || !r.Terminal {
		t.Fatalf("final record = %+v, want terminal failed", r)
	}
}

// TestApplyRunningThenFailed is the in-order path: running then failed both
// apply and the workload ends terminal.
func TestApplyRunningThenFailed(t *testing.T) {
	s := New()
	if !s.Apply(mkIn("9", "laptop", "running", "pc", 100)) {
		t.Fatal("running should be accepted")
	}
	if !s.Apply(mkIn("9", "laptop", "failed", "pc", 100)) {
		t.Fatal("failed after running should be accepted (forward progress)")
	}
	r, _ := s.Get("laptop", "9")
	if r.State != "failed" || !r.Terminal {
		t.Fatalf("final record = %+v, want terminal failed", r)
	}
}

// TestApplyNewerGenerationReplaces is epoch protection: a restarted proxy
// reuses id "5" (createdAt resets forward); its newer-createdAt running must
// replace the prior generation's terminal record, not be mistaken for it.
func TestApplyNewerGenerationReplaces(t *testing.T) {
	s := New()
	if !s.Apply(mkIn("5", "pc", "failed", "pc", 100)) {
		t.Fatal("old-generation terminal should be accepted")
	}
	if !s.Apply(mkIn("5", "pc", "running", "laptop", 200)) {
		t.Fatal("newer-generation running must replace the old terminal")
	}
	r, _ := s.Get("pc", "5")
	if r.State != "running" || r.CreatedAt != 200 || r.ScheduledOn != "laptop" || r.Terminal {
		t.Fatalf("record = %+v, want running gen=200 scheduledOn=laptop", r)
	}
}

// TestApplyOlderGenerationRejected: a late event from a prior generation (older
// createdAt) must never touch the current generation's record — even a terminal
// one, which would otherwise look like "forward progress" by state rank.
func TestApplyOlderGenerationRejected(t *testing.T) {
	s := New()
	if !s.Apply(mkIn("5", "pc", "running", "pc", 200)) {
		t.Fatal("current-generation running should be accepted")
	}
	if s.Apply(mkIn("5", "pc", "completed", "pc", 100)) {
		t.Fatal("older-generation event must be rejected despite higher state rank")
	}
	r, _ := s.Get("pc", "5")
	if r.CreatedAt != 200 || r.State != "running" {
		t.Fatalf("record = %+v, want running gen=200", r)
	}
}

// TestApplyEqualRankReemit: an identical re-emit is a no-op, but a same-state
// re-emit that re-points scheduledOn (a failover) is a meaningful change.
func TestApplyEqualRankReemit(t *testing.T) {
	s := New()
	if !s.Apply(mkIn("1", "a", "running", "node1", 100)) {
		t.Fatal("initial running should be accepted")
	}
	if s.Apply(mkIn("1", "a", "running", "node1", 100)) {
		t.Fatal("identical re-emit should be a no-op")
	}
	if !s.Apply(mkIn("1", "a", "running", "node2", 100)) {
		t.Fatal("running re-pointed to a new scheduledOn should be accepted")
	}
	r, _ := s.Get("a", "1")
	if r.ScheduledOn != "node2" {
		t.Fatalf("scheduledOn = %q, want node2", r.ScheduledOn)
	}
}

// TestApplyCrossNodeIsolation: the same numeric id from two origins is two
// distinct workloads and must never merge against each other.
func TestApplyCrossNodeIsolation(t *testing.T) {
	s := New()
	if !s.Apply(mkIn("1", "A", "failed", "A", 100)) {
		t.Fatal("A/1 should be accepted")
	}
	if !s.Apply(mkIn("1", "B", "running", "B", 100)) {
		t.Fatal("B/1 has the same id but a different origin; must be independent")
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	if a, _ := s.Get("A", "1"); a.State != "failed" {
		t.Fatalf("A/1 state = %q, want failed", a.State)
	}
	if b, _ := s.Get("B", "1"); b.State != "running" {
		t.Fatalf("B/1 state = %q, want running", b.State)
	}
}

func TestApplyRejectsMalformed(t *testing.T) {
	s := New()
	if s.Apply(Incoming{ID: "", Origin: "a", State: "running"}) {
		t.Fatal("missing id must be rejected")
	}
	if s.Apply(Incoming{ID: "1", Origin: "", State: "running"}) {
		t.Fatal("missing origin must be rejected")
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}

func TestRemove(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	if !s.Remove("a", "1") {
		t.Fatal("Remove of present entry should report true")
	}
	if s.Remove("a", "1") {
		t.Fatal("Remove of absent entry should report false")
	}
	if _, ok := s.Get("a", "1"); ok {
		t.Fatal("entry should be gone after Remove")
	}
}

func TestSnapshotOrderedByCreatedAt(t *testing.T) {
	s := New()
	s.Apply(mkIn("2", "a", "running", "a", 300))
	s.Apply(mkIn("1", "a", "running", "a", 100))
	s.Apply(mkIn("1", "b", "running", "b", 200))

	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}
	var got []int64
	for _, raw := range snap {
		var hdr struct {
			CreatedAt int64 `json:"createdAt"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			t.Fatalf("bad snapshot entry: %v", err)
		}
		got = append(got, hdr.CreatedAt)
	}
	want := []int64{100, 200, 300}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("snapshot order = %v, want %v", got, want)
		}
	}
}

func TestActiveSnapshotExcludesTerminalHistory(t *testing.T) {
	s := New()
	s.Apply(mkIn("completed", "a", "completed", "a", 100))
	s.Apply(mkIn("queued", "a", "queued", "b", 200))
	s.Apply(mkIn("running", "b", "running", "a", 300))
	s.Apply(mkIn("failed", "b", "failed", "b", 400))

	snap := s.ActiveSnapshot()
	if len(snap) != 2 {
		t.Fatalf("active snapshot len = %d, want 2", len(snap))
	}
	var ids []string
	for _, raw := range snap {
		var hdr struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			t.Fatalf("bad active snapshot entry: %v", err)
		}
		if hdr.State != "queued" && hdr.State != "running" {
			t.Fatalf("active snapshot included terminal state %q", hdr.State)
		}
		ids = append(ids, hdr.ID)
	}
	if !sameIDSet(ids, []string{"queued", "running"}) {
		t.Fatalf("active snapshot ids = %v, want [queued running]", ids)
	}
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
	if len(active) != 2 {
		t.Fatalf("ActiveForNode(pc) len = %d, want 2", len(active))
	}
	for _, r := range active {
		if r.Terminal {
			t.Fatalf("ActiveForNode returned a terminal record: %+v", r)
		}
		if r.Origin != "pc" && r.ScheduledOn != "pc" {
			t.Fatalf("ActiveForNode returned an unrelated record: %+v", r)
		}
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

	if got := replayIDSet(s.ReplayForNode("host", window)); !sameIDSet(got, []string{"1", "2", "4"}) {
		t.Fatalf("replay set = %v, want [1 2 4] (active + recent terminals; no peer/inferred)", got)
	}

	// Age everything past the window: terminals drop; the still-running record
	// (non-terminal) is always replayed regardless of age.
	clock += window + 1
	if got := replayIDSet(s.ReplayForNode("host", window)); !sameIDSet(got, []string{"1"}) {
		t.Fatalf("post-window replay set = %v, want [1] (only the still-active record)", got)
	}
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
	if err := writer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// A fresh store loads at the same wall clock. Load stamps LastUpdated=now on
	// BOTH restored records, but completedAt is preserved in Info — so keying the
	// replay window on completion time (not LastUpdated) keeps the 1000s-old
	// terminal out while still replaying the 30s-old one.
	s := newStoreAt(path, testNow)
	if err := s.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	got := replayIDSet(s.ReplayForNode("host", window))
	if !sameIDSet(got, []string{"recent"}) {
		t.Fatalf("post-load replay set = %v, want [recent] (old history must not look recent after Load)", got)
	}
}

func replayIDSet(recs []Record) []string {
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids
}

func sameIDSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		seen[id]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// TestInferredMarksRunningFailed: the node-loss sweep may fail an authoritative
// running job.
func TestInferredMarksRunningFailed(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	if !s.ApplyInferred(mkIn("1", "a", "failed", "a", 100)) {
		t.Fatal("inferred failed should mark a running job failed")
	}
	r, _ := s.Get("a", "1")
	if r.State != "failed" || !r.Terminal || !r.Inferred {
		t.Fatalf("record = %+v, want inferred terminal failed", r)
	}
}

// TestAuthoritativeOverridesInferred is the blip-and-return case: a node
// wrongly inferred failed, then the origin re-asserts running (same generation).
// The authoritative event must win.
func TestAuthoritativeOverridesInferred(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	s.ApplyInferred(mkIn("1", "a", "failed", "a", 100)) // node-loss guess
	if !s.Apply(mkIn("1", "a", "running", "a", 100)) {
		t.Fatal("authoritative running must override an inferred failed")
	}
	r, _ := s.Get("a", "1")
	if r.State != "running" || r.Terminal || r.Inferred {
		t.Fatalf("record = %+v, want authoritative running", r)
	}
}

// TestInferredCannotOverrideAuthoritativeTerminal: a guess must never overwrite
// the origin's real terminal.
func TestInferredCannotOverrideAuthoritativeTerminal(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "completed", "a", 100))
	if s.ApplyInferred(mkIn("1", "a", "failed", "a", 100)) {
		t.Fatal("inferred failed must not override an authoritative terminal")
	}
	r, _ := s.Get("a", "1")
	if r.State != "completed" || r.Inferred {
		t.Fatalf("record = %+v, want authoritative completed", r)
	}
}

// TestAuthoritativeTerminalOverridesInferred: the origin's real terminal
// replaces a prior inferred one.
func TestAuthoritativeTerminalOverridesInferred(t *testing.T) {
	s := New()
	s.Apply(mkIn("1", "a", "running", "a", 100))
	s.ApplyInferred(mkIn("1", "a", "failed", "a", 100))
	if !s.Apply(mkIn("1", "a", "completed", "a", 100)) {
		t.Fatal("origin's authoritative terminal must override an inferred one")
	}
	r, _ := s.Get("a", "1")
	if r.State != "completed" || r.Inferred {
		t.Fatalf("record = %+v, want authoritative completed", r)
	}
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
	if !s.Apply(mkInFull("1", "host", "ollama", "ro", "running", "host", 100)) {
		t.Fatal("ollama/1 should be accepted")
	}
	if !s.Apply(mkInFull("1", "host", "lmstudio", "rl", "running", "host", 200)) {
		t.Fatal("lmstudio/1 shares the numeric id but a different engine; must be distinct")
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (engine distinguishes concurrent same-id jobs)", s.Len())
	}
}

// TestRestartReuseKeepsHistory: after a proxy restart reuses id "1" (new runId),
// the prior generation's terminal history must be preserved, not overwritten.
func TestRestartReuseKeepsHistory(t *testing.T) {
	s := New()
	if !s.Apply(mkTermFull("1", "host", "ollama", "run1", 100, 150)) {
		t.Fatal("gen-1 terminal should be accepted")
	}
	if !s.Apply(mkInFull("1", "host", "ollama", "run2", "running", "host", 200)) {
		t.Fatal("gen-2 reused id (new run) should be a distinct workload")
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (runId preserves the prior generation's history)", s.Len())
	}
}

// TestGenerationBeforeProvenance: a stale generation must be rejected before
// provenance is considered, in both interleavings (same identity key, differing
// createdAt).
func TestGenerationBeforeProvenance(t *testing.T) {
	// (a) A stale authoritative event must not replace a newer inferred record.
	s := New()
	s.Apply(mkInFull("1", "a", "ollama", "r", "running", "a", 200))
	s.ApplyInferred(mkInFull("1", "a", "ollama", "r", "failed", "a", 200)) // inferred at gen 200
	if s.Apply(mkInFull("1", "a", "ollama", "r", "running", "a", 100)) {
		t.Fatal("stale authoritative gen-100 must not replace the gen-200 record")
	}
	if r, _ := s.Get("a", "1"); r.CreatedAt != 200 {
		t.Fatalf("createdAt = %d, want 200 (older generation rejected)", r.CreatedAt)
	}

	// (b) A stale inferred failure must not replace a newer authoritative running.
	s.Apply(mkInFull("2", "a", "ollama", "r", "running", "a", 200))
	if s.ApplyInferred(mkInFull("2", "a", "ollama", "r", "failed", "a", 100)) {
		t.Fatal("stale inferred gen-100 must not fail a gen-200 running")
	}
	if r, _ := s.Get("a", "2"); r.State != "running" || r.CreatedAt != 200 {
		t.Fatalf("record = %+v, want running gen 200", r)
	}
}

func TestParseIncoming(t *testing.T) {
	info := mkIn("7", "laptop", "running", "pc", 4242).Info
	in, ok := ParseIncoming(info)
	if !ok {
		t.Fatal("valid workloadInfo should parse")
	}
	if in.ID != "7" || in.Origin != "laptop" || in.State != "running" || in.ScheduledOn != "pc" || in.CreatedAt != 4242 {
		t.Fatalf("parsed = %+v", in)
	}

	if _, ok := ParseIncoming([]byte(`{"originatedFrom":"laptop","state":"running"}`)); ok {
		t.Fatal("missing id should not parse")
	}
	if _, ok := ParseIncoming([]byte(`{"id":"1","state":"running"}`)); ok {
		t.Fatal("missing originatedFrom should not parse")
	}
	if _, ok := ParseIncoming([]byte(`not json`)); ok {
		t.Fatal("invalid JSON should not parse")
	}
}
