// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/schedulerwire"
)

// lbProxy returns a proxy with late binding enabled at the given per-node slot
// ceiling. The wait budget is shortened from lateBindWaitTimeout so a test that
// exercises the give-up path fails with a wrong answer rather than a stalled
// suite; it stays long enough that a missed wakeup is still a visible failure.
func lbProxy(t *testing.T, slots int) *Proxy {
	t.Helper()
	p := prProxy(t)
	var capacity nodeParallelFlags
	if err := capacity.Set(strconv.Itoa(slots)); err != nil {
		t.Fatalf("node-parallel %d: %v", slots, err)
	}
	p.EnableLateBinding(capacity)
	p.lateBind.wait = 5 * time.Second
	return p
}

// reserveAsync runs one reservation on its own goroutine (reserveCandidate
// reorders the slice in place, so each caller gets a copy) and reports the
// reserved id plus its release through the returned channels.
func reserveAsync(ctx context.Context, p *Proxy, candidates []candidate) <-chan struct {
	id      string
	release func()
} {
	done := make(chan struct {
		id      string
		release func()
	}, 1)
	go func() {
		reserved, release := p.reserveCandidate(ctx, append([]candidate(nil), candidates...), nil)
		done <- struct {
			id      string
			release func()
		}{reserved[0].id, release}
	}()
	return done
}

// oneBusyOneIdle ranks "idle" as the more loaded node by the scheduler's
// estimate (GPU pressure) while "busy" is the one a caller then binds a request
// to. It is the fixture that separates the load estimate, which orders the
// choice, from occupancy, which gates it — and it deliberately leaves occupancy
// alone, because a snapshot cannot tell anyone what is generating right now.
func oneBusyOneIdle(p *Proxy) []candidate {
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"idle", "busy"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "idle", Pending: 0, GPUPressure: 3, Rank: 0},
			{ID: "busy", Pending: 1, GPUPressure: 0, Rank: 1},
		},
	})
	return reservationCandidates("idle", "busy")
}

