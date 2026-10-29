// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

var mergeReference = time.Unix(1_700_000_000, 0).UTC() // arbitrary fixed instant

// A discovered candidate copying a member's UUID must keep its own row and
// remain outside membership. Its claimed identity cannot change the verified
// peer's endpoint or turn the candidate into that member.
func TestMergeNodes_UnverifiedClaimsRemainSeparateFromMembers(t *testing.T) {
	const peerUUID = "peer"
	const peerName = "verified"
	const peerAddress = "192.0.2.20"
	const candidateName = "rogue"
	const candidateRecordID = "candidate:rogue"
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{
				HostUUID:             peerUUID,
				DiscoveryRecordID:    "peer:peer",
				IdentityVerification: noderec.IdentityPinned,
				Name:                 peerName,
				IPAddress:            peerAddress,
				Trusted:              true,
			},
			{
				HostUUID:             peerUUID,
				DiscoveryRecordID:    candidateRecordID,
				IdentityVerification: noderec.IdentityUnverified,
				Name:                 candidateName,
				IPAddress:            "192.0.2.30",
			},
		},
	})

	require.Len(t, rows, 2)
	assert.Equal(t, membershipNone, findRow(t, rows, candidateName).membership)
	assert.Equal(t, candidateRecordID, findRow(t, rows, candidateName).key)
	assert.Equal(t, peerAddress, findRow(t, rows, peerName).address)
}

// seenAgo builds a lastSeen timestamp d before a fixed reference instant.
// Presence comes from the service's snapshot, so these ages must not evict rows.
func seenAgo(d time.Duration) int64 { return mergeReference.Add(-d).Unix() }

func findRow(t *testing.T, rows []nodeRow, name string) nodeRow {
	t.Helper()
	for _, r := range rows {
		if r.name == name {
			return r
		}
	}
	require.FailNowf(t, "node row is missing", "no row named %q in %d rows", name, len(rows))
	return nodeRow{}
}

// TestMergeKeepsOfflineMembersListed is the regression guard for the reported
// asymmetry: a cluster member that goes away must stay listed and be marked
// offline, rather than either vanishing or continuing to look present.
// TestFilterNodeRowsMatchesNameAndAddress checks both of the things an operator
// knows a machine by. Addresses matter especially for nodes added by address,
// whose reported name they may never have seen.
func TestFilterNodeRows_MatchesNameAndAddress(t *testing.T) {
	rows := []nodeRow{
		{key: "a", name: "workstation", address: "10.0.0.5", addresses: []string{"10.0.0.5", "192.168.1.9"}},
		{key: "b", name: "laptop", address: "10.0.0.6"},
		{key: "c", name: "Server-01", address: "10.0.1.7"},
	}

	test := func(name, needle string, want ...string) {
		t.Run(name, func(t *testing.T) {
			got := filterNodeRows(rows, needle)
			keys := make([]string, len(got))
			for i, row := range got {
				keys[i] = row.key
			}
			assert.Equal(t, append([]string{}, want...), keys)
		})
	}
	test("name substring", "work", "a")
	test("shared address prefix", "10.0.0.", "a", "b")
	test("secondary address", "192.168.1.9", "a")
	test("case insensitive name", "SERVER", "c")
	test("surrounding whitespace", "  laptop  ", "b")
	test("no matches", "nothing")

	// An empty filter is not a filter.
	assert.Len(t, filterNodeRows(rows, "   "), len(rows), "blank filter must preserve rows")
}

func TestMerge_KeepsOfflineMembersListed(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{HostUUID: "up", Name: "up-host", IPAddress: "10.0.0.1", Port: 14318,
				LastSeen: seenAgo(2 * time.Second)},
		},
		members: []clusterNode{
			{NodeUUID: "up", Name: "up-host", State: "joined"},
			{NodeUUID: "gone", Name: "gone-host", State: "joined", IPAddress: "10.0.0.9"},
		},
	})

	require.Len(t, rows, 2, "both members must be listed")

	gone := findRow(t, rows, "gone-host")
	assert.Equal(t, nodePresence(presenceOffline), gone.presence)
	assert.Equal(t, nodeMembership(membershipMember), gone.membership)

	up := findRow(t, rows, "up-host")
	assert.Equal(t, nodePresence(presenceOnline), up.presence)
}

