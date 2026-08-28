// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"
)

// pinFixture is a peer's minted identity (uuid + self-signed leaf) for use as a
// pin/member in tests.
type pinFixture struct {
	uuid, cert, fp string
}

func newPinFixture(t *testing.T, host string) pinFixture {
	t.Helper()
	uuid, cert, fp, _ := makeNode(t, host)
	return pinFixture{uuid: uuid, cert: cert, fp: fp}
}

// pinMemberFixture mints a peer, pins it, and records it as a cluster-1 member.
func pinMemberFixture(t *testing.T, m *Manager, host string) pinFixture {
	t.Helper()
	f := newPinFixture(t, host)
	pinTrusted(t, m, f.uuid, f.cert, f.fp)
	m.upsertMember(&ClusterNode{NodeUUID: f.uuid, ID: host, ClusterID: "cluster-1", State: stateMember})
	return f
}

// TestSelfRemoveTeardownSerializesRejoin is the follow-up regression: the guard
// must not merely shrink the race window but make compare-and-teardown
// linearizable against every cluster-composition commit. All commits now run
// under rosterMu (withClusterComposition), so a rejoin that arrives while a
// self-remove teardown holds the boundary blocks until teardown finishes.
//
// The two cases split by intent, both queued behind a held teardown boundary:
//   - A broker cluster:set-identity restore carries the (now-stale) persisted
//     clusterId. Behind a teardown it must be REFUSED, leaving the node fully
//     unclustered — never resurrecting a members-less "zombie" cluster.
//   - A full pairing-shaped commit (identity + pin + members, set directly under
//     the boundary) is a genuine rejoin and must SUCCEED, cleanly replacing the
//     torn-down cluster.
//
// The test holds rosterMu to stand in for the reconcile mid compare-and-teardown,
// launches each operation, and asserts it cannot commit while the boundary is
// held; then it runs the teardown, releases, and checks the terminal state.
func TestSelfRemoveTeardownSerializesRejoin(t *testing.T) {
	cases := []struct {
		name       string
		rejoin     func(m *Manager, p2 pinFixture)
		assertDone func(t *testing.T, m *Manager, p1, p2 pinFixture)
	}{
		{
			name: "stale set-identity restore behind teardown is refused",
			rejoin: func(m *Manager, _ pinFixture) {
				// The broker reflecting its still-persisted clusterId (same id)
				// after the node has already been unclustered by the teardown.
				params, _ := json.Marshal(map[string]string{"clusterId": "cluster-1", "clusterFriendlyName": "Restored"})
				m.handleSetIdentity(&Message{Params: params})
			},
			assertDone: func(t *testing.T, m *Manager, p1, _ pinFixture) {
				if id, _ := m.clusterIdentity(); id != "" {
					t.Fatalf("after teardown a stale set-identity restore left clusterId = %q, want empty", id)
				}
				if _, ok := m.trust.Get(p1.uuid); ok {
					t.Fatal("a pin survived teardown+stale-restore; want an empty pin set")
				}
				if n := m.snapshotNodes(); len(n) != 0 {
					t.Fatalf("members = %d after teardown+stale-restore, want 0", len(n))
				}
			},
		},
		{
			name: "full pairing commit stays atomic across teardown",
			rejoin: func(m *Manager, p2 pinFixture) {
				m.withClusterComposition(func() {
					m.setClusterIdentity("cluster-2", "Rejoined")
					_ = m.trust.Pin(&TrustedPin{
						NodeUUID: p2.uuid, NodeID: "peer-2", ClusterID: "cluster-2",
						CertPem: p2.cert, CertFingerprint: p2.fp, PinnedAt: time.Now().UnixMilli(),
					})
					m.upsertMember(&ClusterNode{NodeUUID: p2.uuid, ID: "peer-2", ClusterID: "cluster-2", State: stateMember})
					m.addSelfMember()
				})
			},
			assertDone: func(t *testing.T, m *Manager, p1, p2 pinFixture) {
				if id, _ := m.clusterIdentity(); id != "cluster-2" {
					t.Fatalf("after rejoin clusterId = %q, want cluster-2", id)
				}
				if _, ok := m.trust.Get(p2.uuid); !ok {
					t.Fatal("rejoined peer pin missing after teardown+rejoin")
				}
				if _, ok := m.trust.Get(p1.uuid); ok {
					t.Fatal("old cluster-1 pin survived teardown; state is inconsistent")
				}
				for _, n := range m.snapshotNodes() {
					if n.NodeUUID == p1.uuid {
						t.Fatal("old cluster-1 member survived teardown; state is inconsistent")
					}
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManagerPort(t, 15031)
			p1 := pinMemberFixture(t, m, "peer-1") // pinned member in cluster-1
			p2 := newPinFixture(t, "peer-2")       // identity to rejoin with

			// Stand in for the reconcile holding the boundary across its
			// compare-and-teardown.
			m.rosterMu.Lock()

			done := make(chan struct{})
			go func() {
				tc.rejoin(m, p2)
				close(done)
			}()

			// The boundary is held, so a correctly-serialized rejoin blocks in
			// withClusterComposition and neither completes (done) nor mutates
			// state. The window is a deliberate regression probe: an unguarded
			// commit (the bug this fixes) would run immediately and either close
			// done or flip clusterId to cluster-2 within it, tripping one of the
			// two asserts below.
			select {
			case <-done:
				t.Fatal("rejoin committed while the teardown boundary was held; not serialized")
			case <-time.After(300 * time.Millisecond):
			}
			if id, _ := m.clusterIdentity(); id != "cluster-1" {
				t.Fatalf("clusterId changed to %q while boundary held; rejoin was not serialized", id)
			}

			// The self-remove teardown runs under the held boundary, then releases.
			m.teardownClusterLocalLocked()
			m.rosterMu.Unlock()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("rejoin did not complete after the boundary was released")
			}
			tc.assertDone(t, m, p1, p2)
		})
	}
}