// TestLateBindingOff_CommitsToTheLeastLoadedNodeAndNeverWaits is the
// default-behavior guard: with the flag off a node at capacity is still a
// candidate, the least loaded node wins exactly as before, and the proxy holds
// no waiting apparatus at all.
func TestLateBindingOff_CommitsToTheLeastLoadedNodeAndNeverWaits(t *testing.T) {
	p := prProxy(t)
	candidates := oneBusyOneIdle(p)

	got := reserveAsync(context.Background(), p, candidates)
	select {
	case reserved := <-got:
		if reserved.id != "busy" {
			t.Fatalf("reserved %q, want busy (the least estimated loaded node)", reserved.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reserveCandidate blocked with late binding off")
	}
	if p.lateBind != nil || p.slotFree != nil {
		t.Fatal("late binding state exists without --late-binding")
	}
}

// TestLateBinding_PrefersAFreeNodeOverALowerLoadedFullOne: the only routing
// difference late binding makes while a slot is available is that occupancy
// gates the choice. GPU pressure biases the order but is not a job count, so a
// node whose pressure is high yet has no generation running is still free.
func TestLateBinding_PrefersAFreeNodeOverALowerLoadedFullOne(t *testing.T) {
	p := lbProxy(t, 1)
	candidates := oneBusyOneIdle(p)

	// Make "busy" genuinely busy, and keep its slot. Even carrying that
	// request the scheduler's estimate still ranks it below "idle", whose GPU
	// pressure is high but whose engine is generating nothing.
	if first, _ := p.reserveCandidate(context.Background(), append([]candidate(nil), candidates...), nil); first[0].id != "busy" {
		t.Fatalf("first reservation went to %q, want busy (the least estimated loaded node)", first[0].id)
	}

	reserved, _ := p.reserveCandidate(context.Background(), candidates, nil)
	if reserved[0].id != "idle" {
		t.Fatalf("reserved %q, want idle (busy is at its generation ceiling)", reserved[0].id)
	}
	if got := candidateIDsFrom(reserved); got[1] != "busy" {
		t.Fatalf("failover list = %v, want busy retained behind idle", got)
	}
}

// TestLateBinding_WaitsForAReleasedSlot is the whole point of the change: with
// every eligible node generating, the request waits rather than committing to a
// queue, and takes the slot the moment one is released.
func TestLateBinding_WaitsForAReleasedSlot(t *testing.T) {
	p := lbProxy(t, 1)
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{{ID: "a"}, {ID: "b"}},
	})
	candidates := reservationCandidates("a", "b")

	first, releaseFirst := p.reserveCandidate(context.Background(), append([]candidate(nil), candidates...), nil)
	second, _ := p.reserveCandidate(context.Background(), append([]candidate(nil), candidates...), nil)
	if first[0].id == second[0].id {
		t.Fatalf("two reservations landed on %q; both nodes should be filled", first[0].id)
	}

	third := reserveAsync(context.Background(), p, candidates)
	select {
	case reserved := <-third:
		t.Fatalf("reserved %q with every node at capacity", reserved.id)
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case reserved := <-third:
		if reserved.id != first[0].id {
			t.Fatalf("reserved %q, want the released node %q", reserved.id, first[0].id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("releasing a slot did not wake the waiting request")
	}
}

// TestLateBinding_SnapshotWakesAWaitingRequest: a snapshot replaces the eligible
// set and the ordering, so a request parked when every listed owner was
// generating has to be woken by one — otherwise it waits out its whole budget
// for a node that became routable in the meantime.
func TestLateBinding_SnapshotWakesAWaitingRequest(t *testing.T) {
	p := lbProxy(t, 1)
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a"},
		Ranks: []schedulerwire.NodeRank{{ID: "a"}},
	})
	// "b" is an advertised owner the scheduler has not listed yet.
	candidates := reservationCandidates("a", "b")

	// Take a's only slot and keep it: the release is deliberately never called.
	if reserved, _ := p.reserveCandidate(context.Background(), append([]candidate(nil), candidates...), nil); reserved[0].id != "a" {
		t.Fatalf("first reservation went to %q, want the only listed node a", reserved[0].id)
	}

	waiting := reserveAsync(context.Background(), p, candidates)
	select {
	case <-waiting:
		t.Fatal("reserved a node that is already generating")
	case <-time.After(100 * time.Millisecond):
	}

	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 1}, {ID: "b", Pending: 0}},
	})
	select {
	case reserved := <-waiting:
		if reserved.id != "b" {
			t.Fatalf("reserved %q, want the newly listed free node b", reserved.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a new scheduler snapshot did not wake the waiting request")
	}
}

// TestLateBinding_AReleasedSlotIsFreeWithoutWaitingForTheScheduler is the
// over-blocking regression. The scheduler's pending count is this proxy's own
// dispatches arriving back a full round trip later, and every snapshot clears
// the reservations, so an occupancy model built from pending + reservations
// keeps a finished request occupying its node until some later snapshot happens
// to report the lower count — a late-binding router leaving nodes idle. The
// ledger the gate actually consults is retired by the request that finished, so
// the slot is free the moment it finishes, with no further snapshot.
func TestLateBinding_AReleasedSlotIsFreeWithoutWaitingForTheScheduler(t *testing.T) {
	p := lbProxy(t, 1)
	// Far longer than this test's own patience, so the second request can only
	// be reserved because the first freed its slot — never because the give-up
	// path rescued it.
	p.lateBind.wait = 10 * time.Second
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a"},
		Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 0}},
	})
	candidates := reservationCandidates("a")

	_, releaseFirst := p.reserveCandidate(context.Background(), append([]candidate(nil), candidates...), nil)
	// The scheduler catches up: its next snapshot counts the request just
	// dispatched and, as every snapshot does, clears this proxy's reservations.
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a"},
		Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 1}},
	})

	waiting := reserveAsync(context.Background(), p, candidates)
	select {
	case <-waiting:
		t.Fatal("reserved a node that is still generating")
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case reserved := <-waiting:
		if reserved.id != "a" {
			t.Fatalf("reserved %q, want a", reserved.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the node stayed at capacity after its only request finished: " +
			"occupancy is still waiting for a scheduler snapshot to retire it")
	}
}

