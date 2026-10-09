// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReconcileMTLSFanout drives the real mTLS roster endpoint over loopback
// between two managers cross-pinned as if they had paired, with A also holding a
// third member C endorsed by A. After one reconcile, B must transitively pin C.
func TestReconcileMTLSFanout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mA := newTestManagerPort(t, 15011)
	mB := newTestManagerPort(t, 15012)
	go func() { _ = mA.runHTTP(ctx) }()
	go func() { _ = mB.runHTTP(ctx) }()
	time.Sleep(400 * time.Millisecond)

	// A and B trust each other (simulated completed pairing), with mutual member
	// records so reconcile has an address.
	pinTrusted(t, mA, mB.identity.NodeUUID, string(mB.identity.CertPEM), mB.identity.CertFingerprint)
	pinTrusted(t, mB, mA.identity.NodeUUID, string(mA.identity.CertPEM), mA.identity.CertFingerprint)
	mA.upsertMember(&ClusterNode{NodeUUID: mB.identity.NodeUUID, ID: "node-b", IPAddress: "127.0.0.1", Port: 15012, AdmissionEpoch: 1, State: stateMember})
	mB.upsertMember(&ClusterNode{NodeUUID: mA.identity.NodeUUID, ID: "node-a", IPAddress: "127.0.0.1", Port: 15011, AdmissionEpoch: 1, State: stateMember})

	// A holds a third member C, endorsed by A.
	cUUID, cCert, cFP, _ := makeNode(t, "node-c")
	require.NoError(t, mA.trust.Pin(&TrustedPin{
		NodeUUID: cUUID, NodeID: "node-c", ClusterID: "cluster-1",
		AdmissionEpoch: 1, CertPem: cCert, CertFingerprint: cFP, PinnedAt: time.Now().UnixMilli(),
		Endorsements: []Endorsement{mA.endorsePeer(cUUID, cFP, 1)},
	}), "pin C on A")
	mA.upsertMember(&ClusterNode{NodeUUID: cUUID, ID: "node-c", IPAddress: "10.0.0.9", Port: 14321, AdmissionEpoch: 1, State: stateMember})

	// B reconciles with A and must transitively learn C.
	mB.reconcileWith([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(15011))}, mA.identity.NodeUUID)

	_, ok := mB.trust.Get(cUUID)
	require.True(t, ok, "B should have transitively pinned C after reconciling with A over mTLS")
}

// newTestManager builds a Manager backed by a temp config dir and a no-op codec,
// clustered as "cluster-1". Run() is not started; tests drive merge logic
// directly.
func newTestManager(t *testing.T) *Manager {
	return newTestManagerPort(t, 14999)
}

func newTestManagerPort(t *testing.T, port int) *Manager {
	t.Helper()
	codec := NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), io.Discard})
	mgr, err := NewManager(codec, t.TempDir(), port)
	require.NoError(t, err, "new manager")
	_, err = mgr.ensureAdmission("cluster-1")
	require.NoError(t, err, "establish test admission")
	mgr.setClusterIdentity("cluster-1", "Lab")
	return mgr
}

// makeNode mints a fresh node identity (uuid + self-signed leaf) and returns its
// signing key, simulating a real peer for endorsement crafting.
func makeNode(t *testing.T, host string) (uuid, certPEM, fingerprint string, priv ed25519.PrivateKey) {
	t.Helper()
	uuid, err := newUUIDv4()
	require.NoError(t, err, "uuid")
	certPEMb, keyPEMb, err := generateLeaf(uuid, host)
	require.NoError(t, err, "leaf")
	block, _ := pem.Decode(keyPEMb)
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err, "parse key")
	fp, _ := certFingerprintFromPEM(certPEMb)
	return uuid, string(certPEMb), fp, k.(ed25519.PrivateKey)
}

