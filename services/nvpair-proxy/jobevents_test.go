// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"sync"
	"testing"
)

type emittedEvent struct {
	method string
	wl     Workload
}

// eventRecorder stands in for the codec: it keeps every event in the order it
// was written.
type eventRecorder struct {
	mu     sync.Mutex
	events []emittedEvent
}

func (r *eventRecorder) emit(method string, wl Workload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, emittedEvent{method: method, wl: wl})
}

func (r *eventRecorder) all() []emittedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// TestJobEventsWriteInSeqOrder: an event is written while the job's lock is
// held, so another goroutine cannot number and write a later event in between.
// The seq used to be assigned under the lock and written after it, which let
// the disconnect watcher's terminal reach the wire ahead of an earlier event.
func TestJobEventsWriteInSeqOrder(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var (
		mu        sync.Mutex
		holdNext  bool
		wireOrder []int64
	)
	emit := func(_ string, wl Workload) {
		mu.Lock()
		hold := holdNext
		holdNext = false
		mu.Unlock()
		if hold {
			close(entered)
			<-release
		}
		mu.Lock()
		wireOrder = append(wireOrder, wl.Seq)
		mu.Unlock()
	}
	j := newJobEvents(Workload{ID: "1", State: "queued"}, emit)

	mu.Lock()
	holdNext = true
	mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		j.repoint("nodeA")
	}()
	<-entered
	if j.mu.TryLock() {
		j.mu.Unlock()
		t.Error("the job's lock must be held while its event is written")
	}
	go func() {
		defer wg.Done()
		j.finish("cancelled", "client disconnected before completion")
	}()
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if want := []int64{1, 2, 3}; !slices.Equal(wireOrder, want) {
		t.Fatalf("wire order = %v, want %v", wireOrder, want)
	}
}

// TestJobEventsLifecycle: admission, a dispatch, the commit and the terminal
// are numbered from 1 in the order they happen, each with its method.
func TestJobEventsLifecycle(t *testing.T) {
	rec := &eventRecorder{}
	j := newJobEvents(Workload{ID: "7", Model: "m", Engine: "ollama", State: "queued", CreatedAt: 100}, rec.emit)
	j.dispatch("nodeA")
	j.start("nodeA")
	j.finish("completed", "")

	want := []struct {
		method, state, scheduledOn string
		seq                        int64
	}{
		{workloadSubmittedMethod, "queued", "", 1},
		{workloadSubmittedMethod, "queued", "nodeA", 2},
		{workloadStartedMethod, "running", "nodeA", 3},
		{workloadCompletedMethod, "completed", "nodeA", 4},
	}
	got := rec.all()
	if len(got) != len(want) {
		t.Fatalf("emitted %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.method != w.method || g.wl.State != w.state || g.wl.ScheduledOn != w.scheduledOn || g.wl.Seq != w.seq {
			t.Errorf("event %d = %s %s on %q at seq %d, want %s %s on %q at seq %d",
				i, g.method, g.wl.State, g.wl.ScheduledOn, g.wl.Seq, w.method, w.state, w.scheduledOn, w.seq)
		}
	}
	if got[2].wl.StartedAt == nil {
		t.Error("the started event carries no startedAt")
	}
	if got[3].wl.CompletedAt == nil {
		t.Error("the completed event carries no completedAt")
	}
}

// TestJobEventsOtherTerminalsRideErrored: every terminal state except
// "completed" is sent as workload:errored, carrying its state and reason.
func TestJobEventsOtherTerminalsRideErrored(t *testing.T) {
	test := func(state string) {
		t.Run(state, func(t *testing.T) {
			rec := &eventRecorder{}
			j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
			j.finish(state, "reason")

			got := rec.all()
			if len(got) != 2 {
				t.Fatalf("emitted %d events, want admission and terminal: %+v", len(got), got)
			}
			terminal := got[1]
			if terminal.method != workloadErroredMethod || terminal.wl.State != state {
				t.Fatalf("terminal = %s %s, want %s %s", terminal.method, terminal.wl.State, workloadErroredMethod, state)
			}
			if terminal.wl.Error == nil || *terminal.wl.Error != "reason" {
				t.Fatalf("terminal error = %v, want %q", terminal.wl.Error, "reason")
			}
		})
	}
	test("failed")
	test("cancelled")
}

// TestJobEventsNothingAfterFinish: the terminal is emitted at most once, and a
// late dispatch, slot or commit after it emits nothing.
func TestJobEventsNothingAfterFinish(t *testing.T) {
	rec := &eventRecorder{}
	j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
	attempt := j.dispatch("nodeA")
	j.finish("cancelled", "client disconnected before completion")
	j.finish("completed", "")
	j.markRunning(attempt, "nodeA")
	j.repoint("nodeB")
	j.dispatch("nodeB")
	j.start("nodeB")

	got := rec.all()
	if len(got) != 3 {
		t.Fatalf("emitted %d events, want admission, one dispatch and one terminal: %+v", len(got), got)
	}
	if got[2].wl.State != "cancelled" {
		t.Fatalf("terminal state = %q, want the first finish to win", got[2].wl.State)
	}
}

// TestJobEventsRepointToCurrentPlacement: moving the job to where it already is
// emits nothing.
func TestJobEventsRepointToCurrentPlacement(t *testing.T) {
	rec := &eventRecorder{}
	j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
	j.repoint("")
	j.repoint("nodeA")
	j.repoint("nodeA")

	if got := rec.all(); len(got) != 2 {
		t.Fatalf("emitted %d events, want admission and one re-point: %+v", len(got), got)
	}
}

