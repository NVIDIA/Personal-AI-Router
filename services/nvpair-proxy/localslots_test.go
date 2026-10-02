// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldEngine is a fake local engine that answers each request only when the
// test says so. Requests are numbered as they arrive, from 1.
type heldEngine struct {
	server  *httptest.Server
	arrived chan int
	stop    chan struct{}
	mu      sync.Mutex
	count   int
	gates   map[int]chan struct{}
}

func newHeldEngine(t *testing.T) *heldEngine {
	t.Helper()
	e := &heldEngine{arrived: make(chan int, 16), stop: make(chan struct{}), gates: make(map[int]chan struct{})}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := e.arrive()
		e.arrived <- n
		select {
		case <-e.gate(n):
		case <-r.Context().Done():
			return
		case <-e.stop:
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	t.Cleanup(func() {
		close(e.stop)
		e.server.Close()
	})
	return e
}

func (e *heldEngine) arrive() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.count++
	return e.count
}

func (e *heldEngine) gate(n int) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	g, ok := e.gates[n]
	if !ok {
		g = make(chan struct{})
		e.gates[n] = g
	}
	return g
}

// answer lets request n respond.
func (e *heldEngine) answer(n int) {
	close(e.gate(n))
}

// awaitArrival waits for request n to reach the engine.
func (e *heldEngine) awaitArrival(t *testing.T, n int) {
	t.Helper()
	select {
	case got := <-e.arrived:
		if got != n {
			t.Fatalf("request %d reached the engine, want request %d", got, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("request %d never reached the engine", n)
	}
}

// localSlotFacade builds a facade whose own node, node-a, advertises the
// test's model and is served from the local engine at engineURL with one slot.
// extra nodes are added beside it.
func localSlotFacade(t *testing.T, tc engineCase, rec *recRW, engineURL string, extra ...Node) *facade {
	t.Helper()
	port := tc.profile.FacadePort
	disc := NewDiscovery()
	disc.AddManual(Node{ID: "node-a", Addresses: []string{"127.0.0.1"}, Port: port, Models: []string{tc.advertisedModel}})
	for _, n := range extra {
		disc.AddManual(n)
	}
	f := newTestProxy(tc.profile, NewCodec(rec), disc, port).soleFacade()
	engine := nodeFor(t, "engine", engineURL)
	err := f.setLocalBackend(localBackend{
		Engine:  tc.profile.Name,
		Host:    "127.0.0.1",
		Port:    engine.Port,
		Healthy: true,
		Slots:   &engineSlots{Default: 1},
	})
	if err != nil {
		t.Fatalf("setLocalBackend: %v", err)
	}
	return f
}

// serveAsync runs r through the facade, and closes the returned channel when
// the handler returns.
func serveAsync(f *facade, r *http.Request) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.handleHTTP(httptest.NewRecorder(), r)
	}()
	return done
}

func awaitHandler(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never returned", what)
	}
}

// jobStep is one workload event, reduced to what these tests compare.
type jobStep struct {
	method, state, scheduledOn string
}

func queuedOn(node string) jobStep    { return jobStep{workloadSubmittedMethod, "queued", node} }
func runningOn(node string) jobStep   { return jobStep{workloadStartedMethod, "running", node} }
func completedOn(node string) jobStep { return jobStep{workloadCompletedMethod, "completed", node} }
func cancelledOn(node string) jobStep { return jobStep{workloadErroredMethod, "cancelled", node} }

// jobEventsFor decodes, in order, the workload notifications the proxy has
// written for job id.
func jobEventsFor(t *testing.T, rec *recRW, id string) []emittedEvent {
	t.Helper()
	rec.mu.Lock()
	frames := bytes.Split(bytes.Clone(rec.b), []byte("\n"))
	rec.mu.Unlock()
	var out []emittedEvent
	for _, frame := range frames {
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &msg); err != nil {
			t.Fatalf("decode frame %q: %v", frame, err)
		}
		if !strings.HasPrefix(msg.Method, "workload:") {
			continue
		}
		var params workloadParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatalf("decode %s params %s: %v", msg.Method, msg.Params, err)
		}
		if params.WorkloadInfo.ID == id {
			out = append(out, emittedEvent{method: msg.Method, wl: params.WorkloadInfo})
		}
	}
	return out
}

func stepsOf(events []emittedEvent) []jobStep {
	steps := make([]jobStep, len(events))
	for i, e := range events {
		steps[i] = jobStep{e.method, e.wl.State, e.wl.ScheduledOn}
	}
	return steps
}

func expectSteps(t *testing.T, rec *recRW, id string, want ...jobStep) []emittedEvent {
	t.Helper()
	events := jobEventsFor(t, rec, id)
	if got := stepsOf(events); !slices.Equal(got, want) {
		t.Fatalf("job %s events = %+v, want %+v", id, got, want)
	}
	return events
}

