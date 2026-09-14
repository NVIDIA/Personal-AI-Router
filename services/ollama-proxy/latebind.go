// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Late binding (--late-binding, off by default) changes *when* a request is
// committed to a node, and nothing else.
//
// Today the proxy picks the least estimated loaded eligible node the moment the
// request arrives and forwards immediately. That decision is irrevocable: the
// request queues on that node however the fleet moves afterwards, so a burst
// that lands on a node which then turns out to be busy waits behind it while
// another node goes idle. With late binding the same choice is made only among
// nodes that still have a free generation slot, and a request that finds every
// eligible node at capacity waits for one to free instead of joining a queue.
//
// The scheduler contract is untouched. node/set-priority, its ranks, and the
// ordering they imply are consumed exactly as before; the same least-estimated-
// loaded rule decides, applied to the nodes that have a slot. So the node chosen
// is the one the proxy picks today whenever that node is free, and otherwise a
// free node wins over one that is at capacity.
//
// OCCUPANCY. The gate counts one thing: the generations this proxy has bound to
// a node and not yet seen finish, measured against a per-node ceiling
// (--node-parallel, the node's OLLAMA_NUM_PARALLEL). That ledger is exact by
// construction — one entry is added when a request is bound and removed at its
// terminal workload transition, so every in-flight request is counted exactly
// once, for exactly as long as the node is generating for it.
//
// It deliberately does NOT include the scheduler's pending count, which the load
// estimate still uses to *order* the choice. Pending is the same requests seen
// through a full round trip — proxy -> broker -> workload-manager -> scheduler
// catalog -> recompute -> schedule:priority -> broker -> node/set-priority — so
// folding it into the gate counts a request twice over: once locally and again
// as it comes back around. Worse, it is asymmetric. SetPrioritySnapshot clears
// this proxy's reservations wholesale on every snapshot, so by the time a
// request finishes, its own reservation is usually already gone and giving the
// slot back is a no-op: the node stays at capacity until a later snapshot
// happens to report the lower count. The slot is then dead for a whole
// scheduler round trip on every completion, and if anything downstream fails to
// retire the workload it is dead until the wait budget expires. A gate must be
// prompt and local; a ranking may be lagging and remote.
//
// PRECONDITION. Because the ledger counts this proxy's own dispatches, it only
// describes the node while the proxy is that node's only client: anything else
// generating on that engine — a local `ollama run`, a second router, an
// application pointed straight at the engine port — is invisible here, and the
// ceiling is then fiction. PAIR's own design satisfies this, since the proxy is
// the front door and the engine is moved aside onto a loopback port, but a
// hand-assembled setup need not.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultNodeParallel is the concurrent-generation ceiling assumed for a node
// with no --node-parallel override. The value to match is the node's own
// OLLAMA_NUM_PARALLEL, which current Ollama releases default to a single
// generation at a time — that default is what makes a second request sent to a
// busy node a queued request rather than a concurrent one.
const defaultNodeParallel = 1

// lateBindWaitTimeout bounds how long one request waits for a generation slot
// before giving up and committing to the least loaded node anyway — exactly
// what the proxy does today. It is the safety valve for an occupancy model that
// has gone stale (a node that died mid-generation, a workload whose terminal
// transition never arrived, an engine someone else is also driving), so a wedged
// node degrades routing to the current behavior instead of stalling it. Sized in
// the same range as proxyResponseTimeout: a generation that is ever going to
// free its slot does so well inside this window.
const lateBindWaitTimeout = 2 * time.Minute

// nodeParallelFlags collects repeated --node-parallel values and answers the
// per-node ceiling. A bare "N" sets the count used for every node; "<node-id>=N"
// overrides one node, for a fleet whose machines are configured differently.
type nodeParallelFlags struct {
	slots     int
	overrides map[string]int
}

func (f *nodeParallelFlags) String() string {
	if f == nil {
		return strconv.Itoa(defaultNodeParallel)
	}
	parts := []string{strconv.Itoa(f.slotsFor(""))}
	ids := make([]string, 0, len(f.overrides))
	for id := range f.overrides {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		parts = append(parts, id+"="+strconv.Itoa(f.overrides[id]))
	}
	return strings.Join(parts, ",")
}

func (f *nodeParallelFlags) Set(value string) error {
	id, count, hasID := strings.Cut(value, "=")
	if !hasID {
		id, count = "", value
	}
	slots, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || slots < 1 {
		return fmt.Errorf("want a positive slot count as N or <node-id>=N, got %q", value)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		f.slots = slots
		return nil
	}
	if f.overrides == nil {
		f.overrides = make(map[string]int)
	}
	f.overrides[id] = slots
	return nil
}

// slotsFor returns the concurrent-generation ceiling for one node: its override
// when it has one, otherwise the fleet-wide value.
func (f nodeParallelFlags) slotsFor(id string) int {
	if slots, ok := f.overrides[id]; ok {
		return slots
	}
	if f.slots > 0 {
		return f.slots
	}
	return defaultNodeParallel
}

// lateBindConfig is the whole of the late-binding configuration. A nil
// *lateBindConfig on the proxy means the feature is off, which is the default.
type lateBindConfig struct {
	capacity nodeParallelFlags
	// wait bounds a single request's wait for a free slot (lateBindWaitTimeout
	// in production; tests shorten it to keep the give-up path fast).
	wait time.Duration
}

