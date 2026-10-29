// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// Present a verified engine-control peer followed by an unverified copy with
// another address. Building the peer directory must retain the verified endpoint
// so a later discovery claim cannot redirect engine commands.
func TestPeerDirectory_UnverifiedClaimsCannotReplaceVerifiedEndpoints(t *testing.T) {
	const peerUUID = "peer"
	const peerAddress = "192.0.2.20"
	directory := newPeerDirectory()
	peer := noderec.DirectoryNode{
		HostUUID:             peerUUID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		IP:                   peerAddress,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}
	candidate := peer
	candidate.IdentityVerification = noderec.IdentityUnverified
	candidate.IP = "192.0.2.30"

	directory.set([]noderec.DirectoryNode{peer, candidate})

	resolved, ok := directory.lookup(peerUUID)
	require.True(t, ok)
	assert.Equal(t, []string{peerAddress}, resolved.addresses)
}

// A record with a complete engine-control endpoint must still be absent from
// the directory unless it has both proved peer identity and current trust.
func TestPeerDirectory_RejectsUnverifiedOrUntrustedPeers(t *testing.T) {
	const hostUUID = "peer"
	test := func(name string, verification noderec.IdentityVerification, trusted bool) {
		t.Run(name, func(t *testing.T) {
			directory := newPeerDirectory()
			node := noderec.DirectoryNode{
				HostUUID:             hostUUID,
				IP:                   "192.0.2.20",
				IdentityVerification: verification,
				Trusted:              trusted,
				Services: map[noderec.ServiceKey]noderec.ServiceStatus{
					noderec.ServiceEngineControl: {Port: 14323},
				},
			}

			directory.set([]noderec.DirectoryNode{node})

			_, present := directory.lookup(hostUUID)
			assert.False(t, present)
		})
	}
	test("unverified identity with claimed trust", noderec.IdentityUnverified, true)
	test("missing verification with claimed trust", "", true)
	test("pinned identity without current trust", noderec.IdentityPinned, false)
}

// Locally owned engine-control records remain available without cluster
// membership. Remote peer trust is not required for the local worker's record.
func TestPeerDirectory_AcceptsLocalRecordsWithoutPeerTrust(t *testing.T) {
	const hostUUID = "self"
	const controlPort = 14323
	directory := newPeerDirectory()
	node := noderec.DirectoryNode{
		HostUUID:             hostUUID,
		IP:                   "127.0.0.1",
		IdentityVerification: noderec.IdentityLocal,
		Trusted:              false,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: controlPort},
		},
	}

	directory.set([]noderec.DirectoryNode{node})

	peer, present := directory.lookup(hostUUID)
	require.True(t, present)
	assert.Equal(t, hostUUID, peer.nodeID)
	assert.Equal(t, controlPort, peer.port)
}

func TestPeerDirectory_ReplacesEntriesAndResolvesPeers(t *testing.T) {
	const peerUUID = "uuid-b"
	const noControlUUID = "uuid-c"
	const noAddressUUID = "uuid-d"
	const primaryAddress = "192.168.1.42"
	const secondaryAddress = "10.0.0.42"
	const clusterUUID = "cuuid-b"
	const controlPort = 14323
	d := newPeerDirectory()
	d.set([]noderec.DirectoryNode{
		{
			IdentityVerification: noderec.IdentityPinned,
			Trusted:              true,
			HostUUID:             peerUUID,
			Name:                 "nodeB",
			IP:                   primaryAddress,
			IPs:                  []string{primaryAddress, secondaryAddress},
			ClusterUUID:          clusterUUID,
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineControl: {Port: controlPort},
			},
		},
		{ // advertises em but not ec -> excluded
			IdentityVerification: noderec.IdentityPinned,
			Trusted:              true,
			HostUUID:             noControlUUID,
			Name:                 "nodeC",
			IP:                   "192.168.1.43",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineManager: {Port: 14322},
			},
		},
		{ // ec but no dialable IP -> excluded
			IdentityVerification: noderec.IdentityPinned,
			Trusted:              true,
			HostUUID:             noAddressUUID,
			Name:                 "nodeD",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineControl: {Port: controlPort},
			},
		},
	})

	p, ok := d.lookup(peerUUID)
	require.True(t, ok, "unexpected nodeB entry (%v, %v)", p, ok)
	assert.Equal(t, []string{primaryAddress, secondaryAddress}, p.addresses, "unexpected nodeB addresses")
	require.Equal(t, controlPort, p.port, "unexpected nodeB entry (%v, %v)", p, ok)
	require.Equal(t, clusterUUID, p.clusterUUID, "unexpected nodeB entry (%v, %v)", p, ok)
	_, ok = d.lookup(noControlUUID)
	require.False(t, ok, "nodeC advertises no ec; should not be in directory")
	_, ok = d.lookup(noAddressUUID)
	require.False(t, ok, "nodeD has no dialable IP; should not be in directory")

	// A later snapshot replaces the set wholesale.
	d.set(nil)
	_, ok = d.lookup(peerUUID)
	require.False(t, ok, "empty snapshot should clear the directory")
}

// TestPeerDirectoryKeysByHostUUID: an ec peer with a stable hostUuid is keyed
// (and addressed) by it, not the hostname — so a remote-control target survives
// a rename and same-named peers stay distinct. A record without a
// hostUuid falls back to the instance name.
func TestPeerDirectory_KeysByHostUUID(t *testing.T) {
	const hostUUID = "uuid-b"
	const hostName = "nodeB"
	const address = "192.168.1.42"
	d := newPeerDirectory()
	d.set([]noderec.DirectoryNode{{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		HostUUID:             hostUUID,
		Name:                 hostName,
		IP:                   address,
		ClusterUUID:          "cuuid-b",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}})
	p, ok := d.lookup(hostUUID)
	require.True(t, ok, "expected lookup by hostUuid (%v, %v)", p, ok)
	require.Equal(t, hostUUID, p.nodeID, "expected lookup by hostUuid (%v, %v)", p, ok)
	assert.Equal(t, []string{address}, p.addresses, "expected lookup by hostUuid")
	_, ok = d.lookup(hostName)
	require.False(t, ok, "must not be addressable by hostname when a hostUuid is present")
}
