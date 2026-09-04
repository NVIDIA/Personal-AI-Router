// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package relay is the broker's discovery relay. It owns two
// halves of the star topology and is pure state + fanout logic — the broker
// binary wires it to the scanner peer (daemon) and to client connections:
//
//   - RegistrationCache: the UPWARD half. This node's advertised services,
//     registered by local workers (relayed) and by the broker's engine poller
//     (ol/lm). The broker pushes the cached set down to the daemon and replays
//     it whenever the daemon reports a new epoch (restart), so no worker needs
//     reconnect logic.
//   - Directory: the DOWNWARD half. Every LAN node, fed by the daemon's
//     discovery:node-* events, with per-subscriber filtered fanout and a
//     queryable snapshot for discovery:get-nodes.
package relay

import (
	"sort"
	"sync"

	"nvpair-shared/noderec"
)

// RegistrationCache holds this node's service registrations, keyed by service.
type RegistrationCache struct {
	mu   sync.Mutex
	regs map[noderec.ServiceKey]noderec.RegisterParams
}

func NewRegistrationCache() *RegistrationCache {
	return &RegistrationCache{regs: make(map[noderec.ServiceKey]noderec.RegisterParams)}
}

// Register adds or updates a service registration, reporting whether the cached
// set changed (so the broker only re-pushes to the daemon on a real change).
// update-txt is the same call with a new TXT.
func (c *RegistrationCache) Register(p noderec.RegisterParams) bool {
	if p.Service == "" || p.Port == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.regs[p.Service]; ok && registerEqual(prev, p) {
		return false
	}
	c.regs[p.Service] = p
	return true
}

// Unregister removes a service, reporting whether it existed.
func (c *RegistrationCache) Unregister(s noderec.ServiceKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.regs[s]; !ok {
		return false
	}
	delete(c.regs, s)
	return true
}

