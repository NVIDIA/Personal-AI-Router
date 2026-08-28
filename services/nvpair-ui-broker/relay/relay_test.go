// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"reflect"
	"testing"

	"nvpair-shared/noderec"
)

func TestRegistrationCache(t *testing.T) {
	c := NewRegistrationCache()
	if !c.Register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}) {
		t.Fatal("first register should change")
	}
	if c.Register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}) {
		t.Error("identical re-register should not change")
	}
	// A new TXT (update-txt) is a change.
	if !c.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434, TXT: []string{"models=a"}}) {
		t.Fatal("new service should change")
	}
	if !c.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434, TXT: []string{"models=a;b"}}) {
		t.Error("changed TXT should change")
	}
	if c.Register(noderec.RegisterParams{Service: noderec.ServiceErrors, Port: 0}) {
		t.Error("port 0 should be ignored")
	}

	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	// Sorted by service key: ni before ol.
	if snap[0].Service != noderec.ServiceNodeInfo || snap[1].Service != noderec.ServiceOllama {
		t.Errorf("snapshot not sorted by service: %+v", snap)
	}

	if !c.Unregister(noderec.ServiceNodeInfo) {
		t.Error("unregister existing should be true")
	}
	if c.Unregister(noderec.ServiceNodeInfo) {
		t.Error("unregister absent should be false")
	}
	if len(c.Snapshot()) != 1 {
		t.Error("snapshot should have 1 after unregister")
	}
}

// recordingSub captures the snapshots a subscriber receives. Each Send is one
// full filtered snapshot; snaps holds them in order so a test can assert on the
// latest set and on how many pushes arrived.
type recordingSub struct {
	snaps [][]noderec.DirectoryNode
}

func (r *recordingSub) send(nodes []noderec.DirectoryNode) {
	r.snaps = append(r.snaps, append([]noderec.DirectoryNode(nil), nodes...))
}

func (r *recordingSub) last() []noderec.DirectoryNode {
	if len(r.snaps) == 0 {
		return nil
	}
	return r.snaps[len(r.snaps)-1]
}

func ids(nodes []noderec.DirectoryNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.HostUUID
	}
	return out
}

func olNode(id string) noderec.DirectoryNode {
	return noderec.DirectoryNode{HostUUID: id, Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}}}
}
func erNode(id string) noderec.DirectoryNode {
	return noderec.DirectoryNode{HostUUID: id, Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceErrors: {Port: 14319}}}
}

func TestDirectorySubscribeInitialSnapshot(t *testing.T) {
	d := NewDirectory()
	d.Apply(noderec.NotifyNodeDiscovered, olNode("a"))
	d.Apply(noderec.NotifyNodeDiscovered, erNode("b"))

	// A subscriber filtered to ol gets only the existing ol node as its first
	// delivery.
	rec := &recordingSub{}
	sub := &Subscriber{
		Filter: noderec.SubscribeParams{Services: []noderec.ServiceKey{noderec.ServiceOllama}},
		Send:   rec.send,
	}
	d.Subscribe(sub)
	d.Deliver(sub)
	if got := ids(rec.last()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("initial delivery = %v, want [a]", got)
	}
}

// TestDeliverCapturesAtSendTime guards the subscribe race fix: a change that
// lands after Subscribe but before the initial Deliver must be reflected — the
// delivery captures the snapshot when it runs, not a stale pre-subscribe set, so
// it can't overwrite a concurrent Apply with an older list.
func TestDeliverCapturesAtSendTime(t *testing.T) {
	d := NewDirectory()
	rec := &recordingSub{}
	sub := &Subscriber{Filter: noderec.SubscribeParams{}, Send: rec.send}
	d.Subscribe(sub)
	d.Apply(noderec.NotifyNodeDiscovered, olNode("a"))
	d.Deliver(sub)
	if got := ids(rec.last()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("delivery after a post-subscribe change = %v, want [a]", got)
	}
}

func TestDirectoryFanoutRespectsFilter(t *testing.T) {
	d := NewDirectory()
	olSub := &recordingSub{}
	allSub := &recordingSub{}
	d.Subscribe(&Subscriber{Filter: noderec.SubscribeParams{Services: []noderec.ServiceKey{noderec.ServiceOllama}}, Send: olSub.send})
	d.Subscribe(&Subscriber{Filter: noderec.SubscribeParams{}, Send: allSub.send}) // all

	d.Apply(noderec.NotifyNodeDiscovered, olNode("a"))
	d.Apply(noderec.NotifyNodeDiscovered, erNode("b"))

	// Every change re-pushes each subscriber its full filtered snapshot, so the
	// latest snapshot is the authoritative filtered set.
	if got := ids(olSub.last()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("ol subscriber last snapshot = %v, want [a]", got)
	}
	if got := ids(allSub.last()); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("all subscriber last snapshot = %v, want [a b]", got)
	}
}

func TestDirectoryRemoveAndUnsubscribe(t *testing.T) {
	d := NewDirectory()
	sub := &recordingSub{}
	id := d.Subscribe(&Subscriber{Filter: noderec.SubscribeParams{}, Send: sub.send})

	d.Apply(noderec.NotifyNodeDiscovered, olNode("a"))
	d.Apply(noderec.NotifyNodeRemoved, olNode("a"))
	if len(d.Snapshot("")) != 0 {
		t.Error("node should be gone after removed")
	}
	// The removal re-pushes an empty snapshot (the node is simply absent).
	if got := sub.last(); len(got) != 0 {
		t.Errorf("subscriber last snapshot = %v, want empty after removal", ids(got))
	}

	// After unsubscribe, no more pushes.
	before := len(sub.snaps)
	d.Unsubscribe(id)
	d.Apply(noderec.NotifyNodeDiscovered, olNode("c"))
	if len(sub.snaps) != before {
		t.Errorf("unsubscribed sub still received %d pushes", len(sub.snaps)-before)
	}
}

func TestDirectorySnapshotFilterAndSort(t *testing.T) {
	d := NewDirectory()
	d.Apply(noderec.NotifyNodeDiscovered, erNode("z"))
	d.Apply(noderec.NotifyNodeDiscovered, olNode("a"))
	d.Apply(noderec.NotifyNodeDiscovered, olNode("m"))

	all := d.Snapshot("")
	if len(all) != 3 || all[0].HostUUID != "a" || all[2].HostUUID != "z" {
		t.Fatalf("snapshot(all) not sorted: %+v", all)
	}
	ol := d.Snapshot(noderec.ServiceOllama)
	if len(ol) != 2 {
		t.Fatalf("snapshot(ol) = %d, want 2", len(ol))
	}
}
