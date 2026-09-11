// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"nvpair-shared/appdir"
)

// withIsolatedAppdir points the shared appdir (and thus the entries file) at a
// fresh temp dir for the duration of the test.
func withIsolatedAppdir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func entriesFilePath(t *testing.T) string {
	t.Helper()
	path, err := appdir.Path(entriesFile)
	if err != nil {
		t.Fatalf("appdir path: %v", err)
	}
	return path
}

func TestEntriesRoundTrip(t *testing.T) {
	withIsolatedAppdir(t)
	entries := []ManualEntry{
		{Address: "h1", Name: "one"},
		{OpenAIBaseURL: "http://h2:8888/v1", Name: "two"},
	}
	if err := saveEntries(entries); err != nil {
		t.Fatalf("saveEntries: %v", err)
	}
	got, err := loadEntries()
	if err != nil {
		t.Fatalf("loadEntries: %v", err)
	}
	if len(got) != 2 || got[0].Address != "h1" || got[1].OpenAIBaseURL != "http://h2:8888/v1" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestSaveEntriesIsAtomicNoTmpLeftBehind(t *testing.T) {
	withIsolatedAppdir(t)
	if err := saveEntries([]ManualEntry{{Address: "h1"}}); err != nil {
		t.Fatalf("saveEntries: %v", err)
	}
	dir := filepath.Dir(entriesFilePath(t))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestLoadEntriesMissingFile(t *testing.T) {
	withIsolatedAppdir(t)
	entries, err := loadEntries()
	if err != nil || entries != nil {
		t.Fatalf("missing file = %v, %v; want nil, nil", entries, err)
	}
}

func TestLoadEntriesCorrupt(t *testing.T) {
	withIsolatedAppdir(t)
	path := entriesFilePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEntries(); err == nil {
		t.Fatal("corrupt file: expected error")
	}
}

// TestManagerPersistsAcrossRestart covers the durable-list contract: an entry
// added through node/add survives a "restart" (a fresh manager loading from
// disk), and a node/remove is persisted too.
func TestManagerPersistsAcrossRestart(t *testing.T) {
	withIsolatedAppdir(t)

	m1, rw1, _ := newTestManager()
	m1.handleMessage(requestMessage(1, "node/add", ManualEntry{Address: "h1", Name: "one"}))
	_ = readCaptureUntil(t, rw1, responseWithID(1))

	m2, rw2, _ := newTestManager()
	m2.loadPersistedEntries()
	if got := m2.listNodes(); len(got) != 1 || got[0].ID != "one" {
		t.Fatalf("restored entries = %+v, want one", got)
	}

	m2.handleMessage(requestMessage(2, "node/remove", map[string]string{"id": "one"}))
	_ = readCaptureUntil(t, rw2, responseWithID(2))
	got, err := loadEntries()
	if err != nil {
		t.Fatalf("loadEntries after remove: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("entry still persisted after remove: %+v", got)
	}
}
