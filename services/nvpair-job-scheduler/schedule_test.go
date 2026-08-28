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

func (r *capRW) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Contains(string(r.b), s)
}

// priorities returns every schedule:priority snapshot emitted for an engine.
func (r *capRW) priorities(engine string) []schedulePriorityParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []schedulePriorityParams
	for _, line := range strings.Split(string(r.b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg Message
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Method != "schedule:priority" {
			continue
		}
		var p schedulePriorityParams
		if json.Unmarshal(msg.Params, &p) != nil || p.Engine != engine {
			continue
		}
		out = append(out, p)
	}
	return out
}

// orders returns every schedule:priority node-order emitted for an engine.
func (r *capRW) orders(engine string) [][]string {
	priorities := r.priorities(engine)
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
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
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
	_, a := m.nodes["uuid-a"]
	_, b := m.nodes["uuid-b"]
	_, byName := m.nodes["host-a"]
	m.mu.Unlock()
	if !a || !b {
		t.Fatalf("node universe should key by hostUuid: %v", m.nodes)
	}
	if byName {
		t.Fatal("must not key by hostname")
	}

	// A workload scheduledOn the UUID counts against that node — proving the
	// scheduledOn value the proxy stamps (a UUID) matches the universe key.
	m.catalog[wlKey{origin: "uuid-a", id: "w1"}] = wl("w1", "ollama", "running", "uuid-a", "uuid-a")
	_, ranks := m.rank()
	for _, r := range ranks {
		if r.ID == "uuid-a" && r.Pending != 1 {
			t.Fatalf("scheduledOn=uuid-a should count against uuid-a, ranks=%+v", ranks)
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
	if ranks[0].Pending != 0 || ranks[2].Pending != 2 {
		t.Fatalf("unexpected pending counts: %+v", ranks)
	}
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
	if pendingOf(ranks, "b") != 1 {
		t.Fatalf("pending[b] = %d, want 1", pendingOf(ranks, "b"))
	}
	for id, want := range map[string]int{"a": 3, "b": 0, "c": 1, "d": 2} {
		if got := pressureOf(ranks, id); got != want {
			t.Fatalf("gpuPressure[%s] = %d, want %d; ranks=%+v", id, got, want, ranks)
		}
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
	if pressureOf(ranks, "a") != 0 ||
		pressureOf(ranks, "b") != unknownGPUPressure ||
		pressureOf(ranks, "c") != unknownGPUPressure {
		t.Fatalf("unexpected neutral-pressure ranking: %+v", ranks)
	}
}

// TestRank_UnplacedIgnored: a workload with no scheduledOn counts toward no node.
func TestRank_UnplacedIgnored(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b"},
		wl("1", "ollama", "running", "x", ""),
	)
	_, ranks := m.rank()
	for _, r := range ranks {
		if r.Pending != 0 {
			t.Fatalf("unplaced workload inflated a node: %+v", ranks)
		}
	}
}

// TestRank_ScheduledOnUnknownIgnored: work on a node not in discovery is dropped.
func TestRank_ScheduledOnUnknownIgnored(t *testing.T) {
	m := mgrWith(nopRW{}, []string{"a", "b"},
		wl("1", "ollama", "running", "x", "ghost"),
	)
	_, ranks := m.rank()
	for _, r := range ranks {
		if r.Pending != 0 {
			t.Fatalf("workload on unknown node counted: %+v", ranks)
		}
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
	if pendingOf(ranks, "a") != 3 || pendingOf(ranks, "b") != 1 || pendingOf(ranks, "c") != 0 {
		t.Fatalf("unexpected node-wide counts: %+v", ranks)
	}

	m.recomputeAll(false)
	for _, engine := range schedulerEngines {
		got := rec.orders(engine)
		if len(got) != 1 {
			t.Fatalf("%s emissions = %d, want 1", engine, len(got))
		}
		assertStrs(t, got[0], []string{"c", "b", "a"})
	}
}

// upsertJSON / removeJSON build the wire params applyUpsert / applyRemove parse,
// so these tests exercise the real ingestion path (and its catalog keying), not
// a hand-seeded catalog.
func upsertJSON(t *testing.T, w workload) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(workloadParams{WorkloadInfo: w})
	if err != nil {
		t.Fatalf("marshal upsert: %v", err)
	}
	return b
}

func removeJSON(t *testing.T, id, origin string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(removeParams{WorkloadID: id, OriginatedFrom: origin})
	if err != nil {
		t.Fatalf("marshal remove: %v", err)
	}
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

	if len(m.catalog) != 2 {
		t.Fatalf("catalog len = %d, want 2 (cross-engine id-1 collapsed?)", len(m.catalog))
	}
	if _, ranks := m.rank(); pendingOf(ranks, "a") != 1 || pendingOf(ranks, "b") != 1 {
		t.Fatalf("node-wide pending counts = %+v, want a=1 b=1", ranks)
	}
}

// TestApplyUpsert_SameIDAcrossRestartDistinct: a proxy restart mints a fresh
// runId, so a reused id after restart is a separate job — both must count until
// the old one is retired.
func TestApplyUpsert_SameIDAcrossRestartDistinct(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	m.nodes["a"] = true
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "ollama", RunID: "run-1", State: "running", OriginatedFrom: "x", ScheduledOn: "a"}))
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "ollama", RunID: "run-2", State: "running", OriginatedFrom: "x", ScheduledOn: "a"}))
	if len(m.catalog) != 2 {
		t.Fatalf("catalog len = %d, want 2 (same-id-across-restart collapsed?)", len(m.catalog))
	}
	if _, ranks := m.rank(); pendingOf(ranks, "a") != 2 {
		t.Fatalf("node-wide pending[a] = %d, want 2", pendingOf(ranks, "a"))
	}
}

