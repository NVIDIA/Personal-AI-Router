// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// nopRW discards writes and reads EOF — enough for rank-only tests.
type nopRW struct{}

func (nopRW) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRW) Write(p []byte) (int, error) { return len(p), nil }

// capRW records codec writes so tests can inspect emitted notifications.
type capRW struct {
	mu sync.Mutex
	b  []byte
}

func (r *capRW) Read([]byte) (int, error) { return 0, io.EOF }

func (r *capRW) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	return len(p), nil
}

func (r *capRW) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.b)
}

// priorities returns every schedule:priority snapshot emitted for an engine.
func (r *capRW) priorities(t *testing.T, engine string) []schedulePriorityParams {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []schedulePriorityParams
	for _, line := range strings.Split(string(r.b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg Message
		if !assert.NoError(t, json.Unmarshal([]byte(line), &msg), "decode scheduler frame") || msg.Method != "schedule:priority" {
			continue
		}
		var p schedulePriorityParams
		if !assert.NoError(t, json.Unmarshal(msg.Params, &p), "decode scheduler priority") || p.Engine != engine {
			continue
		}
		out = append(out, p)
	}
	return out
}

// orders returns every schedule:priority node-order emitted for an engine.
func (r *capRW) orders(t *testing.T, engine string) [][]string {
	t.Helper()
	priorities := r.priorities(t, engine)
	out := make([][]string, 0, len(priorities))
	for _, p := range priorities {
		out = append(out, p.Nodes)
	}
	return out
}

// gatedRW blocks the first codec write until release is closed, allowing a
// test to overlap an older recompute with a newer state mutation.
type gatedRW struct {
	recorder capRW
	once     sync.Once
	blocked  chan struct{}
	release  chan struct{}
}

func newGatedRW() *gatedRW {
	return &gatedRW{blocked: make(chan struct{}), release: make(chan struct{})}
}

func (r *gatedRW) Read([]byte) (int, error) { return 0, io.EOF }

func (r *gatedRW) Write(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.blocked)
		<-r.release
	})
	return r.recorder.Write(p)
}

func wl(id, engine, state, origin, scheduledOn string) workload {
	return workload{ID: id, Engine: engine, State: state, OriginatedFrom: origin, ScheduledOn: scheduledOn}
}

func mgrWith(rw io.ReadWriter, nodes []string, wls ...workload) *Manager {
	m := NewManager(NewCodec(rw), time.Second)
	for _, id := range nodes {
		m.nodes[id] = true
	}
	for _, w := range wls {
		m.catalog[wlKey{origin: w.OriginatedFrom, engine: w.Engine, runID: w.RunID, id: w.ID}] = w
	}
	return m
}

func assertStrs(t *testing.T, got, want []string) {
	t.Helper()
	require.Equal(t, want, got, "order")
}

// TestApplyNodesChanged_KeysByHostUUID: the node universe keys on the stable
// hostUuid (so scheduledOn — also a UUID — matches and a rename doesn't drop the
// node). Every node the broker publishes carries a hostUuid, so the hostname is
// never a key.
func TestApplyNodesChanged_KeysByHostUUID(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	m.applyNodesChanged(json.RawMessage(`[
		{"id":"host-a","hostUuid":"uuid-a"},
		{"id":"host-b","hostUuid":"uuid-b"}
	]`))
	m.mu.Lock()
	assert.Contains(t, m.nodes, "uuid-a", "node universe should key by hostUuid")
	assert.Contains(t, m.nodes, "uuid-b", "node universe should key by hostUuid")
	assert.NotContains(t, m.nodes, "host-a", "must not key by hostname")
	m.mu.Unlock()

	// A workload scheduledOn the UUID counts against that node — proving the
	// scheduledOn value the proxy stamps (a UUID) matches the universe key.
	m.catalog[wlKey{origin: "uuid-a", id: "w1"}] = wl("w1", "ollama", "running", "uuid-a", "uuid-a")
	_, ranks := m.rank()
	for _, r := range ranks {
		if r.ID == "uuid-a" {
			assert.Equal(t, 1, r.Pending, "scheduledOn=uuid-a should count against uuid-a, ranks (%v)", ranks)
		}
	}
}

