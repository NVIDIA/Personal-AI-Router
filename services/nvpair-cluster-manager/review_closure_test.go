// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInboundInitialCannotPublishAfterTeardown(t *testing.T) {
	m := newTestManagerPort(t, 15201)
	peer := newTestManagerPort(t, 15202)
	inviteID := "inbound-after-teardown"
	sess := &pairingSession{
		inviteID: inviteID,
		role:     roleJoiner,
		peerPairing: &PairingInfo{
			NodeUUID:  peer.identity.NodeUUID,
			NodeID:    peer.identity.NodeID,
			Name:      peer.identity.Name,
			ClusterID: "cluster-1",
		},
	}
	m.putSession(sess)
	inv := &Invite{
		InviteID:     inviteID,
		FromNodeUUID: peer.identity.NodeUUID,
		ClusterID:    "cluster-1",
		State:        inviteStatePending,
		CreatedAt:    time.Now().UnixMilli(),
	}

	if err := m.teardownClusterLocal(); err != nil {
		t.Fatal(err)
	}
	m.onJoinerInitialComplete(inv, sess)

	if _, ok := m.getInvite(inviteID); ok {
		t.Fatal("stale inbound Initial completion republished an invite after teardown")
	}
}

func TestFailedTeardownBlocksTrustAndReadmission(t *testing.T) {
	m := newTestManagerPort(t, 15203)
	peer := newPinFixture(t, "peer")
	pinTrusted(t, m, peer.uuid, peer.cert, peer.fp)
	m.upsertMember(&ClusterNode{
		NodeUUID: peer.uuid, ID: "peer", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})

	pinPath := m.trust.pinPath(peer.uuid)
	if err := os.Remove(pinPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pinPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pinPath, "block-delete"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.teardownClusterLocal(); err == nil {
		t.Fatal("teardown unexpectedly completed despite the pin-store failure")
	}

	block, _ := pem.Decode([]byte(peer.cert))
	if block == nil {
		t.Fatal("decode peer certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, rosterPath, nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if _, ok := m.verifyClientPin(req); ok {
		t.Error("failed teardown left the old peer authorized for mTLS")
	}
	if _, _, err := m.foundCluster("new cluster"); err == nil {
		t.Error("new admission committed while teardown.pending still existed")
	}
}

func TestRosterPinWithoutPersistedMemberIsPruned(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15204)
	activateTestCluster(t, m, "cluster-1")
	peer := newPinFixture(t, "roster-peer")
	pinTrusted(t, m, peer.uuid, peer.cert, peer.fp)

	restarted := testManagerAt(t, dir, 15205)
	if _, ok := restarted.trust.Get(peer.uuid); ok {
		t.Fatal("restart retained a roster pin whose matching member was never persisted")
	}
}

func TestMalformedInitialDoesNotLeakSession(t *testing.T) {
	m := testManagerAt(t, t.TempDir(), 15206)
	inviteID := "malformed-initial"
	m.admissionMu.Lock()
	before := m.admissionCounter
	m.admissionMu.Unlock()
	rr := httptest.NewRecorder()
	m.handlePairingInitial(rr, &pairingEnvelope{InviteID: inviteID, Phase: "initial"}, []byte("not-eap-noob"), "127.0.0.1")
	if _, ok := m.getSession(inviteID); ok {
		t.Fatal("malformed unauthenticated Initial request retained a live pairing session")
	}
	m.admissionMu.Lock()
	after := m.admissionCounter
	m.admissionMu.Unlock()
	if after != before {
		t.Fatalf("malformed request consumed durable admission epoch: before=%d after=%d", before, after)
	}
}

func TestPreInviteSessionsExpireAndAreBounded(t *testing.T) {
	m := testManagerAt(t, t.TempDir(), 15210)
	expired := &pairingSession{
		inviteID: "pre-invite-expired", role: roleJoiner,
		createdAt: time.Now().Add(-time.Hour).UnixMilli(),
	}
	m.putSession(expired)
	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })
	m.expirePendingInvites(time.Now())
	if _, ok := m.getSession(expired.inviteID); ok {
		t.Fatal("pre-invite session survived its TTL")
	}

	for i := 0; i < maxPreInviteSessions; i++ {
		m.putSession(&pairingSession{
			inviteID: "bounded-" + strconv.Itoa(i), role: roleJoiner,
			createdAt: time.Now().UnixMilli(),
		})
	}
	rr := httptest.NewRecorder()
	m.handlePairingInitial(rr, &pairingEnvelope{InviteID: "over-limit", Phase: "initial"}, []byte(`{"Type":1}`), "127.0.0.1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want %d", rr.Code, http.StatusTooManyRequests)
	}
}

