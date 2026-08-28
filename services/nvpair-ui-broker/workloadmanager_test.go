// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"nvpair-ui-broker/workloadstore"
)

func storeIncoming(id, origin, engine, runID, state, scheduledOn string) workloadstore.Incoming {
	m := map[string]any{
		"id": id, "originatedFrom": origin, "engine": engine, "runId": runID,
		"state": state, "scheduledOn": scheduledOn, "createdAt": 1, "model": "m",
	}
	b, _ := json.Marshal(m)
	in, _ := workloadstore.ParseIncoming(b)
	return in
}

// TestActiveLocalReplayFrames verifies the rehydration set the broker replays to
// a (re)started workload-manager: this node's active AND recently-terminal
// local-origin workloads (so the manager's terminal re-sync window survives a
// restart) — never peer-origin jobs merely scheduled here (the origin is the
// single writer). The terminal time-window and the inferred-exclusion are
// covered at the store level (TestReplayForNode), where the clock is settable.
func TestActiveLocalReplayFrames(t *testing.T) {
	b := &Broker{workloads: workloadstore.New(), nodeID: "host"}

	b.workloads.Apply(storeIncoming("1", "host", "ollama", "r1", "running", "peer"))   // local-origin active → replay
	b.workloads.Apply(storeIncoming("2", "host", "ollama", "r1", "failed", "peer"))    // local-origin recent terminal → replay
	b.workloads.Apply(storeIncoming("3", "peer", "ollama", "r2", "running", "host"))   // peer-origin (scheduled here) → skip
	b.workloads.Apply(storeIncoming("4", "host", "lmstudio", "r3", "queued", "host"))  // local-origin active (other engine) → replay
	b.workloads.Apply(storeIncoming("5", "host", "ollama", "r4", "completed", "host")) // local-origin recent terminal → replay

	frames := b.activeLocalReplayFrames()
	if len(frames) != 4 {
		t.Fatalf("got %d replay frames, want 4 (local-origin active + recent terminals)", len(frames))
	}

	got := map[string]string{} // id -> method
	for _, f := range frames {
		var env struct {
			WorkloadInfo struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"workloadInfo"`
		}
		if err := json.Unmarshal(f.params, &env); err != nil {
			t.Fatalf("bad replay frame params: %v", err)
		}
		got[env.WorkloadInfo.ID] = f.method
	}
	if got["1"] != "workload:started" {
		t.Errorf("id 1 (running) method = %q, want workload:started", got["1"])
	}
	if got["2"] != "workload:errored" {
		t.Errorf("id 2 (failed) method = %q, want workload:errored", got["2"])
	}
	if got["4"] != "workload:submitted" {
		t.Errorf("id 4 (queued) method = %q, want workload:submitted", got["4"])
	}
	if got["5"] != "workload:completed" {
		t.Errorf("id 5 (completed) method = %q, want workload:completed", got["5"])
	}
	if _, ok := got["3"]; ok {
		t.Error("peer-origin workload 3 must not be replayed")
	}
}

// TestWorkloadHistoryFlusherFlushesOnShutdown is the shutdown-flush regression:
// a workload that finishes shortly before shutdown — before the first periodic
// flush — must still be persisted, because the stop func cancels AND joins the
// flusher's final flush before returning. Without the join, the process could
// exit after cancel() but before the goroutine writes, silently dropping it.
func TestWorkloadHistoryFlusherFlushesOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workloads-history.json")
	b := &Broker{workloads: workloadstore.New().WithPersistence(path)}

	stop := b.runWorkloadHistoryFlusher(context.Background())

	// A terminal completes. With the default 5 s periodic flush and an immediate
	// shutdown, only the flusher's shutdown (join) flush can have persisted it.
	if !b.workloads.Apply(storeIncoming("1", "host", "ollama", "r1", "completed", "host")) {
		t.Fatal("terminal apply should be accepted")
	}
	stop() // cancels + joins the flusher; its final flush must have completed

	// Restart: a fresh store loading the same file must see the terminal —
	// proving the shutdown flush ran before stop() returned, not raced with exit.
	s2 := workloadstore.New().WithPersistence(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s2.Get("host", "1"); !ok {
		t.Fatal("terminal workload lost on shutdown before the first periodic flush")
	}
}

// TestWorkloadHistoryFlusherOutlivesParentCancel is the normal-cancellation-path
// regression: SIGINT / JSON-RPC shutdown cancels Serve's context BEFORE the
// deferred worker teardown, and a proxy tearing down can still emit a terminal
// workload:errored. The flusher must not final-flush-and-exit on that parent
// cancel — it stays alive until stop() (deferred after producer teardown) — so a
// terminal applied after parent cancel but before stop() is still persisted.
func TestWorkloadHistoryFlusherOutlivesParentCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workloads-history.json")
	b := &Broker{workloads: workloadstore.New().WithPersistence(path)}

	parent, cancelParent := context.WithCancel(context.Background())
	stop := b.runWorkloadHistoryFlusher(parent)

	// Simulate the shutdown signal cancelling Serve's context. A flusher whose
	// context descended from parent would final-flush and exit here.
	cancelParent()
	time.Sleep(150 * time.Millisecond) // give a (hypothetically coupled) flusher time to exit

	// A producer emits a terminal during teardown — after parent cancel, before stop().
	if !b.workloads.Apply(storeIncoming("1", "host", "ollama", "r1", "failed", "host")) {
		t.Fatal("terminal apply should be accepted")
	}
	stop() // now cancel + join the flusher; its final flush must include the terminal

	s2 := workloadstore.New().WithPersistence(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s2.Get("host", "1"); !ok {
		t.Fatal("terminal applied after parent cancel (during teardown) was lost — flusher exited too early")
	}
}

// TestFailWorkloadsForNodeMatchesByHostUUID: the node-loss sweep must match
// workloads by the stable HostUUID (what the proxy stamps as originatedFrom /
// scheduledOn), not the display hostname. Sweeping by name leaves a
// UUID-stamped workload incorrectly running.
func TestFailWorkloadsForNodeMatchesByHostUUID(t *testing.T) {
	b := &Broker{workloads: workloadstore.New(), nodeID: "self"}
	// A peer-origin running workload stamped with the peer's HostUUID.
	if !b.workloads.Apply(storeIncoming("7", "peer-uuid", "ollama", "r1", "running", "peer-uuid")) {
		t.Fatal("running apply should be accepted")
	}

	// Sweeping by the display name must NOT match (it's not the workload's key).
	b.failWorkloadsForNode("peer-friendly-name", "peer-friendly-name")
	if r, _ := b.workloads.Get("peer-uuid", "7"); r.State != "running" {
		t.Fatalf("state after name-keyed sweep = %q, want running (name must not match a UUID-keyed workload)", r.State)
	}

	// Sweeping by the HostUUID must fail it.
	b.failWorkloadsForNode("peer-uuid", "peer-friendly-name")
	if r, _ := b.workloads.Get("peer-uuid", "7"); r.State != "failed" {
		t.Fatalf("state after UUID-keyed sweep = %q, want failed", r.State)
	}
}
