// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"nvpair-shared/engines"
)

func TestNormalizeSlots(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")
	test := func(name string, profile engineProfile, in *engineSlots, want engineSlots) {
		t.Run(name, func(t *testing.T) {
			if got := profile.normalizeSlots(in); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s normalizeSlots(%+v) = %+v, want %+v", profile.Name, in, got, want)
			}
		})
	}
	test("no slots", ollama, nil, engineSlots{})
	test("a default and per-model counts", lmstudio,
		&engineSlots{Default: 2, Models: map[string]int{"qwen/qwen3-8b": 4}},
		engineSlots{Default: 2, Models: map[string]int{"qwen/qwen3-8b": 4}})
	test("counts below one are dropped", lmstudio,
		&engineSlots{Default: 0, Models: map[string]int{"zero": 0, "negative": -1, "kept": 3}},
		engineSlots{Models: map[string]int{"kept": 3}})
	test("a negative default is dropped", ollama, &engineSlots{Default: -2}, engineSlots{})
	test("no valid model leaves no map", lmstudio,
		&engineSlots{Default: 1, Models: map[string]int{"zero": 0, " ": 2}},
		engineSlots{Default: 1})
	test("Ollama keys take the implied latest tag", ollama,
		&engineSlots{Default: 1, Models: map[string]int{"llama3": 2, "llama3:8b": 3}},
		engineSlots{Default: 1, Models: map[string]int{"llama3:latest": 2, "llama3:8b": 3}})
	test("names that normalize alike keep the smaller count", ollama,
		&engineSlots{Default: 1, Models: map[string]int{"llama3": 4, "llama3:latest": 2}},
		engineSlots{Default: 1, Models: map[string]int{"llama3:latest": 2}})
	test("LM Studio keys are kept exactly", lmstudio,
		&engineSlots{Default: 1, Models: map[string]int{"qwen3-8b": 2, "qwen3-8b:latest": 3}},
		engineSlots{Default: 1, Models: map[string]int{"qwen3-8b": 2, "qwen3-8b:latest": 3}})
}