// TestApplyRemove_DropsEveryEngineForID: the removal wire carries no
// engine/runId, so a remove for (origin,id) retires every composite key sharing
// that pair (mirroring the broker store's Remove).
func TestApplyRemove_DropsEveryEngineForID(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "ollama", RunID: "r-oll", State: "running", OriginatedFrom: "x", ScheduledOn: "a"}))
	m.applyUpsert(upsertJSON(t, workload{ID: "1", Engine: "lmstudio", RunID: "r-lms", State: "running", OriginatedFrom: "x", ScheduledOn: "b"}))
	if !m.applyRemove(removeJSON(t, "1", "x")) {
		t.Fatal("removing pending work should report a load change")
	}
	if len(m.catalog) != 0 {
		t.Fatalf("catalog len = %d after remove, want 0", len(m.catalog))
	}
}

func TestApplyUpsert_ActiveOnlyAndMeaningfulChanges(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	base := workload{ID: "1", Engine: "ollama", RunID: "run", State: "queued", OriginatedFrom: "x", ScheduledOn: "a"}

	if !m.applyUpsert(upsertJSON(t, base)) {
		t.Fatal("new placed work should change pending load")
	}
	base.State = "running"
	if m.applyUpsert(upsertJSON(t, base)) {
		t.Fatal("queued->running on the same node should not change pending load")
	}
	if got := m.catalog[wlKey{origin: "x", engine: "ollama", runID: "run", id: "1"}].State; got != "running" {
		t.Fatalf("catalog state = %q, want running", got)
	}

	base.ScheduledOn = "b"
	if !m.applyUpsert(upsertJSON(t, base)) {
		t.Fatal("failover re-point should change node-wide load")
	}
	base.State = "completed"
	if !m.applyUpsert(upsertJSON(t, base)) {
		t.Fatal("terminal transition should remove pending load")
	}
	if len(m.catalog) != 0 {
		t.Fatalf("terminal workload retained in active catalog: %+v", m.catalog)
	}
	if m.applyUpsert(upsertJSON(t, base)) {
		t.Fatal("duplicate terminal should be a no-op")
	}
}

func TestApplyNodesChanged_ReportsRealSetChanges(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), time.Second)
	first := json.RawMessage(`[{"hostUuid":"b"},{"hostUuid":"a"}]`)
	if !m.applyNodesChanged(first) {
		t.Fatal("initial non-empty node set should report changed")
	}
	if m.applyNodesChanged(json.RawMessage(`[{"hostUuid":"a"},{"hostUuid":"b"}]`)) {
		t.Fatal("reordered copy of the same set should be a no-op")
	}
	if !m.applyNodesChanged(json.RawMessage(`[{"hostUuid":"a"},{"hostUuid":"c"}]`)) {
		t.Fatal("membership replacement should report changed")
	}
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
		got := rec.orders(engine)
		if len(got) != 2 {
			t.Fatalf("%s emissions after start = %d, want 2 (discovery + load change)", engine, len(got))
		}
		assertStrs(t, got[0], []string{"a", "b"})
		assertStrs(t, got[1], []string{"b", "a"})
	}

	job.State = "completed"
	m.handleMessage(&Message{JSONRPC: "2.0", Method: "workloads:upsert", Params: upsertJSON(t, job)})
	for _, engine := range schedulerEngines {
		got := rec.orders(engine)
		if len(got) != 3 {
			t.Fatalf("%s emissions after terminal = %d, want 3", engine, len(got))
		}
		assertStrs(t, got[2], []string{"a", "b"})
	}
	if len(m.catalog) != 0 {
		t.Fatalf("terminal event left %d active workloads", len(m.catalog))
	}
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
		got := rec.orders(engine)
		if len(got) != 3 {
			t.Fatalf("%s emissions = %d, want 3", engine, len(got))
		}
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
		t.Fatal("older recompute did not reach blocked write")
	}
	job := workload{ID: "1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "a"}
	if !m.applyUpsert(upsertJSON(t, job)) {
		t.Fatal("new workload should change pending load")
	}
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
			t.Fatalf("%s recompute did not finish", name)
		}
	}
	for _, engine := range schedulerEngines {
		got := rw.recorder.orders(engine)
		if len(got) != 2 {
			t.Fatalf("%s emissions = %d, want old then new", engine, len(got))
		}
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
		if !m.applyUpsert(upsertJSON(t, job)) {
			t.Fatalf("assignment %d to %s did not change load", i, target)
		}
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
	if minDepth <= 3 {
		t.Fatalf("simulation never crossed proposed threshold: depths=%v", depths)
	}
	if maxDepth-minDepth > 1 {
		t.Fatalf("50 mixed-engine assignments are imbalanced: depths=%v", depths)
	}
}

