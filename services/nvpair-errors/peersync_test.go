// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

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

	got := m.snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (one per node)", len(got))
	}
}

// TestLocalSnapshotFiltersToLocalOrigin: localSnapshot (what we serve
// and push) must contain only this node's own errors, never peers'.
func TestLocalSnapshotFiltersToLocalOrigin(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	m.upsert(localErr("b:one", "node-b", 1000))
	m.upsert(localErr("a:two", "node-a", 1000))

	local := m.localSnapshot()
	if len(local) != 2 {
		t.Fatalf("localSnapshot len = %d, want 2", len(local))
	}
	for _, e := range local {
		if e.NodeID != "node-a" {
			t.Fatalf("localSnapshot leaked foreign entry: %+v", e)
		}
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
	if !changed {
		t.Fatal("first reconcile should report changed=true")
	}
	if n := len(m.snapshot()); n != 3 {
		t.Fatalf("after first reconcile snapshot len = %d, want 3", n)
	}

	// Second push: b:one cleared (absent), b:two refreshed, b:three new.
	changed = m.reconcilePeer("node-b", []ServiceError{
		localErr("b:two", "node-b", 2000),
		localErr("b:three", "node-b", 2000),
	})
	if !changed {
		t.Fatal("second reconcile should report changed=true")
	}

	ids := map[string]bool{}
	for _, e := range m.snapshot() {
		ids[e.NodeID+"/"+e.ID] = true
	}
	if ids["node-b/b:one"] {
		t.Fatal("b:one should have been evicted (absent from authoritative push)")
	}
	if !ids["node-b/b:two"] || !ids["node-b/b:three"] {
		t.Fatalf("expected b:two and b:three present, got %v", ids)
	}
	if !ids["node-a/a:keep"] {
		t.Fatal("local entry a:keep must survive peer reconcile")
	}
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
		if e.NodeID != "node-b" {
			t.Fatalf("entry origin = %q, want node-b (envelope authority)", e.NodeID)
		}
	}
}

// TestReconcilePeerRejectsSelf: a peer must never be able to reconcile
// our own origin's errors.
func TestReconcilePeerRejectsSelf(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	if m.reconcilePeer("node-a", nil) {
		t.Fatal("reconcile of own nodeId must be a no-op")
	}
	if n := len(m.snapshot()); n != 1 {
		t.Fatalf("self-reconcile altered store: len = %d, want 1", n)
	}
}

// TestEvictNodeRemovesPeerEntries: when a peer leaves the network all
// its entries are dropped; local entries remain.
func TestEvictNodeRemovesPeerEntries(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	m.reconcilePeer("node-b", []ServiceError{localErr("b:one", "node-b", 1000)})

	if !m.evictNode("node-b") {
		t.Fatal("evictNode should report changed=true")
	}
	got := m.snapshot()
	if len(got) != 1 || got[0].NodeID != "node-a" {
		t.Fatalf("after evict, snapshot = %+v, want only node-a entry", got)
	}
	if m.evictNode("node-b") {
		t.Fatal("second evict of same node should be a no-op")
	}
}

// TestHTTPIngestReconciles: the POST /v1/errors handler decodes a
// SyncEnvelope and merges it, returning 204. The envelope's nodeId must
// match the mTLS-authenticated caller UUID — the pin store keys peers by
// the same UUID the push side uses as nodeId.
func TestHTTPIngestReconciles(t *testing.T) {
	m := managerForNode("node-a")
	srv, client := servePinnedErrorsMux(t, m)

	env := errors.SyncEnvelope{
		NodeID: "uuid-peer",
		Errors: []ServiceError{localErr("b:one", "uuid-peer", 1000)},
	}
	body, _ := json.Marshal(env)
	resp, err := client.Post(srv.URL+"/v1/errors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204", resp.StatusCode)
	}

	got := m.snapshot()
	if len(got) != 1 || got[0].ID != "b:one" || got[0].NodeID != "uuid-peer" {
		t.Fatalf("after ingest, snapshot = %+v, want single uuid-peer entry", got)
	}
}

// TestHTTPIngestRejectsSpoofedNodeID: a pinned caller cannot reconcile
// under another node's identity — the envelope's nodeId must equal the
// authenticated caller UUID, otherwise the push is a 400 and nothing is
// reconciled.
func TestHTTPIngestRejectsSpoofedNodeID(t *testing.T) {
	m := managerForNode("node-a")
	srv, client := servePinnedErrorsMux(t, m)

	env := errors.SyncEnvelope{
		NodeID: "uuid-victim",
		Errors: []ServiceError{localErr("v:one", "uuid-victim", 1000)},
	}
	body, _ := json.Marshal(env)
	resp, err := client.Post(srv.URL+"/v1/errors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST status = %d, want 400", resp.StatusCode)
	}
	if got := m.snapshot(); len(got) != 0 {
		t.Fatalf("spoofed ingest reconciled %+v, want empty snapshot", got)
	}
}

