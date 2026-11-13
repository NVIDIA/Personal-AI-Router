// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// Feed a verified workload peer and an unverified copy of its UUID into the
// same snapshot. Only the verified endpoint may become a synchronization target;
// the later candidate must neither replace it nor add another target.
func TestRelayPeerSource_UnverifiedClaimsCannotReplaceVerifiedEndpoints(t *testing.T) {
	const peerAddress = "192.0.2.20"
	source := newRelayPeerSource("self")
	peer := noderec.DirectoryNode{
		HostUUID:             "peer",
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		IP:                   peerAddress,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceWorkload: {Port: 14320},
		},
	}
	candidate := peer
	candidate.IdentityVerification = noderec.IdentityUnverified
	candidate.IP = "192.0.2.30"

	source.set([]noderec.DirectoryNode{peer, candidate})

	peers, err := source.Nodes(context.Background())
	require.NoError(t, err)
	require.Len(t, peers, 1)
	assert.Equal(t, []string{peerAddress}, peers[0].Addresses)
}

func TestRelayPeerSource_RemovesPeersWhenTrustIsWithdrawn(t *testing.T) {
	const peerUUID = "peer"
	const peerAddress = "192.0.2.20"
	source := newRelayPeerSource("self")
	peer := noderec.DirectoryNode{
		HostUUID:             peerUUID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		IP:                   peerAddress,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceWorkload: {Port: 14320},
		},
	}
	source.set([]noderec.DirectoryNode{peer})
	peers, err := source.Nodes(context.Background())
	require.NoError(t, err)
	require.Len(t, peers, 1)
	assert.Equal(t, peerUUID, peers[0].ID)
	assert.Equal(t, []string{peerAddress}, peers[0].Addresses)

	peer.Trusted = false
	source.set([]noderec.DirectoryNode{peer})

	peers, err = source.Nodes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, peers)
}

// TestSameNameDistinctUUIDPeersSurviveReplace: two peers
// that share a hostname but hold distinct hostUuids must remain two broadcast
// targets. relayPeerSource keys its map by hostUuid, but peerSet.Replace re-keys
// by PeerNode.ID — so PeerNode.ID must carry the hostUuid, not the (colliding)
// hostname, or one peer silently misses every workload broadcast.
func TestSameNameDistinctUUIDPeers_SurviveReplace(t *testing.T) {
	src := newRelayPeerSource("self-uuid")
	node := func(uuid, ip string) noderec.DirectoryNode {
		return noderec.DirectoryNode{
			IdentityVerification: noderec.IdentityPinned,
			Trusted:              true,
			HostUUID:             uuid,
			Name:                 "samehost",
			IP:                   ip,
			IPs:                  []string{ip, "192.168.1." + ip[len(ip)-1:]},
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceWorkload: {Port: 14320},
			},
		}
	}
	src.set([]noderec.DirectoryNode{node("uuid-1", "10.0.0.1"), node("uuid-2", "10.0.0.2")})

	nodes, err := src.Nodes(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	ids := map[string]bool{}
	wantAddresses := map[string][]string{
		"uuid-1": {"10.0.0.1", "192.168.1.1"},
		"uuid-2": {"10.0.0.2", "192.168.1.2"},
	}
	for _, n := range nodes {
		ids[n.ID] = true
		assert.Equal(t, wantAddresses[n.ID], n.Addresses, "addresses for %s", n.ID)
	}
	assert.Contains(t, ids, "uuid-1", "PeerNode.ID must be the hostUuid")
	assert.Contains(t, ids, "uuid-2", "PeerNode.ID must be the hostUuid")

	// peerSet.Replace keys the broadcast set by PeerNode.ID: both distinct-UUID
	// peers must be added, not collapsed into one under the shared hostname.
	ps := newPeerSet(14320)
	added, _ := ps.Replace(nodes)
	assert.Len(t, added, 2, "same-named peers must remain distinct")
	wantCandidates := map[string][]string{
		"uuid-1": {"10.0.0.1:14320", "192.168.1.1:14320"},
		"uuid-2": {"10.0.0.2:14320", "192.168.1.2:14320"},
	}
	for _, target := range ps.targets() {
		assert.Equal(t, wantCandidates[target.id], target.candidates, "candidates for %s", target.id)
	}
}

// TestSelfFilteredByUUID confirms the self-filter drops our own record by
// hostUuid, keeping a same-named peer that holds a different UUID.
func TestPeerDirectory_ExcludesLocalUUID(t *testing.T) {
	const selfUUID = "self-uuid"
	const peerUUID = "peer-uuid"
	const name = "host"
	src := newRelayPeerSource(selfUUID)
	svc := map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceWorkload: {Port: 14320}}
	src.set([]noderec.DirectoryNode{
		{ // us
			IdentityVerification: noderec.IdentityLocal,
			HostUUID:             selfUUID,
			Name:                 name,
			IP:                   "10.0.0.1",
			Services:             svc,
		},
		{ // same name, different machine
			IdentityVerification: noderec.IdentityPinned,
			Trusted:              true,
			HostUUID:             peerUUID,
			Name:                 name,
			IP:                   "10.0.0.2",
			Services:             svc,
		},
	})
	nodes, err := src.Nodes(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, peerUUID, nodes[0].ID, "self-filter should drop only our own UUID")
}
