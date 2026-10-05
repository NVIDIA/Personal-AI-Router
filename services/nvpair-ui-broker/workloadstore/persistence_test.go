// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, s.Flush(), "flush")

	s2 := newStoreAt(path, testNow)
	require.NoError(t, s2.Load(), "load")
	require.Equal(t, 2, s2.Len(), "loaded")
	{
		_, ok := s2.Get("a", "1")
		require.True(t, ok, "terminal a/1 should have been persisted")
	}
	{
		_, ok := s2.Get("c", "3")
		require.False(t, ok, "active c/3 must not be persisted")
	}
}

func TestFlushCoalescesOnDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)

	// A running-only store isn't dirty → flush writes nothing.
	s.Apply(mkIn("1", "a", "running", "a", testNow-1000))
	require.False(t, s.dirty, "non-terminal apply must not mark dirty")
	require.NoError(t, s.Flush(), "flush")
	{
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err), "no file should be written before any terminal record")
	}

	// A terminal transition marks dirty and flush writes it.
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-150))
	require.True(t, s.dirty, "terminal apply should mark dirty")
	require.NoError(t, s.Flush(), "flush")
	require.False(t, s.dirty, "dirty should be cleared after a successful flush")
	{
		_, err := os.Stat(path)
		require.NoError(t, err, "file should exist after terminal flush")
	}
}

func TestCountCapEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.historyCap = 2

	s.Apply(mkTerm("1", "a", testNow-1000, testNow-300))
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-200))
	s.Apply(mkTerm("3", "a", testNow-1000, testNow-100))
	// prune runs during flush
	require.NoError(t, s.Flush(), "flush")
	require.Equal(t, 2, s.Len())
	{
		_, ok := s.Get("a", "1")
		require.False(t, ok, "oldest (completedAt=testNow-300) should be evicted")
	}
	{
		_, ok := s.Get("a", "3")
		require.True(t, ok, "newest (completedAt=testNow-100) should be kept")
	}
}

func TestAgeCapEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := New().WithPersistence(path)
	s.maxAgeMs = 1000
	s.now = func() time.Time { return time.UnixMilli(10000) }

	s.Apply(mkTerm("old", "a", 0, 8000)) // age 2000 > 1000 → evicted
	s.Apply(mkTerm("new", "a", 0, 9500)) // age 500 → kept
	require.NoError(t, s.Flush(), "flush")
	{
		_, ok := s.Get("a", "old")
		require.False(t, ok, "record older than maxAge should be pruned")
	}
	{
		_, ok := s.Get("a", "new")
		require.True(t, ok, "record within maxAge should remain")
	}
}

func TestCheckpointRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-100))
	require.NoError(t, s.Flush(), "flush")
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-50))
	require.NoError(t, s.Checkpoint(), "checkpoint")
	{
		_, err := os.Stat(path)
		require.NoError(t, err, "primary should exist after checkpoint")
	}
	{
		_, err := os.Stat(path + ".1")
		require.NoError(t, err, "rotation .1 should exist after checkpoint")
	}
}

func TestLoadFallsBackOnCorruptPrimary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-100))
	// primary = {1}
	require.NoError(t, s.Flush(), "flush")
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-50))
	// primary = {1,2}, .1 = {1}
	require.NoError(t, s.Checkpoint(), "checkpoint")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600), "corrupt primary")

	s2 := newStoreAt(path, testNow)
	_ = s2.Load() // parse error on primary is non-fatal; falls back to .1
	{
		_, ok := s2.Get("a", "1")
		require.True(t, ok, "should have recovered a/1 from rotation .1")
	}
}

func TestLoadMissingFileIsClean(t *testing.T) {
	s := New().WithPersistence(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, s.Load(), "missing file should load cleanly")
	require.Equal(t, 0, s.Len())
}

func TestInferredTerminalNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wl.json")
	s := newStoreAt(path, testNow)
	s.Apply(mkTerm("2", "a", testNow-1000, testNow-100))         // authoritative terminal → persisted
	s.ApplyInferred(mkTerm("1", "a", testNow-1000, testNow-100)) // inferred terminal → not persisted
	require.NoError(t, s.Flush(), "flush")

	s2 := newStoreAt(path, testNow)
	require.NoError(t, s2.Load(), "load")
	{
		_, ok := s2.Get("a", "2")
		require.True(t, ok, "authoritative terminal should persist")
	}
	{
		_, ok := s2.Get("a", "1")
		require.False(t, ok, "inferred terminal must not be persisted")
	}
}

func TestDisabledPersistenceNoops(t *testing.T) {
	s := New() // no path
	s.Apply(mkTerm("1", "a", testNow-1000, testNow-100))
	require.NoError(t, s.Flush(), "flush with persistence off should be a no-op")
	require.NoError(t, s.Load(), "load with persistence off should be a no-op")
}