func TestDirectPairingRejectsRemovedAdmission(t *testing.T) {
	m := newTestManagerPort(t, 15207)
	peer := newPinFixture(t, "readmission")
	pinTrusted(t, m, peer.uuid, peer.cert, peer.fp)
	m.upsertMember(&ClusterNode{
		NodeUUID: peer.uuid, ID: "readmission", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	proof, err := m.newRemovalProof(peer.uuid, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.putRemovalProof(proof); err != nil {
		t.Fatal(err)
	}
	if m.removalProofBlocksAdmission(peer.uuid, "other-cluster", 1) {
		t.Fatal("cluster-scoped removal proof blocked admission to another cluster")
	}
	if err := m.trust.Remove(peer.uuid); err != nil {
		t.Fatal(err)
	}
	m.removeMemberByUUID(peer.uuid)

	pi, cert := pairingInfoFor(t, peer)
	sess := putInviterSession(t, m, "readmission", "cluster-1")
	committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
	if err == nil || committed {
		t.Fatalf("removed admission commit = %v, err = %v; want rejection", committed, err)
	}
}

func TestRestartRollsBackProvisionalAdmission(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15211)
	epoch, err := m.reserveAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	peer := newPinFixture(t, "provisional-peer")
	pinTrusted(t, m, peer.uuid, peer.cert, peer.fp)
	m.upsertMember(&ClusterNode{
		NodeUUID: peer.uuid, ID: "provisional-peer", ClusterID: "cluster-new",
		AdmissionEpoch: 1, State: stateMember,
	})
	m.addSelfMemberForAdmission("cluster-new", epoch)
	if err := m.persistMembersErr(); err != nil {
		t.Fatal(err)
	}

	restarted := testManagerAt(t, dir, 15212)
	if cid, _ := restarted.currentAdmission(); cid != "" {
		t.Fatalf("provisional admission became active after restart: %q", cid)
	}
	if len(restarted.trust.List()) != 0 || len(restarted.snapshotNodes()) != 0 {
		t.Fatal("restart retained provisional members or pins")
	}
}

func TestBareRejectionRemovesDepartedPeerButKeepsCluster(t *testing.T) {
	m := newTestManagerPort(t, 15208)
	startPeerStub(t, m, "departed", http.StatusForbidden, false)

	m.reconcilePeersAndMaybeSelfRemove()

	if cid, _ := m.clusterIdentity(); cid != "cluster-1" {
		t.Fatalf("surviving cluster id = %q, want cluster-1", cid)
	}
	if nodes := m.snapshotNodes(); len(nodes) != 0 {
		t.Fatalf("departed peer remained in roster: %+v", nodes)
	}
}

func TestRemovalIgnoresUnrelatedCompositionChange(t *testing.T) {
	m := newTestManagerPort(t, 15209)
	target := newPinFixture(t, "target")
	pinTrusted(t, m, target.uuid, target.cert, target.fp)
	m.upsertMember(&ClusterNode{
		NodeUUID: target.uuid, ID: "target", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	targetUUID := target.uuid
	m.testRemovalPrepared = make(chan struct{})
	m.testRemovalContinue = make(chan struct{})
	done := make(chan struct{})
	go func() {
		m.handleNodesRemove(&Message{Params: mustJSON(t, removeParams{NodeUUID: &targetUUID})})
		close(done)
	}()
	<-m.testRemovalPrepared
	m.upsertMember(&ClusterNode{
		NodeUUID: "unrelated", ID: "unrelated", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	close(m.testRemovalContinue)
	<-done
	if _, ok := m.trust.Get(target.uuid); ok {
		t.Fatal("target pin survived because an unrelated member changed")
	}
	if _, ok := m.memberByNodeID(target.uuid); ok {
		t.Fatal("target member survived because an unrelated member changed")
	}
}

func TestRosterFixpointContinuesAfterAdmissionUpgrade(t *testing.T) {
	m := newTestManagerPort(t, 15213)
	aUUID, aCert, aFP, aPriv := makeNode(t, "node-a")
	pinTrusted(t, m, aUUID, aCert, aFP)
	m.upsertMember(&ClusterNode{
		NodeUUID: aUUID, ID: "node-a", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	cUUID, cCert, cFP, _ := makeNode(t, "node-c")
	_, selfEpoch := m.currentAdmission()
	endA2 := signEndorsement(m.identity.Signer, m.identity.NodeUUID,
		aUUID, aFP, "cluster-1", time.Now().UnixMilli(), 2, selfEpoch)
	endC := signEndorsement(aPriv, aUUID,
		cUUID, cFP, "cluster-1", time.Now().UnixMilli(), 1, 2)

	entries := []RosterEntry{
		{
			NodeUUID: cUUID, NodeID: "node-c", AdmissionEpoch: 1,
			CertPem: cCert, CertFingerprint: cFP, Endorsements: []Endorsement{endC},
		},
		{
			NodeUUID: aUUID, NodeID: "node-a", AdmissionEpoch: 2,
			CertPem: aCert, CertFingerprint: aFP, Endorsements: []Endorsement{endA2},
		},
	}
	if !m.applyMembers(entries, "cluster-1", aUUID) {
		t.Fatal("roster upgrade reported no change")
	}
	if _, ok := m.trust.Get(cUUID); !ok {
		t.Fatal("fixpoint stopped after upgrading the endorser admission")
	}
}

func TestJoinerFailureAfterInviterCommitRollsBackPairing(t *testing.T) {
	m := newTestManagerPort(t, 15214)
	peer := newPinFixture(t, "joiner")
	pi, cert := pairingInfoFor(t, peer)
	inviteID := "late-joiner-failure"
	m.putInvite(&Invite{
		InviteID: inviteID, ClusterID: "cluster-1",
		State: inviteStatePending, CreatedAt: time.Now().UnixMilli(),
	})
	sess := putInviterSession(t, m, inviteID, "cluster-1")
	if err := m.finalizePairing(inviteID, sess, pi, cert); err != nil {
		t.Fatal(err)
	}

	m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "fail",
	})
	if _, ok := m.trust.Get(peer.uuid); !ok {
		t.Fatal("unauthenticated post-success failure rolled the inviter back")
	}
	m.handlePairingFailedFrom(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "fail",
	}, peer.uuid)

	if _, ok := m.trust.Get(peer.uuid); ok {
		t.Fatal("inviter retained peer after joiner reported post-success commit failure")
	}
	if _, ok := m.memberByNodeID(peer.uuid); ok {
		t.Fatal("inviter retained member after joiner reported post-success commit failure")
	}
	invite, ok := m.getInvite(inviteID)
	if !ok || invite.State != inviteStateFailed {
		t.Fatalf("invite state = %+v, want failed", invite)
	}
}

func TestJoinerAckFinalizesInviterSession(t *testing.T) {
	m := newTestManagerPort(t, 15215)
	peer := newPinFixture(t, "joiner")
	pi, cert := pairingInfoFor(t, peer)
	inviteID := "joiner-ack"
	m.putInvite(&Invite{
		InviteID: inviteID, ClusterID: "cluster-1",
		State: inviteStatePending, CreatedAt: time.Now().UnixMilli(),
	})
	sess := putInviterSession(t, m, inviteID, "cluster-1")
	if err := m.finalizePairing(inviteID, sess, pi, cert); err != nil {
		t.Fatal(err)
	}

	m.handlePairingAck(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "ack",
	}, "wrong-peer")
	if _, ok := m.getSession(inviteID); !ok {
		t.Fatal("unauthenticated acknowledgment finalized the inviter session")
	}
	m.handlePairingAck(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "ack",
	}, peer.uuid)

	if _, ok := m.getSession(inviteID); ok {
		t.Fatal("acknowledged inviter session was not deleted")
	}
	if _, ok := m.trust.Get(peer.uuid); !ok {
		t.Fatal("acknowledgment removed the committed peer")
	}
	invite, ok := m.getInvite(inviteID)
	if !ok || invite.State != inviteStatePaired {
		t.Fatalf("invite state = %+v, want paired", invite)
	}
}

func TestFinalCompletionRetriesLostResponse(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		respondPairing(w, []byte("eap-success"))
	}))
	defer server.Close()

	blob, err := postCompletionBlob(server.Client(), server.Listener.Addr().String(),
		"retry-final", []byte(`{"Type":6}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "eap-success" || attempts.Load() != 2 {
		t.Fatalf("retry result = %q after %d attempts", blob, attempts.Load())
	}
}

func TestDuplicateFinalCompletionReturnsCachedSuccess(t *testing.T) {
	m := newTestManagerPort(t, 15216)
	inviteID := "cached-final"
	m.putInvite(&Invite{InviteID: inviteID, State: inviteStatePaired})
	m.putSession(&pairingSession{
		inviteID: inviteID, role: roleInviter, awaitingAck: true,
		completionResponse: []byte("cached-eap-success"),
	})
	rec := httptest.NewRecorder()
	m.handlePairingCompletion(rec, &pairingEnvelope{InviteID: inviteID}, []byte(`{"Type":6}`), "127.0.0.1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Y2FjaGVkLWVhcC1zdWNjZXNz") {
		t.Fatalf("cached completion response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAckTimeoutKeepsCommittedPairAndClearsInviteProvenance(t *testing.T) {
	m := newTestManagerPort(t, 15217)
	peer := newPinFixture(t, "joiner")
	pi, cert := pairingInfoFor(t, peer)
	inviteID := "ack-timeout"
	m.putInvite(&Invite{
		InviteID: inviteID, ClusterID: "cluster-1",
		State: inviteStatePending, CreatedAt: time.Now().UnixMilli(),
	})
	sess := putInviterSession(t, m, inviteID, "cluster-1")
	if err := m.finalizePairing(inviteID, sess, pi, cert); err != nil {
		t.Fatal(err)
	}
	m.setInviteCreatedCluster(true)
	sess.createdAt = time.Now().Add(-time.Hour).UnixMilli()
	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })
	m.expirePendingInvites(time.Now())

	if _, ok := m.getSession(inviteID); ok {
		t.Fatal("unacknowledged retry session survived its TTL")
	}
	if _, ok := m.trust.Get(peer.uuid); !ok {
		t.Fatal("ack timeout removed a fully committed peer")
	}
	if m.isInviteCreatedCluster() {
		t.Fatal("ack timeout left a real paired cluster marked invite-created")
	}
}