// TestJobEventsRepointFromRunning: a job that moves on from the node running it
// is waiting again, so it reads queued with no StartedAt until its next node
// starts it. That includes a new attempt on the same node.
func TestJobEventsRepointFromRunning(t *testing.T) {
	test := func(name string, move func(j *jobEvents), wantScheduledOn string) {
		t.Run(name, func(t *testing.T) {
			rec := &eventRecorder{}
			j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
			j.markRunning(j.dispatch("nodeA"), "nodeA")
			move(j)

			got := rec.all()
			if len(got) != 4 {
				t.Fatalf("emitted %d events, want admission, dispatch, slot and the move: %+v", len(got), got)
			}
			moved := got[3]
			if moved.method != workloadSubmittedMethod || moved.wl.State != "queued" || moved.wl.ScheduledOn != wantScheduledOn {
				t.Fatalf("move = %s %s on %q, want %s queued on %q",
					moved.method, moved.wl.State, moved.wl.ScheduledOn, workloadSubmittedMethod, wantScheduledOn)
			}
			if moved.wl.StartedAt != nil {
				t.Fatalf("a re-queued job carries startedAt %d", *moved.wl.StartedAt)
			}
		})
	}
	test("to another node", func(j *jobEvents) { j.dispatch("nodeB") }, "nodeB")
	test("to no node", func(j *jobEvents) { j.repoint("") }, "")
	test("to the same node", func(j *jobEvents) { j.dispatch("nodeA") }, "nodeA")
}

// TestJobEventsMarkRunning: a slot starts the job only for the attempt that
// holds it, while the job still waits on that attempt's node.
func TestJobEventsMarkRunning(t *testing.T) {
	t.Run("the current attempt runs", func(t *testing.T) {
		rec := &eventRecorder{}
		j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
		j.markRunning(j.dispatch("nodeA"), "nodeA")

		got := rec.all()
		if len(got) != 3 {
			t.Fatalf("emitted %d events, want admission, dispatch and slot: %+v", len(got), got)
		}
		slot := got[2]
		if slot.method != workloadStartedMethod || slot.wl.State != "running" || slot.wl.ScheduledOn != "nodeA" {
			t.Fatalf("slot = %s %s on %q, want %s running on nodeA",
				slot.method, slot.wl.State, slot.wl.ScheduledOn, workloadStartedMethod)
		}
		if slot.wl.StartedAt == nil {
			t.Fatal("the slot's started event carries no startedAt")
		}
	})

	ignored := func(name string, setup func(j *jobEvents) (attempt uint64, nodeID string)) {
		t.Run(name, func(t *testing.T) {
			rec := &eventRecorder{}
			j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
			attempt, nodeID := setup(j)
			before := len(rec.all())
			j.markRunning(attempt, nodeID)
			if got := rec.all(); len(got) != before {
				t.Fatalf("markRunning(%d, %q) emitted %+v", attempt, nodeID, got[before:])
			}
		})
	}
	ignored("an attempt the job has moved on from", func(j *jobEvents) (uint64, string) {
		stale := j.dispatch("nodeA")
		j.repoint("")
		j.dispatch("nodeA")
		return stale, "nodeA"
	})
	ignored("another node", func(j *jobEvents) (uint64, string) {
		return j.dispatch("nodeA"), "nodeB"
	})
	ignored("a job between attempts", func(j *jobEvents) (uint64, string) {
		attempt := j.dispatch("nodeA")
		j.repoint("")
		return attempt, "nodeA"
	})
	ignored("a job already running", func(j *jobEvents) (uint64, string) {
		attempt := j.dispatch("nodeA")
		j.markRunning(attempt, "nodeA")
		return attempt, "nodeA"
	})
	ignored("a finished job", func(j *jobEvents) (uint64, string) {
		attempt := j.dispatch("nodeA")
		j.finish("completed", "")
		return attempt, "nodeA"
	})
}

// TestJobEventsCommitAfterSlot: a job a slot already started on the serving
// node emits nothing at the commit, and keeps the StartedAt the slot gave it.
func TestJobEventsCommitAfterSlot(t *testing.T) {
	rec := &eventRecorder{}
	j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
	j.markRunning(j.dispatch("nodeA"), "nodeA")
	j.start("nodeA")
	j.finish("completed", "")

	got := rec.all()
	if len(got) != 4 {
		t.Fatalf("emitted %d events, want admission, dispatch, slot and terminal: %+v", len(got), got)
	}
	slot, terminal := got[2], got[3]
	if slot.method != workloadStartedMethod || terminal.method != workloadCompletedMethod {
		t.Fatalf("events end %s, %s, want %s, %s", slot.method, terminal.method, workloadStartedMethod, workloadCompletedMethod)
	}
	if slot.wl.StartedAt == nil || terminal.wl.StartedAt == nil || *terminal.wl.StartedAt != *slot.wl.StartedAt {
		t.Fatalf("terminal startedAt = %v, want the slot's %v", terminal.wl.StartedAt, slot.wl.StartedAt)
	}
}

// TestJobEventsNilIsNoop: a request that is not tracked as a workload has a nil
// job, and the request path calls it unconditionally.
func TestJobEventsNilIsNoop(t *testing.T) {
	var j *jobEvents
	j.repoint("nodeA")
	if attempt := j.dispatch("nodeA"); attempt != 0 {
		t.Fatalf("a nil job numbered an attempt %d", attempt)
	}
	j.markRunning(1, "nodeA")
	j.start("nodeA")
	j.finish("completed", "")
}
