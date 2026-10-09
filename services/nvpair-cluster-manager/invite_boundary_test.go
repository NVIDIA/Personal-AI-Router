// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// inviterSession builds a bare inviter-side pairing session for the boundary
// guards, which only key off inviteID / identity — the EAP server is irrelevant
// here.
func inviterSession(inviteID, clusterID string) *pairingSession {
	return &pairingSession{inviteID: inviteID, role: roleInviter, clusterID: clusterID}
}

// assertFullyUnclustered fails unless the node is torn down to empty: no
// identity, no pins, no members. It is the invariant control (2) requires after
// a teardown races the Initial Exchange.
func assertFullyUnclustered(t *testing.T, m *Manager) {
	t.Helper()
	id, _ := m.clusterIdentity()
	require.Equal(t, "", id, "clusterId")
	require.Empty(t, m.trust.List(), "pins")
	require.Empty(t, m.snapshotNodes(), "members")
}

// TestInitialExchangeTeardownBoundary is the deterministic control for the
// invite blocker: a teardown (leave / inbound removal / self-remove) that lands
// around the Initial Exchange must abandon the pairing, never resurrecting a
// session or invite into an emptied cluster. It drives the two teardown-boundary
// guards handleInviteNode/runInitialExchange rely on — registerInviterSession
// (session install) and withLivePairing (invite republication) — directly,
// since the network exchange between them is not what the guards protect.
func TestInitialExchangeTeardownBoundary(t *testing.T) {
	t.Run("teardown before initial invite publication leaves no pending invite", func(t *testing.T) {
		m := newTestManagerPort(t, 15080)
		pinMemberFixture(t, m, "peer-1")
		inv := &Invite{
			InviteID: "inv-publish", ClusterID: "cluster-1",
			State: inviteStatePending, CreatedAt: time.Now().UnixMilli(),
		}

		// Hold the boundary so publication queues behind teardown exactly in the
		// old sessGen-snapshot -> putInvite window.
		m.rosterMu.Lock()
		type result struct {
			gen uint64
			ok  bool
		}
		done := make(chan result, 1)
		go func() {
			gen, ok := m.publishInitialInvite(inv, "cluster-1")
			done <- result{gen, ok}
		}()
		select {
		case <-done:
			require.FailNow(t, "initial invite published while teardown boundary was held")
		case <-time.After(200 * time.Millisecond):
		}
		m.teardownClusterLocalLocked()
		m.rosterMu.Unlock()

		select {
		case got := <-done:
			require.False(t, got.ok, "stale publication succeeded with session generation")
		case <-time.After(5 * time.Second):
			require.FailNow(t, "blocked publication did not return")
		}
		_, ok := m.getInvite(inv.InviteID)
		require.False(t, ok, "pending invite survived teardown/publication race")
		assertFullyUnclustered(t, m)
	})

	t.Run("teardown before session registration refuses and stays empty", func(t *testing.T) {
		m := newTestManagerPort(t, 15081)
		pinMemberFixture(t, m, "peer-1") // a real cluster-1 with a pin + member

		cid0, _ := m.clusterIdentity()
		sessGen0 := m.sessGen.Load()
		publishPendingInvite(t, m, "inv-1")

		// The inbound removal lands before the Initial Exchange registers.
		m.teardownClusterLocal()

		sess := inviterSession("inv-1", cid0)
		require.False(t, m.registerInviterSession(sess, cid0, sessGen0), "registerInviterSession succeeded after teardown; want refusal")
		_, ok := m.getSession("inv-1")
		require.False(t, ok, "a session was installed after teardown")
		assertFullyUnclustered(t, m)
	})

	t.Run("teardown after registration refuses the invite republish", func(t *testing.T) {
		m := newTestManagerPort(t, 15082)
		pinMemberFixture(t, m, "peer-1")

		cid0, _ := m.clusterIdentity()
		sessGen0 := m.sessGen.Load()
		publishPendingInvite(t, m, "inv-2")

		sess := inviterSession("inv-2", cid0)
		require.True(t, m.registerInviterSession(sess, cid0, sessGen0), "registerInviterSession refused a live cluster")

		// The teardown lands while the exchange is in flight, clearing the session.
		m.teardownClusterLocal()

		published := false
		require.False(t, m.withLivePairing(sess, func() { published = true }), "withLivePairing ran after the session was torn down")
		require.False(t, published, "invite republish ran after teardown")
		_, ok := m.getInvite("inv-2")
		require.False(t, ok, "an invite record survived the teardown")
		assertFullyUnclustered(t, m)
	})

	t.Run("rejoin into the same cluster still refuses the stale registration", func(t *testing.T) {
		m := newTestManagerPort(t, 15083)
		pinMemberFixture(t, m, "peer-1")

		cid0, _ := m.clusterIdentity() // cluster-1
		sessGen0 := m.sessGen.Load()
		publishPendingInvite(t, m, "inv-3")

		// Removed, then re-paired back into the SAME cluster while the exchange
		// was running: clusterId is cluster-1 again, but resetSessions bumped the
		// session generation, so the stale registration must still be refused —
		// the gap a clusterId-only check would miss.
		m.teardownClusterLocal()
		m.withClusterComposition(func() {
			m.setClusterIdentity(cid0, "Rejoined")
			m.addSelfMember()
		})

		sess := inviterSession("inv-3", cid0)
		require.False(t, m.registerInviterSession(sess, cid0, sessGen0), "registerInviterSession succeeded after a same-cluster rejoin; want refusal")
		_, ok := m.getSession("inv-3")
		require.False(t, ok, "a stale session was installed after a same-cluster rejoin")
	})

	t.Run("live pairing registers and republishes", func(t *testing.T) {
		m := newTestManagerPort(t, 15084)
		pinMemberFixture(t, m, "peer-1")

		cid0, _ := m.clusterIdentity()
		sessGen0 := m.sessGen.Load()
		publishPendingInvite(t, m, "inv-4")

		sess := inviterSession("inv-4", cid0)
		require.True(t, m.registerInviterSession(sess, cid0, sessGen0), "registerInviterSession refused a live, unchanged cluster")
		inv, _ := m.getInvite("inv-4")

		pin := "123456"
		published := false
		require.True(t, m.withLivePairing(sess, func() {
			inv.Pin = &pin
			m.putInvite(inv)
			published = true
		}), "withLivePairing refused a live pairing")
		require.True(t, published, "invite republish did not run for a live pairing")
		got, ok := m.getInvite("inv-4")
		require.True(t, ok, "invite PIN not published for a live pairing (%v)", got)
		require.NotNil(t, got.Pin, "invite PIN not published for a live pairing (%v)", got)
		require.Equal(t, pin, *got.Pin, "invite PIN not published for a live pairing (%v)", got)
	})
}
