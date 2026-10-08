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

// mergeReference is a fixed instant for building lastSeen timestamps.
//
// The merge itself no longer reads a clock — presence comes from whether the
// backend still lists a node, not from how old its record is — so this exists
// only to construct records of a given age and prove they are ignored.
var mergeReference = time.Unix(1_700_000_000, 0)

// seenAgo builds a lastSeen timestamp d before the reference instant.
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
func TestFilterNodeRowsMatchesNameAndAddress(t *testing.T) {
	rows := []nodeRow{
		{key: "a", name: "workstation", address: "10.0.0.5", addresses: []string{"10.0.0.5", "192.168.1.9"}},
		{key: "b", name: "laptop", address: "10.0.0.6"},
		{key: "c", name: "Server-01", address: "10.0.1.7"},
	}

	cases := map[string][]string{
		"work":        {"a"},      // name substring
		"10.0.0.":     {"a", "b"}, // shared address prefix
		"192.168.1.9": {"a"},      // a secondary address
		"SERVER":      {"c"},      // case-insensitive
		"  laptop  ":  {"b"},      // surrounding whitespace ignored
		"nothing":     {},
	}
	for needle, want := range cases {
		got := filterNodeRows(rows, needle)
		if !assert.Len(t, got, len(want), "filter %q", needle) {
			continue
		}
		for i, key := range want {
			assert.Equal(t, key, got[i].key, "filter %q row %d", needle, i)
		}
	}

	// An empty filter is not a filter.
	assert.Len(t, filterNodeRows(rows, "   "), len(rows), "blank filter must preserve rows")
}

func TestMergeKeepsOfflineMembersListed(t *testing.T) {
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
func TestMergeDoesNotAgeOutDiscoveryOnItsOwnClock(t *testing.T) {
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
func TestMergeNeedsSomewhereToReachANode(t *testing.T) {
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
func TestMergeDeduplicatesAcrossFeeds(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{{
			HostUUID:  "u1",
			Name:      "host",
			IPAddress: "10.0.0.5",
			LastSeen:  seenAgo(time.Second),
			Trusted:   true,
			Models:    []string{"llama3.2", "qwen3"},
		}},
		members: []clusterNode{{NodeUUID: "u1", Name: "host", State: "joined"}},
		manual:  []manualNode{{ID: "m1", Address: "10.0.0.5", NodeInfoUp: true}},
	})

	require.Len(t, rows, 1, "one machine must produce one row")
	row := rows[0]
	assert.Equal(t, "m1", row.manualID, "manual handle must survive the merge")
	assert.Equal(t, nodeMembership(membershipMember), row.membership)
	assert.Equal(t, 2, row.modelCount())
}

// TestMergeManualProbeBeatsDiscoverySilence covers a host on a network that
// filters multicast: it never announces, but a successful probe is direct
// evidence that it is up.
func TestMergeManualProbeBeatsDiscoverySilence(t *testing.T) {
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
func TestMergeUsesEveryFactTheManualWorkerReports(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		manual: []manualNode{{ID: "m1", Name: "lms-only", Address: "10.0.0.7", LMStudioUp: true}},
	})
	assert.Equal(t, nodePresence(presenceOnline), findRow(t, rows, "lms-only").presence, "a host answering only on LM Studio must read online")

	// Typed as a hostname, discovered by IP: only the UUID ties them together.
	rows = mergeNodes(nodeFeeds{
		discovered: []availableNode{{HostUUID: "u1", Name: "gpu-box", IPAddress: "10.0.0.5", Port: 14318}},
		manual:     []manualNode{{ID: "m1", Address: "gpu-box.lan", HostUUID: "u1", NodeInfoUp: true}},
	})
	require.Len(t, rows, 1, "the manual entry must be joined by its UUID")
	assert.Equal(t, "m1", rows[0].manualID, "the joined row must retain its manual handle")

	// A hand-added node reached on a non-default node-info port, over TLS.
	rows = mergeNodes(nodeFeeds{
		manual: []manualNode{{
			ID: "m2", Name: "tls-box", Address: "10.0.0.9", NodeInfoUp: true,
			NodeInfoPort: 14319, TLSEnabled: true, TelemetryValid: true,
			GPUs: []noderec.GPUInfo{{Name: "GPU 0", VramBytes: 1 << 30}},
		}},
	})
	tls := findRow(t, rows, "tls-box")
	assert.Equal(t, 14319, tls.port, "node-info port must match the probe")
	assert.True(t, tls.nodeInfoTLS, "TLS node-info must be carried with the worker's reading")
	if assert.NotNil(t, tls.probedTelemetry, "worker's reading must be carried") {
		assert.Len(t, tls.probedTelemetry.GPUs, 1)
	}
}

