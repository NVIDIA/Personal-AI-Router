// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"
	"time"
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
	t.Fatalf("no row named %q in %d rows", name, len(rows))
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
		if len(got) != len(want) {
			t.Errorf("filter %q matched %d rows, want %d", needle, len(got), len(want))
			continue
		}
		for i, key := range want {
			if got[i].key != key {
				t.Errorf("filter %q row %d = %q, want %q", needle, i, got[i].key, key)
			}
		}
	}

	// An empty filter is not a filter.
	if got := filterNodeRows(rows, "   "); len(got) != len(rows) {
		t.Errorf("blank filter dropped rows: %d of %d", len(got), len(rows))
	}
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

	if len(rows) != 2 {
		t.Fatalf("got %d rows, want both members listed", len(rows))
	}

	gone := findRow(t, rows, "gone-host")
	if gone.presence != presenceOffline {
		t.Errorf("departed member presence = %v, want Offline", gone.presence)
	}
	if gone.membership != membershipMember {
		t.Errorf("departed member membership = %v, want Member", gone.membership)
	}

	up := findRow(t, rows, "up-host")
	if up.presence != presenceOnline {
		t.Errorf("live member presence = %v, want Online", up.presence)
	}
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
		if got := findRow(t, rows, name).presence; got != presenceOnline {
			t.Errorf("%s presence = %v, want Online: it is in the snapshot with an address",
				name, got)
		}
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
		if got := findRow(t, rows, name).presence; got != presenceOffline {
			t.Errorf("%s presence = %v, want Offline", name, got)
		}
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

	if len(rows) != 1 {
		t.Fatalf("one machine produced %d rows", len(rows))
	}
	row := rows[0]
	if row.manualID != "m1" {
		t.Errorf("manual handle lost in merge: %q", row.manualID)
	}
	if row.membership != membershipMember {
		t.Errorf("membership = %v, want Member", row.membership)
	}
	if row.modelCount() != 2 {
		t.Errorf("model count = %d, want 2", row.modelCount())
	}
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

	if got := findRow(t, rows, "reachable").presence; got != presenceOnline {
		t.Errorf("probed-up manual node presence = %v, want Online", got)
	}
	if got := findRow(t, rows, "dead").presence; got != presenceOffline {
		t.Errorf("unreachable manual node presence = %v, want Offline", got)
	}
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
		if got := membership.invitable(); got != want {
			t.Errorf("%v invitable = %v, want %v", membership, got, want)
		}
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
	if !self.self {
		t.Error("self node not marked")
	}
	if self.presence != presenceOnline {
		t.Errorf("self presence = %v; this machine is by definition reachable", self.presence)
	}
	if rows[0].name != "this-host" {
		t.Errorf("self sorted to position of %q, want first", rows[0].name)
	}
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

	if rows[0].name != "zzz" {
		t.Errorf("first row = %q, want the online node despite its later name", rows[0].name)
	}
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

	if got := findRow(t, rows, "foreign").membership; got != membershipForeign {
		t.Errorf("foreign membership = %v", got)
	}
	if got := findRow(t, rows, "standalone").membership; !got.invitable() {
		t.Errorf("standalone node reported not invitable (%v)", got)
	}
}

func TestMergeEmptyFeeds(t *testing.T) {
	if rows := mergeNodes(nodeFeeds{}); len(rows) != 0 {
		t.Errorf("empty feeds produced %d rows", len(rows))
	}
}