// pinTrusted pins a node into the manager's trusted store as an already-paired
// member (no endorsement needed — direct trust).
func pinTrusted(t *testing.T, m *Manager, uuid, certPEM, fp string) {
	t.Helper()
	require.NoError(t, m.trust.Pin(&TrustedPin{
		NodeUUID:        uuid,
		NodeID:          uuid,
		ClusterID:       "cluster-1",
		AdmissionEpoch:  1,
		CertPem:         certPEM,
		CertFingerprint: fp,
		PinnedAt:        time.Now().UnixMilli(),
	}), "pin")
}

// TestMergeRosterTransitivePin verifies the core fan-out property: a node we
// have never paired with gets pinned because a node we DO trust endorsed it.
func TestMergeRosterTransitivePin(t *testing.T) {
	m := newTestManager(t)

	// A is directly trusted (as if we paired it).
	aUUID, aCert, aFP, aPriv := makeNode(t, "node-a")
	pinTrusted(t, m, aUUID, aCert, aFP)

	// C is unknown to us, but A endorses C.
	cUUID, cCert, cFP, _ := makeNode(t, "node-c")
	endC := signEndorsement(aPriv, aUUID, cUUID, cFP, "cluster-1", time.Now().UnixMilli(), 1, 1)

	roster := &Roster{
		ClusterID: "cluster-1",
		Members: []RosterEntry{{
			NodeUUID: cUUID, NodeID: "node-c", Addr: "10.0.0.3:14321", AdmissionEpoch: 1,
			CertPem: cCert, CertFingerprint: cFP, Endorsements: []Endorsement{endC},
		}},
	}
	require.True(t, m.mergeRoster(roster, aUUID), "merge should have reported a change")
	_, ok := m.trust.Get(cUUID)
	require.True(t, ok, "C should have been transitively pinned via A's endorsement")
	_, ok = m.memberByNodeID(cUUID)
	require.True(t, ok, "C should have been recorded as a member")
}

// TestMergeRosterFixpoint verifies multi-hop transitive pinning: we trust A, A
// endorses B, B endorses C; one merge must pin both B and C (C only becomes
// acceptable after B is pinned in the same pass loop).
func TestMergeRosterFixpoint(t *testing.T) {
	m := newTestManager(t)
	aUUID, aCert, aFP, aPriv := makeNode(t, "node-a")
	pinTrusted(t, m, aUUID, aCert, aFP)

	bUUID, bCert, bFP, bPriv := makeNode(t, "node-b")
	cUUID, cCert, cFP, _ := makeNode(t, "node-c")
	endB := signEndorsement(aPriv, aUUID, bUUID, bFP, "cluster-1", time.Now().UnixMilli(), 1, 1)
	endC := signEndorsement(bPriv, bUUID, cUUID, cFP, "cluster-1", time.Now().UnixMilli(), 1, 1)

	roster := &Roster{ClusterID: "cluster-1", Members: []RosterEntry{
		{NodeUUID: cUUID, NodeID: "node-c", AdmissionEpoch: 1, CertPem: cCert, CertFingerprint: cFP, Endorsements: []Endorsement{endC}},
		{NodeUUID: bUUID, NodeID: "node-b", AdmissionEpoch: 1, CertPem: bCert, CertFingerprint: bFP, Endorsements: []Endorsement{endB}},
	}}
	m.mergeRoster(roster, aUUID)
	_, ok := m.trust.Get(bUUID)
	require.True(t, ok, "B should be pinned (endorsed by trusted A)")
	_, ok = m.trust.Get(cUUID)
	require.True(t, ok, "C should be pinned at fixpoint (endorsed by newly-pinned B)")
}