// TestRank_ColdStartIDSort: with no workloads every node ties at 0, so the order
// is a stable node-id sort.
func TestRank_ColdStartIDSort(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"c", "a", "b"})
	order, _ := m.rank()
	assertStrs(t, order, []string{"a", "b", "c"})
}

// TestRank_AscendingByPending: least-loaded node first.
func TestRank_AscendingByPending(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b", "c"},
		wl("1", "ollama", "running", "x", "a"),
		wl("2", "ollama", "running", "x", "a"),
		wl("3", "ollama", "queued", "x", "c"),
	)
	order, ranks := m.rank()
	assertStrs(t, order, []string{"b", "c", "a"}) // 0, 1, 2
	assert.Equal(t, 0, ranks[0].Pending, "unexpected pending counts (%v)", ranks)
	assert.Equal(t, 2, ranks[2].Pending, "unexpected pending counts (%v)", ranks)
}

func TestRank_CombinesPendingAndGPUPressure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := mgrWith(nopRW{}, []string{"a", "b", "c", "d"},
		wl("1", "ollama", "running", "x", "b"),
	)
	for id, utilization := range map[string]uint32{
		"a": 90,
		"b": 0,
		"c": 50,
		"d": 75,
	} {
		m.applyTelemetryAt(noderec.NodeTelemetry{
			HostUUID:          id,
			GPUUtilizationPct: utilization,
			TelemetryValid:    true,
		}, now)
	}

	order, ranks := m.rankAt(now)
	assertStrs(t, order, []string{"b", "c", "d", "a"})
	assert.Equal(t, 1, pendingOf(ranks, "b"))
	for id, want := range map[string]int{"a": 3, "b": 0, "c": 1, "d": 2} {
		assert.Equal(t, want, pressureOf(ranks, id), "gpuPressure[%s], ranks %v", id, ranks)
	}
}

func TestRank_UnknownAndStaleTelemetryUseNeutralPressure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := mgrWith(nopRW{}, []string{"a", "b", "c"})
	m.applyTelemetryAt(noderec.NodeTelemetry{
		HostUUID:       "a",
		TelemetryValid: true,
	}, now)
	m.applyTelemetryAt(noderec.NodeTelemetry{
		HostUUID:          "c",
		GPUUtilizationPct: 100,
		TelemetryValid:    true,
		MSSince:           gpuTelemetryFreshness.Milliseconds() + 1,
	}, now)

	order, ranks := m.rankAt(now)
	assertStrs(t, order, []string{"a", "b", "c"})
	assert.Equal(t, 0, pressureOf(ranks, "a"), "unexpected neutral-pressure ranking (%v)", ranks)
	assert.Equal(t, unknownGPUPressure, pressureOf(ranks, "b"), "unexpected neutral-pressure ranking (%v)", ranks)
	assert.Equal(t, unknownGPUPressure, pressureOf(ranks, "c"), "unexpected neutral-pressure ranking (%v)", ranks)
}

// TestRank_UnplacedIgnored: a workload with no scheduledOn counts toward no node.
func TestRank_UnplacedIgnored(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b"},
		wl("1", "ollama", "running", "x", ""),
	)
	_, ranks := m.rank()
	for _, r := range ranks {
		assert.Equal(t, 0, r.Pending, "unplaced workload inflated a node (%v)", ranks)
	}
}

// TestRank_ScheduledOnUnknownIgnored: work on a node not in discovery is dropped.
func TestRank_ScheduledOnUnknownIgnored(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b"},
		wl("1", "ollama", "running", "x", "ghost"),
	)
	_, ranks := m.rank()
	for _, r := range ranks {
		assert.Equal(t, 0, r.Pending, "workload on unknown node counted (%v)", ranks)
	}
}

