// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emitDedupKey exercises the production dedup path with a successful broker
// emit, then reports whether that key was already recorded.
func emitDedupKey(t *testing.T, d *dedupIndex, key string) bool {
	t.Helper()
	duplicate, err := d.emitOnce(key, func() error { return nil })
	require.NoError(t, err, "emit dedup key")
	return duplicate
}

// TestDedupIndex_EmitOnceSerializesOnlyMatchingKeys proves that an in-flight
// emit blocks only requests with the same key. The test controls this order:
//
//  1. The "first" goroutine reserves its key, enters its emit callback, and
//     signals firstEntered. It then waits on releaseFirst, keeping that key
//     in flight.
//  2. While "first" is still waiting, the "second" goroutine emits a
//     different key. The test requires secondDone before closing releaseFirst.
//  3. The test closes releaseFirst and waits for the original emit to finish.
//
// A single lock held across all emit callbacks would make step 2 time out.
func TestDedupIndex_EmitOnceSerializesOnlyMatchingKeys(t *testing.T) {
	d := newDedupIndex(10)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	// Release the blocked goroutine even if an assertion fails early.
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	firstDone := make(chan error, 1)
	go func() {
		_, err := d.emitOnce("first", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		firstDone <- err
	}()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "first key did not begin emitting")
	}

	type result struct {
		duplicate bool
		err       error
	}
	secondDone := make(chan result, 1)
	go func() {
		duplicate, err := d.emitOnce("second", func() error { return nil })
		secondDone <- result{duplicate: duplicate, err: err}
	}()
	select {
	case got := <-secondDone:
		require.NoError(t, got.err, "different key emit")
		assert.False(t, got.duplicate, "different key must be new")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "different key blocked behind the first emit")
	}
	close(releaseFirst)
	select {
	case err := <-firstDone:
		require.NoError(t, err, "first key emit")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "first key did not finish")
	}
}

// TestDedupIndex_WaiterRetriesAfterFailedEmit proves that a failed emit does
// not permanently reserve or record its key. Unlike the different-key test
// above, both goroutines use "same":
//
//  1. The first goroutine reserves "same", enters its emit callback, signals
//     firstEntered, and waits on releaseFirst.
//  2. The second goroutine requests "same". It must wait while the first
//     callback is in flight; returning or emitting before releaseFirst closes
//     fails the test.
//  3. The test closes releaseFirst. The first callback returns an error, so
//     "same" remains unrecorded. The waiting goroutine then emits it once and
//     succeeds.
//
// The final retry count distinguishes a real retry from a duplicate response.
func TestDedupIndex_WaiterRetriesAfterFailedEmit(t *testing.T) {
	d := newDedupIndex(10)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	wantErr := errors.New("broker unavailable")
	firstDone := make(chan error, 1)
	go func() {
		_, err := d.emitOnce("same", func() error {
			close(firstEntered)
			<-releaseFirst
			return wantErr
		})
		firstDone <- err
	}()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "first request did not begin emitting")
	}

	var retries atomic.Int32
	secondDone := make(chan error, 1)
	go func() {
		duplicate, err := d.emitOnce("same", func() error {
			retries.Add(1)
			return nil
		})
		if duplicate {
			secondDone <- errors.New("failed key was treated as a duplicate")
			return
		}
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		require.FailNow(t, "waiting request completed before first emit", "%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case err := <-firstDone:
		require.ErrorIs(t, err, wantErr)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "first request did not finish")
	}
	select {
	case err := <-secondDone:
		require.NoError(t, err, "waiting retry")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "waiting retry did not finish")
	}
	assert.Equal(t, int32(1), retries.Load(), "retry emits")
}

func TestDedupEmitOnce(t *testing.T) {
	d := newDedupIndex(8)

	assert.False(t, emitDedupKey(t, d, "a"), "first sighting of a should be new")
	assert.True(t, emitDedupKey(t, d, "a"), "second sighting of a should be a duplicate")
	assert.False(t, emitDedupKey(t, d, "b"), "first sighting of b should be new")
}

func TestDedupEviction(t *testing.T) {
	d := newDedupIndex(2)

	emitDedupKey(t, d, "a") // {a}
	emitDedupKey(t, d, "b") // {a,b}
	emitDedupKey(t, d, "c") // evicts a -> {b,c}

	assert.False(t, emitDedupKey(t, d, "a"), "a should have been evicted and read as new")
}

func TestDedupKeysDistinguishStateAndKind(t *testing.T) {
	d := newDedupIndex(8)

	w := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateQueued}
	wRunning := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateRunning}

	assert.False(t, emitDedupKey(t, d, keyLifecycle(w)), "queued should be new")
	assert.False(t, emitDedupKey(t, d, keyLifecycle(wRunning)), "running for same id is a different key, should be new")
	assert.True(t, emitDedupKey(t, d, keyLifecycle(w)), "repeat queued should dedup")
	// A removal keyed on the same id must not collide with a lifecycle key.
	assert.False(t, emitDedupKey(t, d, keyRemove("node-A", "wl-1")), "removal of wl-1 must not collide with lifecycle keys")
}