// TestMergeRosterRejectsUntrustedEndorser verifies the blast-radius bound: an
// entry endorsed only by a node we do not trust is NOT pinned.
func TestMergeRosterRejectsUntrustedEndorser(t *testing.T) {
	m := newTestManager(t)

	// X is a stranger (never pinned) who endorses C.
	xUUID, _, _, xPriv := makeNode(t, "node-x")
	cUUID, cCert, cFP, _ := makeNode(t, "node-c")
	endC := signEndorsement(xPriv, xUUID, cUUID, cFP, "cluster-1", time.Now().UnixMilli())

	roster := &Roster{ClusterID: "cluster-1", Members: []RosterEntry{
		{NodeUUID: cUUID, NodeID: "node-c", CertPem: cCert, CertFingerprint: cFP, Endorsements: []Endorsement{endC}},
	}}
	require.False(t, m.mergeRoster(roster, cUUID), "merge should report no change for an unendorsed entry")
	_, ok := m.trust.Get(cUUID)
	require.False(t, ok, "C must NOT be pinned: its only endorser is untrusted")
}

// TestMergeRosterTamperedFingerprint verifies an endorsement cannot be replayed
// onto a different cert: the entry's fingerprint must match its cert and the
// endorsement.
func TestMergeRosterTamperedFingerprint(t *testing.T) {
	m := newTestManager(t)
	aUUID, aCert, aFP, aPriv := makeNode(t, "node-a")
	pinTrusted(t, m, aUUID, aCert, aFP)

	cUUID, _, cFP, _ := makeNode(t, "node-c")
	endC := signEndorsement(aPriv, aUUID, cUUID, cFP, "cluster-1", time.Now().UnixMilli())

	// Swap in a different node's cert while keeping C's uuid + A's endorsement.
	_, otherCert, _, _ := makeNode(t, "node-other")
	roster := &Roster{ClusterID: "cluster-1", Members: []RosterEntry{
		{NodeUUID: cUUID, NodeID: "node-c", CertPem: otherCert, CertFingerprint: cFP, Endorsements: []Endorsement{endC}},
	}}
	m.mergeRoster(roster, aUUID)
	_, ok := m.trust.Get(cUUID)
	require.False(t, ok, "entry with a cert that doesn't match its fingerprint must be rejected")
}

// TestMergeTombstoneRemoves verifies a signed removal from a trusted member
// de-pins a node, and that a stale add cannot resurrect it.
func TestMergeTombstoneRemoves(t *testing.T) {
	m := newTestManager(t)
	aUUID, aCert, aFP, aPriv := makeNode(t, "node-a")
	pinTrusted(t, m, aUUID, aCert, aFP)

	// Pin C transitively first.
	cUUID, cCert, cFP, _ := makeNode(t, "node-c")
	addedAt := time.Now().UnixMilli()
	endC := signEndorsement(aPriv, aUUID, cUUID, cFP, "cluster-1", addedAt, 1, 1)
	m.mergeRoster(&Roster{ClusterID: "cluster-1", Members: []RosterEntry{
		{NodeUUID: cUUID, NodeID: "node-c", AdmissionEpoch: 1, CertPem: cCert, CertFingerprint: cFP, Endorsements: []Endorsement{endC}},
	}}, aUUID)
	_, ok := m.trust.Get(cUUID)
	require.True(t, ok, "precondition: C should be pinned")

	// A tombstones C (newer than the add).
	tomb := signTombstone(aPriv, aUUID, cUUID, "cluster-1", addedAt+1000, 1, 1)
	proof := RemovalProof{Tombstone: tomb, SignerCertPem: aCert, SignerFingerprint: aFP}
	require.True(t, m.mergeRoster(&Roster{ClusterID: "cluster-1", RemovalProofs: []RemovalProof{proof}}, aUUID), "tombstone merge should report a change")
	_, ok = m.trust.Get(cUUID)
	require.False(t, ok, "C should be de-pinned by the tombstone")

	// A stale re-add (older than the tombstone) must not resurrect C.
	m.mergeRoster(&Roster{ClusterID: "cluster-1", Members: []RosterEntry{
		{NodeUUID: cUUID, NodeID: "node-c", AdmissionEpoch: 1, CertPem: cCert, CertFingerprint: cFP, Endorsements: []Endorsement{endC}},
	}}, aUUID)
	_, ok = m.trust.Get(cUUID)
	require.False(t, ok, "a stale add must not resurrect a tombstoned node")
}
