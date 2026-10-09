// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/errors"
)

// managerForNode builds a manager with a fixed local node id and no
// codec output consumer needed (we read state via snapshot accessors,
// not the wire). The captureRW drains emitUpdate frames so writes don't
// block on a full channel.
func managerForNode(nodeID string) *Manager {
	rw := newCaptureRW()
	m := NewManager(NewCodec(rw))
	m.localNodeID = nodeID
	return m
}

func localErr(id, nodeID string, ts int64) ServiceError {
	return ServiceError{ID: id, Message: id + " broke", Timestamp: ts, NodeID: nodeID}
}

// TestCompositeKeyAvoidsCrossNodeCollision: the same error id reported
// by two different nodes must coexist as two entries, not clobber each
// other. This is the whole reason the store keys by (nodeId, id).
func TestCompositeKeyAvoidsCrossNodeCollision(t *testing.T) {
	m := managerForNode("node-a")

	const id = "ollama-local:not-running"
	m.upsert(localErr(id, "node-a", 1000))
	m.upsert(localErr(id, "node-b", 1000))

	require.Len(t, m.snapshot(), 2, "snapshot len")
}

// TestLocalSnapshotFiltersToLocalOrigin: localSnapshot (what we serve
// and push) must contain only this node's own errors, never peers'.
func TestLocalSnapshotFiltersToLocalOrigin(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	m.upsert(localErr("b:one", "node-b", 1000))
	m.upsert(localErr("a:two", "node-a", 1000))

	local := m.localSnapshot()
	require.Len(t, local, 2, "localSnapshot len")
	for _, e := range local {
		assert.Equal(t, "node-a", e.NodeID, "localSnapshot leaked foreign entry (%v)", e)
	}
}

// TestReconcilePeerUpsertsAndEvicts: a peer's pushed set is
// authoritative for its nodeId — new entries appear, and entries the
// peer no longer reports are evicted. Local entries are untouched.
func TestReconcilePeerUpsertsAndEvicts(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:keep", "node-a", 1000))

	// First push from node-b: two errors.
	changed := m.reconcilePeer("node-b", []ServiceError{
		localErr("b:one", "node-b", 1000),
		localErr("b:two", "node-b", 1000),
	})
	require.True(t, changed, "first reconcile should report changed=true")
	assert.Len(t, m.snapshot(), 3, "after first reconcile snapshot")

	// Second push: b:one cleared (absent), b:two refreshed, b:three new.
	changed = m.reconcilePeer("node-b", []ServiceError{
		localErr("b:two", "node-b", 2000),
		localErr("b:three", "node-b", 2000),
	})
	require.True(t, changed, "second reconcile should report changed=true")

	ids := map[string]bool{}
	for _, e := range m.snapshot() {
		ids[e.NodeID+"/"+e.ID] = true
	}
	assert.NotContains(t, ids, "node-b/b:one", "b:one should have been evicted (absent from authoritative push)")
	assert.Contains(t, ids, "node-b/b:two", "expected b:two and b:three present")
	assert.Contains(t, ids, "node-b/b:three", "expected b:two and b:three present")
	assert.Contains(t, ids, "node-a/a:keep", "local entry a:keep must survive peer reconcile")
}

// TestReconcilePeerStampsOrigin: a peer cannot inject an entry
// attributed to a third node — the envelope nodeId is stamped onto
// every entry regardless of the per-error NodeID field.
func TestReconcilePeerStampsOrigin(t *testing.T) {
	m := managerForNode("node-a")
	m.reconcilePeer("node-b", []ServiceError{
		{ID: "spoof", Message: "x", Timestamp: 1, NodeID: "node-c"},
	})
	for _, e := range m.snapshot() {
		assert.Equal(t, "node-b", e.NodeID, "entry origin")
	}
}

// TestReconcilePeerRejectsSelf: a peer must never be able to reconcile
// our own origin's errors.
func TestReconcilePeerRejectsSelf(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	assert.False(t, m.reconcilePeer("node-a", nil), "reconcile of own nodeId must be a no-op")
	assert.Len(t, m.snapshot(), 1, "self-reconcile altered store")
}

// TestEvictNodeRemovesPeerEntries: when a peer leaves the network all
// its entries are dropped; local entries remain.
func TestEvictNodeRemovesPeerEntries(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	m.reconcilePeer("node-b", []ServiceError{localErr("b:one", "node-b", 1000)})

	assert.True(t, m.evictNode("node-b"), "evictNode should report changed=true")
	got := m.snapshot()
	require.Len(t, got, 1, "after evict, snapshot")
	assert.Equal(t, "node-a", got[0].NodeID, "after evict, snapshot (%v)", got)
	assert.False(t, m.evictNode("node-b"), "second evict of same node should be a no-op")
}

// TestHTTPIngestReconciles: the POST /v1/errors handler decodes a
// SyncEnvelope and merges it, returning 204.
func TestHTTPIngestReconciles(t *testing.T) {
	m := managerForNode("node-a")
	srv, client := servePinnedErrorsMux(t, m)

	env := errors.SyncEnvelope{
		NodeID: "node-b",
		Errors: []ServiceError{localErr("b:one", "node-b", 1000)},
	}
	body, err := json.Marshal(env)
	assert.NoError(t, err, "marshal sync envelope")
	resp, err := client.Post(srv.URL+"/v1/errors", "application/json", bytes.NewReader(body))
	require.NoError(t, err, "POST")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "POST status")

	got := m.snapshot()
	require.Len(t, got, 1, "after ingest, snapshot")
	assert.Equal(t, "b:one", got[0].ID, "after ingest, snapshot (%v)", got)
	assert.Equal(t, "node-b", got[0].NodeID, "after ingest, snapshot (%v)", got)
}

