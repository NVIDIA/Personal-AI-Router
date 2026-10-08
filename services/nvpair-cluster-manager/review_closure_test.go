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

	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	require.NoError(t, m.teardownClusterLocal())
	m.onJoinerInitialComplete(inv, sess)

	_, ok := m.getInvite(inviteID)
	require.False(t, ok, "stale inbound Initial completion republished an invite after teardown")
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
	require.NoError(t, os.Remove(pinPath))
	require.NoError(t, os.Mkdir(pinPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(pinPath, "block-delete"), []byte("x"), 0o600))
	require.Error(t, m.teardownClusterLocal(), "teardown unexpectedly completed despite the pin-store failure")

	block, _ := pem.Decode([]byte(peer.cert))
	require.NotNil(t, block, "decode peer certificate")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, rosterPath, nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	_, ok := m.verifyClientPin(req)
	assert.False(t, ok, "failed teardown left the old peer authorized for mTLS")
	_, _, err = m.foundCluster("new cluster")
	assert.Error(t, err, "new admission committed while teardown.pending still existed")
}

func TestRosterPinWithoutPersistedMemberIsPruned(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15204)
	activateTestCluster(t, m, "cluster-1")
	peer := newPinFixture(t, "roster-peer")
	pinTrusted(t, m, peer.uuid, peer.cert, peer.fp)

	restarted := testManagerAt(t, dir, 15205)
	_, ok := restarted.trust.Get(peer.uuid)
	require.False(t, ok, "restart retained a roster pin whose matching member was never persisted")
}

func TestMalformedInitialDoesNotLeakSession(t *testing.T) {
	m := testManagerAt(t, t.TempDir(), 15206)
	inviteID := "malformed-initial"
	m.admissionMu.Lock()
	before := m.admissionCounter
	m.admissionMu.Unlock()
	rr := httptest.NewRecorder()
	m.handlePairingInitial(rr, &pairingEnvelope{InviteID: inviteID, Phase: "initial"}, []byte("not-eap-noob"), "127.0.0.1")
	_, ok := m.getSession(inviteID)
	require.False(t, ok, "malformed unauthenticated Initial request retained a live pairing session")
	m.admissionMu.Lock()
	after := m.admissionCounter
	m.admissionMu.Unlock()
	require.Equal(t, before, after, "malformed request consumed durable admission epoch: before")
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
	_, ok := m.getSession(expired.inviteID)
	require.False(t, ok, "pre-invite session survived its TTL")

	for i := 0; i < maxPreInviteSessions; i++ {
		m.putSession(&pairingSession{
			inviteID: "bounded-" + strconv.Itoa(i), role: roleJoiner,
			createdAt: time.Now().UnixMilli(),
		})
	}
	rr := httptest.NewRecorder()
	m.handlePairingInitial(rr, &pairingEnvelope{InviteID: "over-limit", Phase: "initial"}, []byte(`{"Type":1}`), "127.0.0.1")
	require.Equal(t, http.StatusTooManyRequests, rr.Code, "over-limit status")
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
	require.NoError(t, err)
	_, err = m.putRemovalProof(proof)
	require.NoError(t, err)
	require.False(t, m.removalProofBlocksAdmission(peer.uuid, "other-cluster", 1), "cluster-scoped removal proof blocked admission to another cluster")
	require.NoError(t, m.trust.Remove(peer.uuid))
	m.removeMemberByUUID(peer.uuid)

	pi, cert := pairingInfoFor(t, peer)
	sess := putInviterSession(t, m, "readmission", "cluster-1")
	committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
	require.Error(t, err, "removed admission commit (%v, %v)", committed, err)
	require.False(t, committed, "removed admission commit (%v, %v)", committed, err)
}

func TestRestartRollsBackProvisionalAdmission(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15211)
	epoch, err := m.reserveAdmissionEpoch()
	require.NoError(t, err)
	peer := newPinFixture(t, "provisional-peer")
	pinTrusted(t, m, peer.uuid, peer.cert, peer.fp)
	m.upsertMember(&ClusterNode{
		NodeUUID: peer.uuid, ID: "provisional-peer", ClusterID: "cluster-new",
		AdmissionEpoch: 1, State: stateMember,
	})
	m.addSelfMemberForAdmission("cluster-new", epoch)
	require.NoError(t, m.persistMembersErr())

	restarted := testManagerAt(t, dir, 15212)
	cid, _ := restarted.currentAdmission()
	require.Equal(t, "", cid, "provisional admission became active after restart")
	require.Empty(t, restarted.trust.List(), "restart retained provisional members or pins")
	require.Empty(t, restarted.snapshotNodes(), "restart retained provisional members or pins")
}

