// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStaleEndorserAdmissionCannotIntroduceMember(t *testing.T) {
	m := newTestManagerPort(t, 15126)
	endorser := newTestManagerPort(t, 15127)
	pinTrusted(t, m, endorser.identity.NodeUUID, string(endorser.identity.CertPEM), endorser.identity.CertFingerprint)
	require.NoError(t, m.trust.Pin(&TrustedPin{
		NodeUUID: endorser.identity.NodeUUID, NodeID: "endorser", ClusterID: "cluster-1",
		AdmissionEpoch: 2, CertPem: string(endorser.identity.CertPEM),
		CertFingerprint: endorser.identity.CertFingerprint,
	}))
	targetUUID, targetCert, targetFP, _ := makeNode(t, "target")
	stale := signEndorsement(endorser.identity.Signer, endorser.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 1, 1)
	entry := RosterEntry{
		NodeUUID: targetUUID, NodeID: "target", AdmissionEpoch: 1,
		CertPem: targetCert, CertFingerprint: targetFP, Endorsements: []Endorsement{stale},
	}
	require.False(t, m.applyMembers([]RosterEntry{entry}, "cluster-1", endorser.identity.NodeUUID), "stale endorser admission introduced a member")
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
	_, ok := m.trust.Get(target.identity.NodeUUID)
	require.True(t, ok, "legacy downgrade de-pinned an admission-aware member")
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
	require.NoError(t, m.trust.Pin(&TrustedPin{
		NodeUUID: target.identity.NodeUUID, NodeID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 2, CertPem: string(target.identity.CertPEM),
		CertFingerprint: target.identity.CertFingerprint,
	}))
	m.upsertMember(&ClusterNode{
		NodeUUID: target.identity.NodeUUID, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 2, State: stateMember,
	})
	m.rosterMu.Unlock()
	close(m.testRemovalContinue)
	<-done
	pin, ok := m.trust.Get(target.identity.NodeUUID)
	require.True(t, ok, "newer target admission was removed (%v)", pin)
	require.Equal(t, uint64(2), pin.AdmissionEpoch, "newer target admission was removed (%v)", pin)
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
	require.Equal(t, uint64(1), pinAgain.AdmissionEpoch, "snapshot mutation escaped into internal state")
	require.Equal(t, uint64(1), memberAgain.AdmissionEpoch, "snapshot mutation escaped into internal state")
	require.Equal(t, joined, *memberAgain.JoinedAt, "snapshot mutation escaped into internal state")

	to, code := "peer", "123456"
	inv := &Invite{InviteID: "inv-copy", ToNodeID: &to, Pin: &code, State: inviteStatePending}
	m.putInvite(inv)
	*inv.Pin = "mutated"
	got, _ := m.getInvite(inv.InviteID)
	*got.ToNodeID = "changed"
	*got.Pin = "changed"
	again, _ := m.getInvite(inv.InviteID)
	require.Equal(t, "peer", *again.ToNodeID, "invite snapshot mutation escaped into internal state")
	require.Equal(t, "123456", *again.Pin, "invite snapshot mutation escaped into internal state")

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	m.trust.dir = blocker
	_, err := m.trust.UpdateIdentity(peer.identity.NodeUUID, "new-id", "new-name")
	require.Error(t, err, "trust identity update unexpectedly persisted")
	unchanged, _ := m.trust.Get(peer.identity.NodeUUID)
	require.NotEqual(t, "new-id", unchanged.NodeID, "failed trust write mutated live identity")
	require.NotEqual(t, "new-name", unchanged.Name, "failed trust write mutated live identity")
	end := m.endorsePeer(peer.identity.NodeUUID, peer.identity.CertFingerprint, 1)
	before := len(unchanged.Endorsements)
	require.Error(t, m.trust.AddEndorsements(peer.identity.NodeUUID, []Endorsement{end}), "endorsement update unexpectedly persisted")
	unchanged, _ = m.trust.Get(peer.identity.NodeUUID)
	require.Len(t, unchanged.Endorsements, before, "failed trust write mutated live endorsements")
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
	require.NoError(t, m.persistMembersErr())
	require.NoError(t, m.beginDurableTeardown())
	require.NoError(t, m.clearAdmission())

	restarted := testManagerAt(t, dir, 15135)
	_, ok := restarted.trust.Get(peer.identity.NodeUUID)
	require.False(t, ok, "restart left a pin from interrupted teardown")
	require.Empty(t, restarted.snapshotNodes(), "restart left membership from interrupted teardown")
	staleID := "cluster-1"
	restarted.handleSetIdentity(&Message{Params: mustJSON(t, setIdentityParams{ClusterID: &staleID})})
	cid, _ := restarted.clusterIdentity()
	require.Equal(t, "", cid, "stale settings resurrected cluster")
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
	require.NoError(t, err)
	_, err = m.putRemovalProof(proof)
	require.NoError(t, err)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	m.clusterDir = blocker
	require.Error(t, m.replayRemovalProofs(), "removal replay ignored durable member cleanup failure")
}