// TestRank_TerminalStatesExcluded: only queued/running count.
func TestRank_TerminalStatesExcluded(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b"},
		wl("1", "ollama", "running", "x", "a"),   // a = 1
		wl("2", "ollama", "completed", "x", "b"), // ignored
	)
	order, _ := m.rank()
	assertStrs(t, order, []string{"b", "a"}) // b=0 wins; a would tie only if completed counted
}

// TestRank_CancelledExcluded pins that isPending is an allow-list, which is
// what keeps a new terminal state from silently counting as load. "cancelled"
// is not queued and not running, so it must contribute nothing — a job the
// requester walked away from is not occupying its node.
func TestRank_CancelledExcluded(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b"},
		wl("1", "ollama", "running", "x", "a"),   // a = 1
		wl("2", "ollama", "cancelled", "x", "b"), // must not count toward b
	)
	order, ranks := m.rank()
	assertStrs(t, order, []string{"b", "a"})
	assert.Equal(t, 0, pendingOf(ranks, "b"), "pending for b")
}

// TestRank_NodeWideMixedEngineSynthetic verifies the requested synthetic
// ranking: A has three mixed-engine pending jobs, B has one, and C has none.
// Both engine outputs must therefore receive C,B,A.
func TestRank_NodeWideMixedEngineSynthetic(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"a", "b", "c"},
		workload{ID: "1", Engine: "ollama", RunID: "o1", State: "running", OriginatedFrom: "x", ScheduledOn: "a"},
		workload{ID: "2", Engine: "lmstudio", RunID: "l1", State: "queued", OriginatedFrom: "x", ScheduledOn: "a"},
		workload{ID: "3", Engine: "ollama", RunID: "o2", State: "queued", OriginatedFrom: "x", ScheduledOn: "a"},
		workload{ID: "4", Engine: "lmstudio", RunID: "l2", State: "running", OriginatedFrom: "x", ScheduledOn: "b"},
	)
	order, ranks := m.rank()
	assertStrs(t, order, []string{"c", "b", "a"})
	assert.Equal(t, 3, pendingOf(ranks, "a"), "unexpected node-wide counts (%v)", ranks)
	assert.Equal(t, 1, pendingOf(ranks, "b"), "unexpected node-wide counts (%v)", ranks)
	assert.Equal(t, 0, pendingOf(ranks, "c"), "unexpected node-wide counts (%v)", ranks)

	m.recomputeAll(false)
	for _, engine := range schedulerEngines {
		got := rec.orders(t, engine)
		require.Len(t, got, 1, " (%v)", engine)
		assertStrs(t, got[0], []string{"c", "b", "a"})
	}
}

// upsertJSON / removeJSON build the wire params applyUpsert / applyRemove parse,
// so these tests exercise the real ingestion path (and its catalog keying), not
// a hand-seeded catalog.
func upsertJSON(t *testing.T, w workload) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(workloadParams{WorkloadInfo: w})
	require.NoError(t, err, "marshal upsert")
	return b
}

func removeJSON(t *testing.T, id, origin string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(removeParams{WorkloadID: id, OriginatedFrom: origin})
	require.NoError(t, err, "marshal remove")
	return b
}

func pendingOf(ranks []NodeRank, id string) int {
	for _, r := range ranks {
		if r.ID == id {
			return r.Pending
		}
	}
	return -1
}

func pressureOf(ranks []NodeRank, id string) int {
	for _, rank := range ranks {
		if rank.ID == id {
			return rank.GPUPressure
		}
	}
	return -1
}

