// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pairingInfoFor builds an authenticated PairingInfo + parsed cert for a peer
// fixture, standing in for the joiner's PeerInfo that onInviterPaired feeds to
// commitPairing.
func pairingInfoFor(t *testing.T, f pinFixture) (*PairingInfo, *x509.Certificate) {
	t.Helper()
	pi := &PairingInfo{
		V:              pairingInfoVersion,
		NodeUUID:       f.uuid,
		NodeID:         "peer-2",
		Name:           "peer-2",
		ClusterID:      "",
		AdmissionEpoch: legacyAdmissionEpoch,
		Cert:           f.cert,
	}
	raw, err := json.Marshal(pi)
	if err != nil {
		t.Fatalf("marshal pairing info: %v", err)
	}
	parsed, cert, err := parsePairingInfo(raw)
	if err != nil {
		t.Fatalf("parse pairing info: %v", err)
	}
	return parsed, cert
}

// putInviterSession registers an inviter-role pairing session scoped to
// clusterID, mirroring what runInitialExchange records before the joiner's
// Completion lands.
func putInviterSession(t *testing.T, m *Manager, inviteID, clusterID string) *pairingSession {
	t.Helper()
	sess := &pairingSession{inviteID: inviteID, role: roleInviter, clusterID: clusterID}
	m.putSession(sess)
	return sess
}

// TestCommitPairingEpochGate verifies that an
// inviter's pairing Completion that lands after the node left or was removed
// must not resurrect the joiner's pin/member into an emptied cluster. teardown
// now abandons in-flight sessions/invites, and commitPairing rechecks both the
// live cluster identity and the session under the same rosterMu boundary before
// writing anything.
func TestCommitPairingEpochGate(t *testing.T) {
	t.Run("commit after teardown is discarded", func(t *testing.T) {
		m := newTestManagerPort(t, 15041) // clustered as cluster-1
		f := newPinFixture(t, "peer-2")
		pi, cert := pairingInfoFor(t, f)
		sess := putInviterSession(t, m, "inv-teardown", "cluster-1")

		// The node is removed / leaves before the Completion lands (clears both
		// the cluster identity and the pairing session).
		m.teardownClusterLocal()

		committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("commitPairing err: %v", err)
		}
		if committed {
			t.Fatal("commitPairing committed after teardown; pin/member resurrected into an emptied cluster")
		}
		if _, ok := m.trust.Get(f.uuid); ok {
			t.Fatal("joiner pinned after teardown")
		}
		if id, _ := m.clusterIdentity(); id != "" {
			t.Fatalf("clusterId = %q after teardown, want empty", id)
		}
		for _, n := range m.snapshotNodes() {
			if n.NodeUUID == f.uuid {
				t.Fatal("joiner recorded as a member after teardown")
			}
		}
	})

	t.Run("happy path still commits", func(t *testing.T) {
		m := newTestManagerPort(t, 15042) // clustered as cluster-1
		f := newPinFixture(t, "peer-2")
		pi, cert := pairingInfoFor(t, f)
		sess := putInviterSession(t, m, "inv-happy", "cluster-1")

		committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("commitPairing err: %v", err)
		}
		if !committed {
			t.Fatal("commitPairing did not commit a normal pairing in the same cluster")
		}
		if _, ok := m.trust.Get(f.uuid); !ok {
			t.Fatal("joiner not pinned after a normal pairing")
		}
		pin, _ := m.trust.Get(f.uuid)
		if pin.ClusterID != "cluster-1" {
			t.Fatalf("joiner pin clusterId = %q, want session cluster", pin.ClusterID)
		}
		found := false
		for _, n := range m.snapshotNodes() {
			if n.NodeUUID == f.uuid {
				found = true
				if n.ClusterID != "cluster-1" {
					t.Fatalf("joiner member clusterId = %q, want session cluster", n.ClusterID)
				}
			}
		}
		if !found {
			t.Fatal("joiner not recorded as a member after a normal pairing")
		}
	})

	t.Run("abandoned session is discarded even when clusterId is unchanged", func(t *testing.T) {
		// Models "removed, then rejoined the SAME cluster mid-pairing": the epoch
		// (clusterId) matches again, so only the session-liveness gate catches
		// the stale Completion. Standing in for the teardown+rejoin, the session
		// is dropped while clusterId stays cluster-1.
		m := newTestManagerPort(t, 15044) // clustered as cluster-1
		f := newPinFixture(t, "peer-2")
		pi, cert := pairingInfoFor(t, f)
		sess := putInviterSession(t, m, "inv-abandoned", "cluster-1")
		m.deleteSession("inv-abandoned")

		committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("commitPairing err: %v", err)
		}
		if committed {
			t.Fatal("commitPairing committed for a session the teardown abandoned")
		}
		if _, ok := m.trust.Get(f.uuid); ok {
			t.Fatal("joiner pinned via an abandoned session")
		}
		for _, n := range m.snapshotNodes() {
			if n.NodeUUID == f.uuid {
				t.Fatal("joiner recorded as a member via an abandoned session")
			}
		}
	})

	t.Run("removal racing an outbound completion", func(t *testing.T) {
		m := newTestManagerPort(t, 15043) // clustered as cluster-1
		f := newPinFixture(t, "peer-2")
		pi, cert := pairingInfoFor(t, f)
		sess := putInviterSession(t, m, "inv-race", "cluster-1")

		// Stand in for the self-remove holding the boundary across its
		// compare-and-teardown while the Completion lands concurrently.
		m.rosterMu.Lock()

		type result struct {
			committed bool
			err       error
		}
		resCh := make(chan result, 1)
		go func() {
			c, e := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
			resCh <- result{c, e}
		}()

		// A correctly-serialized commit blocks in withClusterComposition while
		// the boundary is held; an unguarded commit (the bug this fixes) would
		// run immediately and pin/record the joiner.
		select {
		case <-resCh:
			t.Fatal("commitPairing ran while the teardown boundary was held; not serialized")
		case <-time.After(300 * time.Millisecond):
		}

		// The teardown runs under the held boundary, then releases.
		m.teardownClusterLocalLocked()
		m.rosterMu.Unlock()

		select {
		case r := <-resCh:
			if r.err != nil {
				t.Fatalf("commitPairing err: %v", r.err)
			}
			if r.committed {
				t.Fatal("commitPairing committed after the racing teardown; pin/member resurrected")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("commitPairing did not return after the boundary was released")
		}
		if _, ok := m.trust.Get(f.uuid); ok {
			t.Fatal("joiner pinned despite the racing teardown")
		}
		if id, _ := m.clusterIdentity(); id != "" {
			t.Fatalf("clusterId = %q after teardown, want empty", id)
		}
	})
}

