// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

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
	if !m.mergeRoster(roster, aUUID) {
		t.Fatal("rename should report a change")
	}
	n, ok := m.memberByNodeID(aUUID)
	if !ok || n.ID != "new-a" || n.Name != "new-a" {
		t.Fatalf("member not renamed: got id=%q name=%q", n.ID, n.Name)
	}
	pin, ok := m.trust.Get(aUUID)
	if !ok || pin.NodeID != "new-a" || pin.Name != "new-a" {
		t.Fatalf("pin not renamed: got nodeId=%q name=%q", pin.NodeID, pin.Name)
	}
	if m.mergeRoster(roster, aUUID) {
		t.Fatal("re-applying the same name should be a no-op")
	}
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
	if n.ID != "b-current" || n.Name != "b-current" {
		t.Fatalf("a third party's stale name must not rename B: got id=%q name=%q", n.ID, n.Name)
	}
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
	if !ok {
		t.Fatal("self member missing after refresh")
	}
	if self.ID != m.identity.NodeID || self.Name != m.identity.Name {
		t.Fatalf("self not re-stamped: got id=%q name=%q want id=%q name=%q",
			self.ID, self.Name, m.identity.NodeID, m.identity.Name)
	}
}
