// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
	"nvpair-shared/schedulerwire"

	"nvpair-ui-broker/workloadstore"
)

// TestSchedulerFeedBaselinePrecedesConcurrentLiveWorkload exercises the
// scheduler-spawn ordering: active workload upserts are queued first, followed
// by telemetry and discovery, and a concurrent live transition can only follow
// all three. Terminal history must never be replayed.
func TestSchedulerFeedBaselinePrecedesConcurrentLiveWorkload(t *testing.T) {
	brokerSide, schedulerSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = schedulerSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	b := &Broker{workloads: workloadstore.New(), telemetry: newTelemetryCache()}
	b.workloads.Apply(storeIncoming(t, "active", "host", "ollama", "run", "running", "a"))
	b.workloads.Apply(storeIncoming(t, "historic", "host", "ollama", "run", "completed", "a"))
	b.telemetry.Upsert(sourceScanner, noderec.NodeTelemetry{
		HostUUID:          "a",
		GPUUtilizationPct: 50,
		TelemetryValid:    true,
	}, time.Now())

	feedLocked := make(chan struct{})
	initDone := make(chan int, 1)
	go func() {
		// This is the same lock order and baseline sequence used by
		// spawnJobScheduler.
		b.workloadEmitMu.Lock()
		b.schedulerFeedMu.Lock()
		b.setScheduler(worker)
		close(feedLocked)
		replayed := b.replayActiveWorkloadsToScheduler(worker)
		b.replayTelemetryToScheduler(worker)
		assert.NoError(t, worker.Notify("discovery:nodes-changed", []AvailableNode{{HostUUID: "a"}}))
		b.schedulerFeedMu.Unlock()
		b.workloadEmitMu.Unlock()
		initDone <- replayed
	}()
	<-feedLocked

	liveInfo := storeIncoming(t, "live", "host", "lmstudio", "run", "queued", "a").Info
	liveParams, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": liveInfo})
	require.NoError(t, err, "marshal live workload")
	liveDone := make(chan struct{})
	go func() {
		b.emitWorkloadEvent("workloads:upsert", liveParams)
		close(liveDone)
	}()

	require.NoError(t, schedulerSide.SetReadDeadline(time.Now().Add(2*time.Second)), "set read deadline")
	codec := NewCodec(schedulerSide)
	first := readSchedulerTestMessage(t, codec)
	second := readSchedulerTestMessage(t, codec)
	third := readSchedulerTestMessage(t, codec)
	fourth := readSchedulerTestMessage(t, codec)

	require.Equal(t, "workloads:upsert", first.Method, "first scheduler frame")
	require.Equal(t, "active", workloadIDFromParams(t, first.Params), "first scheduler frame")
	require.Equal(t, schedulerwire.MethodTelemetry, second.Method, "second scheduler frame")
	require.Equal(t, "discovery:nodes-changed", third.Method, "third scheduler frame")
	require.Equal(t, "workloads:upsert", fourth.Method, "fourth scheduler frame")
	require.Equal(t, "live", workloadIDFromParams(t, fourth.Params), "fourth scheduler frame")

	select {
	case replayed := <-initDone:
		require.Equal(t, 1, replayed, "replayed")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "scheduler initialization did not finish")
	}
	select {
	case <-liveDone:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "live workload fanout did not finish")
	}
}

func readSchedulerTestMessage(t *testing.T, codec *Codec) *Message {
	t.Helper()
	msg, err := codec.Read()
	require.NoError(t, err, "read scheduler frame")
	return msg
}

func workloadIDFromParams(t *testing.T, params json.RawMessage) string {
	t.Helper()
	var env struct {
		WorkloadInfo struct {
			ID string `json:"id"`
		} `json:"workloadInfo"`
	}
	require.NoError(t, json.Unmarshal(params, &env), "decode workload params")
	return env.WorkloadInfo.ID
}

