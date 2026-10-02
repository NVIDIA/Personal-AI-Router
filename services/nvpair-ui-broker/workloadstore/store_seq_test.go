// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadstore

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// mkSeqInfo marshals a workloadInfo for one fixed identity. A zero seq leaves
// the field out, as an origin that does not number its events would.
func mkSeqInfo(t *testing.T, id, state, scheduledOn string, createdAt, completedAt, seq int64) json.RawMessage {
	t.Helper()
	m := map[string]any{
		"id":             id,
		"originatedFrom": "origin",
		"engine":         "ollama",
		"runId":          "run",
		"state":          state,
		"scheduledOn":    scheduledOn,
		"createdAt":      createdAt,
		"model":          "m",
	}
	if completedAt > 0 {
		m["completedAt"] = completedAt
	}
	if seq > 0 {
		m["seq"] = seq
	}
	info, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal workloadInfo: %v", err)
	}
	return info
}

// mkSeq builds an event through ParseIncoming, so the store reads seq from the
// wire field exactly as it does in the broker.
func mkSeq(t *testing.T, id, state, scheduledOn string, seq int64) Incoming {
	t.Helper()
	info := mkSeqInfo(t, id, state, scheduledOn, 100, 0, seq)
	in, ok := ParseIncoming(info)
	if !ok {
		t.Fatalf("ParseIncoming rejected %s", info)
	}
	return in
}

func mustApply(t *testing.T, s *Store, in Incoming) {
	t.Helper()
	if !s.Apply(in) {
		t.Fatalf("setup event %s was rejected", in.Info)
	}
}

func mustGet(t *testing.T, s *Store, id string) Record {
	t.Helper()
	r, ok := s.Get("origin", id)
	if !ok {
		t.Fatalf("record origin/%s is missing", id)
	}
	return r
}

// TestApplySeqRejectsDelayedRepoint is the race the seq rule exists for: the
// origin's heartbeat captures a queued record, the job is re-pointed to another
// node, and the heartbeat's older copy is delivered afterwards. State rank
// cannot tell the two queued events apart, so the stale node used to win.
func TestApplySeqRejectsDelayedRepoint(t *testing.T) {
	s := New()
	mustApply(t, s, mkSeq(t, "1", "queued", "nodeA", 1))
	mustApply(t, s, mkSeq(t, "1", "queued", "nodeB", 2))

	if s.Apply(mkSeq(t, "1", "queued", "nodeA", 1)) {
		t.Fatal("the delayed copy of the nodeA event must be rejected")
	}
	if r := mustGet(t, s, "1"); r.ScheduledOn != "nodeB" || r.Seq != 2 {
		t.Fatalf("record = %+v, want queued on nodeB at seq 2", r)
	}
}

// TestApplySeqOrdersAcrossRank: a higher seq is newer even when its state ranks
// lower, and a lower seq is older even when its state ranks higher.
func TestApplySeqOrdersAcrossRank(t *testing.T) {
	t.Run("newer queued replaces older running", func(t *testing.T) {
		s := New()
		mustApply(t, s, mkSeq(t, "1", "running", "nodeA", 3))

		if !s.Apply(mkSeq(t, "1", "queued", "nodeB", 4)) {
			t.Fatal("queued at seq 4 is newer than running at seq 3 and must apply")
		}
		if r := mustGet(t, s, "1"); r.State != "queued" || r.ScheduledOn != "nodeB" {
			t.Fatalf("record = %+v, want queued on nodeB", r)
		}
	})

	t.Run("older running delivered late is rejected", func(t *testing.T) {
		s := New()
		mustApply(t, s, mkSeq(t, "1", "queued", "nodeB", 4))

		if s.Apply(mkSeq(t, "1", "running", "nodeA", 3)) {
			t.Fatal("running at seq 3 is older than queued at seq 4 and must be rejected")
		}
		if r := mustGet(t, s, "1"); r.State != "queued" || r.ScheduledOn != "nodeB" {
			t.Fatalf("record = %+v, want queued on nodeB", r)
		}
	})
}

// TestApplySeqKeepsTerminalFinal: no later event replaces a terminal record,
// whatever seq it carries.
func TestApplySeqKeepsTerminalFinal(t *testing.T) {
	s := New()
	mustApply(t, s, mkSeq(t, "1", "completed", "nodeA", 5))

	if s.Apply(mkSeq(t, "1", "queued", "nodeB", 6)) {
		t.Fatal("an event after the terminal must be rejected")
	}
	if r := mustGet(t, s, "1"); r.State != "completed" || !r.Terminal {
		t.Fatalf("record = %+v, want terminal completed", r)
	}
}

// TestApplySeqDuplicateIsStillSighting: a re-asserted event changes nothing but
// still counts as the origin speaking, which keeps the staleness sweep off a
// job the origin keeps re-asserting.
func TestApplySeqDuplicateIsStillSighting(t *testing.T) {
	s := New()
	clock := time.UnixMilli(1_000)
	s.now = func() time.Time { return clock }
	mustApply(t, s, mkSeq(t, "1", "running", "nodeA", 2))
	clock = clock.Add(time.Minute)

	if s.Apply(mkSeq(t, "1", "running", "nodeA", 2)) {
		t.Fatal("a re-asserted seq must not count as a change")
	}
	if r := mustGet(t, s, "1"); !r.LastSeen.Equal(clock) {
		t.Fatalf("LastSeen = %v, want %v", r.LastSeen, clock)
	}
}

