// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.Len(t, frames, 4)

	got := map[string]string{} // id -> method
	for _, f := range frames {
		var env struct {
			WorkloadInfo struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"workloadInfo"`
		}
		require.NoError(t, json.Unmarshal(f.params, &env), "bad replay frame params")
		got[env.WorkloadInfo.ID] = f.method
	}
	assert.Equal(t, "workload:started", got["1"], "id 1 (running) method")
	assert.Equal(t, "workload:errored", got["2"], "id 2 (failed) method")
	assert.Equal(t, "workload:submitted", got["4"], "id 4 (queued) method")
	assert.Equal(t, "workload:completed", got["5"], "id 5 (completed) method")
	{
		_, ok := got["3"]
		assert.False(t, ok, "peer-origin workload 3 must not be replayed")
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
	require.True(t, b.workloads.Apply(storeIncoming("1", "host", "ollama", "r1", "completed", "host")), "terminal apply should be accepted")
	stop() // cancels + joins the flusher; its final flush must have completed

	// Restart: a fresh store loading the same file must see the terminal —
	// proving the shutdown flush ran before stop() returned, not raced with exit.
	s2 := workloadstore.New().WithPersistence(path)
	require.NoError(t, s2.Load(), "load")
	{
		_, ok := s2.Get("host", "1")
		require.True(t, ok, "terminal workload lost on shutdown before the first periodic flush")
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
	require.True(t, b.workloads.Apply(storeIncoming("1", "host", "ollama", "r1", "failed", "host")), "terminal apply should be accepted")
	stop() // now cancel + join the flusher; its final flush must include the terminal

	s2 := workloadstore.New().WithPersistence(path)
	require.NoError(t, s2.Load(), "load")
	{
		_, ok := s2.Get("host", "1")
		require.True(t, ok, "terminal applied after parent cancel (during teardown) was lost — flusher exited too early")
	}
}

// TestFailWorkloadsForNodeMatchesByHostUUID: the node-loss sweep must match
// workloads by the stable HostUUID (what the proxy stamps as originatedFrom /
// scheduledOn), not the display hostname. Sweeping by name leaves a
// UUID-stamped workload incorrectly running.
func TestFailWorkloadsForNodeMatchesByHostUUID(t *testing.T) {
	b := &Broker{workloads: workloadstore.New(), nodeID: "self"}
	// A peer-origin running workload stamped with the peer's HostUUID.
	require.True(t, b.workloads.Apply(storeIncoming("7", "peer-uuid", "ollama", "r1", "running", "peer-uuid")), "running apply should be accepted")

	// Sweeping by the display name must NOT match (it's not the workload's key).
	b.failWorkloadsForNode("peer-friendly-name", "peer-friendly-name")
	{
		r, _ := b.workloads.Get("peer-uuid", "7")
		require.Equal(t, "running", r.State, "state after name-keyed sweep")
	}

	// Sweeping by the HostUUID must fail it.
	b.failWorkloadsForNode("peer-uuid", "peer-friendly-name")
	{
		r, _ := b.workloads.Get("peer-uuid", "7")
		require.Equal(t, "failed", r.State, "state after UUID-keyed sweep")
	}
}