// EnableLateBinding switches the proxy from committing a request to a node on
// arrival to committing it when that node has a free generation slot. It is
// called once from main before Run — the configuration and the condition
// variable are read-only from then on, so the request path needs no lock to
// find out whether the feature is on.
func (p *Proxy) EnableLateBinding(capacity nodeParallelFlags) {
	p.lateBind = &lateBindConfig{capacity: capacity, wait: lateBindWaitTimeout}
	p.slotFree = sync.NewCond(&p.priorityMu)
	p.lateBindInFlight = make(map[string]int)
}

// noRelease is the release side of a reservation that was never taken.
func noRelease() {}

// releaseSlot returns the release side of one generation slot: it retires this
// request's entry from the node's in-flight ledger and wakes every parked
// request so one of them can take the freed slot. sync.Once makes the returned
// function idempotent — the handler calls it at the terminal workload transition
// and defers it as a backstop — so the ledger entry added when the request was
// bound is removed exactly once. Nothing else may retire it: a scheduler
// snapshot resets the reservations it owns, and must leave this ledger alone,
// or a node would keep a slot it is no longer using.
func (p *Proxy) releaseSlot(id string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.priorityMu.Lock()
			defer p.priorityMu.Unlock()
			if held := p.lateBindInFlight[id]; held > 1 {
				p.lateBindInFlight[id] = held - 1
			} else {
				delete(p.lateBindInFlight, id)
			}
			p.slotFree.Broadcast()
		})
	}
}

// hasFreeSlotLocked reports whether a node is below its concurrent-generation
// ceiling. Occupancy is this proxy's in-flight ledger for that node and nothing
// else: each request it bound contributes one, from the moment it is bound to
// its terminal transition. The scheduler's pending count and GPU pressure are
// deliberately excluded — pending is the same requests arriving back around a
// round trip later (see the file header), and pressure is a smoothed 0-3 score
// rather than a count of jobs, so a node whose pressure alone reached the
// ceiling would look permanently full. Both still order the choice; neither
// gates it. Callers hold priorityMu.
func (p *Proxy) hasFreeSlotLocked(id string) bool {
	return p.lateBindInFlight[id] < p.lateBind.capacity.slotsFor(id)
}

// waitForFreeSlotLocked parks a request until an eligible candidate has a free
// generation slot, the wait budget expires, or the request is cancelled. It is
// called with priorityMu held and returns holding it: sync.Cond releases the
// mutex for the duration of each wait, so reservations, releases, scheduler
// snapshots and other requests all keep making progress while a request is
// parked here.
//
// best and free are the current pick pair from pickCandidateLocked. Returning
// free (>= 0) hands back a candidate with a slot; returning best is the give-up
// path, which commits to the least loaded node exactly as the proxy does with
// the feature off, so a wedged node degrades routing rather than stopping it.
//
// onQueued, when non-nil, is called once at the moment this request is about to
// park — and only then, so a request that binds straight away emits exactly the
// events it emits today. It is the request's cluster visibility: a parked
// request has not been forwarded, so nothing else would announce it.
func (p *Proxy) waitForFreeSlotLocked(ctx context.Context, candidateIndex map[string]int, onQueued func(), best, free int) int {
	if free >= 0 || best < 0 {
		return free
	}
	// This request is going to queue. Say so before parking, with priorityMu
	// dropped: the announcement writes to the orchestrator channel, and no
	// routing decision should ever queue behind that write. Re-deriving the pick
	// after re-acquiring is what makes dropping the mutex safe — a slot freed
	// during the window is seen here rather than missed, so the broadcast we
	// were not holding the mutex to receive costs nothing.
	if onQueued != nil {
		p.priorityMu.Unlock()
		onQueued()
		p.priorityMu.Lock()
		if best, free = p.pickCandidateLocked(candidateIndex); free >= 0 || best < 0 {
			return free
		}
	}
	// Every eligible node is at capacity. Park until something changes.
	//
	// No wakeup can be lost: each waker takes priorityMu before broadcasting, so
	// a broadcast cannot land in the window between the checks below and
	// Cond.Wait releasing the mutex, and every state change that could free a
	// slot (releaseSlot, SetPrioritySnapshot) happens under the same mutex. The
	// two wakers below fire independently of any node activity, so a parked
	// request always leaves this loop: the timer bounds the wait, and because
	// r.Context() descends from the proxy's root context (serveHTTP's
	// BaseContext), the cancel path covers both a client hanging up and shutdown.
	wake := func() {
		p.priorityMu.Lock()
		defer p.priorityMu.Unlock()
		p.slotFree.Broadcast()
	}
	deadline := time.Now().Add(p.lateBind.wait)
	timer := time.AfterFunc(p.lateBind.wait, wake)
	defer timer.Stop()
	stopWakeOnCancel := context.AfterFunc(ctx, wake)
	defer stopWakeOnCancel()

	for {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return best
		}
		p.slotFree.Wait()
		// Broadcast wakes every parked request, so re-derive the pick under the
		// mutex: another of them may have taken the slot that woke us, and a new
		// snapshot may have changed the eligible set entirely.
		if best, free = p.pickCandidateLocked(candidateIndex); free >= 0 || best < 0 {
			return free
		}
	}
}