// TestAReusedAddressDoesNotMergeTwoMachines checks an address is not taken for
// an identity. A manual entry whose probe read one UUID must not fold into a
// discovered row with a different UUID at the same address — that is another
// machine now answering there. An entry the probe has not identified still
// joins by address, as it must for a host discovery reports by IP.
func TestAReusedAddressDoesNotMergeTwoMachines(t *testing.T) {
	discovered := []availableNode{{HostUUID: "machine-b", Name: "b", IPAddress: "10.0.0.5", Port: 14318}}

	rows := mergeNodes(nodeFeeds{
		discovered: discovered,
		manual:     []manualNode{{ID: "m1", Name: "a", Address: "10.0.0.5", HostUUID: "machine-a", NodeInfoUp: true}},
	})
	require.Len(t, rows, 2, "two machines at one address must remain distinct")
	assert.Empty(t, findRow(t, rows, "b").manualID, "machine b must not take the manual entry for machine a")

	rows = mergeNodes(nodeFeeds{
		discovered: discovered,
		manual:     []manualNode{{ID: "m1", Address: "10.0.0.5"}},
	})
	require.Len(t, rows, 1, "an unidentified entry must join the machine at its address")
	assert.Equal(t, "m1", rows[0].manualID)
}

// TestTLSNodeInfoIsNotPolledInPlainText checks the detail screen shows the
// worker's reading for a TLS node rather than polling it itself. That endpoint
// needs the backend's cluster trust, and a plain-HTTP poll to it only fails.
func TestTLSNodeInfoIsNotPolledInPlainText(t *testing.T) {
	d := newNodeDetail(nil, nodeRow{
		key: "manual:m2", name: "tls-box", address: "10.0.0.9", port: 14319,
		presence: presenceOnline, nodeInfoTLS: true,
		probedTelemetry: &nodeTelemetry{TelemetryValid: true,
			GPUs: []noderec.GPUInfo{{Name: "GPU 0", VramBytes: 1 << 30}}},
	})
	d.SetSize(100, 30)
	assert.Nil(t, d.telemetryCmd(), "the detail screen must not poll a TLS node-info endpoint over plain HTTP")
	assert.Contains(t, d.View(), "GPU 0", "the worker's hardware reading must be shown")
}

// TestMembershipGovernsInvitability is the guard for re-inviting a node that
// already has a relationship. Only a standalone node may be invited.
func TestMembershipGovernsInvitability(t *testing.T) {
	cases := map[nodeMembership]bool{
		membershipNone:    true,
		membershipMember:  false,
		membershipForeign: false,
		membershipPending: false,
	}
	for membership, want := range cases {
		assert.Equal(t, want, membership.invitable(), "%v", membership)
	}
}

// TestMergeMarksSelf checks this machine is identified and always reads online.
func TestMergeMarksSelf(t *testing.T) {
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
func TestMergeSortsOnlineBeforeOffline(t *testing.T) {
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
func TestMergeForeignClusterNotInvitable(t *testing.T) {
	rows := mergeNodes(nodeFeeds{
		discovered: []availableNode{
			{HostUUID: "f", Name: "foreign", Clustered: true, LastSeen: seenAgo(time.Second)},
			{HostUUID: "s", Name: "standalone", LastSeen: seenAgo(time.Second)},
		},
	})

	assert.Equal(t, nodeMembership(membershipForeign), findRow(t, rows, "foreign").membership)
	assert.True(t, findRow(t, rows, "standalone").membership.invitable(), "standalone node must be invitable")
}

func TestMergeEmptyFeeds(t *testing.T) {
	assert.Empty(t, mergeNodes(nodeFeeds{}))
}
