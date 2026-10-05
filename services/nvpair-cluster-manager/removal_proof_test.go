// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testManagerAt(t *testing.T, dir string, port int) *Manager {
	t.Helper()
	codec := NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), io.Discard})
	m, err := NewManager(codec, dir, port)
	require.NoError(t, err, "new manager")
	return m
}

func activateTestCluster(t *testing.T, m *Manager, clusterID string) uint64 {
	t.Helper()
	var epoch uint64
	var err error
	if m.admissionWasRetired() {
		epoch, err = m.reserveAdmissionEpoch()
		if err == nil {
			err = m.activateAdmission(clusterID, epoch)
		}
	} else {
		epoch, err = m.ensureAdmission(clusterID)
	}
	require.NoError(t, err, "ensure admission")
	m.setClusterIdentity(clusterID, "Lab")
	return epoch
}

func proofRejectionBody(t *testing.T, p RemovalProof) []byte {
	t.Helper()
	b, err := json.Marshal(rosterRejection{
		Tombstones:    []Tombstone{p.Tombstone},
		RemovalProofs: []RemovalProof{p},
	})
	require.NoError(t, err, "marshal rejection")
	return b
}

func cloneProof(t *testing.T, p RemovalProof) RemovalProof {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var out RemovalProof
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func TestAdmissionEpochPersistsAndAdvancesOnReadmission(t *testing.T) {
	dir := t.TempDir()
	first := testManagerAt(t, dir, 15101)
	epoch1 := activateTestCluster(t, first, "cluster-1")

	restarted := testManagerAt(t, dir, 15101)
	{
		cid, epoch := restarted.currentAdmission()
		require.Equal(t, "cluster-1", cid, "restart admission (%v, %v, %v)", cid, epoch, epoch1)
		require.True(t, epoch == epoch1, "restart admission (%v, %v, %v)", cid, epoch, epoch1)
	}
	{
		got, err := restarted.ensureAdmission("cluster-1")
		require.NoError(t, err, "startup restore minted a new admission: (%v, %v)", got, err)
		require.True(t, got == epoch1, "startup restore minted a new admission: (%v, %v)", got, err)
	}

	restarted.teardownClusterLocal()
	epoch2 := activateTestCluster(t, restarted, "cluster-1")
	require.Greater(t, epoch2, epoch1, "same-cluster readmission epoch")
}

func TestRemovalProofTargetsExactAdmission(t *testing.T) {
	victim := newTestManagerPort(t, 15102)
	remover := newTestManagerPort(t, 15103)
	pinTrusted(t, victim, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)

	_, epoch1 := victim.currentAdmission()
	proof, err := remover.newRemovalProof(victim.identity.NodeUUID, epoch1)
	require.NoError(t, err)
	proof = remover.withLocalRelayEndorsement(proof)
	body := proofRejectionBody(t, proof)
	require.True(t, victim.rejectionProvesRemoval(body, remover.identity.NodeUUID), "proof for the current admission was rejected")

	victim.teardownClusterLocal()
	epoch2 := activateTestCluster(t, victim, "cluster-1")
	pinTrusted(t, victim, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	require.Greater(t, epoch2, epoch1, "readmission epoch")
	require.False(t, victim.rejectionProvesRemoval(body, remover.identity.NodeUUID), "old admission proof evicted a legitimate same-cluster readmission")
}

func TestRemovalProofRelaysUnknownRemover(t *testing.T) {
	victim := newTestManagerPort(t, 15104)
	relay := newTestManagerPort(t, 15105)
	remover := newTestManagerPort(t, 15106)
	pinTrusted(t, victim, relay.identity.NodeUUID, string(relay.identity.CertPEM), relay.identity.CertFingerprint)
	pinTrusted(t, relay, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	pinTrusted(t, relay, victim.identity.NodeUUID, string(victim.identity.CertPEM), victim.identity.CertFingerprint)
	{
		_, ok := victim.trust.Get(remover.identity.NodeUUID)
		require.False(t, ok, "precondition: victim must never have pinned remover")
	}

	_, victimEpoch := victim.currentAdmission()
	proof, err := remover.newRemovalProof(victim.identity.NodeUUID, victimEpoch)
	require.NoError(t, err)
	require.True(t, relay.verifyRemovalProof(proof, "cluster-1"), "relay did not verify directly-trusted remover")
	{
		_, err := relay.putRemovalProof(proof)
		require.NoError(t, err, "relay persist")
	}
	relayed, ok := relay.removalProofFor(victim.identity.NodeUUID)
	require.True(t, ok, "relay lost proof")
	require.True(t, victim.rejectionProvesRemoval(proofRejectionBody(t, relayed), relay.identity.NodeUUID), "victim could not verify unknown remover through trusted relay")
}

func TestRemovalProofSurvivesRestartBeyondTwentyFourHours(t *testing.T) {
	relayDir := t.TempDir()
	relay := testManagerAt(t, relayDir, 15107)
	activateTestCluster(t, relay, "cluster-1")
	victim := newTestManagerPort(t, 15108)
	remover := newTestManagerPort(t, 15109)
	pinTrusted(t, victim, relay.identity.NodeUUID, string(relay.identity.CertPEM), relay.identity.CertFingerprint)
	pinTrusted(t, relay, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	pinTrusted(t, relay, victim.identity.NodeUUID, string(victim.identity.CertPEM), victim.identity.CertFingerprint)

	_, victimEpoch := victim.currentAdmission()
	_, removerEpoch := remover.currentAdmission()
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	tomb := signTombstone(remover.identity.Signer, remover.identity.NodeUUID,
		victim.identity.NodeUUID, "cluster-1", old, victimEpoch, removerEpoch)
	proof := RemovalProof{
		Tombstone:         tomb,
		SignerCertPem:     string(remover.identity.CertPEM),
		SignerFingerprint: remover.identity.CertFingerprint,
	}
	require.True(t, relay.verifyRemovalProof(proof, "cluster-1"), "relay rejected old but valid proof")
	{
		_, err := relay.putRemovalProof(proof)
		require.NoError(t, err)
	}
	// Prove restart does not depend on the original remover remaining pinned.
	require.NoError(t, relay.trust.Remove(remover.identity.NodeUUID))

	restarted := testManagerAt(t, relayDir, 15107)
	persisted, ok := restarted.removalProofFor(victim.identity.NodeUUID)
	require.True(t, ok, ">24-hour proof disappeared across restart")
	require.True(t, persisted.Tombstone.RemovedAt == old, "removedAt (%v)", old)
	require.True(t, victim.rejectionProvesRemoval(proofRejectionBody(t, persisted), restarted.identity.NodeUUID), "victim could not verify persisted proof after relay restart")
}

func TestRemovalProofReplayFinishesInterruptedRemoval(t *testing.T) {
	dir := t.TempDir()
	remover := testManagerAt(t, dir, 15117)
	activateTestCluster(t, remover, "cluster-1")
	target := newTestManagerPort(t, 15118)
	pinTrusted(t, remover, target.identity.NodeUUID, string(target.identity.CertPEM), target.identity.CertFingerprint)
	remover.upsertMember(&ClusterNode{
		NodeUUID: target.identity.NodeUUID, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	remover.persistMembers()

	proof, err := remover.newRemovalProof(target.identity.NodeUUID, 1)
	require.NoError(t, err)
	{
		_, err := remover.putRemovalProof(proof)
		require.NoError(t, err)
	}
	// Simulate a crash after proof persistence but before the normal de-pin and
	// member deletion.
	restarted := testManagerAt(t, dir, 15117)
	{
		_, ok := restarted.trust.Get(target.identity.NodeUUID)
		require.False(t, ok, "restart did not replay proof against stale pin")
	}
	{
		_, ok := restarted.memberByNodeID(target.identity.NodeUUID)
		require.False(t, ok, "restart did not replay proof against stale member")
	}
	{
		_, ok := restarted.removalProofFor(target.identity.NodeUUID)
		require.True(t, ok, "replay discarded proof before a newer admission superseded it")
	}
}

func TestLegacyTombstoneCannotProveSelfRemoval(t *testing.T) {
	victim := newTestManagerPort(t, 15119)
	peer := newTestManagerPort(t, 15120)
	pinTrusted(t, victim, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)
	legacy := signTombstone(peer.identity.Signer, peer.identity.NodeUUID,
		victim.identity.NodeUUID, "cluster-1", time.Now().UnixMilli())
	body, err := json.Marshal(rosterRejection{Tombstones: []Tombstone{legacy}})
	require.NoError(t, err)
	require.False(t, victim.rejectionProvesRemoval(body, peer.identity.NodeUUID), "legacy timestamp-only tombstone was accepted as self-removal proof")
}

func TestRemovalProofTamperingFailsClosed(t *testing.T) {
	victim := newTestManagerPort(t, 15110)
	relay := newTestManagerPort(t, 15111)
	remover := newTestManagerPort(t, 15112)
	pinTrusted(t, victim, relay.identity.NodeUUID, string(relay.identity.CertPEM), relay.identity.CertFingerprint)
	pinTrusted(t, relay, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	pinTrusted(t, relay, victim.identity.NodeUUID, string(victim.identity.CertPEM), victim.identity.CertFingerprint)
	_, victimEpoch := victim.currentAdmission()
	proof, err := remover.newRemovalProof(victim.identity.NodeUUID, victimEpoch)
	require.NoError(t, err)
	{
		_, err := relay.putRemovalProof(proof)
		require.NoError(t, err)
	}
	base, _ := relay.removalProofFor(victim.identity.NodeUUID)

	cases := map[string]func(*RemovalProof){
		"victim uuid":       func(p *RemovalProof) { p.Tombstone.NodeUUID = "other" },
		"cluster":           func(p *RemovalProof) { p.Tombstone.ClusterID = "other" },
		"victim admission":  func(p *RemovalProof) { p.Tombstone.AdmissionEpoch++ },
		"remover admission": func(p *RemovalProof) { p.Tombstone.ByAdmissionEpoch++ },
		"signer cert": func(p *RemovalProof) {
			p.SignerCertPem = string(victim.identity.CertPEM)
		},
		"fingerprint":       func(p *RemovalProof) { p.SignerFingerprint = "sha256:bad" },
		"tombstone sig":     func(p *RemovalProof) { p.Tombstone.SigV2 = "bad" },
		"relay endorsement": func(p *RemovalProof) { p.Endorsements[0].SigV2 = "bad" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := cloneProof(t, base)
			mutate(&p)
			require.False(t, victim.rejectionProvesRemoval(proofRejectionBody(t, p), relay.identity.NodeUUID), "tampered proof was accepted")
		})
	}
}

func TestNewerAdmissionSupersedesProofAndStaleGossip(t *testing.T) {
	m := newTestManagerPort(t, 15113)
	endorser := newTestManagerPort(t, 15114)
	pinTrusted(t, m, endorser.identity.NodeUUID, string(endorser.identity.CertPEM), endorser.identity.CertFingerprint)

	targetUUID, targetCert, targetFP, _ := makeNode(t, "target")
	pinTrusted(t, m, targetUUID, targetCert, targetFP)
	m.upsertMember(&ClusterNode{
		NodeUUID: targetUUID, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	proof, err := endorser.newRemovalProof(targetUUID, 1)
	require.NoError(t, err)
	require.True(t, m.applyRemovalProofs([]RemovalProof{proof}, "cluster-1"), "authenticated target admission was not removed")

	_, endorserEpoch := endorser.currentAdmission()
	end2 := signEndorsement(endorser.identity.Signer, endorser.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 2, endorserEpoch)
	entry2 := RosterEntry{
		NodeUUID: targetUUID, NodeID: "target", AdmissionEpoch: 2,
		CertPem: targetCert, CertFingerprint: targetFP, Endorsements: []Endorsement{end2},
	}
	require.True(t, m.applyMembers([]RosterEntry{entry2}, "cluster-1", endorser.identity.NodeUUID), "newer admission was not accepted")
	{
		_, ok := m.removalProofFor(targetUUID)
		require.False(t, ok, "older proof survived a durably accepted newer admission")
	}
	pin, ok := m.trust.Get(targetUUID)
	require.True(t, ok, "target pin after readmission (%v)", pin)
	require.Equal(t, uint64(2), pin.AdmissionEpoch, "target pin after readmission (%v)", pin)

	end1 := signEndorsement(endorser.identity.Signer, endorser.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 1, endorserEpoch)
	stale := entry2
	stale.AdmissionEpoch = 1
	stale.Endorsements = []Endorsement{end1}
	m.applyMembers([]RosterEntry{stale}, "cluster-1", endorser.identity.NodeUUID)
	pin, _ = m.trust.Get(targetUUID)
	require.Equal(t, uint64(2), pin.AdmissionEpoch, "stale gossip downgraded admission to")
}

func TestUnboundHighEpochProofCannotPoisonReadmission(t *testing.T) {
	m := newTestManagerPort(t, 15121)
	remover := newTestManagerPort(t, 15122)
	pinTrusted(t, m, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	targetUUID, targetCert, targetFP, _ := makeNode(t, "target")

	proof, err := remover.newRemovalProof(targetUUID, ^uint64(0))
	require.NoError(t, err)
	m.applyRemovalProofs([]RemovalProof{proof}, "cluster-1")
	{
		_, ok := m.removalProofFor(targetUUID)
		require.False(t, ok, "proof for an unauthenticated target admission was persisted")
	}

	_, removerEpoch := remover.currentAdmission()
	end := signEndorsement(remover.identity.Signer, remover.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 1, removerEpoch)
	entry := RosterEntry{
		NodeUUID: targetUUID, NodeID: "target", AdmissionEpoch: 1,
		CertPem: targetCert, CertFingerprint: targetFP, Endorsements: []Endorsement{end},
	}
	require.True(t, m.applyMembers([]RosterEntry{entry}, "cluster-1", remover.identity.NodeUUID), "invented high epoch blocked a legitimate admission")
}

func TestRemovalPersistenceFailureLeavesMemberIntact(t *testing.T) {
	m := newTestManagerPort(t, 15115)
	peer := newTestManagerPort(t, 15116)
	pinTrusted(t, m, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)
	m.upsertMember(&ClusterNode{
		NodeUUID: peer.identity.NodeUUID, ID: "peer", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	m.clusterDir = blocker
	nodeUUID := peer.identity.NodeUUID
	m.handleNodesRemove(&Message{Params: mustJSON(t, removeParams{NodeUUID: &nodeUUID})})
	{
		_, ok := m.trust.Get(peer.identity.NodeUUID)
		require.True(t, ok, "pin was removed despite proof persistence failure")
	}
	{
		_, ok := m.memberByNodeID(peer.identity.NodeUUID)
		require.True(t, ok, "member was removed despite proof persistence failure")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
