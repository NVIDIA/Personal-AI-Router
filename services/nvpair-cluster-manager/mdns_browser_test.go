// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/noderec"
)

// A candidate copying a verified UUID must still be selectable for pairing by
// its discovery record ID. UUID lookup must keep returning the verified peer's
// endpoint, regardless of which record arrives first.
func TestBrowser_ResolvesDiscoveryRecordsWithoutReplacingVerifiedHost(t *testing.T) {
	const peerUUID = "peer"
	const peerAddress = "192.0.2.20"
	const candidateRecordID = "candidate:rogue"
	const candidateAddress = "192.0.2.30"
	const clusterPort = 14321
	peer := noderec.DirectoryNode{
		HostUUID:             peerUUID,
		DiscoveryRecordID:    "peer:peer",
		IdentityVerification: noderec.IdentityPinned,
		IP:                   peerAddress,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceCluster: {Port: clusterPort},
		},
	}
	candidate := peer
	candidate.DiscoveryRecordID = candidateRecordID
	candidate.IdentityVerification = noderec.IdentityUnverified
	candidate.IP = candidateAddress
	test := func(name string, records []noderec.DirectoryNode) {
		t.Run(name, func(t *testing.T) {
			browser := newBrowser()
			browser.setRelay(records)

			addresses, port, ok := browser.Resolve(candidateRecordID)
			require.True(t, ok)
			assert.Equal(t, []string{candidateAddress}, addresses)
			assert.Equal(t, clusterPort, port)
			addresses, _, ok = browser.Resolve(peerUUID)
			require.True(t, ok)
			assert.Equal(t, []string{peerAddress}, addresses)
		})
	}
	test("peer arrives first", []noderec.DirectoryNode{peer, candidate})
	test("candidate arrives first", []noderec.DirectoryNode{candidate, peer})
}

// An advertised UUID is untrusted text and may equal another record's ID.
// Selecting that record must still resolve its own endpoint in either order.
func TestBrowser_UUIDClaimsCannotReplaceDiscoveryRecordLookups(t *testing.T) {
	selected := noderec.DirectoryNode{
		HostUUID: "selected-uuid", DiscoveryRecordID: "observation:selected", Name: "selected-node",
		IdentityVerification: noderec.IdentityUnverified, IP: "192.0.2.20",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCluster: {Port: 14321}},
	}
	claiming := noderec.DirectoryNode{
		HostUUID: selected.DiscoveryRecordID, DiscoveryRecordID: "observation:other", Name: "other-node",
		IdentityVerification: noderec.IdentityUnverified, IP: "192.0.2.30",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCluster: {Port: 14322}},
	}
	test := func(name string, records []noderec.DirectoryNode) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			browser := newBrowser()
			browser.setRelay(records)
			resolve := func(key, wantAddress string, wantPort int) {
				t.Helper()
				addresses, port, found := browser.Resolve(key)
				require.True(t, found, "resolve %q", key)
				assert.Equal(t, []string{wantAddress}, addresses, "resolve %q", key)
				assert.Equal(t, wantPort, port, "resolve %q", key)
			}

			resolve("observation:selected", "192.0.2.20", 14321)
			resolve("observation:other", "192.0.2.30", 14322)
			// Both candidates must remain selectable by hostname despite the colliding text.
			resolve("selected-node", "192.0.2.20", 14321)
			resolve("other-node", "192.0.2.30", 14322)
		})
	}
	test("selected record arrives first", []noderec.DirectoryNode{selected, claiming})
	test("claiming record arrives first", []noderec.DirectoryNode{claiming, selected})
}

// Every lookup form must preserve address ranking so pairing and reconnection
// can try the publisher's alternative addresses when the first is unreachable.
func TestBrowser_ResolvesRecordUUIDAndHostnameWithRankedAddresses(t *testing.T) {
	const peerUUID = "peer-uuid"
	const recordID = "peer:peer-uuid"
	const hostName = "peer-workstation"
	const primaryAddress = "192.168.240.1"
	const secondaryAddress = "10.172.55.129"
	const clusterPort = 14321
	browser := newBrowser()
	browser.setRelay([]noderec.DirectoryNode{{
		HostUUID: peerUUID, DiscoveryRecordID: recordID, Name: hostName,
		IdentityVerification: noderec.IdentityPinned,
		IP:                   primaryAddress,
		IPs:                  []string{primaryAddress, secondaryAddress},
		Services:             map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCluster: {Port: clusterPort}},
	}})
	test := func(name, key string) {
		t.Run(name, func(t *testing.T) {
			addresses, port, found := browser.Resolve(key)

			require.True(t, found)
			assert.Equal(t, []string{primaryAddress, secondaryAddress}, addresses)
			assert.Equal(t, clusterPort, port)
		})
	}
	test("discovery record ID", recordID)
	test("host UUID", peerUUID)
	test("hostname", hostName)
}