// TestLateBinding_ASnapshotDoesNotFreeASlotThatIsStillGenerating is the other
// half of counting each in-flight request exactly once. A snapshot clears the
// reservations wholesale, so a snapshot taken before the scheduler heard about a
// request would hand its node's ceiling back while it is still generating. The
// request is counted by the ledger for its whole life, whatever the snapshot
// says.
func TestLateBinding_ASnapshotDoesNotFreeASlotThatIsStillGenerating(t *testing.T) {
	p := lbProxy(t, 1)
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a"},
		Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 0}},
	})
	candidates := reservationCandidates("a")

	_, release := p.reserveCandidate(context.Background(), append([]candidate(nil), candidates...), nil)
	// A snapshot the scheduler computed before it saw that request: node idle.
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a"},
		Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 0}},
	})

	waiting := reserveAsync(context.Background(), p, candidates)
	select {
	case reserved := <-waiting:
		t.Fatalf("reserved %q while its only slot is still generating: "+
			"the snapshot gave back capacity that is in use", reserved.id)
	case <-time.After(200 * time.Millisecond):
	}

	release()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("releasing the slot did not wake the waiting request")
	}
}

// TestLateBinding_CancelledRequestStopsWaiting: a client that hangs up (and,
// through the same root context, proxy shutdown) must not leave a request parked
// for the rest of its budget.
func TestLateBinding_CancelledRequestStopsWaiting(t *testing.T) {
	p := lbProxy(t, 1)
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a"},
		Ranks: []schedulerwire.NodeRank{{ID: "a"}},
	})
	// Occupy the only slot and keep it, so the next request has to wait.
	p.reserveCandidate(context.Background(), reservationCandidates("a"), nil)

	ctx, cancel := context.WithCancel(context.Background())
	waiting := reserveAsync(ctx, p, reservationCandidates("a"))
	select {
	case <-waiting:
		t.Fatal("reserved a node that is already generating")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the request did not wake it")
	}
}

// TestLateBinding_WaitBudgetFallsBackToImmediateCommit: an occupancy model that
// has gone stale must degrade to the current behavior rather than stall. Nothing
// here ever frees the slot, so the request has to give up and commit.
func TestLateBinding_WaitBudgetFallsBackToImmediateCommit(t *testing.T) {
	p := lbProxy(t, 1)
	p.lateBind.wait = 25 * time.Millisecond
	p.SetPrioritySnapshot(schedulerwire.Priority{
		Nodes: []string{"a", "b"},
		Ranks: []schedulerwire.NodeRank{
			{ID: "a", Pending: 4},
			{ID: "b", Pending: 2},
		},
	})

	// Fill both nodes and never release, standing in for an occupancy model that
	// has gone stale — a node that died mid-generation, a workload whose
	// terminal transition never arrived. Nothing here can free a slot.
	for range 2 {
		p.reserveCandidate(context.Background(), reservationCandidates("a", "b"), nil)
	}

	waiting := reserveAsync(context.Background(), p, reservationCandidates("a", "b"))
	select {
	case reserved := <-waiting:
		if reserved.id != "b" {
			t.Fatalf("gave up onto %q, want the least loaded node b", reserved.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a wedged occupancy model stalled the request past its wait budget")
	}
}

// TestLateBinding_ConcurrentBurstNeverExceedsCapacity: the reservation is a
// semaphore, so a burst may hold at most the configured number of slots on any
// node at any instant.
func TestLateBinding_ConcurrentBurstNeverExceedsCapacity(t *testing.T) {
	const slots = 2
	p := lbProxy(t, slots)
	ids := []string{"a", "b", "c"}
	ranks := make([]schedulerwire.NodeRank, 0, len(ids))
	for i, id := range ids {
		ranks = append(ranks, schedulerwire.NodeRank{ID: id, Rank: i})
	}
	p.SetPrioritySnapshot(schedulerwire.Priority{Nodes: ids, Ranks: ranks})
	candidates := reservationCandidates(ids...)

	var (
		mu    sync.Mutex
		held  = map[string]int{}
		worst = map[string]int{}
	)
	var wg sync.WaitGroup
	for range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reserved, release := p.reserveCandidate(context.Background(), append([]candidate(nil), candidates...), nil)
			id := reserved[0].id
			mu.Lock()
			held[id]++
			if held[id] > worst[id] {
				worst[id] = held[id]
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			held[id]--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if worst[id] > slots {
			t.Fatalf("node %q held %d concurrent generations, ceiling is %d (all: %v)", id, worst[id], slots, worst)
		}
	}
}

// TestLateBinding_HandleHTTPHoldsTheSecondRequestUntilTheFirstFinishes drives
// the real handler: the second inference request must not reach an engine that
// is still generating, and must be forwarded as soon as the first request
// reaches its terminal workload transition.
func TestLateBinding_HandleHTTPHoldsTheSecondRequestUntilTheFirstFinishes(t *testing.T) {
	var (
		mu       sync.Mutex
		received int
	)
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	inflight := func() int {
		mu.Lock()
		defer mu.Unlock()
		return received
	}

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received++
		first := received == 1
		mu.Unlock()
		if first {
			<-release // hold the single generation slot until the test lets go
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	// Deferred order matters (LIFO): unblock the held handler before the server
	// is torn down, including on t.Fatal.
	defer engine.Close()
	defer doRelease()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "solo-node", engine.URL, "llama"))
	p := testProxy(disc, 11435)
	p.EnableLateBinding(nodeParallelFlags{slots: 1})
	// A budget far longer than the test's own patience, so the second request
	// can only be forwarded because the first released its slot — never because
	// the give-up path rescued it.
	p.lateBind.wait = 10 * time.Second
	p.SetPriority([]string{"solo-node"})

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat",
			strings.NewReader(`{"model":"llama"}`)))
		return rec
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		post()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for inflight() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if inflight() != 1 {
		t.Fatal("the first request never reached the engine")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		post()
	}()
	time.Sleep(200 * time.Millisecond)
	if got := inflight(); got != 1 {
		t.Fatalf("%d requests reached the engine while its only slot was busy, want 1", got)
	}

	doRelease()
	<-firstDone
	select {
	case <-secondDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the second request was not forwarded when the first released its slot")
	}
	if got := inflight(); got != 2 {
		t.Fatalf("engine saw %d requests, want 2", got)
	}
}

