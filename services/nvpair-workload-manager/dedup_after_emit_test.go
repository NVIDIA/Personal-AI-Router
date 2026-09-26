// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"testing"
)

// TestInterNodeDedupRecordedOnlyAfterSuccessfulEmit: the old code recorded the
// dedup key before the broker emit, so a failed emit (500) was followed by the
// peer's retry being swallowed as a duplicate — the event was lost with no
// anti-entropy repair. Now the first (failed) attempt answers 500 without
// recording the key, and the retry is emitted to the broker exactly once.
func TestInterNodeDedupRecordedOnlyAfterSuccessfulEmit(t *testing.T) {
	self, peer := newPinnedPeerMeshes(t)
	dedup := newDedupIndex(100)

	var emitted int
	failEmit := true
	srv := NewServer(0, dedup, self,
		func(w *Workload) error {
			if failEmit {
				return fmt.Errorf("broker gone")
			}
			emitted++
			return nil
		},
		func(workloadID, nodeID string) error { return nil },
	)
	post := serveEventsOverMTLS(t, srv, self, peer)

	frame := []byte(`{"jsonrpc":"2.0","method":"workload:started","params":` +
		`{"workloadInfo":{"id":"7","model":"llama3","engine":"ollama",` +
		`"runId":"r1","state":"running","originatedFrom":"uuid-peer"}}}`)

	if code := post(frame); code != http.StatusInternalServerError {
		t.Fatalf("failed emit status = %d, want 500", code)
	}
	if emitted != 0 {
		t.Fatalf("emitted = %d after failed emit, want 0", emitted)
	}

	// The retry must NOT be treated as a duplicate: it reaches the broker.
	failEmit = false
	if code := post(frame); code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if emitted != 1 {
		t.Fatalf("emitted = %d after retry, want 1", emitted)
	}

	// And a genuine duplicate afterwards is still deduplicated.
	if code := post(frame); code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200", code)
	}
	if emitted != 1 {
		t.Fatalf("emitted = %d after duplicate, want still 1", emitted)
	}
}

// TestInterNodeRemoveDedupRecordedOnlyAfterSuccessfulEmit: the removal path
// had the same record-before-emit flaw — a failed workloads:remove emit would
// swallow the retry and leave the ghost workload in the broker catalog.
func TestInterNodeRemoveDedupRecordedOnlyAfterSuccessfulEmit(t *testing.T) {
	self, peer := newPinnedPeerMeshes(t)
	dedup := newDedupIndex(100)

	var emitted int
	failEmit := true
	srv := NewServer(0, dedup, self,
		func(w *Workload) error { return nil },
		func(workloadID, nodeID string) error {
			if failEmit {
				return fmt.Errorf("broker gone")
			}
			emitted++
			return nil
		},
	)
	post := serveEventsOverMTLS(t, srv, self, peer)

	frame := []byte(`{"jsonrpc":"2.0","method":"workloads:remove",` +
		`"params":{"workloadId":"7","originatedFrom":"uuid-peer"}}`)

	if code := post(frame); code != http.StatusInternalServerError {
		t.Fatalf("failed remove status = %d, want 500", code)
	}
	failEmit = false
	if code := post(frame); code != http.StatusOK {
		t.Fatalf("remove retry status = %d, want 200", code)
	}
	if emitted != 1 {
		t.Fatalf("remove emitted = %d after retry, want 1", emitted)
	}
	if code := post(frame); code != http.StatusOK {
		t.Fatalf("remove duplicate status = %d, want 200", code)
	}
	if emitted != 1 {
		t.Fatalf("remove emitted = %d after duplicate, want still 1", emitted)
	}
}
