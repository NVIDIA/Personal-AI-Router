// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testNow is a realistic epoch-ms baseline so records with completedAt near it
// are well within the default age cap (toy values like 100 would be pruned as
// "ancient" against a real clock).
const testNow int64 = 1_700_000_000_000

// newStoreAt builds a persistent store pinned to a fixed clock for
// deterministic age/eviction behavior.
func newStoreAt(path string, now int64) *Store {
	s := New().WithPersistence(path)
	at := time.UnixMilli(now)
	s.now = func() time.Time { return at }
	return s
}

// mkTerm builds a terminal (failed) Incoming with a completedAt, via
// ParseIncoming so Info and the projected fields stay consistent.
func mkTerm(id, origin string, createdAt, completedAt int64) Incoming {
	m := map[string]any{
		"id":             id,
		"originatedFrom": origin,
		"state":          "failed",
		"scheduledOn":    origin,
		"createdAt":      createdAt,
		"completedAt":    completedAt,
		"model":          "granite-embedding:latest",
		"engine":         "ollama",
	}
	b, _ := json.Marshal(m)
	in, _ := ParseIncoming(b)
	return in
}

func TestPersistenceRoundTripTerminalOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-150))
	s.Apply(mkTerm("2", "b", testNow-1000, testNow-160))
	s.Apply(mkIn("3", "c", "running", "c", testNow-1000)) // active — must not persist
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	s2 := newStoreAt(path, testNow)
	if err := s2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if s2.Len() != 2 {
		t.Fatalf("loaded %d records, want 2 (terminal only)", s2.Len())
	}
	if _, ok := s2.Get("a", "1"); !ok {
		t.Fatal("terminal a/1 should have been persisted")
	}
	if _, ok := s2.Get("c", "3"); ok {
		t.Fatal("active c/3 must not be persisted")
	}
}

func TestFlushCoalescesOnDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)

	// A running-only store isn't dirty → flush writes nothing.
	s.Apply(mkIn("1", "a", "running", "a", testNow-1000))
	if s.dirty {
		t.Fatal("non-terminal apply must not mark dirty")
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no file should be written before any terminal record")
	}

	// A terminal transition marks dirty and flush writes it.
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-150))
	if !s.dirty {
		t.Fatal("terminal apply should mark dirty")
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if s.dirty {
		t.Fatal("dirty should be cleared after a successful flush")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist after terminal flush: %v", err)
	}
}

func TestCountCapEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.historyCap = 2

	s.Apply(mkTerm("1", "a", testNow-1000, testNow-300))
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-200))
	s.Apply(mkTerm("3", "a", testNow-1000, testNow-100))
	if err := s.Flush(); err != nil { // prune runs during flush
		t.Fatalf("flush: %v", err)
	}
	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2 after cap eviction", s.Len())
	}
	if _, ok := s.Get("a", "1"); ok {
		t.Fatal("oldest (completedAt=testNow-300) should be evicted")
	}
	if _, ok := s.Get("a", "3"); !ok {
		t.Fatal("newest (completedAt=testNow-100) should be kept")
	}
}

func TestAgeCapEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := New().WithPersistence(path)
	s.maxAgeMs = 1000
	s.now = func() time.Time { return time.UnixMilli(10000) }

	s.Apply(mkTerm("old", "a", 0, 8000)) // age 2000 > 1000 → evicted
	s.Apply(mkTerm("new", "a", 0, 9500)) // age 500 → kept
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, ok := s.Get("a", "old"); ok {
		t.Fatal("record older than maxAge should be pruned")
	}
	if _, ok := s.Get("a", "new"); !ok {
		t.Fatal("record within maxAge should remain")
	}
}

func TestCheckpointRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-100))
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-50))
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("primary should exist after checkpoint: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation .1 should exist after checkpoint: %v", err)
	}
}

func TestLoadFallsBackOnCorruptPrimary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-100))
	if err := s.Flush(); err != nil { // primary = {1}
		t.Fatalf("flush: %v", err)
	}
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-50))
	if err := s.Checkpoint(); err != nil { // primary = {1,2}, .1 = {1}
		t.Fatalf("checkpoint: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("corrupt primary: %v", err)
	}

	s2 := newStoreAt(path, testNow)
	_ = s2.Load() // parse error on primary is non-fatal; falls back to .1
	if _, ok := s2.Get("a", "1"); !ok {
		t.Fatal("should have recovered a/1 from rotation .1")
	}
}

func TestLoadMissingFileIsClean(t *testing.T) {
	s := New().WithPersistence(filepath.Join(t.TempDir(), "absent.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("missing file should load cleanly, got: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("len = %d, want 0", s.Len())
	}
}

func TestInferredTerminalNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-100))         // authoritative terminal → persisted
	s.ApplyInferred(mkTerm("1", "a", testNow-1000, testNow-100)) // inferred terminal → not persisted
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	s2 := newStoreAt(path, testNow)
	if err := s2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s2.Get("a", "2"); !ok {
		t.Fatal("authoritative terminal should persist")
	}
	if _, ok := s2.Get("a", "1"); ok {
		t.Fatal("inferred terminal must not be persisted")
	}
}

func TestDisabledPersistenceNoops(t *testing.T) {
	s := New() // no path
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-100))
	if err := s.Flush(); err != nil {
		t.Fatalf("flush with persistence off should be a no-op: %v", err)
	}
	if err := s.Load(); err != nil {
		t.Fatalf("load with persistence off should be a no-op: %v", err)
	}
}