// TestApplyUnsequencedUsesRank: when either side has no seq, the rank rule
// decides as it always has.
func TestApplyUnsequencedUsesRank(t *testing.T) {
	t.Run("no seq on either event", func(t *testing.T) {
		s := New()
		mustApply(t, s, mkSeq(t, "1", "running", "nodeA", 0))

		if s.Apply(mkSeq(t, "1", "queued", "nodeB", 0)) {
			t.Fatal("queued after running is a rank regression and must be rejected")
		}
	})

	t.Run("seq on the incoming event only", func(t *testing.T) {
		s := New()
		mustApply(t, s, mkSeq(t, "1", "running", "nodeA", 0))

		if s.Apply(mkSeq(t, "1", "queued", "nodeB", 4)) {
			t.Fatal("against an unsequenced record, queued after running must be rejected by rank")
		}
	})

	t.Run("seq on the stored record only", func(t *testing.T) {
		s := New()
		mustApply(t, s, mkSeq(t, "1", "queued", "nodeA", 4))

		if !s.Apply(mkSeq(t, "1", "running", "nodeA", 0)) {
			t.Fatal("running after queued is forward by rank and must apply")
		}
	})
}

// TestApplySeqLeavesProvenanceAlone: a guess copies the seq of the record it was
// made from, so a guess and the origin's event at that seq are decided by
// provenance.
func TestApplySeqLeavesProvenanceAlone(t *testing.T) {
	t.Run("node-loss guess fails a sequenced running job", func(t *testing.T) {
		s := New()
		mustApply(t, s, mkSeq(t, "1", "running", "nodeA", 3))

		if !s.ApplyInferred(mkSeq(t, "1", "failed", "nodeA", 3)) {
			t.Fatal("the guess must still fail a running job that carries the same seq")
		}
	})

	t.Run("origin re-assertion overrides a wrong guess", func(t *testing.T) {
		s := New()
		mustApply(t, s, mkSeq(t, "1", "running", "nodeA", 3))
		if !s.ApplyInferred(mkSeq(t, "1", "failed", "nodeA", 3)) {
			t.Fatal("setup guess was rejected")
		}

		if !s.Apply(mkSeq(t, "1", "running", "nodeA", 3)) {
			t.Fatal("the origin's re-assertion at an equal seq must override the guess")
		}
		if r := mustGet(t, s, "1"); r.State != "running" || r.Inferred {
			t.Fatalf("record = %+v, want authoritative running", r)
		}
	})
}

// TestApplySeqRejectsDelayedEventOverGuess: a guess keeps the seq of the record
// it replaced, so a delayed origin event older than that record cannot override
// the guess and move the job back to a node it had already left, while a newer
// one still does.
func TestApplySeqRejectsDelayedEventOverGuess(t *testing.T) {
	guessed := func(t *testing.T) *Store {
		t.Helper()
		s := New()
		mustApply(t, s, mkSeq(t, "1", "queued", "nodeA", 1))
		mustApply(t, s, mkSeq(t, "1", "queued", "nodeB", 2))
		if !s.ApplyInferred(mkSeq(t, "1", "failed", "nodeB", 2)) {
			t.Fatal("setup guess was rejected")
		}
		return s
	}

	t.Run("older event is rejected", func(t *testing.T) {
		s := guessed(t)

		if s.Apply(mkSeq(t, "1", "queued", "nodeA", 1)) {
			t.Fatal("the delayed nodeA event is older than the guessed record and must be rejected")
		}
		if r := mustGet(t, s, "1"); r.State != "failed" || !r.Inferred || r.ScheduledOn != "nodeB" {
			t.Fatalf("record = %+v, want the inferred failure on nodeB", r)
		}
	})

	t.Run("newer event overrides the guess", func(t *testing.T) {
		s := guessed(t)

		if !s.Apply(mkSeq(t, "1", "running", "nodeB", 3)) {
			t.Fatal("an origin event newer than the guessed record must override it")
		}
		if r := mustGet(t, s, "1"); r.State != "running" || r.Inferred || r.Seq != 3 {
			t.Fatalf("record = %+v, want authoritative running at seq 3", r)
		}
	})
}

// TestLoadKeepsSeq: Load rebuilds records from their persisted Info, so a
// restored terminal keeps the seq it was stored with.
func TestLoadKeepsSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	writer := newStoreAt(path, testNow)
	info := mkSeqInfo(t, "1", "completed", "nodeA", testNow-1_000, testNow-100, 7)
	in, ok := ParseIncoming(info)
	if !ok {
		t.Fatalf("ParseIncoming rejected %s", info)
	}
	mustApply(t, writer, in)
	if err := writer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	s := newStoreAt(path, testNow)
	if err := s.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if r := mustGet(t, s, "1"); r.Seq != 7 {
		t.Fatalf("restored seq = %d, want 7", r.Seq)
	}
}
