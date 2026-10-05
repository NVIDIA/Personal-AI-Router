// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReconcileRefreshesRenamedPeerName verifies that when a still-trusted
// peer renames its PC and reconciles, its own roster entry (sender == entry
// uuid) refreshes both the member record and the persisted pin to the new
// display name, keyed by the stable nodeUuid — and a second identical reconcile
// is a no-op.
func TestReconcileRefreshesRenamedPeerName(t *testing.T) {
	m := newTestManager(t)
	aUUID, aCert, aFP, _ := makeNode(t, "old-a")
	pinTrusted(t, m, aUUID, aCert, aFP)
	m.upsertMember(&ClusterNode{
		NodeUUID: aUUID, ID: "old-a", Name: "old-a",
		IPAddress: "10.0.0.1", Port: 14321, State: stateMember,
	})

	roster := &Roster{ClusterID: "cluster-1", Members: []RosterEntry{
		{NodeUUID: aUUID, NodeID: "new-a", Name: "new-a", CertPem: aCert, CertFingerprint: aFP},
	}}
	require.True(t, m.mergeRoster(roster, aUUID), "rename should report a change")
	n, ok := m.memberByNodeID(aUUID)
	require.True(t, ok, "member not renamed: got id")
	require.Equal(t, "new-a", n.ID, "member not renamed: got id")
	require.Equal(t, "new-a", n.Name, "member not renamed: got id")
	pin, ok := m.trust.Get(aUUID)
	require.True(t, ok, "pin not renamed: got nodeId")
	require.Equal(t, "new-a", pin.NodeID, "pin not renamed: got nodeId")
	require.Equal(t, "new-a", pin.Name, "pin not renamed: got nodeId")
	require.False(t, m.mergeRoster(roster, aUUID), "re-applying the same name should be a no-op")
}

// TestReconcileIgnoresThirdPartyName verifies the anti-flap guard: only a peer's
// OWN self entry may rename it. A third party's (possibly stale) view of that
// peer, carried in someone else's roster, must not overwrite the current name.
func TestReconcileIgnoresThirdPartyName(t *testing.T) {
	m := newTestManager(t)
	aUUID, aCert, aFP, _ := makeNode(t, "a")
	bUUID, bCert, bFP, _ := makeNode(t, "b")
	pinTrusted(t, m, aUUID, aCert, aFP)
	pinTrusted(t, m, bUUID, bCert, bFP)
	m.upsertMember(&ClusterNode{NodeUUID: bUUID, ID: "b-current", Name: "b-current", State: stateMember})

	// A reconciles, carrying a stale name for B in A's roster (a third-party entry).
	roster := &Roster{ClusterID: "cluster-1", Members: []RosterEntry{
		{NodeUUID: aUUID, NodeID: "a", Name: "a", CertPem: aCert, CertFingerprint: aFP},
		{NodeUUID: bUUID, NodeID: "b-stale", Name: "b-stale", CertPem: bCert, CertFingerprint: bFP},
	}}
	m.mergeRoster(roster, aUUID)

	n, _ := m.memberByNodeID(bUUID)
	require.Equal(t, "b-current", n.ID, "a third party's stale name must not rename B: got id")
	require.Equal(t, "b-current", n.Name, "a third party's stale name must not rename B: got id")
}

// TestRefreshSelfMemberIdentityOnRestart verifies that a self entry restored
// from members.json under the PC's old name is re-stamped to the current
// identity on startup, so the local node doesn't linger as a stale-named ghost
// in its own roster.
func TestRefreshSelfMemberIdentityOnRestart(t *testing.T) {
	m := newTestManager(t)
	m.upsertMember(&ClusterNode{
		NodeUUID: m.identity.NodeUUID, ID: "old-name", Name: "old-name",
		IPAddress: "127.0.0.1", Port: m.port, State: stateMember,
	})

	m.refreshSelfMemberIdentity()

	self, ok := m.memberByNodeID(m.identity.NodeUUID)
	require.True(t, ok, "self member missing after refresh")
	require.True(t, self.ID == m.identity.NodeID, "self not re-stamped: got id")
	require.True(t, self.Name == m.identity.Name, "self not re-stamped: got id")
}