func TestDeliverPrioritySkipsStaleGenerationsAndPreservesNewest(t *testing.T) {
	b := &Broker{}
	oldInput := schedulerwire.Priority{
		Nodes: []string{"old"},
		Ranks: []schedulerwire.NodeRank{{ID: "old", Pending: 1}},
	}
	oldGeneration, _ := b.cachePrioritySnapshot(oldInput)
	oldInput.Nodes[0] = "mutated-after-cache"
	oldInput.Ranks[0].Pending = 99

	var appliedMu sync.Mutex
	var applied []schedulerwire.Priority
	record := func(priority schedulerwire.Priority) {
		appliedMu.Lock()
		applied = append(applied, priority.Clone())
		appliedMu.Unlock()
	}

	oldEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		b.deliverPrioritySnapshot(oldGeneration, func(priority schedulerwire.Priority) {
			record(priority)
			close(oldEntered)
			<-releaseOld
		})
		close(oldDone)
	}()
	waitSchedulerTestChannel(t, oldEntered, "old delivery did not start")

	middleGeneration, _ := b.cachePrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"middle"},
		Ranks: []schedulerwire.NodeRank{{ID: "middle", Pending: 2}},
	})
	middleDone := make(chan struct{})
	go func() {
		b.deliverPrioritySnapshot(middleGeneration, record)
		close(middleDone)
	}()
	newPriority := schedulerwire.Priority{
		Nodes: []string{"new"},
		Ranks: []schedulerwire.NodeRank{{ID: "new", Pending: 3}},
	}
	newGeneration, _ := b.cachePrioritySnapshot(newPriority)
	newDone := make(chan struct{})
	go func() {
		b.deliverPrioritySnapshot(newGeneration, record)
		close(newDone)
	}()

	close(releaseOld)
	waitSchedulerTestChannel(t, oldDone, "old delivery did not finish")
	waitSchedulerTestChannel(t, middleDone, "stale middle delivery did not finish")
	waitSchedulerTestChannel(t, newDone, "new delivery did not finish")

	appliedMu.Lock()
	defer appliedMu.Unlock()
	require.Len(t, applied, 2, "applied snapshots")
	wantOld := schedulerwire.Priority{
		Nodes: []string{"old"},
		Ranks: []schedulerwire.NodeRank{{ID: "old", Pending: 1}},
	}
	// Ranking rather than DeepEqual: a cached snapshot also carries the
	// generation the broker stamped on it, which is not part of what the
	// scheduler produced.
	require.True(t, applied[0].SameRanking(wantOld), "first applied snapshot (%v)", wantOld)
	require.Equal(t, oldGeneration, applied[0].Generation, "first applied generation")
	require.True(t, applied[1].SameRanking(newPriority), "last applied snapshot (%v)", newPriority)
	require.Equal(t, newGeneration, applied[1].Generation, "last applied generation")
}

// The scheduler computes one node-wide ranking and emits it once per engine, so
// the broker sees the same content twice per recompute. The duplicate has to be
// dropped before it mints a generation: a second delivery of one recompute
// clears the proxy's optimistic reservations a second time, discarding the
// dispatches the first delivery's pending counts do not yet include.
func TestDuplicatePerEngineEmissionDoesNotMintAGeneration(t *testing.T) {
	b := &Broker{}
	ranking := schedulerwire.Priority{
		Nodes: []string{"b", "a"},
		Ranks: []schedulerwire.NodeRank{{ID: "b", Pending: 1}, {ID: "a", Pending: 4}},
	}

	first, fresh := b.cachePrioritySnapshot(ranking)
	require.True(t, fresh, "first emission of a ranking was treated as a duplicate")

	second, fresh := b.cachePrioritySnapshot(ranking)
	require.False(t, fresh, "the sibling engine's identical emission minted a second generation")
	require.Equal(t, first, second, "duplicate emission moved the generation")

	// A genuinely changed ranking still advances, so the dedupe is not just
	// swallowing everything after the first.
	changed, fresh := b.cachePrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 0}, {ID: "b", Pending: 5}},
	})
	require.True(t, fresh, "changed ranking must be fresh")
	require.Greater(t, changed, first, "changed ranking must advance its generation")
}

// An empty ranking is a legitimate state — it means the scheduler has no
// influence — and must be distinguishable from having no ranking at all, or a
// repush would either send nothing or send an uncached zero value.
func TestEmptyRankingIsCachedNotTreatedAsAbsent(t *testing.T) {
	b := &Broker{}
	_, fresh := b.cachePrioritySnapshot(schedulerwire.Priority{})
	require.True(t, fresh, "first empty ranking was treated as a duplicate")
	b.schedMu.Lock()
	have := b.havePriority
	b.schedMu.Unlock()
	require.True(t, have, "an empty ranking did not mark a ranking as cached, so repush would skip it")
}

func TestRepushPriorityDeliversCompleteSnapshotToReplacementProxy(t *testing.T) {
	brokerSide, proxySide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = proxySide.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(brokerSide))}
	go proxy.peer.Serve(nil, nil)

	b := &Broker{}
	want := schedulerwire.Priority{
		Nodes: []string{"b", "a"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "b", Pending: 1, Rank: 0},
			{ID: "a", Pending: 4, Rank: 1},
		},
	}
	b.cachePrioritySnapshot(want)
	b.setProxy(proxy) // replacement proxy starts with no scheduler state

	replayed := make(chan struct{})
	go func() {
		b.repushPriority(context.Background())
		close(replayed)
	}()

	require.NoError(t, proxySide.SetDeadline(time.Now().Add(2*time.Second)), "set proxy pipe deadline")
	codec := NewCodec(proxySide)
	request := readSchedulerTestMessage(t, codec)
	require.Equal(t, "node/set-priority", request.Method, "replacement request method")
	var got schedulerwire.Priority
	require.NoError(t, json.Unmarshal(request.Params, &got), "decode replacement priority")
	require.True(t, got.SameRanking(want), "replacement priority (%v, %v)", got, want)
	// The replay reuses the cached generation rather than minting one, so the
	// child can recognize a redelivery it has already applied. A repush that
	// arrived unversioned would clear reservations on every proxy ready.
	require.NotEqual(t, uint64(0), got.Generation, "replayed snapshot carried no generation; the child cannot detect a redelivery")
	require.NoError(t, codec.Respond(request.ID, map[string]int{"count": len(got.Nodes)}), "respond to replacement priority")
	waitSchedulerTestChannel(t, replayed, "priority replay did not finish")
}

func waitSchedulerTestChannel(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		require.FailNow(t, failure)
	}
}
