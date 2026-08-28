// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// selfRemovalProof builds the JSON body a peer returns alongside a 403 to prove
// it removed victim from cluster: a tombstone naming the victim, scoped to that
// cluster, signed by the peer (whose cert the victim still pins). This mirrors
// what handleRoster (rejectRosterReconcile) attaches on the not-pinned path, so
// the victim's rejectionProvesRemoval accepts it as authenticated proof.
func selfRemovalProof(peer, victim *Manager, cluster string) []byte {
	_, victimEpoch := victim.currentAdmission()
	proof, err := peer.newRemovalProof(victim.identity.NodeUUID, victimEpoch)
	if err != nil {
		panic(err)
	}
	proof = peer.withLocalRelayEndorsement(proof)
	b, _ := json.Marshal(rosterRejection{
		Tombstones:    []Tombstone{proof.Tombstone},
		RemovalProofs: []RemovalProof{proof},
	})
	return b
}

// TestSelfRemoveGuardStaleVerdict is the regression for the TOCTOU flagged in
// review of the offline-removed self-remove backstop. reconcilePeersAndMaybeSelfRemove
// snapshots its peers, waits on network reconciles (up to pairingHTTPTimeout
// each), then tears down all local cluster state if every peer 403s. If a
// pairing/rejoin completes *during* that wait — adopting a new clusterId, or
// re-pinning us back into the same one — the now-stale unanimous verdict must
// not erase that freshly-established state. The guard snapshots the cluster
// identity plus a generation counter before the fan-out and revalidates both
// under rosterMu before self-removing.
//
// The test drives the real mTLS reconcile against a peer that blocks before
// returning 403, so the state change lands deterministically inside the wait
// window rather than relying on timing.
func TestSelfRemoveGuardStaleVerdict(t *testing.T) {
	cases := []struct {
		name string
		// mutate runs while the peer reconcile is blocked, i.e. after the verdict's
		// inputs are snapshotted but before it decides. nil means no change.
		mutate func(m *Manager)
		// wantCluster is the clusterId expected after the pass ("" = torn down).
		wantCluster string
	}{
		{
			name:        "no change self-removes",
			mutate:      nil,
			wantCluster: "",
		},
		{
			name:        "rejoin into a different cluster is preserved",
			mutate:      func(m *Manager) { m.setClusterIdentity("cluster-2", "New Cluster") },
			wantCluster: "cluster-2",
		},
		{
			name: "re-pin back into the same cluster is preserved",
			mutate: func(m *Manager) {
				// A re-pair back into the same cluster keeps clusterId but bumps
				// the generation (fresh pin + member record) — precisely the case
				// a clusterId-only revalidation would miss.
				m.upsertMember(&ClusterNode{NodeUUID: "rejoined-peer", ID: "rejoined-peer", State: stateMember})
			},
			wantCluster: "cluster-1",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mA := newTestManagerPort(t, 15021)
			// peer contributes only its (pinned) identity/cert; its own server is
			// never started — the blocking stub below stands in for it.
			peer := newTestManagerPort(t, 15022)
			pinTrusted(t, mA, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)

			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			// The peer rejects us with authenticated proof of removal (a signed
			// tombstone naming mA, scoped to cluster-1). That makes the unanimous
			// rejection *actionable*, so this test exercises the stale-verdict
			// guard rather than the "no proof, stay put" path. In the rejoin case
			// mA's current cluster no longer matches the tombstone, so the proof no
			// longer applies — which the guard also (correctly) treats as staying.
			proofBody := selfRemovalProof(peer, mA, "cluster-1")
			mux := http.NewServeMux()
			mux.HandleFunc(rosterPath, func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(entered) })
				<-release
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write(proofBody)
			})
			ln, err := tls.Listen("tcp", "127.0.0.1:0", peer.buildServerTLSConfig())
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer ln.Close()
			srv := &http.Server{Handler: mux}
			go func() { _ = srv.Serve(ln) }()
			defer srv.Close()

			mA.upsertMember(&ClusterNode{
				NodeUUID:  peer.identity.NodeUUID,
				ID:        "peer",
				IPAddress: "127.0.0.1",
				Port:      ln.Addr().(*net.TCPAddr).Port,
				State:     stateMember,
			})

			done := make(chan struct{})
			go func() {
				mA.reconcilePeersAndMaybeSelfRemove()
				close(done)
			}()

			// The verdict's inputs (clusterId + generation) are already snapshotted
			// by the time the peer handler is reached and blocks.
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("peer reconcile never reached the handler")
			}
			if tc.mutate != nil {
				tc.mutate(mA)
			}
			close(release)

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("reconcile pass did not finish")
			}

			if got, _ := mA.clusterIdentity(); got != tc.wantCluster {
				t.Fatalf("after reconcile pass clusterId = %q, want %q", got, tc.wantCluster)
			}
		})
	}
}

