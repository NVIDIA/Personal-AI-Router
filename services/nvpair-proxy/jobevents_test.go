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
	j.repoint("nodeA")
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
// late dispatch or commit after it emits nothing.
func TestJobEventsNothingAfterFinish(t *testing.T) {
	rec := &eventRecorder{}
	j := newJobEvents(Workload{ID: "1", State: "queued"}, rec.emit)
	j.finish("cancelled", "client disconnected before completion")
	j.finish("completed", "")
	j.repoint("nodeB")
	j.start("nodeB")

	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("emitted %d events, want admission and one terminal: %+v", len(got), got)
	}
	if got[1].wl.State != "cancelled" {
		t.Fatalf("terminal state = %q, want the first finish to win", got[1].wl.State)
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

// TestJobEventsNilIsNoop: a request that is not tracked as a workload has a nil
// job, and the request path calls it unconditionally.
func TestJobEventsNilIsNoop(t *testing.T) {
	var j *jobEvents
	j.repoint("nodeA")
	j.start("nodeA")
	j.finish("completed", "")
}
