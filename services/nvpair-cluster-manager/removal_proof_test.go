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
)

func testManagerAt(t *testing.T, dir string, port int) *Manager {
	t.Helper()
	codec := NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), io.Discard})
	m, err := NewManager(codec, dir, port)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
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
	if err != nil {
		t.Fatalf("ensure admission: %v", err)
	}
	m.setClusterIdentity(clusterID, "Lab")
	return epoch
}

func proofRejectionBody(t *testing.T, p RemovalProof) []byte {
	t.Helper()
	b, err := json.Marshal(rosterRejection{
		Tombstones:    []Tombstone{p.Tombstone},
		RemovalProofs: []RemovalProof{p},
	})
	if err != nil {
		t.Fatalf("marshal rejection: %v", err)
	}
	return b
}

func cloneProof(t *testing.T, p RemovalProof) RemovalProof {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out RemovalProof
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdmissionEpochPersistsAndAdvancesOnReadmission(t *testing.T) {
	dir := t.TempDir()
	first := testManagerAt(t, dir, 15101)
	epoch1 := activateTestCluster(t, first, "cluster-1")

	restarted := testManagerAt(t, dir, 15101)
	if cid, epoch := restarted.currentAdmission(); cid != "cluster-1" || epoch != epoch1 {
		t.Fatalf("restart admission = (%q,%d), want (cluster-1,%d)", cid, epoch, epoch1)
	}
	if got, err := restarted.ensureAdmission("cluster-1"); err != nil || got != epoch1 {
		t.Fatalf("startup restore minted a new admission: got=%d err=%v", got, err)
	}

	restarted.teardownClusterLocal()
	epoch2 := activateTestCluster(t, restarted, "cluster-1")
	if epoch2 <= epoch1 {
		t.Fatalf("same-cluster readmission epoch = %d, want > %d", epoch2, epoch1)
	}
}

func TestRemovalProofTargetsExactAdmission(t *testing.T) {
	victim := newTestManagerPort(t, 15102)
	remover := newTestManagerPort(t, 15103)
	pinTrusted(t, victim, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)

	_, epoch1 := victim.currentAdmission()
	proof, err := remover.newRemovalProof(victim.identity.NodeUUID, epoch1)
	if err != nil {
		t.Fatal(err)
	}
	proof = remover.withLocalRelayEndorsement(proof)
	body := proofRejectionBody(t, proof)
	if !victim.rejectionProvesRemoval(body, remover.identity.NodeUUID) {
		t.Fatal("proof for the current admission was rejected")
	}

	victim.teardownClusterLocal()
	epoch2 := activateTestCluster(t, victim, "cluster-1")
	pinTrusted(t, victim, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	if epoch2 <= epoch1 {
		t.Fatalf("readmission epoch = %d, want > %d", epoch2, epoch1)
	}
	if victim.rejectionProvesRemoval(body, remover.identity.NodeUUID) {
		t.Fatal("old admission proof evicted a legitimate same-cluster readmission")
	}
}

func TestRemovalProofRelaysUnknownRemover(t *testing.T) {
	victim := newTestManagerPort(t, 15104)
	relay := newTestManagerPort(t, 15105)
	remover := newTestManagerPort(t, 15106)
	pinTrusted(t, victim, relay.identity.NodeUUID, string(relay.identity.CertPEM), relay.identity.CertFingerprint)
	pinTrusted(t, relay, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	pinTrusted(t, relay, victim.identity.NodeUUID, string(victim.identity.CertPEM), victim.identity.CertFingerprint)
	if _, ok := victim.trust.Get(remover.identity.NodeUUID); ok {
		t.Fatal("precondition: victim must never have pinned remover")
	}

	_, victimEpoch := victim.currentAdmission()
	proof, err := remover.newRemovalProof(victim.identity.NodeUUID, victimEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if !relay.verifyRemovalProof(proof, "cluster-1") {
		t.Fatal("relay did not verify directly-trusted remover")
	}
	if _, err := relay.putRemovalProof(proof); err != nil {
		t.Fatalf("relay persist: %v", err)
	}
	relayed, ok := relay.removalProofFor(victim.identity.NodeUUID)
	if !ok {
		t.Fatal("relay lost proof")
	}
	if !victim.rejectionProvesRemoval(proofRejectionBody(t, relayed), relay.identity.NodeUUID) {
		t.Fatal("victim could not verify unknown remover through trusted relay")
	}
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
	if !relay.verifyRemovalProof(proof, "cluster-1") {
		t.Fatal("relay rejected old but valid proof")
	}
	if _, err := relay.putRemovalProof(proof); err != nil {
		t.Fatal(err)
	}
	// Prove restart does not depend on the original remover remaining pinned.
	if err := relay.trust.Remove(remover.identity.NodeUUID); err != nil {
		t.Fatal(err)
	}

	restarted := testManagerAt(t, relayDir, 15107)
	persisted, ok := restarted.removalProofFor(victim.identity.NodeUUID)
	if !ok {
		t.Fatal(">24-hour proof disappeared across restart")
	}
	if persisted.Tombstone.RemovedAt != old {
		t.Fatalf("removedAt = %d, want %d", persisted.Tombstone.RemovedAt, old)
	}
	if !victim.rejectionProvesRemoval(proofRejectionBody(t, persisted), restarted.identity.NodeUUID) {
		t.Fatal("victim could not verify persisted proof after relay restart")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remover.putRemovalProof(proof); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after proof persistence but before the normal de-pin and
	// member deletion.
	restarted := testManagerAt(t, dir, 15117)
	if _, ok := restarted.trust.Get(target.identity.NodeUUID); ok {
		t.Fatal("restart did not replay proof against stale pin")
	}
	if _, ok := restarted.memberByNodeID(target.identity.NodeUUID); ok {
		t.Fatal("restart did not replay proof against stale member")
	}
	if _, ok := restarted.removalProofFor(target.identity.NodeUUID); !ok {
		t.Fatal("replay discarded proof before a newer admission superseded it")
	}
}

func TestLegacyTombstoneCannotProveSelfRemoval(t *testing.T) {
	victim := newTestManagerPort(t, 15119)
	peer := newTestManagerPort(t, 15120)
	pinTrusted(t, victim, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)
	legacy := signTombstone(peer.identity.Signer, peer.identity.NodeUUID,
		victim.identity.NodeUUID, "cluster-1", time.Now().UnixMilli())
	body, err := json.Marshal(rosterRejection{Tombstones: []Tombstone{legacy}})
	if err != nil {
		t.Fatal(err)
	}
	if victim.rejectionProvesRemoval(body, peer.identity.NodeUUID) {
		t.Fatal("legacy timestamp-only tombstone was accepted as self-removal proof")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.putRemovalProof(proof); err != nil {
		t.Fatal(err)
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
			if victim.rejectionProvesRemoval(proofRejectionBody(t, p), relay.identity.NodeUUID) {
				t.Fatal("tampered proof was accepted")
			}
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
	if err != nil {
		t.Fatal(err)
	}
	if !m.applyRemovalProofs([]RemovalProof{proof}, "cluster-1") {
		t.Fatal("authenticated target admission was not removed")
	}

	_, endorserEpoch := endorser.currentAdmission()
	end2 := signEndorsement(endorser.identity.Signer, endorser.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 2, endorserEpoch)
	entry2 := RosterEntry{
		NodeUUID: targetUUID, NodeID: "target", AdmissionEpoch: 2,
		CertPem: targetCert, CertFingerprint: targetFP, Endorsements: []Endorsement{end2},
	}
	if !m.applyMembers([]RosterEntry{entry2}, "cluster-1", endorser.identity.NodeUUID) {
		t.Fatal("newer admission was not accepted")
	}
	if _, ok := m.removalProofFor(targetUUID); ok {
		t.Fatal("older proof survived a durably accepted newer admission")
	}
	pin, ok := m.trust.Get(targetUUID)
	if !ok || pin.AdmissionEpoch != 2 {
		t.Fatalf("target pin after readmission = %+v", pin)
	}

	end1 := signEndorsement(endorser.identity.Signer, endorser.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 1, endorserEpoch)
	stale := entry2
	stale.AdmissionEpoch = 1
	stale.Endorsements = []Endorsement{end1}
	m.applyMembers([]RosterEntry{stale}, "cluster-1", endorser.identity.NodeUUID)
	pin, _ = m.trust.Get(targetUUID)
	if pin.AdmissionEpoch != 2 {
		t.Fatalf("stale gossip downgraded admission to %d", pin.AdmissionEpoch)
	}
}

func TestUnboundHighEpochProofCannotPoisonReadmission(t *testing.T) {
	m := newTestManagerPort(t, 15121)
	remover := newTestManagerPort(t, 15122)
	pinTrusted(t, m, remover.identity.NodeUUID, string(remover.identity.CertPEM), remover.identity.CertFingerprint)
	targetUUID, targetCert, targetFP, _ := makeNode(t, "target")

	proof, err := remover.newRemovalProof(targetUUID, ^uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	m.applyRemovalProofs([]RemovalProof{proof}, "cluster-1")
	if _, ok := m.removalProofFor(targetUUID); ok {
		t.Fatal("proof for an unauthenticated target admission was persisted")
	}

	_, removerEpoch := remover.currentAdmission()
	end := signEndorsement(remover.identity.Signer, remover.identity.NodeUUID,
		targetUUID, targetFP, "cluster-1", time.Now().UnixMilli(), 1, removerEpoch)
	entry := RosterEntry{
		NodeUUID: targetUUID, NodeID: "target", AdmissionEpoch: 1,
		CertPem: targetCert, CertFingerprint: targetFP, Endorsements: []Endorsement{end},
	}
	if !m.applyMembers([]RosterEntry{entry}, "cluster-1", remover.identity.NodeUUID) {
		t.Fatal("invented high epoch blocked a legitimate admission")
	}
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
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.clusterDir = blocker
	nodeUUID := peer.identity.NodeUUID
	m.handleNodesRemove(&Message{Params: mustJSON(t, removeParams{NodeUUID: &nodeUUID})})
	if _, ok := m.trust.Get(peer.identity.NodeUUID); !ok {
		t.Fatal("pin was removed despite proof persistence failure")
	}
	if _, ok := m.memberByNodeID(peer.identity.NodeUUID); !ok {
		t.Fatal("member was removed despite proof persistence failure")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