// TestDedupDistinguishesNodes guards the cross-node collision: Workload.id is
// only unique per node (spec §11), so the same id+state from two different
// nodes must be treated as two distinct workloads, never deduplicated against
// each other.
func TestDedupDistinguishesNodes(t *testing.T) {
	d := newDedupIndex(8)

	nodeA := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateQueued}
	nodeB := &Workload{ID: "wl-1", OriginatedFrom: "node-B", State: StateQueued}

	assert.False(t, emitDedupKey(t, d, keyLifecycle(nodeA)), "node-A wl-1 should be new")
	assert.False(t, emitDedupKey(t, d, keyLifecycle(nodeB)), "node-B wl-1 has the same id but a different node, must not dedup against node-A")
	assert.True(t, emitDedupKey(t, d, keyLifecycle(nodeA)), "repeat of node-A wl-1 should dedup")
}

// TestDedupDistinguishesEngineAndRun guards the identity fix: id "1" is reused
// by the two engine proxies (each counts from 1) and after a restart (new
// runId). The dedup must treat those as distinct workloads, not collapse them.
func TestDedupDistinguishesEngineAndRun(t *testing.T) {
	d := newDedupIndex(8)
	ollama := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1", State: StateRunning}
	lmstudio := &Workload{ID: "1", OriginatedFrom: "host", Engine: "lmstudio", RunID: "r2", State: StateRunning}
	restarted := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r3", State: StateRunning}

	assert.False(t, emitDedupKey(t, d, keyLifecycle(ollama)), "ollama host/1 should be new")
	assert.False(t, emitDedupKey(t, d, keyLifecycle(lmstudio)), "lmstudio host/1 shares the id but a different engine; must not dedup")
	assert.False(t, emitDedupKey(t, d, keyLifecycle(restarted)), "a reused id from a new run must not dedup against the old run")
	assert.True(t, emitDedupKey(t, d, keyLifecycle(ollama)), "repeat of ollama host/1 should dedup")
}

// TestDedupDistinguishesPlacement guards the re-point: a failover or a retry
// moves a workload to another node while its state stays "running", so the two
// events differ on scheduledOn and nothing else. With scheduledOn out of the
// key the second was dropped as a duplicate, and every peer went on showing
// the job on the node it was first sent to for the rest of its life. A repeat
// of the same placement must still dedup, which is what keeps a broadcast
// retry from being re-applied.
func TestDedupDistinguishesPlacement(t *testing.T) {
	d := newDedupIndex(8)

	first := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1", State: StateRunning, ScheduledOn: "node-A"}
	repointed := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1", State: StateRunning, ScheduledOn: "node-B"}

	assert.False(t, emitDedupKey(t, d, keyLifecycle(first)), "first placement on node-A should be new")
	assert.False(t, emitDedupKey(t, d, keyLifecycle(repointed)), "a re-point to node-B differs only in scheduledOn and must not dedup against node-A")
	assert.True(t, emitDedupKey(t, d, keyLifecycle(first)), "a resent frame for the node-A placement should still dedup")
}

// TestDedupDistinguishesRepeatedPlacements is the reason the key carries the
// producer's event sequence rather than only the workload's current shape.
//
// This index is a permanent set, so any shape-derived key collides as soon as a
// workload revisits a shape it already had — and the retry loop does that
// routinely: queued on A, placement cleared between attempts, then queued on A
// again. Keyed on shape alone the third event matched the first, so every peer
// dropped it and their brokers kept the interim unplaced record while the job
// was really running on A, which took it out of A's pending load.
//
// The redelivery case still has to dedup, because that is what stops an
// out-of-order broadcast retry from reverting a placement.
func TestDedupDistinguishesRepeatedPlacements(t *testing.T) {
	d := newDedupIndex(8)

	base := func(seq int64, node string) *Workload {
		return &Workload{
			ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1",
			State: StateQueued, ScheduledOn: node, Seq: seq,
		}
	}

	onA := base(1, "node-A")
	cleared := base(2, "")
	backOnA := base(3, "node-A")

	assert.False(t, emitDedupKey(t, d, keyLifecycle(onA)), "first placement on node-A should be new")
	assert.False(t, emitDedupKey(t, d, keyLifecycle(cleared)), "clearing the placement between attempts should be new")
	assert.False(t, emitDedupKey(t, d, keyLifecycle(backOnA)), "re-dispatching to node-A repeats an earlier shape and must NOT dedup against it")
	// A redelivery of any of those frames carries its original sequence, so it
	// is still recognised as one.
	assert.True(t, emitDedupKey(t, d, keyLifecycle(base(1, "node-A"))), "a resent frame for the first placement should still dedup")
	assert.True(t, emitDedupKey(t, d, keyLifecycle(base(3, "node-A"))), "a resent frame for the third event should still dedup")
}

// TestDedupRemovalDistinguishesNodes mirrors TestDedupDistinguishesNodes for
// the removal path: the same workloadId removed on two nodes must be two
// distinct dedup entries.
func TestDedupRemovalDistinguishesNodes(t *testing.T) {
	d := newDedupIndex(8)

	assert.False(t, emitDedupKey(t, d, keyRemove("node-A", "wl-1")), "removal of node-A wl-1 should be new")
	assert.False(t, emitDedupKey(t, d, keyRemove("node-B", "wl-1")), "removal of node-B wl-1 must not dedup against node-A")
	assert.True(t, emitDedupKey(t, d, keyRemove("node-A", "wl-1")), "repeat removal of node-A wl-1 should dedup")
}
