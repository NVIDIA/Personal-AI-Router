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
	b.workloads.Apply(storeIncoming("active", "host", "ollama", "run", "running", "a"))
	b.workloads.Apply(storeIncoming("historic", "host", "ollama", "run", "completed", "a"))
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
		_ = worker.Notify("discovery:nodes-changed", []AvailableNode{{HostUUID: "a"}})
		b.schedulerFeedMu.Unlock()
		b.workloadEmitMu.Unlock()
		initDone <- replayed
	}()
	<-feedLocked

	liveInfo := storeIncoming("live", "host", "lmstudio", "run", "queued", "a").Info
	liveParams, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": liveInfo})
	if err != nil {
		t.Fatalf("marshal live workload: %v", err)
	}
	liveDone := make(chan struct{})
	go func() {
		b.emitWorkloadEvent("workloads:upsert", liveParams)
		close(liveDone)
	}()

	if err := schedulerSide.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	codec := NewCodec(schedulerSide)
	first := readSchedulerTestMessage(t, codec)
	second := readSchedulerTestMessage(t, codec)
	third := readSchedulerTestMessage(t, codec)
	fourth := readSchedulerTestMessage(t, codec)

	if first.Method != "workloads:upsert" || workloadIDFromParams(t, first.Params) != "active" {
		t.Fatalf("first scheduler frame = %s %s, want active baseline upsert", first.Method, first.Params)
	}
	if second.Method != schedulerwire.MethodTelemetry {
		t.Fatalf("second scheduler frame = %s, want telemetry baseline", second.Method)
	}
	if third.Method != "discovery:nodes-changed" {
		t.Fatalf("third scheduler frame = %s, want discovery baseline", third.Method)
	}
	if fourth.Method != "workloads:upsert" || workloadIDFromParams(t, fourth.Params) != "live" {
		t.Fatalf("fourth scheduler frame = %s %s, want concurrent live upsert", fourth.Method, fourth.Params)
	}

	select {
	case replayed := <-initDone:
		if replayed != 1 {
			t.Fatalf("replayed = %d, want 1 active record", replayed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler initialization did not finish")
	}
	select {
	case <-liveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("live workload fanout did not finish")
	}
}

func readSchedulerTestMessage(t *testing.T, codec *Codec) *Message {
	t.Helper()
	msg, err := codec.Read()
	if err != nil {
		t.Fatalf("read scheduler frame: %v", err)
	}
	return msg
}

func workloadIDFromParams(t *testing.T, params json.RawMessage) string {
	t.Helper()
	var env struct {
		WorkloadInfo struct {
			ID string `json:"id"`
		} `json:"workloadInfo"`
	}
	if err := json.Unmarshal(params, &env); err != nil {
		t.Fatalf("decode workload params: %v", err)
	}
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
	if len(applied) != 2 {
		t.Fatalf("applied snapshots = %v, want old then new (middle skipped)", applied)
	}
	wantOld := schedulerwire.Priority{
		Nodes: []string{"old"},
		Ranks: []schedulerwire.NodeRank{{ID: "old", Pending: 1}},
	}
	// Ranking rather than DeepEqual: a cached snapshot also carries the
	// generation the broker stamped on it, which is not part of what the
	// scheduler produced.
	if !applied[0].SameRanking(wantOld) {
		t.Fatalf("first applied snapshot = %#v, want cached copy %#v", applied[0], wantOld)
	}
	if applied[0].Generation != oldGeneration {
		t.Fatalf("first applied generation = %d, want %d", applied[0].Generation, oldGeneration)
	}
	if !applied[1].SameRanking(newPriority) {
		t.Fatalf("last applied snapshot = %#v, want newest %#v", applied[1], newPriority)
	}
	if applied[1].Generation != newGeneration {
		t.Fatalf("last applied generation = %d, want %d", applied[1].Generation, newGeneration)
	}
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
	if !fresh {
		t.Fatal("first emission of a ranking was treated as a duplicate")
	}

	second, fresh := b.cachePrioritySnapshot(ranking)
	if fresh {
		t.Fatal("the sibling engine's identical emission minted a second generation")
	}
	if second != first {
		t.Fatalf("duplicate emission moved the generation from %d to %d", first, second)
	}

	// A genuinely changed ranking still advances, so the dedupe is not just
	// swallowing everything after the first.
	changed, fresh := b.cachePrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 0}, {ID: "b", Pending: 5}},
	})
	if !fresh || changed <= first {
		t.Fatalf("changed ranking = generation %d fresh %v, want a newer generation", changed, fresh)
	}
}

// An empty ranking is a legitimate state — it means the scheduler has no
// influence — and must be distinguishable from having no ranking at all, or a
// repush would either send nothing or send an uncached zero value.
func TestEmptyRankingIsCachedNotTreatedAsAbsent(t *testing.T) {
	b := &Broker{}
	if _, fresh := b.cachePrioritySnapshot(schedulerwire.Priority{}); !fresh {
		t.Fatal("first empty ranking was treated as a duplicate")
	}
	b.schedMu.Lock()
	have := b.havePriority
	b.schedMu.Unlock()
	if !have {
		t.Fatal("an empty ranking did not mark a ranking as cached, so repush would skip it")
	}
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

	if err := proxySide.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set proxy pipe deadline: %v", err)
	}
	codec := NewCodec(proxySide)
	request := readSchedulerTestMessage(t, codec)
	if request.Method != "node/set-priority" {
		t.Fatalf("replacement request method = %q, want node/set-priority", request.Method)
	}
	var got schedulerwire.Priority
	if err := json.Unmarshal(request.Params, &got); err != nil {
		t.Fatalf("decode replacement priority: %v", err)
	}
	if !got.SameRanking(want) {
		t.Fatalf("replacement priority = %#v, want %#v", got, want)
	}
	// The replay reuses the cached generation rather than minting one, so the
	// child can recognize a redelivery it has already applied. A repush that
	// arrived unversioned would clear reservations on every proxy ready.
	if got.Generation == 0 {
		t.Fatal("replayed snapshot carried no generation; the child cannot detect a redelivery")
	}
	if err := codec.Respond(request.ID, map[string]int{"count": len(got.Nodes)}); err != nil {
		t.Fatalf("respond to replacement priority: %v", err)
	}
	waitSchedulerTestChannel(t, replayed, "priority replay did not finish")
}

func waitSchedulerTestChannel(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}
