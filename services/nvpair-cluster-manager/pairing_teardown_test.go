// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "marshal pairing info")
	parsed, cert, err := parsePairingInfo(raw)
	require.NoError(t, err, "parse pairing info")
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
		require.NoError(t, err, "commitPairing err")
		require.False(t, committed, "commitPairing committed after teardown; pin/member resurrected into an emptied cluster")
		_, ok := m.trust.Get(f.uuid)
		require.False(t, ok, "joiner pinned after teardown")
		id, _ := m.clusterIdentity()
		require.Equal(t, "", id, "clusterId")
		for _, n := range m.snapshotNodes() {
			require.NotEqual(t, f.uuid, n.NodeUUID, "joiner recorded as a member after teardown")
		}
	})

	t.Run("happy path still commits", func(t *testing.T) {
		m := newTestManagerPort(t, 15042) // clustered as cluster-1
		f := newPinFixture(t, "peer-2")
		pi, cert := pairingInfoFor(t, f)
		sess := putInviterSession(t, m, "inv-happy", "cluster-1")

		committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
		require.NoError(t, err, "commitPairing err")
		require.True(t, committed, "commitPairing did not commit a normal pairing in the same cluster")
		_, ok := m.trust.Get(f.uuid)
		require.True(t, ok, "joiner not pinned after a normal pairing")
		pin, _ := m.trust.Get(f.uuid)
		require.Equal(t, "cluster-1", pin.ClusterID, "joiner pin clusterId")
		found := false
		for _, n := range m.snapshotNodes() {
			if n.NodeUUID == f.uuid {
				found = true
				require.Equal(t, "cluster-1", n.ClusterID, "joiner member clusterId")
			}
		}
		require.True(t, found, "joiner not recorded as a member after a normal pairing")
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
		require.NoError(t, err, "commitPairing err")
		require.False(t, committed, "commitPairing committed for a session the teardown abandoned")
		_, ok := m.trust.Get(f.uuid)
		require.False(t, ok, "joiner pinned via an abandoned session")
		for _, n := range m.snapshotNodes() {
			require.NotEqual(t, f.uuid, n.NodeUUID, "joiner recorded as a member via an abandoned session")
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
			require.FailNow(t, "commitPairing ran while the teardown boundary was held; not serialized")
		case <-time.After(300 * time.Millisecond):
		}

		// The teardown runs under the held boundary, then releases.
		m.teardownClusterLocalLocked()
		m.rosterMu.Unlock()

		select {
		case r := <-resCh:
			require.Nil(t, r.err, "commitPairing err")
			require.False(t, r.committed, "commitPairing committed after the racing teardown; pin/member resurrected")
		case <-time.After(5 * time.Second):
			require.FailNow(t, "commitPairing did not return after the boundary was released")
		}
		_, ok := m.trust.Get(f.uuid)
		require.False(t, ok, "joiner pinned despite the racing teardown")
		id, _ := m.clusterIdentity()
		require.Equal(t, "", id, "clusterId")
	})
}

func TestPairingCommitPersistenceFailureGrantsNoPin(t *testing.T) {
	m := newTestManagerPort(t, 15137)
	f := newPinFixture(t, "peer-2")
	pi, cert := pairingInfoFor(t, f)
	sess := putInviterSession(t, m, "inv-persist-fail", "cluster-1")
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	m.clusterDir = blocker

	committed, err := m.commitPairing(sess, pi, cert, time.Now().UnixMilli())
	require.Error(t, err, "commit (%v, %v)", committed, err)
	require.False(t, committed, "commit (%v, %v)", committed, err)
	_, ok := m.trust.Get(f.uuid)
	require.False(t, ok, "pairing persistence failure still granted an mTLS pin")
	_, ok = m.memberByNodeID(f.uuid)
	require.False(t, ok, "pairing persistence failure left an in-memory member")
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

	require.ErrorIs(t, m.finalizePairing(inviteID, sess, pi, cert), errPairingCommitStale, "finalizePairing error")

	_, ok := m.trust.Get(f.uuid)
	require.False(t, ok, "joiner pinned after a cluster switch")
	inv, ok := m.getInvite(inviteID)
	require.True(t, ok, "invite missing; want a failed invite record, not a silent drop")
	require.Equal(t, inviteStateFailed, inv.State, "invite state")
	_, ok = m.getSession(inviteID)
	require.False(t, ok, "session not deleted after an aborted completion")
}

func TestPairingCommitFailureSuppressesEAPSuccess(t *testing.T) {
	success := []byte("synthetic-eap-success")
	rr := httptest.NewRecorder()
	respondPairingCommit(rr, success, errPairingCommitStale)
	require.Equal(t, http.StatusConflict, rr.Code, "status")
	require.NotContains(t, rr.Body.String(), string(success), "response leaked EAP-Success after inviter-side commit refusal")

	rr = httptest.NewRecorder()
	respondPairingCommit(rr, success, nil)
	require.Equal(t, http.StatusOK, rr.Code, "success status")
	require.Contains(t, rr.Body.String(), "msg", "successful commit did not return the pairing frame")
}