// Snapshot returns the current registrations, sorted by service for a
// deterministic replay order.
func (c *RegistrationCache) Snapshot() []noderec.RegisterParams {
	c.mu.Lock()
	out := make([]noderec.RegisterParams, 0, len(c.regs))
	for _, p := range c.regs {
		out = append(out, p)
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func registerEqual(a, b noderec.RegisterParams) bool {
	if a.Service != b.Service || a.Port != b.Port || len(a.TXT) != len(b.TXT) {
		return false
	}
	for i := range a.TXT {
		if a.TXT[i] != b.TXT[i] {
			return false
		}
	}
	return true
}

// Subscriber is a client interested in directory changes: a service filter and a
// callback invoked with the subscriber's full filtered node set on every change.
// Consumers replace their set from it rather than applying deltas, so a dropped
// or reordered push can't leave them drifted — every push is the authoritative
// current list.
//
// Deliveries are asynchronous: each subscriber owns a pump goroutine (started
// by Directory.Subscribe) that serializes its sends and coalesces concurrent
// triggers into one delivery that captures the snapshot at send time. A slow or
// blocked Send therefore stalls only its own subscriber, never the directory
// update path that feeds it (the scanner's read pump calls Apply).
type Subscriber struct {
	Filter noderec.SubscribeParams
	Send   func(nodes []noderec.DirectoryNode)

	// kick carries a pending-delivery signal (capacity 1: extra signals while
	// one is already pending coalesce — the pump captures the latest snapshot
	// when it wakes, so early triggers can't deliver stale state). done closes
	// on Unsubscribe and stops the pump.
	kick chan struct{}
	done chan struct{}
}

// Directory is the broker's view of all LAN nodes (keyed by hostUuid) plus its
// subscriber set. It's fed by the daemon's node-* events via Apply and queried
// by discovery:get-nodes via Snapshot.
type Directory struct {
	mu     sync.Mutex
	nodes  map[string]noderec.DirectoryNode
	subs   map[int]*Subscriber
	nextID int
}

func NewDirectory() *Directory {
	return &Directory{
		nodes: make(map[string]noderec.DirectoryNode),
		subs:  make(map[int]*Subscriber),
	}
}

// Subscribe registers a subscriber and starts its delivery pump goroutine. The
// initial snapshot arrives via the pump after any pending Deliver call —
// snapshot is captured at send time, so a concurrent Apply can't sneak a newer
// snapshot in and have this initial delivery overwrite it with an older one.
func (d *Directory) Subscribe(sub *Subscriber) (id int) {
	sub.kick = make(chan struct{}, 1)
	sub.done = make(chan struct{})
	d.mu.Lock()
	d.nextID++
	id = d.nextID
	d.subs[id] = sub
	d.mu.Unlock()
	go d.pump(sub)
	return id
}

// pump serializes one subscriber's deliveries. Every wake re-captures the
// latest filtered snapshot, so coalesced triggers always deliver current state.
// done takes priority over a pending kick: once Unsubscribe has closed done, a
// trigger that raced the close must not produce a Send against a consumer
// that's gone.
func (d *Directory) pump(sub *Subscriber) {
	for {
		select {
		case <-sub.done:
			return
		default:
		}
		select {
		case <-sub.kick:
			sub.Send(d.filtered(sub.Filter))
		case <-sub.done:
			return
		}
	}
}

// Deliver asks for a delivery of the subscriber's current filtered snapshot.
// Non-blocking: it schedules the send on the subscriber's pump and never blocks
// the caller — Apply runs on the scanner read pump, and a subscriber whose Send
// blocks (a stalled worker's stdin pipe) must not stall the directory or the
// other subscribers. Multiple pending triggers coalesce into one send of the
// latest state.
func (d *Directory) Deliver(sub *Subscriber) {
	select {
	case sub.kick <- struct{}{}:
	default:
	}
}

// filtered returns the nodes matching a subscriber's filter, sorted by
// hostUuid for a deterministic set.
func (d *Directory) filtered(f noderec.SubscribeParams) []noderec.DirectoryNode {
	d.mu.Lock()
	nodes := d.filteredLocked(f)
	d.mu.Unlock()
	return nodes
}

// filteredLocked returns the nodes matching a subscriber's filter, sorted by
// hostUuid for a deterministic set. Caller must hold d.mu.
func (d *Directory) filteredLocked(f noderec.SubscribeParams) []noderec.DirectoryNode {
	out := make([]noderec.DirectoryNode, 0, len(d.nodes))
	for _, n := range d.nodes {
		if f.Matches(n) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out
}

// Unsubscribe removes a subscriber and stops its delivery pump, waiting for
// the pump to exit so a Send cannot run against a consumer that's gone.
func (d *Directory) Unsubscribe(id int) {
	d.mu.Lock()
	sub := d.subs[id]
	delete(d.subs, id)
	d.mu.Unlock()
	if sub != nil {
		close(sub.done)
	}
}

// Apply folds a daemon node-* delta into the directory, then re-sends every
// subscriber its full filtered snapshot. method (one of noderec.NotifyNode{
// Discovered,Updated,Removed}) only updates the directory — removed drops the
// node by hostUuid, discovered/updated upsert it. Every subscriber is re-pushed
// on any change (not just those matching the changed node) so each consumer's
// set is always the authoritative current list; the push is idempotent (the
// consumer replaces with the same set), so a re-push for an unrelated change is a
// cheap no-op at this scale.
func (d *Directory) Apply(method string, node noderec.DirectoryNode) {
	d.mu.Lock()
	if method == noderec.NotifyNodeRemoved {
		delete(d.nodes, node.HostUUID)
	} else {
		d.nodes[node.HostUUID] = node
	}
	subs := make([]*Subscriber, 0, len(d.subs))
	for _, s := range d.subs {
		subs = append(subs, s)
	}
	d.mu.Unlock()
	// Deliver captures each subscriber's snapshot at send time (under its
	// per-subscriber lock), so this fan-out and a concurrent initial delivery
	// can't reorder into a stale set.
	for _, s := range subs {
		d.Deliver(s)
	}
}

// Snapshot returns the directory optionally filtered to one service, sorted by
// hostUuid.
func (d *Directory) Snapshot(filter noderec.ServiceKey) []noderec.DirectoryNode {
	d.mu.Lock()
	out := make([]noderec.DirectoryNode, 0, len(d.nodes))
	for _, n := range d.nodes {
		if filter != "" && !n.HasService(filter) {
			continue
		}
		out = append(out, n)
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out
}