func TestNodeParallelFlags_ParsesDefaultsAndPerNodeOverrides(t *testing.T) {
	var capacity nodeParallelFlags
	if got := capacity.slotsFor("anything"); got != defaultNodeParallel {
		t.Fatalf("unset --node-parallel = %d, want %d", got, defaultNodeParallel)
	}
	for _, value := range []string{"4", "big-rig=8", " small-rig = 1 "} {
		if err := capacity.Set(value); err != nil {
			t.Fatalf("--node-parallel %q: %v", value, err)
		}
	}
	for id, want := range map[string]int{"unlisted": 4, "big-rig": 8, "small-rig": 1} {
		if got := capacity.slotsFor(id); got != want {
			t.Fatalf("slots for %q = %d, want %d", id, got, want)
		}
	}
	if got := capacity.String(); got != "4,big-rig=8,small-rig=1" {
		t.Fatalf("--node-parallel String() = %q", got)
	}
	for _, value := range []string{"0", "-1", "", "node=", "node=zero"} {
		if err := capacity.Set(value); err == nil {
			t.Fatalf("--node-parallel %q was accepted", value)
		}
	}
}

// workloadSequence returns the workload:* lifecycle methods the codec wrote, in
// the order they were written, so a test can assert a lifecycle rather than a
// set of events that happened to occur.
func workloadSequence(rec *recRW) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []string
	for _, frame := range strings.Split(string(rec.b), "\n") {
		for _, method := range []string{
			workloadSubmittedMethod, workloadStartedMethod,
			workloadCompletedMethod, workloadErroredMethod,
		} {
			if strings.Contains(frame, `"method":"`+method+`"`) {
				out = append(out, method)
			}
		}
	}
	return out
}

// workloadFrame returns the first frame the codec wrote for one workload method,
// so a test can assert what that event actually carried.
func workloadFrame(rec *recRW, method string) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, frame := range strings.Split(string(rec.b), "\n") {
		if strings.Contains(frame, `"method":"`+method+`"`) {
			return frame
		}
	}
	return ""
}

// oneSlotEngine is a single-slot engine: the first request it receives is held
// until the returned release is called, standing in for a node that is
// generating. It reports how many requests have reached it.
func oneSlotEngine(t *testing.T) (server *httptest.Server, received func() int, release func()) {
	t.Helper()
	var (
		mu    sync.Mutex
		count int
	)
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := count == 0
		count++
		mu.Unlock()
		if first {
			<-gate
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	// Deferred order matters (LIFO): unblock the held handler before the server
	// is torn down, including on t.Fatal.
	t.Cleanup(server.Close)
	t.Cleanup(release)
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}, release
}