// sendLocalBackend drives node/set-local-backend through the proxy's real
// dispatch and checks whether the proxy accepted it.
func sendLocalBackend(t *testing.T, p *Proxy, out *bytes.Buffer, engine, params string, wantAccepted bool) {
	t.Helper()
	id := json.RawMessage(`1`)
	out.Reset()
	p.handleMessage(&Message{
		Method: engines.AddressMethod(engine, "node/set-local-backend"),
		Params: json.RawMessage(params),
		ID:     &id,
	})
	var reply struct {
		Result *struct {
			OK bool `json:"ok"`
		} `json:"result"`
		Error *RPCError `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply %q: %v", out.String(), err)
	}
	accepted := reply.Error == nil && reply.Result != nil && reply.Result.OK
	if accepted != wantAccepted {
		t.Fatalf("node/set-local-backend %s: accepted = %v (error %+v), want %v", params, accepted, reply.Error, wantAccepted)
	}
}

func trackedSlots(f *facade) engineSlots {
	f.slots.mu.Lock()
	defer f.slots.mu.Unlock()
	return f.slots.slots
}

const ollamaBackendWithSlots = `{"engine":"ollama","host":"127.0.0.1","port":11435,"healthy":true,` +
	`"slots":{"default":2,"models":{"llama3":4,"broken":0}}}`

// The facade's tracker keeps the normalized counts, and the stored backend
// keeps none, so there is one copy of them.
func TestSetLocalBackendTracksNormalizedSlots(t *testing.T) {
	profile, _ := profileFor("ollama")
	var out bytes.Buffer
	p := newTestProxy(profile, NewCodec(&out), nil, 0)
	f := p.soleFacade()

	sendLocalBackend(t, p, &out, profile.Name, ollamaBackendWithSlots, true)
	want := engineSlots{Default: 2, Models: map[string]int{"llama3:latest": 4}}
	if got := trackedSlots(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked slots = %+v, want %+v", got, want)
	}
	if got := f.currentLocalBackend(); got.Port != 11435 || !got.Healthy || got.Slots != nil {
		t.Fatalf("stored backend = %+v with slots %+v, want healthy port 11435 and no slots", got, got.Slots)
	}
}

func TestSetLocalBackendReplacesTrackedSlots(t *testing.T) {
	profile, _ := profileFor("ollama")
	tracked := engineSlots{Default: 2, Models: map[string]int{"llama3:latest": 4}}
	test := func(name, params string, accepted bool, want engineSlots) {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			p := newTestProxy(profile, NewCodec(&out), nil, 0)
			f := p.soleFacade()
			sendLocalBackend(t, p, &out, profile.Name, ollamaBackendWithSlots, true)

			sendLocalBackend(t, p, &out, profile.Name, params, accepted)
			if got := trackedSlots(f); !reflect.DeepEqual(got, want) {
				t.Fatalf("tracked slots = %+v, want %+v", got, want)
			}
		})
	}
	test("new slots replace them",
		`{"engine":"ollama","host":"127.0.0.1","port":11435,"healthy":true,"slots":{"default":3}}`,
		true, engineSlots{Default: 3})
	test("a backend without slots clears them",
		`{"engine":"ollama","host":"127.0.0.1","port":11435,"healthy":false}`,
		true, engineSlots{})
	test("a rejected backend leaves them alone",
		`{"engine":"ollama","host":"192.0.2.1","port":11435,"healthy":true,"slots":{"default":9}}`,
		false, tracked)
}

// acquireTickets queues n tickets for model, in order.
func acquireTickets(tracker *slotTracker, model string, n int) []*slotTicket {
	tickets := make([]*slotTicket, n)
	for i := range tickets {
		tickets[i] = tracker.acquire(model)
	}
	return tickets
}

// runningStates reports, in order, which tickets hold a slot.
func runningStates(tickets ...*slotTicket) []bool {
	states := make([]bool, len(tickets))
	for i, k := range tickets {
		states[i] = k.isRunning()
	}
	return states
}

func expectRunning(t *testing.T, tickets []*slotTicket, want ...bool) {
	t.Helper()
	if got := runningStates(tickets...); !slices.Equal(got, want) {
		t.Fatalf("running = %v, want %v", got, want)
	}
}

// A model's tickets run in the order they were queued, as many at once as the
// model's own count allows, else the default, else one.
func TestSlotTrackerCapacity(t *testing.T) {
	test := func(name string, slots engineSlots, want ...bool) {
		t.Run(name, func(t *testing.T) {
			tracker := &slotTracker{}
			tracker.setSlots(slots)
			expectRunning(t, acquireTickets(tracker, "m", len(want)), want...)
		})
	}
	test("one when the engine reports none", engineSlots{}, true, false, false)
	test("the default", engineSlots{Default: 2}, true, true, false, false)
	test("the model's own count", engineSlots{Default: 3, Models: map[string]int{"m": 1}}, true, false, false)
	test("the default for an unlisted model", engineSlots{Default: 2, Models: map[string]int{"other": 1}},
		true, true, false)
}

// Each model has its own queue, so one model's waiting tickets never hold up
// another's.
func TestSlotTrackerQueuesPerModel(t *testing.T) {
	tracker := &slotTracker{}
	first := tracker.acquire("m1")
	waiting := tracker.acquire("m1")
	other := tracker.acquire("m2")
	expectRunning(t, []*slotTicket{first, waiting, other}, true, false, true)
}

// A released ticket's slot goes to the oldest waiting ticket, and closes its
// ready channel.
func TestSlotTrackerPromotesInOrder(t *testing.T) {
	tracker := &slotTracker{}
	tickets := acquireTickets(tracker, "m", 3)
	select {
	case <-tickets[1].readyCh():
		t.Fatal("a waiting ticket's ready channel is closed")
	default:
	}

	tickets[0].release()
	expectRunning(t, tickets[1:], true, false)
	select {
	case <-tickets[1].readyCh():
	default:
		t.Fatal("a promoted ticket's ready channel is still open")
	}

	tickets[1].release()
	expectRunning(t, tickets[2:], true)
}

func TestSlotTrackerCountChanges(t *testing.T) {
	t.Run("a higher count runs waiting tickets", func(t *testing.T) {
		tracker := &slotTracker{}
		tickets := acquireTickets(tracker, "m", 3)
		tracker.setSlots(engineSlots{Default: 3})
		expectRunning(t, tickets, true, true, true)
	})
	t.Run("a lower count takes effect only as tickets release", func(t *testing.T) {
		tracker := &slotTracker{}
		tracker.setSlots(engineSlots{Default: 3})
		tickets := acquireTickets(tracker, "m", 3)
		tracker.setSlots(engineSlots{Default: 1})
		expectRunning(t, tickets, true, true, true)

		late := tracker.acquire("m")
		tickets[0].release()
		tickets[1].release()
		expectRunning(t, []*slotTicket{tickets[2], late}, true, false)
		tickets[2].release()
		expectRunning(t, []*slotTicket{late}, true)
	})
}

// Output proves the engine is processing a request, so its ticket runs even
// past the count, and keeps the slot it proved until it releases.
func TestSlotTrackerOutputProvesASlot(t *testing.T) {
	tracker := &slotTracker{}
	tickets := acquireTickets(tracker, "m", 3)
	tickets[1].observeOutput()
	expectRunning(t, tickets, true, true, false)

	tickets[0].release()
	expectRunning(t, tickets[1:], true, false)
	tickets[1].release()
	expectRunning(t, tickets[2:], true)
}

func TestSlotTrackerRelease(t *testing.T) {
	t.Run("releasing twice frees one slot", func(t *testing.T) {
		tracker := &slotTracker{}
		tracker.setSlots(engineSlots{Default: 2})
		tickets := acquireTickets(tracker, "m", 4)
		tickets[0].release()
		tickets[0].release()
		expectRunning(t, tickets[1:], true, true, false)
	})
	t.Run("releasing a waiting ticket runs nothing", func(t *testing.T) {
		tracker := &slotTracker{}
		tickets := acquireTickets(tracker, "m", 3)
		tickets[1].release()
		expectRunning(t, []*slotTicket{tickets[0], tickets[2]}, true, false)
		tickets[0].release()
		expectRunning(t, tickets[2:], true)
	})
	t.Run("output after release changes nothing", func(t *testing.T) {
		tracker := &slotTracker{}
		tickets := acquireTickets(tracker, "m", 3)
		tickets[1].release()
		tickets[1].observeOutput()
		expectRunning(t, tickets, true, false, false)
	})
	t.Run("an emptied queue is removed", func(t *testing.T) {
		tracker := &slotTracker{}
		for _, k := range acquireTickets(tracker, "m", 2) {
			k.release()
		}
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		if len(tracker.queues) != 0 {
			t.Fatalf("queues = %v, want none left", tracker.queues)
		}
	})
}

// The facade keys tickets by the normalized model, so the names routing treats
// as one model share one queue and one count.
func TestAcquireSlotNormalizesModel(t *testing.T) {
	profile, _ := profileFor("ollama")
	f := newTestProxy(profile, NewCodec(rwNop{}), nil, 0).soleFacade()
	f.slots.setSlots(engineSlots{Default: 1, Models: map[string]int{"llama3:latest": 2}})
	tickets := []*slotTicket{f.acquireSlot("llama3"), f.acquireSlot("llama3:latest"), f.acquireSlot("llama3")}
	expectRunning(t, tickets, true, true, false)
}

// A request the tracker does not count has a nil ticket, and the request path
// calls it unconditionally.
func TestSlotTicketNilIsNoop(t *testing.T) {
	var k *slotTicket
	if k.isRunning() {
		t.Fatal("a nil ticket reports running")
	}
	if k.readyCh() != nil {
		t.Fatal("a nil ticket has a ready channel")
	}
	k.observeOutput()
	k.release()
}

// Many requests queueing, running and releasing at once, while the counts
// change under them, still give every ticket its slot, with nothing left
// queued.
func TestSlotTrackerConcurrentUse(t *testing.T) {
	tracker := &slotTracker{}
	const requests = 64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for n := 1; ; n = n%3 + 1 {
			select {
			case <-stop:
				return
			default:
				tracker.setSlots(engineSlots{Default: n})
			}
		}
	}()
	defer func() {
		close(stop)
		<-stopped
	}()
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := tracker.acquire(fmt.Sprintf("m%d", i%2))
			if i%5 == 0 {
				k.observeOutput()
			}
			<-k.readyCh()
			k.release()
		}()
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a ticket never got its slot")
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.queues) != 0 {
		t.Fatalf("queues = %v, want none left", tracker.queues)
	}
}