// awaitSteps waits for job id's events to become exactly want, for the
// transitions a slot's waiter makes from its own goroutine.
func awaitSteps(t *testing.T, rec *recRW, id string, want ...jobStep) []emittedEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events := jobEventsFor(t, rec, id)
		got := stepsOf(events)
		if slices.Equal(got, want) {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s events = %+v, want %+v", id, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// With one slot, a request the engine has but has not started reads queued on
// this node, and running once the request ahead of it finishes, before its own
// answer arrives.
func TestLocalAttemptWaitsForASlot(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		engine := newHeldEngine(t)
		rec := &recRW{}
		f := localSlotFacade(t, tc, rec, engine.server.URL)

		first := serveAsync(f, tc.inferenceRequest())
		engine.awaitArrival(t, 1)
		second := serveAsync(f, tc.inferenceRequest())
		engine.awaitArrival(t, 2)
		expectSteps(t, rec, "1", queuedOn(""), queuedOn("node-a"), runningOn("node-a"))
		expectSteps(t, rec, "2", queuedOn(""), queuedOn("node-a"))

		engine.answer(1)
		awaitHandler(t, first, "the first request")
		awaitSteps(t, rec, "2", queuedOn(""), queuedOn("node-a"), runningOn("node-a"))

		engine.answer(2)
		awaitHandler(t, second, "the second request")
		expectSteps(t, rec, "1", queuedOn(""), queuedOn("node-a"), runningOn("node-a"), completedOn("node-a"))
		expectSteps(t, rec, "2", queuedOn(""), queuedOn("node-a"), runningOn("node-a"), completedOn("node-a"))
	})
}

// A request that takes a free slot reads running before the engine answers.
// The stream's first content then emits nothing more, and the job keeps the
// StartedAt its slot gave it.
func TestLocalAttemptStreamKeepsItsSlotStart(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		arrived := make(chan struct{}, 1)
		finish := make(chan struct{})
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case arrived <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":false}`+"\n")
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			select {
			case <-finish:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, `{"done":true}`+"\n")
		}))
		defer engine.Close()
		var finishOnce sync.Once
		release := func() { finishOnce.Do(func() { close(finish) }) }
		defer release()

		rec := &recRW{}
		f := localSlotFacade(t, tc, rec, engine.URL)
		done := serveAsync(f, tc.inferenceRequest())
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("the request never reached the engine")
		}
		if !waitFor(t, rec, "proxy/request-started") {
			t.Fatal("the stream never committed")
		}
		expectSteps(t, rec, "1", queuedOn(""), queuedOn("node-a"), runningOn("node-a"))

		release()
		awaitHandler(t, done, "the request")
		events := expectSteps(t, rec, "1", queuedOn(""), queuedOn("node-a"), runningOn("node-a"), completedOn("node-a"))
		slot, terminal := events[2].wl.StartedAt, events[3].wl.StartedAt
		if slot == nil || terminal == nil || *terminal != *slot {
			t.Fatalf("completed startedAt = %v, want the slot's %v", terminal, slot)
		}
	})
}

// A request that held a slot and then fails over is waiting again: it reads
// queued on the next node, with no StartedAt, until that node starts it. The
// nodes the scheduler sees it pending on are the same as without the slot.
func TestLocalAttemptFailoverRequeues(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer busy.Close()
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer other.Close()

		rec := &recRW{}
		f := localSlotFacade(t, tc, rec, busy.URL, nodeForModel(t, "node-b", other.URL, tc.advertisedModel))
		f.handleHTTP(httptest.NewRecorder(), tc.inferenceRequest())

		events := expectSteps(t, rec, "1",
			queuedOn(""), queuedOn("node-a"), runningOn("node-a"),
			queuedOn("node-b"), runningOn("node-b"), completedOn("node-b"))
		if requeued := events[3].wl.StartedAt; requeued != nil {
			t.Fatalf("the failed-over job carries startedAt %d while it waits on node-b", *requeued)
		}
		var pending []string
		for _, e := range events[:len(events)-1] {
			if len(pending) == 0 || pending[len(pending)-1] != e.wl.ScheduledOn {
				pending = append(pending, e.wl.ScheduledOn)
			}
		}
		if want := []string{"", "node-a", "node-b"}; !slices.Equal(pending, want) {
			t.Fatalf("pending on %q, want %q", pending, want)
		}
	})
}

// A client that leaves while its request waits for a slot ends the job as
// cancelled, and the slot that frees afterwards does not start it.
func TestLocalAttemptCancelledWhileWaiting(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		engine := newHeldEngine(t)
		rec := &recRW{}
		f := localSlotFacade(t, tc, rec, engine.server.URL)

		first := serveAsync(f, tc.inferenceRequest())
		engine.awaitArrival(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		second := serveAsync(f, tc.inferenceRequest().WithContext(ctx))
		engine.awaitArrival(t, 2)

		cancel()
		awaitHandler(t, second, "the cancelled request")
		engine.answer(1)
		awaitHandler(t, first, "the first request")

		expectSteps(t, rec, "2", queuedOn(""), queuedOn("node-a"), cancelledOn("node-a"))
	})
}