// TestApplyUpsert_CrossEngineSameIDDistinct: Ollama and LM Studio each mint id
// "1" for the same origin. They must remain two catalog entries (keyed by
// engine + runId), so node-wide pending includes both. Keyed on the bare
// (origin,id) the second upsert would clobber the first.
func TestApplyUpsert_CrossEngineSameIDDistinct(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	m.nodes["a"] = true
	m.nodes["b"] = true
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "ollama", RunID: "r-oll", State: "running", OriginatedFrom: "x", ScheduledOn: "a"}))
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "lmstudio", RunID: "r-lms", State: "running", OriginatedFrom: "x", ScheduledOn: "b"}))

	require.Len(t, m.catalog, 2, "catalog len")
	_, ranks := m.rank()
	assert.Equal(t, 1, pendingOf(ranks, "a"), "node-wide pending counts (%v)", ranks)
	assert.Equal(t, 1, pendingOf(ranks, "b"), "node-wide pending counts (%v)", ranks)
}

// TestApplyUpsert_SameIDAcrossRestartDistinct: a proxy restart mints a fresh
// runId, so a reused id after restart is a separate job — both must count until
// the old one is retired.
func TestApplyUpsert_SameIDAcrossRestartDistinct(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	m.nodes["a"] = true
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "ollama", RunID: "run-1", State: "running", OriginatedFrom: "x", ScheduledOn: "a"}))
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "ollama", RunID: "run-2", State: "running", OriginatedFrom: "x", ScheduledOn: "a"}))
	require.Len(t, m.catalog, 2, "catalog len")
	_, ranks := m.rank()
	assert.Equal(t, 2, pendingOf(ranks, "a"), "node-wide pending[a]")
}

// TestApplyRemove_DropsEveryEngineForID: the removal wire carries no
// engine/runId, so a remove for (origin,id) retires every composite key sharing
// that pair (mirroring the broker store's Remove).
func TestApplyRemove_DropsEveryEngineForID(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "ollama", RunID: "r-oll", State: "running", OriginatedFrom: "x", ScheduledOn: "a"}))
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "lmstudio", RunID: "r-lms", State: "running", OriginatedFrom: "x", ScheduledOn: "b"}))
	assert.True(t, m.applyRemove(removeJSON(t, "1", "x")), "removing pending work should report a load change")
	require.Empty(t, m.catalog, "catalog len")
}

func TestApplyUpsert_ActiveOnlyAndMeaningfulChanges(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	base := workload{ID: "1", Engine: "ollama", RunID: "run", State: "queued", OriginatedFrom: "x", ScheduledOn: "a"}

	assert.True(t, m.applyUpsert(upsertJSON(t, base)), "new placed work should change pending load")
	base.State = "running"
	assert.False(t, m.applyUpsert(upsertJSON(t, base)), "queued->running on the same node should not change pending load")
	assert.Equal(t, "running", m.catalog[wlKey{origin: "x", engine: "ollama", runID: "run", id: "1"}].State, "catalog state")

	base.ScheduledOn = "b"
	assert.True(t, m.applyUpsert(upsertJSON(t, base)), "failover re-point should change node-wide load")
	base.State = "completed"
	assert.True(t, m.applyUpsert(upsertJSON(t, base)), "terminal transition should remove pending load")
	require.Empty(t, m.catalog, "terminal workload retained in active catalog")
	assert.False(t, m.applyUpsert(upsertJSON(t, base)), "duplicate terminal should be a no-op")
}

func TestApplyNodesChanged_ReportsRealSetChanges(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	first := json.RawMessage(`[{"hostUuid":"b"},{"hostUuid":"a"}]`)
	assert.True(t, m.applyNodesChanged(first), "initial non-empty node set should report changed")
	assert.False(t, m.applyNodesChanged(json.RawMessage(`[{"hostUuid":"a"},{"hostUuid":"b"}]`)), "reordered copy of the same set should be a no-op")
	assert.True(t, m.applyNodesChanged(json.RawMessage(`[{"hostUuid":"a"},{"hostUuid":"c"}]`)), "membership replacement should report changed")
}