// TestMergeDoesNotAgeOutDiscoveryOnItsOwnClock is the regression guard for a
// healthy peer being marked Offline for going quiet.
//
// The scanner reports a node only when its record changes, so a peer with a
// stable advertisement stops producing events and its timestamp stops advancing
// — while the peer is perfectly reachable. Grading that age marked it Offline
// after 45 seconds. Eviction belongs to the backend, which has real evidence
// (mDNS silence plus a TCP probe plus inference traffic); a node still in the
// snapshot has survived that and must be shown as reachable.
//
// An hour-old timestamp is used deliberately: under the previous rule it was
// forty-eight thresholds stale.
func TestMerge_DoesNotAgeOutDiscoveryOnItsOwnClock(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{HostUUID: "fresh", Name: "fresh", IPAddress: "10.0.0.1", Port: 14318,
				LastSeen: seenAgo(5 * time.Second)},
			{HostUUID: "quiet", Name: "quiet", IPAddress: "10.0.0.2", Port: 14318,
				LastSeen: seenAgo(time.Hour)},
			{HostUUID: "never", Name: "never", IPAddress: "10.0.0.3", Port: 14318},
		},
	})

	for _, name := range []string{"fresh", "quiet", "never"} {
		assert.Equal(t, nodePresence(presenceOnline), findRow(t, rows, name).presence, "%s is in the snapshot with an address", name)
	}
}

// TestMergeNeedsSomewhereToReachANode mirrors the desktop app's rule, whose
// nodeInfoUp starts as `Boolean(ipAddress) && port > 0`. A snapshot entry with
// nowhere to connect is not a reachable node.
func TestMerge_NeedsSomewhereToReachANode(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{HostUUID: "noaddr", Name: "noaddr", Port: 14318},
			{HostUUID: "noport", Name: "noport", IPAddress: "10.0.0.4"},
		},
	})

	for _, name := range []string{"noaddr", "noport"} {
		assert.Equal(t, nodePresence(presenceOffline), findRow(t, rows, name).presence, "%s", name)
	}
}

// TestMergeDeduplicatesAcrossFeeds checks one machine appearing in all three
// feeds renders as a single row carrying every feed's contribution.
func TestMerge_DeduplicatesAcrossFeeds(t *testing.T) {
	const hostUUID = "u1"
	const hostName = "host"
	const address = "10.0.0.5"
	const manualID = "m1"
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{{
			HostUUID:  hostUUID,
			Name:      hostName,
			IPAddress: address,
			LastSeen:  seenAgo(time.Second),
			Trusted:   true,
			Models:    []string{"llama3.2", "qwen3"},
		}},
		members: []clusterNode{{
			NodeUUID: hostUUID,
			Name:     hostName,
			State:    "joined",
		}},
		manual: []manualNode{{
			IdentityVerification: noderec.IdentityPinned,
			HostUUID:             hostUUID,
			ID:                   manualID,
			Address:              address,
			NodeInfoUp:           true,
		}},
	})

	require.Len(t, rows, 1, "one machine must produce one row")
	row := rows[0]
	assert.Equal(t, manualID, row.manualID, "manual handle must survive the merge")
	assert.Equal(t, nodeMembership(membershipMember), row.membership)
	assert.Equal(t, 2, row.modelCount())
}

// TestMergeManualProbeBeatsDiscoverySilence covers a host on a network that
// filters multicast: it never announces, but a successful probe is direct
// evidence that it is up.
func TestMergeManualProbe_BeatsDiscoverySilence(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		manual: []manualNode{
			{ID: "m1", Name: "reachable", Address: "10.0.0.7", NodeInfoUp: true},
			{ID: "m2", Name: "dead", Address: "10.0.0.8"},
		},
	})

	assert.Equal(t, nodePresence(presenceOnline), findRow(t, rows, "reachable").presence)
	assert.Equal(t, nodePresence(presenceOffline), findRow(t, rows, "dead").presence)
}