// TestEmit_OnChangeOnly: emit on the first snapshot and on changes, silent
// otherwise, and always on a forced tick.
func TestEmit_OnChangeOnly(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"a", "b"})

	m.recomputeAll(false)
	if got := rec.orders("ollama"); len(got) != 1 {
		t.Fatalf("first compute should emit once, got %d", len(got))
	}
	m.recomputeAll(false) // unchanged
	if got := rec.orders("ollama"); len(got) != 1 {
		t.Fatalf("unchanged order must not re-emit, got %d", len(got))
	}
	m.recomputeAll(true) // forced
	if got := rec.orders("ollama"); len(got) != 2 {
		t.Fatalf("forced tick must re-emit, got %d", len(got))
	}
}

func TestEmit_IncludesPendingRanks(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"a", "b"},
		workload{ID: "b1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "b"},
	)

	m.recomputeAll(false)
	got := rec.priorities("ollama")
	if len(got) != 1 {
		t.Fatalf("priority snapshots = %d, want 1", len(got))
	}
	want := []NodeRank{
		{ID: "a", Pending: 0, GPUPressure: unknownGPUPressure, Rank: 0},
		{ID: "b", Pending: 1, GPUPressure: unknownGPUPressure, Rank: 1},
	}
	if len(got[0].Ranks) != len(want) {
		t.Fatalf("ranks = %+v, want %+v", got[0].Ranks, want)
	}
	for i := range want {
		if got[0].Ranks[i] != want[i] {
			t.Fatalf("rank[%d] = %+v, want %+v", i, got[0].Ranks[i], want[i])
		}
	}
}

func TestEmit_PendingOnlyChangeRefreshesSnapshot(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"a", "b"},
		workload{ID: "b1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "b"},
		workload{ID: "b2", Engine: "lmstudio", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "b"},
	)
	m.recomputeAll(false)

	job := workload{ID: "a1", Engine: "ollama", RunID: "run", State: "running", OriginatedFrom: "local", ScheduledOn: "a"}
	if !m.applyUpsert(upsertJSON(t, job)) {
		t.Fatal("new workload should change pending load")
	}
	m.recomputeAll(false)
	m.recomputeAll(false) // identical snapshot stays quiet

	got := rec.priorities("ollama")
	if len(got) != 2 {
		t.Fatalf("priority snapshots = %d, want initial plus pending-only refresh", len(got))
	}
	assertStrs(t, got[0].Nodes, []string{"a", "b"})
	assertStrs(t, got[1].Nodes, []string{"a", "b"})
	if got[0].Ranks[0].Pending != 0 || got[1].Ranks[0].Pending != 1 {
		t.Fatalf("a pending counts = %d then %d, want 0 then 1",
			got[0].Ranks[0].Pending, got[1].Ranks[0].Pending)
	}
}

// TestEmit_EmptyUniverseSilent: with no nodes there is nothing to emit.
func TestEmit_EmptyUniverseSilent(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, nil)
	m.recomputeAll(false)
	if got := rec.orders("ollama"); len(got) != 0 {
		t.Fatalf("empty universe should stay silent, got %v", got)
	}
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
	if !rec.has(`"interval_ms":200`) {
		t.Fatalf("expected clamp to 200ms, got: %s", rec.b)
	}
}

// TestNewManager_Floor: the constructor clamps the initial interval too.
func TestNewManager_Floor(t *testing.T) {
	m := NewManager(NewCodec(nopRW{}), 10*time.Millisecond)
	if m.interval != intervalFloor {
		t.Fatalf("interval = %v, want floor %v", m.interval, intervalFloor)
	}
}