// startPeerStub mints a peer identity, pins it into m as a cluster-1 member, and
// starts an mTLS roster server that answers every reconcile with status (a 200
// carries an empty roster, so the peer "accepts" us; a 403 means it dropped our
// pin). When proof is set, a 403 additionally carries a signed removal tombstone
// naming m (as a real removing peer would), so m's rejectionProvesRemoval treats
// it as authenticated grounds to self-remove; a bare 403 (proof=false) is a peer
// that left or lost its pin without removing us. The member is reachable at the
// stub's ephemeral port so m's real mTLS reconcile drives against it.
// Listeners/servers are torn down via t.Cleanup.
func startPeerStub(t *testing.T, m *Manager, id string, status int, proof bool) {
	t.Helper()
	peer := newTestManagerPort(t, 0) // identity/cert only; port is unused (never Run)
	pinTrusted(t, m, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)

	var proofBody []byte
	if proof && status == http.StatusForbidden {
		cid, _ := m.clusterIdentity()
		proofBody = selfRemovalProof(peer, m, cid)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(rosterPath, func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(proofBody) // nil body ⇒ a bare 403 with no removal proof
	})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", peer.buildServerTLSConfig())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	m.upsertMember(&ClusterNode{
		NodeUUID:       peer.identity.NodeUUID,
		ID:             id,
		IPAddress:      "127.0.0.1",
		Port:           ln.Addr().(*net.TCPAddr).Port,
		ClusterID:      "cluster-1",
		AdmissionEpoch: 1,
		State:          stateMember,
	})
}

// TestSelfRemoveRequiresUnanimousRejection hardens the soundness guarantee for
// clusters with more than one peer: a node tears down only when EVERY peer
// rejects its pin. The 2-node accept/unreachable tests cannot exercise this —
// they cannot tell "requires unanimity" apart from "requires no rejection." Here
// one peer answers 200 (still holds the pin) and another 403 (dropped it): the
// rejection is not unanimous, so the node must stay clustered. This is exactly
// the trust-fan-out window where a peer that has not pinned us yet 403s while our
// endorser still 200s — a false self-eviction here would be the bug. The second
// case (every peer 403s, each with a signed removal tombstone) is the positive
// control: it proves the reconcile really reaches the peers and that unanimity
// plus authenticated proof, not merely "the pass did nothing," is what drives
// teardown. (A unanimous 403 with *no* proof is covered separately — it must
// NOT self-remove.)
func TestSelfRemoveRequiresUnanimousRejection(t *testing.T) {
	cases := []struct {
		name        string
		peerStatus  []int
		wantCluster string // "" = torn down
	}{
		{
			name:        "mixed 200 and 403 stays clustered",
			peerStatus:  []int{http.StatusOK, http.StatusForbidden},
			wantCluster: "cluster-1",
		},
		{
			name:        "unanimous 403 across multiple peers self-removes",
			peerStatus:  []int{http.StatusForbidden, http.StatusForbidden},
			wantCluster: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mA := newTestManagerPort(t, 15061) // clustered as cluster-1
			for i, st := range tc.peerStatus {
				startPeerStub(t, mA, fmt.Sprintf("peer-%d", i), st, st == http.StatusForbidden)
			}

			mA.reconcilePeersAndMaybeSelfRemove()

			if got, _ := mA.clusterIdentity(); got != tc.wantCluster {
				t.Fatalf("after reconcile clusterId = %q, want %q", got, tc.wantCluster)
			}
		})
	}
}

// TestSelfRemoveRequiresRemovalProof is the regression for the blocker: a
// unanimous roster 403 is not by itself grounds to self-evict. handleRoster 403s
// any caller it no longer pins, so a peer that voluntarily left (dropping every
// pin, including ours) 403s us exactly like one that removed us. The survivor of
// a departure must stay; only an authenticated, cluster-scoped removal tombstone
// (which a leaving peer does not hold for us) may tear us down.
//
// The stubs answer 403 either bare (proof=false: the peer left or lost its pin)
// or with a signed tombstone naming us (proof=true: the peer removed us),
// covering both the 2-node and multi-node shapes.
func TestSelfRemoveRequiresRemovalProof(t *testing.T) {
	type peerSpec struct {
		status int
		proof  bool
	}
	forbidden := http.StatusForbidden
	cases := []struct {
		name        string
		peers       []peerSpec
		wantCluster string // "" = torn down
	}{
		{
			name:        "only peer left (bare 403, no proof) — survivor stays",
			peers:       []peerSpec{{forbidden, false}},
			wantCluster: "cluster-1",
		},
		{
			name:        "only peer removed us (403 + signed tombstone) — self-removes",
			peers:       []peerSpec{{forbidden, true}},
			wantCluster: "",
		},
		{
			name:        "every peer left (unanimous bare 403) — survivor stays",
			peers:       []peerSpec{{forbidden, false}, {forbidden, false}},
			wantCluster: "cluster-1",
		},
		{
			name:        "removed by one peer while another left — self-removes",
			peers:       []peerSpec{{forbidden, true}, {forbidden, false}},
			wantCluster: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mA := newTestManagerPort(t, 15071) // clustered as cluster-1
			for i, ps := range tc.peers {
				startPeerStub(t, mA, fmt.Sprintf("peer-%d", i), ps.status, ps.proof)
			}

			mA.reconcilePeersAndMaybeSelfRemove()

			if got, _ := mA.clusterIdentity(); got != tc.wantCluster {
				t.Fatalf("after reconcile clusterId = %q, want %q", got, tc.wantCluster)
			}
		})
	}
}
