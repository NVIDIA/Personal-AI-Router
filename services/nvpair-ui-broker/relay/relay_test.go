// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestRegistrationCache(t *testing.T) {
	c := NewRegistrationCache()
	require.True(t, c.Register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}), "first register should change")
	assert.False(t, c.Register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}), "identical re-register should not change")
	// A new TXT (update-txt) is a change.
	require.True(t, c.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434, TXT: []string{"models=a"}}), "new service should change")
	assert.True(t, c.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434, TXT: []string{"models=a;b"}}), "changed TXT should change")
	assert.False(t, c.Register(noderec.RegisterParams{Service: noderec.ServiceErrors, Port: 0}), "port 0 should be ignored")

	snap := c.Snapshot()
	require.Len(t, snap, 2, "snapshot len")
	// Sorted by service key: ni before ol.
	assert.Equal(t, noderec.ServiceNodeInfo, snap[0].Service, "snapshot not sorted by service (%v)", snap)
	assert.Equal(t, noderec.ServiceOllama, snap[1].Service, "snapshot not sorted by service (%v)", snap)

	assert.True(t, c.Unregister(noderec.ServiceNodeInfo), "unregister existing should be true")
	assert.False(t, c.Unregister(noderec.ServiceNodeInfo), "unregister absent should be false")
	assert.Len(t, c.Snapshot(), 1, "snapshot should have 1 after unregister")
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
	{
		got := ids(rec.last())
		require.Equal(t, []string{"a"}, got, "initial delivery")
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
	{
		got := ids(rec.last())
		require.Equal(t, []string{"a"}, got, "delivery after a post-subscribe change")
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
	{
		got := ids(olSub.last())
		assert.Equal(t, []string{"a"}, got, "ol subscriber last snapshot")
	}
	{
		got := ids(allSub.last())
		assert.Equal(t, []string{"a", "b"}, got, "all subscriber last snapshot")
	}
}

func TestDirectoryRemoveAndUnsubscribe(t *testing.T) {
	d := NewDirectory()
	sub := &recordingSub{}
	id := d.Subscribe(&Subscriber{Filter: noderec.SubscribeParams{}, Send: sub.send})

	d.Apply(noderec.NotifyNodeDiscovered, olNode("a"))
	d.Apply(noderec.NotifyNodeRemoved, olNode("a"))
	assert.Empty(t, d.Snapshot(""), "node should be gone after removed")
	// The removal re-pushes an empty snapshot (the node is simply absent).
	assert.Empty(t, sub.last(), "subscriber last snapshot")

	// After unsubscribe, no more pushes.
	before := len(sub.snaps)
	d.Unsubscribe(id)
	d.Apply(noderec.NotifyNodeDiscovered, olNode("c"))
	assert.Len(t, sub.snaps, before, "unsubscribed sub still received")
}

func TestDirectorySnapshotFilterAndSort(t *testing.T) {
	d := NewDirectory()
	d.Apply(noderec.NotifyNodeDiscovered, erNode("z"))
	d.Apply(noderec.NotifyNodeDiscovered, olNode("a"))
	d.Apply(noderec.NotifyNodeDiscovered, olNode("m"))

	all := d.Snapshot("")
	require.Len(t, all, 3, "snapshot(all) not sorted")
	require.Equal(t, "a", all[0].HostUUID, "snapshot(all) not sorted (%v)", all)
	require.Equal(t, "z", all[2].HostUUID, "snapshot(all) not sorted (%v)", all)
	ol := d.Snapshot(noderec.ServiceOllama)
	require.Len(t, ol, 2, "snapshot(ol)")
}