// TestLateBinding_AQueuedRequestIsAnnouncedThenRepointedAtItsNode is the
// visibility regression. Late binding parks a request instead of forwarding it,
// and an unforwarded request emits nothing at all — so a burst larger than the
// fleet showed job cards only for the requests that won a slot, and the rest
// looked like nothing was happening. The protocol already has the state: this
// asserts the parked request is announced as queued work with no node yet, and
// that the same workload id is re-pointed at the node once one is bound.
func TestLateBinding_AQueuedRequestIsAnnouncedThenRepointedAtItsNode(t *testing.T) {
	engine, received, release := oneSlotEngine(t)

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "solo-node", engine.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 11435)
	p.EnableLateBinding(nodeParallelFlags{slots: 1})
	p.lateBind.wait = 10 * time.Second
	p.SetPriority([]string{"solo-node"})

	post := func() {
		p.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(
			http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama"}`)))
	}

	first := make(chan struct{})
	go func() { defer close(first); post() }()
	deadline := time.Now().Add(2 * time.Second)
	for received() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if received() != 1 {
		t.Fatal("the first request never reached the engine")
	}

	second := make(chan struct{})
	go func() { defer close(second); post() }()
	deadline = time.Now().Add(2 * time.Second)
	for !rec.has(workloadSubmittedMethod) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// The second request is parked: it has not been forwarded, and the only
	// thing that can make it visible is the queued announcement.
	if got := received(); got != 1 {
		t.Fatalf("%d requests reached the engine while its only slot was busy, want 1", got)
	}
	queued := workloadFrame(rec, workloadSubmittedMethod)
	if queued == "" {
		t.Fatal("a request waiting for a generation slot emitted no workload:submitted, so it is invisible while it waits")
	}
	if !strings.Contains(queued, `"state":"queued"`) {
		t.Fatalf("workload:submitted did not carry state queued: %s", queued)
	}
	if strings.Contains(queued, `"scheduledOn"`) {
		t.Fatalf("workload:submitted named a node before one was chosen: %s", queued)
	}

	release()
	<-first
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("the queued request was never forwarded")
	}

	// submitted names no node; started re-points the same id at the one that
	// ran it. Both requests then reach a terminal, so neither card is left open.
	started := workloadFrame(rec, workloadStartedMethod)
	if !strings.Contains(started, `"scheduledOn":"solo-node"`) {
		t.Fatalf("workload:started did not name the node that ran it: %s", started)
	}
	sequence := workloadSequence(rec)
	var submitted, terminal int
	for _, method := range sequence {
		switch method {
		case workloadSubmittedMethod:
			submitted++
		case workloadCompletedMethod, workloadErroredMethod:
			terminal++
		}
	}
	if submitted != 1 {
		t.Fatalf("workload:submitted emitted %d times, want 1 (only the request that queued): %v", submitted, sequence)
	}
	if terminal != 2 {
		t.Fatalf("%d terminal transitions for 2 requests: %v", terminal, sequence)
	}
	if sequence[0] != workloadStartedMethod || sequence[1] != workloadSubmittedMethod {
		t.Fatalf("lifecycle order = %v, want the forwarded request started before the queued one is submitted", sequence)
	}
}

// TestLateBindingOff_NeverAnnouncesAQueuedWorkload is the default-behavior
// guard for the event stream. With the flag off the proxy still forwards on
// arrival and queues nothing, so workload:submitted must not appear: the
// component README's claim about the events it emits stays true for every
// installation that has not opted in.
func TestLateBindingOff_NeverAnnouncesAQueuedWorkload(t *testing.T) {
	engine, received, release := oneSlotEngine(t)

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "solo-node", engine.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 11435)
	p.SetPriority([]string{"solo-node"})

	post := func() {
		p.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(
			http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama"}`)))
	}
	done := make(chan struct{}, 2)
	for range 2 {
		go func() { post(); done <- struct{}{} }()
	}
	// Both requests are forwarded on arrival; the engine serializes them.
	deadline := time.Now().Add(2 * time.Second)
	for received() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := received(); got != 2 {
		t.Fatalf("%d requests reached the engine, want both forwarded immediately", got)
	}
	release()
	for range 2 {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("a request never finished")
		}
	}
	if rec.has(workloadSubmittedMethod) {
		t.Fatalf("workload:submitted emitted without --late-binding: %v", workloadSequence(rec))
	}
}