// TestMergeUsesEveryFactTheManualWorkerReports is the regression guard for the
// merged table reading the manual-node worker's reply too narrowly.
//
// It decoded only Ollama and node-info liveness and an address, so a host
// running only LM Studio read Offline, a hostname typed by hand could not be
// joined to the same machine discovered by IP, and the node-info port and TLS
// setting the probe found were dropped.
func TestMerge_UsesEveryFactTheManualWorkerReports(t *testing.T) {
	const manualID = "m1"
	const hostUUID = "u1"
	const lmName = "lms-only"
	const tlsName = "tls-box"
	const tlsPort = 14319
	rows := mergeNodes(nodeFeeds{
		manual: []manualNode{{
			ID:         manualID,
			Name:       lmName,
			Address:    "10.0.0.7",
			LMStudioUp: true,
		}},
	})
	assert.Equal(t, nodePresence(presenceOnline), findRow(t, rows, lmName).presence, "a host answering only on LM Studio must read online")

	// Typed as a hostname, discovered by IP: only the UUID ties them together.
	rows = mergeNodes(nodeFeeds{
		discovered: []availableNode{{
			HostUUID:  hostUUID,
			Name:      "gpu-box",
			IPAddress: "10.0.0.5",
			Port:      14318,
		}},
		manual: []manualNode{{
			IdentityVerification: noderec.IdentityPinned,
			ID:                   manualID,
			Address:              "gpu-box.lan",
			HostUUID:             hostUUID,
			NodeInfoUp:           true,
		}},
	})
	require.Len(t, rows, 1, "the manual entry must be joined by its UUID")
	assert.Equal(t, manualID, rows[0].manualID, "the joined row must retain its manual handle")

	// A hand-added node reached on a non-default node-info port, over TLS.
	rows = mergeNodes(nodeFeeds{
		manual: []manualNode{{
			IdentityVerification: noderec.IdentityPinned,
			ID:                   "m2",
			Name:                 tlsName,
			Address:              "10.0.0.9",
			NodeInfoUp:           true,
			NodeInfoPort:         tlsPort,
			TLSEnabled:           true,
			TelemetryValid:       true,
			GPUs:                 []noderec.GPUInfo{{Name: "GPU 0", VramBytes: 1 << 30}},
		}},
	})
	tls := findRow(t, rows, tlsName)
	assert.Equal(t, tlsPort, tls.port, "node-info port must match the probe")
	assert.True(t, tls.nodeInfoTLS, "TLS node-info must be carried with the worker's reading")
	if assert.NotNil(t, tls.probedTelemetry, "worker's reading must be carried") {
		assert.Len(t, tls.probedTelemetry.GPUs, 1)
	}
}

// TestAReusedAddressDoesNotMergeTwoMachines checks an address is not taken for
// an identity. A manual entry whose probe read one UUID must not fold into a
// discovered row with a different UUID at the same address — that is another
// machine now answering there. An unidentified configuration also remains
// separate: sharing an address does not establish host identity.
func TestAReusedAddress_DoesNotMergeTwoMachines(t *testing.T) {
	const address = "10.0.0.5"
	const peerName = "b"
	const manualID = "m1"
	discovered := []availableNode{{
		HostUUID:  "machine-b",
		Name:      peerName,
		IPAddress: address,
		Port:      14318,
	}}

	rows := mergeNodes(nodeFeeds{
		discovered: discovered,
		manual: []manualNode{{
			ID:         manualID,
			Name:       "a",
			Address:    address,
			HostUUID:   "machine-a",
			NodeInfoUp: true,
		}},
	})
	require.Len(t, rows, 2, "two machines at one address must remain distinct")
	assert.Empty(t, findRow(t, rows, peerName).manualID, "machine b must not take the manual entry for machine a")

	rows = mergeNodes(nodeFeeds{
		discovered: discovered,
		manual: []manualNode{{
			ID:      manualID,
			Address: address,
		}},
	})
	require.Len(t, rows, 2, "an address alone cannot join a configured target to another record")
	assert.Empty(t, findRow(t, rows, peerName).manualID)
}

