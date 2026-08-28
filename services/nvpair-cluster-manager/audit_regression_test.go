// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleEndorserAdmissionCannotIntroduceMember(t *testing.T) {
	m := newTestManagerPort(t, 15126)
	endorser := newTestManagerPort(t, 15127)
	pinTrusted(t, m, endorser.identity.NodeUUID, string(endorser.identity.CertPEM), endorser.identity.CertFingerprint)
	if err := m.trust.Pin(&TrustedPin{
		NodeUUID: endorser.identity.NodeUUID, NodeID: "endorser", ClusterID: "cluster-1",
		AdmissionEpoch: 2, CertPem: string(endorser.identity.CertPEM),
		CertFingerprint: endorser.identity.CertFingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	targetUUID, targetCert, targetFP, _ := makeNode(t, "target")
	stale := signEndorsement(endorser.identity.Signer, endorser.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 1, 1)
	entry := RosterEntry{
		NodeUUID: targetUUID, NodeID: "target", AdmissionEpoch: 1,
		CertPem: targetCert, CertFingerprint: targetFP, Endorsements: []Endorsement{stale},
	}
	if m.applyMembers([]RosterEntry{entry}, "cluster-1", endorser.identity.NodeUUID) {
		t.Fatal("stale endorser admission introduced a member")
	}
}

func TestLegacyTombstoneCannotEvictAdmissionAwareMember(t *testing.T) {
	m := newTestManagerPort(t, 15128)
	remover := newTestManagerPort(t, 15129)
	target := newTestManagerPort(t, 15130)
	pinTrusted(t, m, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	pinTrusted(t, m, target.identity.NodeUUID, string(target.identity.CertPEM), target.identity.CertFingerprint)
	m.upsertMember(&ClusterNode{
		NodeUUID: target.identity.NodeUUID, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	legacy := signTombstone(remover.identity.Signer, remover.identity.NodeUUID,
		target.identity.NodeUUID, "cluster-1", time.Now().UnixMilli())
	m.applyTombstones([]Tombstone{legacy}, "cluster-1")
	if _, ok := m.trust.Get(target.identity.NodeUUID); !ok {
		t.Fatal("legacy downgrade de-pinned an admission-aware member")
	}
}

func TestRemovalRevalidatesTargetAdmissionBeforeDepin(t *testing.T) {
	m := newTestManagerPort(t, 15131)
	target := newTestManagerPort(t, 15132)
	pinTrusted(t, m, target.identity.NodeUUID, string(target.identity.CertPEM), target.identity.CertFingerprint)
	m.upsertMember(&ClusterNode{
		NodeUUID: target.identity.NodeUUID, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	targetUUID := target.identity.NodeUUID
	params := mustJSON(t, removeParams{NodeUUID: &targetUUID})

	m.testRemovalPrepared = make(chan struct{})
	m.testRemovalContinue = make(chan struct{})
	done := make(chan struct{})
	go func() {
		m.handleNodesRemove(&Message{Params: params})
		close(done)
	}()
	<-m.testRemovalPrepared
	m.rosterMu.Lock()
	if err := m.trust.Pin(&TrustedPin{
		NodeUUID: target.identity.NodeUUID, NodeID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 2, CertPem: string(target.identity.CertPEM),
		CertFingerprint: target.identity.CertFingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	m.upsertMember(&ClusterNode{
		NodeUUID: target.identity.NodeUUID, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 2, State: stateMember,
	})
	m.rosterMu.Unlock()
	close(m.testRemovalContinue)
	<-done
	pin, ok := m.trust.Get(target.identity.NodeUUID)
	if !ok || pin.AdmissionEpoch != 2 {
		t.Fatalf("newer target admission was removed: %+v", pin)
	}
}

func TestTrustAndMembershipSnapshotsAreDeepCopies(t *testing.T) {
	m := newTestManagerPort(t, 15133)
	peer := newTestManagerPort(t, 15134)
	pinTrusted(t, m, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)
	joined := time.Now().UnixMilli()
	m.upsertMember(&ClusterNode{
		NodeUUID: peer.identity.NodeUUID, ID: "peer", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember, JoinedAt: &joined,
	})

	pin, _ := m.trust.Get(peer.identity.NodeUUID)
	pin.AdmissionEpoch = 99
	member, _ := m.memberByNodeID(peer.identity.NodeUUID)
	member.AdmissionEpoch = 99
	*member.JoinedAt = 0
	pinAgain, _ := m.trust.Get(peer.identity.NodeUUID)
	memberAgain, _ := m.memberByNodeID(peer.identity.NodeUUID)
	if pinAgain.AdmissionEpoch != 1 || memberAgain.AdmissionEpoch != 1 || *memberAgain.JoinedAt != joined {
		t.Fatal("snapshot mutation escaped into internal state")
	}

	to, code := "peer", "123456"
	inv := &Invite{InviteID: "inv-copy", ToNodeID: &to, Pin: &code, State: inviteStatePending}
	m.putInvite(inv)
	*inv.Pin = "mutated"
	got, _ := m.getInvite(inv.InviteID)
	*got.ToNodeID = "changed"
	*got.Pin = "changed"
	again, _ := m.getInvite(inv.InviteID)
	if *again.ToNodeID != "peer" || *again.Pin != "123456" {
		t.Fatal("invite snapshot mutation escaped into internal state")
	}

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.trust.dir = blocker
	if _, err := m.trust.UpdateIdentity(peer.identity.NodeUUID, "new-id", "new-name"); err == nil {
		t.Fatal("trust identity update unexpectedly persisted")
	}
	unchanged, _ := m.trust.Get(peer.identity.NodeUUID)
	if unchanged.NodeID == "new-id" || unchanged.Name == "new-name" {
		t.Fatal("failed trust write mutated live identity")
	}
	end := m.endorsePeer(peer.identity.NodeUUID, peer.identity.CertFingerprint, 1)
	before := len(unchanged.Endorsements)
	if err := m.trust.AddEndorsements(peer.identity.NodeUUID, []Endorsement{end}); err == nil {
		t.Fatal("endorsement update unexpectedly persisted")
	}
	unchanged, _ = m.trust.Get(peer.identity.NodeUUID)
	if len(unchanged.Endorsements) != before {
		t.Fatal("failed trust write mutated live endorsements")
	}
}

func TestRestartFinishesInterruptedTeardownAndRejectsStaleRestore(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15135)
	activateTestCluster(t, m, "cluster-1")
	peer := newTestManagerPort(t, 15136)
	pinTrusted(t, m, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)
	m.upsertMember(&ClusterNode{
		NodeUUID: peer.identity.NodeUUID, ID: "peer", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	if err := m.persistMembersErr(); err != nil {
		t.Fatal(err)
	}
	if err := m.beginDurableTeardown(); err != nil {
		t.Fatal(err)
	}
	if err := m.clearAdmission(); err != nil {
		t.Fatal(err)
	}

	restarted := testManagerAt(t, dir, 15135)
	if _, ok := restarted.trust.Get(peer.identity.NodeUUID); ok {
		t.Fatal("restart left a pin from interrupted teardown")
	}
	if len(restarted.snapshotNodes()) != 0 {
		t.Fatal("restart left membership from interrupted teardown")
	}
	staleID := "cluster-1"
	restarted.handleSetIdentity(&Message{Params: mustJSON(t, setIdentityParams{ClusterID: &staleID})})
	if cid, _ := restarted.clusterIdentity(); cid != "" {
		t.Fatalf("stale settings resurrected cluster %q", cid)
	}
}

func TestRemovalReplayFailsClosedOnPersistenceError(t *testing.T) {
	m := newTestManagerPort(t, 15138)
	target := newTestManagerPort(t, 15139)
	pinTrusted(t, m, target.identity.NodeUUID, string(target.identity.CertPEM), target.identity.CertFingerprint)
	m.upsertMember(&ClusterNode{
		NodeUUID: target.identity.NodeUUID, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	proof, err := m.newRemovalProof(target.identity.NodeUUID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.putRemovalProof(proof); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.clusterDir = blocker
	if err := m.replayRemovalProofs(); err == nil {
		t.Fatal("removal replay ignored durable member cleanup failure")
	}
}