func TestBareRejectionRemovesDepartedPeerButKeepsCluster(t *testing.T) {
	m := newTestManagerPort(t, 15208)
	startPeerStub(t, m, "departed", http.StatusForbidden, false)

	m.reconcilePeersAndMaybeSelfRemove()

	cid, _ := m.clusterIdentity()
	require.Equal(t, "cluster-1", cid, "surviving cluster id")
	require.Empty(t, m.snapshotNodes(), "departed peer remained in roster")
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
	params := mustJSON(t, removeParams{NodeUUID: &targetUUID})
	go func() {
		m.handleNodesRemove(&Message{Params: params})
		close(done)
	}()
	<-m.testRemovalPrepared
	m.upsertMember(&ClusterNode{
		NodeUUID: "unrelated", ID: "unrelated", ClusterID: "cluster-1",
		AdmissionEpoch: 1, State: stateMember,
	})
	close(m.testRemovalContinue)
	<-done
	_, ok := m.trust.Get(target.uuid)
	require.False(t, ok, "target pin survived because an unrelated member changed")
	_, ok = m.memberByNodeID(target.uuid)
	require.False(t, ok, "target member survived because an unrelated member changed")
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
	require.True(t, m.applyMembers(entries, "cluster-1", aUUID), "roster upgrade reported no change")
	_, ok := m.trust.Get(cUUID)
	require.True(t, ok, "fixpoint stopped after upgrading the endorser admission")
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
	require.NoError(t, m.finalizePairing(inviteID, sess, pi, cert))

	m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "fail",
	})
	_, ok := m.trust.Get(peer.uuid)
	require.True(t, ok, "unauthenticated post-success failure rolled the inviter back")
	m.handlePairingFailedFrom(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "fail",
	}, peer.uuid)

	_, ok = m.trust.Get(peer.uuid)
	require.False(t, ok, "inviter retained peer after joiner reported post-success commit failure")
	_, ok = m.memberByNodeID(peer.uuid)
	require.False(t, ok, "inviter retained member after joiner reported post-success commit failure")
	invite, ok := m.getInvite(inviteID)
	require.True(t, ok, "invite state (%v)", invite)
	require.Equal(t, inviteStateFailed, invite.State, "invite state (%v)", invite)
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
	require.NoError(t, m.finalizePairing(inviteID, sess, pi, cert))

	m.handlePairingAck(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "ack",
	}, "wrong-peer")
	_, ok := m.getSession(inviteID)
	require.True(t, ok, "unauthenticated acknowledgment finalized the inviter session")
	m.handlePairingAck(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: inviteID, Phase: "ack",
	}, peer.uuid)

	_, ok = m.getSession(inviteID)
	require.False(t, ok, "acknowledged inviter session was not deleted")
	_, ok = m.trust.Get(peer.uuid)
	require.True(t, ok, "acknowledgment removed the committed peer")
	invite, ok := m.getInvite(inviteID)
	require.True(t, ok, "invite state (%v)", invite)
	require.Equal(t, inviteStatePaired, invite.State, "invite state (%v)", invite)
}

func TestFinalCompletionRetriesLostResponse(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if !assert.NoError(t, err, "hijack the connection to simulate a lost completion response") {
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
	require.NoError(t, err)
	require.Equal(t, "eap-success", string(blob), "retry result (%v)", blob)
	require.Equal(t, int32(2), attempts.Load(), "retry result (%v)", blob)
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
	require.Equal(t, http.StatusOK, rec.Code, "cached completion response")
	require.Contains(t, rec.Body.String(), "Y2FjaGVkLWVhcC1zdWNjZXNz", "cached completion response")
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
	require.NoError(t, m.finalizePairing(inviteID, sess, pi, cert))
	m.setInviteCreatedCluster(true)
	sess.createdAt = time.Now().Add(-time.Hour).UnixMilli()
	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })
	m.expirePendingInvites(time.Now())

	_, ok := m.getSession(inviteID)
	require.False(t, ok, "unacknowledged retry session survived its TTL")
	_, ok = m.trust.Get(peer.uuid)
	require.True(t, ok, "ack timeout removed a fully committed peer")
	require.False(t, m.isInviteCreatedCluster(), "ack timeout left a real paired cluster marked invite-created")
}
