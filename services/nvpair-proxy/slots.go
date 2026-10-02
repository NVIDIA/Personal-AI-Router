// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"sync"
)

// engineSlots is how many requests the local engine processes at once for one
// model. The broker relays it from engine-manager on node/set-local-backend.
// Models holds per-model counts, and Default covers every other model.
type engineSlots struct {
	Default int            `json:"default"`
	Models  map[string]int `json:"models,omitempty"`
}

// normalizeSlots keeps only counts of at least 1, and keys the per-model counts
// by the normalized model name routing compares. When two names normalize to
// one key, the smaller count wins. A missing default stays zero.
func (p engineProfile) normalizeSlots(s *engineSlots) engineSlots {
	var out engineSlots
	if s == nil {
		return out
	}
	if s.Default >= 1 {
		out.Default = s.Default
	}
	for model, n := range s.Models {
		key := p.normalizeModel(model)
		if key == "" || n < 1 {
			continue
		}
		if out.Models == nil {
			out.Models = make(map[string]int)
		}
		if cur, seen := out.Models[key]; !seen || n < cur {
			out.Models[key] = n
		}
	}
	return out
}

// slotTracker estimates which of the requests a facade sends its local engine
// hold one of the engine's slots, and which are waiting for one. It only
// decides what a job's state reads: nothing that routes, retries or times out a
// request consults it.
//
// Each model has a queue of tickets, in the order their requests went to the
// engine. A ticket runs once it is among the first tickets of its model to fit
// the model's slot count, or once the engine has produced output for it, which
// proves it holds a slot whatever the count says. A running ticket never goes
// back to waiting, so a lower count applies only as tickets release.
//
// The lock is never held across a call out of the tracker. The only side
// effect under it is closing a ticket's ready channel.
type slotTracker struct {
	mu     sync.Mutex
	slots  engineSlots
	queues map[string][]*slotTicket
}

// slotTicket is one request's place in its model's queue. A nil *slotTicket is
// a request the tracker does not count; every method is then a no-op.
type slotTicket struct {
	tracker  *slotTracker
	model    string
	running  bool
	released bool
	// ready is closed exactly once, when running becomes true.
	ready chan struct{}
}

// setSlots replaces the counts, and runs any waiting ticket a higher count now
// fits. The zero value means the engine reported none.
func (t *slotTracker) setSlots(s engineSlots) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.slots = s
	for model := range t.queues {
		t.promoteLocked(model)
	}
}

// acquireSlot queues a ticket on the facade's tracker for a request about to go
// to the local engine.
func (f *facade) acquireSlot(model string) *slotTicket {
	return f.slots.acquire(f.profile.normalizeModel(model))
}

// markRunningOnSlot marks the job's attempt on nodeID running once ticket holds
// a slot: at once if it already does, otherwise from a goroutine that waits for
// one and gives up when ctx, the attempt's context, ends. markRunning ignores a
// slot that reaches it after the job has moved to another attempt or finished.
func markRunningOnSlot(ctx context.Context, ticket *slotTicket, job *jobEvents, attempt uint64, nodeID string) {
	if ticket.isRunning() {
		job.markRunning(attempt, nodeID)
		return
	}
	go func() {
		select {
		case <-ticket.readyCh():
			job.markRunning(attempt, nodeID)
		case <-ctx.Done():
		}
	}()
}

// acquire queues a ticket for a request about to go to the engine. model is the
// normalized model name.
func (t *slotTracker) acquire(model string) *slotTicket {
	k := &slotTicket{tracker: t, model: model, ready: make(chan struct{})}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.queues == nil {
		t.queues = make(map[string][]*slotTicket)
	}
	t.queues[model] = append(t.queues[model], k)
	t.promoteLocked(model)
	return k
}

// capacityLocked is how many of model's tickets may run at once: the model's
// own count, else the default, else 1.
func (t *slotTracker) capacityLocked(model string) int {
	if n, ok := t.slots.Models[model]; ok {
		return n
	}
	if t.slots.Default > 0 {
		return t.slots.Default
	}
	return 1
}

// promoteLocked runs waiting tickets, in queue order, until model's running
// tickets fill its capacity.
func (t *slotTracker) promoteLocked(model string) {
	queue := t.queues[model]
	running := 0
	for _, k := range queue {
		if k.running {
			running++
		}
	}
	capacity := t.capacityLocked(model)
	for _, k := range queue {
		if running >= capacity {
			return
		}
		if !k.running {
			k.runLocked()
			running++
		}
	}
}

func (k *slotTicket) runLocked() {
	k.running = true
	close(k.ready)
}

// isRunning reports whether the ticket holds a slot.
func (k *slotTicket) isRunning() bool {
	if k == nil {
		return false
	}
	k.tracker.mu.Lock()
	defer k.tracker.mu.Unlock()
	return k.running
}

// readyCh is closed when the ticket starts running.
func (k *slotTicket) readyCh() <-chan struct{} {
	if k == nil {
		return nil
	}
	return k.ready
}

// observeOutput records that the engine produced output for the request, which
// means it holds a slot even when the count says it should still be waiting.
func (k *slotTicket) observeOutput() {
	if k == nil {
		return
	}
	t := k.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if k.released || k.running {
		return
	}
	k.runLocked()
}

// release takes the ticket out of its queue and, when that frees a slot, runs
// the next waiting ticket. Releasing again is a no-op.
func (k *slotTicket) release() {
	if k == nil {
		return
	}
	t := k.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if k.released {
		return
	}
	k.released = true
	queue := t.queues[k.model]
	if i := slices.Index(queue, k); i >= 0 {
		queue = slices.Delete(queue, i, i+1)
	}
	if len(queue) == 0 {
		delete(t.queues, k.model)
		return
	}
	t.queues[k.model] = queue
	t.promoteLocked(k.model)
}