func TestHandleMessage_RebalancesImmediatelyAcrossEngines(t *testing.T) {
	rec := &capRW{}
	m := NewManager(NewCodec(rec), 24*time.Hour)
	m.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  "discovery:nodes-changed",
		Params:  json.RawMessage(`[{"hostUuid":"b"},{"hostUuid":"a"}]`),
	})

	job := workload{ID: "1", Engine: "lmstudio", RunID: "run", State: "queued", OriginatedFrom: "local", ScheduledOn: "a"}
	m.handleMessage(&Message{JSONRPC: "2.0", Method: "workloads:upsert", Params: upsertJSON(t, job)})
	job.State = "running"
	m.handleMessage(&Message{JSONRPC: "2.0", Method: "workloads:upsert", Params: upsertJSON(t, job)})

	for _, engine := range schedulerEngines {
		got := rec.orders(t, engine)
		require.Len(t, got, 2, " (%v)", engine)
		assertStrs(t, got[0], []string{"a", "b"})
		assertStrs(t, got[1], []string{"b", "a"})
	}

	job.State = "completed"
	m.handleMessage(&Message{JSONRPC: "2.0", Method: "workloads:upsert", Params: upsertJSON(t, job)})
	for _, engine := range schedulerEngines {
		got := rec.orders(t, engine)
		require.Len(t, got, 3, " (%v)", engine)
		assertStrs(t, got[2], []string{"a", "b"})
	}
	require.Empty(t, m.catalog, "terminal event left")
}

func TestHandleMessage_RebalancesOnFailoverRepoint(t *testing.T) {
	rec := &capRW{}
	m := NewManager(NewCodec(rec), 24*time.Hour)
	m.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  "discovery:nodes-changed",
		Params:  json.RawMessage(`[{"hostUuid":"a"},{"hostUuid":"b"},{"hostUuid":"c"}]`),
	})
	job := workload{ID: "1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "a"}
	m.handleMessage(&Message{JSONRPC: "2.0", Method: "workloads:upsert", Params: upsertJSON(t, job)})
	job.ScheduledOn = "c"
	m.handleMessage(&Message{JSONRPC: "2.0", Method: "workloads:upsert", Params: upsertJSON(t, job)})

	for _, engine := range schedulerEngines {
		got := rec.orders(t, engine)
		require.Len(t, got, 3, " (%v)", engine)
		assertStrs(t, got[1], []string{"b", "c", "a"})
		assertStrs(t, got[2], []string{"a", "b", "c"})
	}
}

func TestRecomputeAll_SerializesOlderAndNewerResults(t *testing.T) {
	rw := newGatedRW()
	m := mgrWith(rw, []string{"a", "b"})
	olderDone := make(chan struct{})
	go func() {
		m.recomputeAll(false)
		close(olderDone)
	}()

	select {
	case <-rw.blocked:
	case <-time.After(time.Second):
		require.FailNow(t, "older recompute did not reach blocked write")
	}
	job := workload{ID: "1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "a"}
	assert.True(t, m.applyUpsert(upsertJSON(t, job)), "new workload should change pending load")
	newerDone := make(chan struct{})
	go func() {
		m.recomputeAll(false)
		close(newerDone)
	}()
	close(rw.release)

	for name, done := range map[string]<-chan struct{}{"older": olderDone, "newer": newerDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			require.FailNow(t, fmt.Sprintf("%s did not complete", name))
		}
	}
	for _, engine := range schedulerEngines {
		got := rw.recorder.orders(t, engine)
		require.Len(t, got, 2, " (%v)", engine)
		assertStrs(t, got[0], []string{"a", "b"})
		assertStrs(t, got[1], []string{"b", "a"})
	}
}

