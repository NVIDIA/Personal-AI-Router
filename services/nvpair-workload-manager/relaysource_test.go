// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"

	"nvpair-shared/noderec"
)

// TestSameNameDistinctUUIDPeersSurviveReplace: two peers
// that share a hostname but hold distinct hostUuids must remain two broadcast
// targets. relayPeerSource keys its map by hostUuid, but peerSet.Replace re-keys
// by PeerNode.ID — so PeerNode.ID must carry the hostUuid, not the (colliding)
// hostname, or one peer silently misses every workload broadcast.
func TestSameNameDistinctUUIDPeersSurviveReplace(t *testing.T) {
	src := newRelayPeerSource("self-uuid")
	node := func(uuid, ip string) noderec.DirectoryNode {
		return noderec.DirectoryNode{
			HostUUID: uuid, Name: "samehost", IP: ip, IPs: []string{ip, "192.168.1." + ip[len(ip)-1:]},
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceWorkload: {Port: 14320}},
		}
	}
	src.set([]noderec.DirectoryNode{node("uuid-1", "10.0.0.1"), node("uuid-2", "10.0.0.2")})

	nodes, _ := src.Nodes(context.Background())
	if len(nodes) != 2 {
		t.Fatalf("relay source kept %d peers, want 2", len(nodes))
	}
	ids := map[string]bool{}
	wantAddresses := map[string][]string{
		"uuid-1": {"10.0.0.1", "192.168.1.1"},
		"uuid-2": {"10.0.0.2", "192.168.1.2"},
	}
	for _, n := range nodes {
		ids[n.ID] = true
		if !slices.Equal(n.Addresses, wantAddresses[n.ID]) {
			t.Fatalf("PeerNode.Addresses = %v, want %v", n.Addresses, wantAddresses[n.ID])
		}
	}
	if !ids["uuid-1"] || !ids["uuid-2"] {
		t.Fatalf("PeerNode.ID must be the hostUuid; got %v", ids)
	}

	// peerSet.Replace keys the broadcast set by PeerNode.ID: both distinct-UUID
	// peers must be added, not collapsed into one under the shared hostname.
	ps := newPeerSet(14320)
	added, _ := ps.Replace(nodes)
	if len(added) != 2 {
		t.Fatalf("peerSet.Replace added %d targets, want 2 (same-named peers collapsed)", len(added))
	}
	wantCandidates := map[string][]string{
		"uuid-1": {"10.0.0.1:14320", "192.168.1.1:14320"},
		"uuid-2": {"10.0.0.2:14320", "192.168.1.2:14320"},
	}
	for _, target := range ps.targets() {
		if !slices.Equal(target.candidates, wantCandidates[target.id]) {
			t.Fatalf("target candidates = %v, want %v", target.candidates, wantCandidates[target.id])
		}
	}
}

// TestSelfFilteredByUUID confirms the self-filter drops our own record by
// hostUuid, keeping a same-named peer that holds a different UUID.
func TestSelfFilteredByUUID(t *testing.T) {
	src := newRelayPeerSource("self-uuid")
	svc := map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceWorkload: {Port: 14320}}
	src.set([]noderec.DirectoryNode{
		{HostUUID: "self-uuid", Name: "host", IP: "10.0.0.1", Services: svc}, // us
		{HostUUID: "peer-uuid", Name: "host", IP: "10.0.0.2", Services: svc}, // same name, different machine
	})
	nodes, _ := src.Nodes(context.Background())
	if len(nodes) != 1 || nodes[0].ID != "peer-uuid" {
		t.Fatalf("self-filter should drop only our own UUID, kept %+v", nodes)
	}
}