// TestHTTPIngestRejectsMissingNodeID: an envelope without a nodeId is a
// 400, not a silent accept.
func TestHTTPIngestRejectsMissingNodeID(t *testing.T) {
	m := managerForNode("node-a")
	srv, client := servePinnedErrorsMux(t, m)

	body, _ := json.Marshal(errors.SyncEnvelope{Errors: []ServiceError{localErr("x", "node-b", 1)}})
	resp, err := client.Post(srv.URL+"/v1/errors", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST status = %d, want 400", resp.StatusCode)
	}
}

// TestHTTPServeLocalReturnsLocalSnapshot: GET /v1/errors returns this
// node's local-origin errors as a SyncEnvelope, excluding peer entries.
func TestHTTPServeLocalReturnsLocalSnapshot(t *testing.T) {
	m := managerForNode("node-a")
	m.upsert(localErr("a:one", "node-a", 1000))
	m.reconcilePeer("node-b", []ServiceError{localErr("b:one", "node-b", 1000)})

	srv, client := servePinnedErrorsMux(t, m)

	resp, err := client.Get(srv.URL + "/v1/errors")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var env errors.SyncEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.NodeID != "node-a" {
		t.Fatalf("envelope nodeId = %q, want node-a", env.NodeID)
	}
	if len(env.Errors) != 1 || env.Errors[0].ID != "a:one" {
		t.Fatalf("served errors = %+v, want only local a:one", env.Errors)
	}
}

// TestOnLocalChangeFiresForLocalNotPeer: a local report/clear triggers
// the push hook; a peer reconcile does not (no echo loop).
func TestOnLocalChangeFiresForLocalNotPeer(t *testing.T) {
	m := managerForNode("node-a")
	fired := 0
	m.SetOnLocalChange(func() { fired++ })

	m.handleReport(nil, mustJSON(t, localErr("a:one", "node-a", 1000)))
	if fired != 1 {
		t.Fatalf("local report fired hook %d times, want 1", fired)
	}

	m.ReconcilePeer("node-b", []ServiceError{localErr("b:one", "node-b", 1000)})
	if fired != 1 {
		t.Fatalf("peer reconcile must NOT fire local-change hook (fired=%d)", fired)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestPeerHostPorts covers address propagation: preserve every ranked address,
// fall back to the hostname, and reject nodes with no usable destination.
func TestPeerHostPorts(t *testing.T) {
	cases := []struct {
		name string
		node RawNode
		want []string
	}{
		{"ip preserved", RawNode{Addresses: []string{"192.168.1.5"}, Host: "h.local.", Port: 14319}, []string{"192.168.1.5:14319"}},
		{"host fallback", RawNode{Host: "h.local.", Port: 14319}, []string{"h.local.:14319"}},
		{"no address", RawNode{Port: 14319}, nil},
		{"no port", RawNode{Addresses: []string{"192.168.1.5"}}, nil},
		{"all ipv4 deterministic", RawNode{Addresses: []string{"192.168.1.9", "192.168.1.5"}, Port: 14319}, []string{"192.168.1.5:14319", "192.168.1.9:14319"}},
		{"ipv4 before ipv6", RawNode{Addresses: []string{"2001:db8::1", "192.168.1.5"}, Port: 14319}, []string{"192.168.1.5:14319", "[2001:db8::1]:14319"}},
		// No private range outranks another. Which one a peer can actually be
		// reached on is not something its subnet number states, so two private
		// addresses tie and the tie is broken deterministically rather than by
		// the order the browse happened to resolve them in.
		{"private ranges tie", RawNode{Addresses: []string{"10.221.0.9", "192.168.1.5"}, Port: 14319}, []string{"10.221.0.9:14319", "192.168.1.5:14319"}},
		{"ip= TXT leads without dropping others", RawNode{Addresses: []string{"10.221.0.9"}, TXT: []string{"ip=192.168.1.5"}, Port: 14319}, []string{"192.168.1.5:14319", "10.221.0.9:14319"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerHostPorts(tc.node); !slices.Equal(got, tc.want) {
				t.Fatalf("peerHostPorts = %v, want %v", got, tc.want)
			}
		})
	}
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
	if len(ps.peers) != 0 {
		t.Fatalf("unclustered node retained %d peer(s), want 0", len(ps.peers))
	}
}