// TestHTTPIngestRejectsMissingNodeID: an envelope without a nodeId is a
// 400, not a silent accept.
func TestHTTPIngestRejectsMissingNodeID(t *testing.T) {
	m := managerForNode("node-a")
	srv, client := servePinnedErrorsMux(t, m)

	body, err := json.Marshal(errors.SyncEnvelope{Errors: []ServiceError{localErr("x", "node-b", 1)}})
	assert.NoError(t, err, "marshal sync envelope without node ID")
	resp, err := client.Post(srv.URL+"/v1/errors", "application/json", bytes.NewReader(body))
	require.NoError(t, err, "POST")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "POST status")
}

// TestHTTPServeLocalReturnsLocalSnapshot: GET /v1/errors returns this
// node's local-origin errors as a SyncEnvelope, excluding peer entries.
func TestHTTPServeLocalReturnsLocalSnapshot(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	m.reconcilePeer("node-b", []ServiceError{localErr("b:one", "node-b", 1000)})

	srv, client := servePinnedErrorsMux(t, m)

	resp, err := client.Get(srv.URL + "/v1/errors")
	require.NoError(t, err, "GET")
	defer resp.Body.Close()

	var env errors.SyncEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env), "decode")
	assert.Equal(t, "node-a", env.NodeID, "envelope nodeId")
	require.Len(t, env.Errors, 1, "served errors")
	assert.Equal(t, "a:one", env.Errors[0].ID, "served errors")
}

// TestOnLocalChangeFiresForLocalNotPeer: a local report/clear triggers
// the push hook; a peer reconcile does not (no echo loop).
func TestOnLocalChangeFiresForLocalNotPeer(t *testing.T) {
	m := managerForNode("node-a")
	fired := 0
	m.SetOnLocalChange(func() { fired++ })

	m.handleReport(nil, mustJSON(t, localErr("a:one", "node-a", 1000)))
	assert.Equal(t, 1, fired, "local report fired hook")

	m.ReconcilePeer("node-b", []ServiceError{localErr("b:one", "node-b", 1000)})
	assert.Equal(t, 1, fired, "peer reconcile must NOT fire local-change hook (fired")
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err, "marshal")
	return b
}

// TestPeerHostPorts covers address propagation: preserve every ranked address,
// fall back to the hostname, and reject nodes with no usable destination.
func TestPeerHostPorts(t *testing.T) {
	test := func(name string, node RawNode, want []string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, peerHostPorts(node))
		})
	}
	test("ip preserved", RawNode{Addresses: []string{"192.168.1.5"}, Host: "h.local.", Port: 14319}, []string{"192.168.1.5:14319"})
	test("host fallback", RawNode{Host: "h.local.", Port: 14319}, []string{"h.local.:14319"})
	test("no address", RawNode{Port: 14319}, []string{})
	test("no port", RawNode{Addresses: []string{"192.168.1.5"}}, nil)
	test("all ipv4 deterministic", RawNode{Addresses: []string{"192.168.1.9", "192.168.1.5"}, Port: 14319}, []string{"192.168.1.5:14319", "192.168.1.9:14319"})
	test("ipv4 before ipv6", RawNode{Addresses: []string{"2001:db8::1", "192.168.1.5"}, Port: 14319}, []string{"192.168.1.5:14319", "[2001:db8::1]:14319"})
	// No private range outranks another. Which one a peer can actually be
	// reached on is not something its subnet number states, so two private
	// addresses tie and the tie is broken deterministically rather than by
	// the order the browse happened to resolve them in.
	test("private ranges tie", RawNode{Addresses: []string{"10.221.0.9", "192.168.1.5"}, Port: 14319}, []string{"10.221.0.9:14319", "192.168.1.5:14319"})
	test("ip= TXT leads without dropping others", RawNode{Addresses: []string{"10.221.0.9"}, TXT: []string{"ip=192.168.1.5"}, Port: 14319}, []string{"192.168.1.5:14319", "10.221.0.9:14319"})
}

// TestUnclusteredNodeKeepsNoPeers: a node that belongs to no cluster holds no
// pins, so every discovered peer is dropped from the push set and nothing is ever
// sent. This is the outbound half of "the cluster data plane is always mTLS" —
// previously such a node kept the peer and pushed its error snapshot in the clear.
func TestUnclusteredNodeKeepsNoPeers(t *testing.T) {
	ps := NewPeerSync(managerForNode("node-a"), clustertrust.Open(t.TempDir()))
	ps.handleEvent(context.Background(), DiscoveryEvent{
		Type: "discovered",
		Node: RawNode{
			ID:        "node-b",
			Addresses: []string{"192.168.1.5"},
			Port:      14319,
			TXT:       []string{"cluster-uuid=uuid-peer"},
		},
	})

	ps.mu.RLock()
	defer ps.mu.RUnlock()
	require.Empty(t, ps.peers, "unclustered node retained")
}