func TestFeedbackBurstBalancesPastDepthThree(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b", "c"})
	depths := map[string]int{"a": 0, "b": 0, "c": 0}
	for i := 0; i < 50; i++ {
		order, _ := m.rank()
		target := order[0]
		engine := schedulerEngines[i%len(schedulerEngines)]
		job := workload{
			ID:             fmt.Sprintf("%d", i),
			Engine:         engine,
			RunID:          "burst",
			State:          "running",
			OriginatedFrom: "local",
			ScheduledOn:    target,
		}
		assert.True(t, m.applyUpsert(upsertJSON(t, job)), "assignment (%v, %v)", i, target)
		depths[target]++
	}

	minDepth, maxDepth := 50, 0
	for _, depth := range depths {
		if depth < minDepth {
			minDepth = depth
		}
		if depth > maxDepth {
			maxDepth = depth
		}
	}
	assert.Greater(t, minDepth, 3, "simulation never crossed proposed threshold: depths (%v)", depths)
	assert.LessOrEqual(t, maxDepth-minDepth, 1, "50 mixed-engine assignments are imbalanced: depths (%v)", depths)
}

// TestEmit_OnChangeOnly: emit on the first snapshot and on changes, silent
// otherwise, and always on a forced tick.
func TestEmit_OnChangeOnly(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"a", "b"})

	m.recomputeAll(false)
	require.Len(t, rec.orders(t, "ollama"), 1, "first compute should emit once")
	m.recomputeAll(false) // unchanged
	require.Len(t, rec.orders(t, "ollama"), 1, "unchanged order must not re-emit")
	m.recomputeAll(true) // forced
	require.Len(t, rec.orders(t, "ollama"), 2, "forced tick must re-emit")
}

func TestEmit_IncludesPendingRanks(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"a", "b"},
		workload{ID: "b1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "b"},
	)

	m.recomputeAll(false)
	got := rec.priorities(t, "ollama")
	require.Len(t, got, 1, "priority snapshots")
	want := []NodeRank{
		{ID: "a", Pending: 0, GPUPressure: unknownGPUPressure, Rank: 0},
		{ID: "b", Pending: 1, GPUPressure: unknownGPUPressure, Rank: 1},
	}
	assert.Equal(t, want, got[0].Ranks, "pending ranks")
}

func TestEmit_PendingOnlyChangeRefreshesSnapshot(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"a", "b"},
		workload{ID: "b1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "b"},
		workload{ID: "b2", Engine: "lmstudio", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "b"},
	)
	m.recomputeAll(false)

	job := workload{ID: "a1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "a"}
	assert.True(t, m.applyUpsert(upsertJSON(t, job)), "new workload should change pending load")
	m.recomputeAll(false)
	m.recomputeAll(false) // identical snapshot stays quiet

	got := rec.priorities(t, "ollama")
	require.Len(t, got, 2, "priority snapshots")
	assertStrs(t, got[0].Nodes, []string{"a", "b"})
	assertStrs(t, got[1].Nodes, []string{"a", "b"})
	assert.Equal(t, 0, got[0].Ranks[0].Pending, "a pending counts")
	assert.Equal(t, 1, got[1].Ranks[0].Pending, "a pending counts")
}

// TestEmit_EmptyUniverseSilent: with no nodes there is nothing to emit.
func TestEmit_EmptyUniverseSilent(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, nil)
	m.recomputeAll(false)
	require.Empty(t, rec.orders(t, "ollama"), "empty universe should stay silent")
}

// TestSetInterval_Floor: a sub-floor interval is clamped to 200ms.
func TestSetInterval_Floor(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, nil)
	id := json.RawMessage(`1`)
	m.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "scheduler:set-interval",
		Params:  json.RawMessage(`{"interval_ms":50}`),
	})
	assert.Contains(t, rec.String(), `"interval_ms":200`, "expected clamp to 200ms")
}

// TestNewManager_Floor: the constructor clamps the initial interval too.
func TestNewManager_Floor(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), 10*time.Millisecond)
	assert.Equal(t, intervalFloor, m.interval, "interval")
}