// Withdrawing a manual node's identity proof must remove its old hardware from
// an open detail screen without starting an HTTP poll against its TLS endpoint.
func TestOpenDetail_HidesManualHardwareAfterProofWithdrawal(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	manual := manualNode{
		ID: "tls-node", Name: "tls-box", Address: "10.0.0.9",
		HostUUID: "peer", IdentityVerification: noderec.IdentityPinned,
		OllamaUp: true, NodeInfoUp: true, NodeInfoPort: 14319, TLSEnabled: true,
		TelemetryValid: true,
		GPUs:           []noderec.GPUInfo{{Name: "GPU 0", VramBytes: 1 << 30}},
	}
	v.Update(manualNodesMsg{nodes: []manualNode{manual}})
	v.openDetail()
	require.NotNil(t, v.detail)
	require.Contains(t, v.View(), "GPU 0", "the authenticated hardware must be visible before proof withdrawal")
	assert.Nil(t, v.detail.telemetryCmd(), "the detail screen must not poll a TLS node-info endpoint over plain HTTP")

	// The engine still answers, but node-info can no longer prove its identity.
	manual.IdentityVerification = noderec.IdentityUnverified
	manual.HostUUID = ""
	manual.NodeInfoUp = false
	manual.TelemetryValid = false
	manual.GPUs = nil
	v.Update(manualNodesMsg{nodes: []manualNode{manual}})

	assert.Equal(t, presenceOnline, v.detail.node.presence, "the engine still answers after node-info proof is withdrawn")
	assert.NotContains(t, v.View(), "GPU 0", "the old authenticated hardware must leave the open screen")
	assert.Nil(t, v.detail.telemetryCmd(), "proof withdrawal must not start a plaintext poll against the configured TLS endpoint")
}

// TestMembershipGovernsInvitability is the guard for re-inviting a node that
// already has a relationship. Only a standalone node may be invited.
func TestNodeInvitation_RequiresEligibleClusterMembership(t *testing.T) {
	test := func(name string, membership nodeMembership, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, membership.invitable())
		})
	}
	test("standalone node can be invited", membershipNone, true)
	test("cluster member cannot be invited", membershipMember, false)
	test("foreign member cannot be invited", membershipForeign, false)
	test("pairing node cannot be invited", membershipPending, false)
}

// TestMergeMarksSelf checks this machine is identified and always reads online.
func TestMerge_MarksSelf(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{HostUUID: "me", Name: "this-host", LastSeen: seenAgo(time.Hour)},
			{HostUUID: "other", Name: "other-host", LastSeen: seenAgo(time.Second)},
		},
		selfUUID: "me",
	})

	self := findRow(t, rows, "this-host")
	assert.True(t, self.self, "self node not marked")
	assert.Equal(t, nodePresence(presenceOnline), self.presence, "this machine is by definition reachable")
	assert.Equal(t, "this-host", rows[0].name, "self must sort first")
}

// TestMergeSortsOnlineBeforeOffline checks reachable nodes lead the list. The
// offline one here is a cluster member discovery has never reported, which is
// what an unreachable node now looks like.
func TestMerge_SortsOnlineBeforeOffline(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{HostUUID: "z", Name: "zzz", IPAddress: "10.0.0.2", Port: 14318,
				LastSeen: seenAgo(time.Second)},
		},
		members: []clusterNode{{ID: "a", NodeUUID: "a", Name: "aaa", State: "joined"}},
	})

	assert.Equal(t, "zzz", rows[0].name, "online node must sort first despite its later name")
}

// TestMergeForeignClusterNotInvitable checks a node clustered elsewhere is
// distinguished from one of ours.
func TestMergeNodes_MarksForeignClusterNodesUninvitable(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{HostUUID: "f", Name: "foreign", Clustered: true, LastSeen: seenAgo(time.Second)},
			{HostUUID: "s", Name: "standalone", LastSeen: seenAgo(time.Second)},
		},
	})

	assert.Equal(t, nodeMembership(membershipForeign), findRow(t, rows, "foreign").membership)
	assert.True(t, findRow(t, rows, "standalone").membership.invitable(), "standalone node must be invitable")
}

func TestMergeNodes_ReturnsEmptyRowsForEmptyFeeds(t *testing.T) {
	assert.Empty(t, mergeNodes(nodeFeeds{}))
}