func TestPairingCommitPersistenceFailureGrantsNoPin(t *testing.T) {
	m := newTestManagerPort(t, 15137)
	f := newPinFixture(t, "peer-2")
	pi, cert := pairingInfoFor(t, f)
	sess := putInviterSession(t, m, "inv-persist-fail", "cluster-1")
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.clusterDir = blocker

	committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
	if err == nil || committed {
		t.Fatalf("commit = %v, err = %v; want durable refusal", committed, err)
	}
	if _, ok := m.trust.Get(f.uuid); ok {
		t.Fatal("pairing persistence failure still granted an mTLS pin")
	}
	if _, ok := m.memberByNodeID(f.uuid); ok {
		t.Fatal("pairing persistence failure left an in-memory member")
	}
}

// TestFinalizePairingAbortFailsLingeringInvite covers the post-commit bookkeeping
// of the discard path: when the node's cluster changes out from under an in-flight
// invite via a route that does NOT clear invites (a broker-driven
// cluster:set-identity), the aborted completion must fail the invite rather than
// leave a phantom "pending" one, and must drop the session.
func TestFinalizePairingAbortFailsLingeringInvite(t *testing.T) {
	m := newTestManagerPort(t, 15045) // clustered as cluster-1
	f := newPinFixture(t, "peer-2")
	pi, cert := pairingInfoFor(t, f)

	inviteID := "inv-switch"
	m.putInvite(&Invite{InviteID: inviteID, ClusterID: "cluster-1", State: inviteStatePending, CreatedAt: time.Now().UnixMilli()})
	sess := putInviterSession(t, m, inviteID, "cluster-1")

	// Broker switches this node into a different cluster (no teardown, so the
	// invite and session survive) before the Completion lands.
	m.setClusterIdentity("cluster-2", "Switched")

	err := m.finalizePairing(inviteID, sess, pi, cert)
	if !errors.Is(err, errPairingCommitStale) {
		t.Fatalf("finalizePairing error = %v, want stale commit refusal", err)
	}

	if _, ok := m.trust.Get(f.uuid); ok {
		t.Fatal("joiner pinned after a cluster switch")
	}
	inv, ok := m.getInvite(inviteID)
	if !ok {
		t.Fatal("invite missing; want a failed invite record, not a silent drop")
	}
	if inv.State != inviteStateFailed {
		t.Fatalf("invite state = %q, want %q", inv.State, inviteStateFailed)
	}
	if _, ok := m.getSession(inviteID); ok {
		t.Fatal("session not deleted after an aborted completion")
	}
}

func TestPairingCommitFailureSuppressesEAPSuccess(t *testing.T) {
	success := []byte("synthetic-eap-success")
	rr := httptest.NewRecorder()
	respondPairingCommit(rr, success, errPairingCommitStale)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusConflict)
	}
	if bytes.Contains(rr.Body.Bytes(), success) {
		t.Fatal("response leaked EAP-Success after inviter-side commit refusal")
	}

	rr = httptest.NewRecorder()
	respondPairingCommit(rr, success, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("success status = %d, want 200", rr.Code)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("msg")) {
		t.Fatal("successful commit did not return the pairing frame")
	}
}
