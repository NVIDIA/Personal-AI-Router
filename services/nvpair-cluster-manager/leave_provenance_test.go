// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMaybeLeavePreservesIntentionalSolo verifies that an explicitly created
// cluster of one must survive a declined invite. leaveIfSolo used to erase it.
func TestMaybeLeavePreservesIntentionalSolo(t *testing.T) {
	m := newTestManager(t) // clustered as "cluster-1", inviteCreated=false
	m.addSelfMember()

	inv := &Invite{
		InviteID:     "inv-intentional",
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStateDeclined,
		CreatedAt:    time.Now().UnixMilli(),
	}
	m.putInvite(inv)

	require.False(t, m.maybeLeaveInviteCreatedCluster(), "intentional solo cluster must not leave on decline cleanup")
	id, _ := m.clusterIdentity()
	require.NotEqual(t, "", id, "intentional solo cluster was erased")
}

// TestMaybeLeaveKeepsSiblingPendingInvite verifies that declining one outbound
// invite while another is still pending must not tear down an invite-created
// cluster — otherwise the sibling Completion could join a vanished cluster id.
func TestMaybeLeaveKeepsSiblingPendingInvite(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	m.setInviteCreatedCluster(true)

	now := time.Now().UnixMilli()
	m.putInvite(&Invite{
		InviteID:     "inv-declined",
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStateDeclined,
		CreatedAt:    now,
	})
	m.putInvite(&Invite{
		InviteID:     "inv-sibling",
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStatePending,
		CreatedAt:    now,
	})

	require.False(t, m.maybeLeaveInviteCreatedCluster(), "must keep invite-created cluster while a sibling outbound invite is pending")
	id, _ := m.clusterIdentity()
	require.NotEqual(t, "", id, "invite-created cluster was erased while sibling invite still pending")

	// Finish the sibling; now cleanup should leave.
	m.finishInvite("inv-sibling", inviteStateDeclined)
	require.True(t, m.maybeLeaveInviteCreatedCluster(), "invite-created solo cluster must leave once no pending outbound remains")
	id, _ = m.clusterIdentity()
	require.Equal(t, "", id, "expected unclustered after last invite declined")
}

// TestExpirePendingInviteCleansThrowaway is the lost-decline fallback: if the
// joiner's decline POST never arrives, the inviter expires the pending invite
// after the TTL and dissolves an invite-created solo cluster.
func TestExpirePendingInviteCleansThrowaway(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	m.setInviteCreatedCluster(true)

	created := time.Now().Add(-time.Hour).UnixMilli()
	m.putInvite(&Invite{
		InviteID:     "inv-stale",
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStatePending,
		CreatedAt:    created,
	})
	m.putSession(&pairingSession{inviteID: "inv-stale", role: roleInviter})

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })

	m.expirePendingInvites(time.Now())

	inv, ok := m.getInvite("inv-stale")
	require.True(t, ok, "invite state (%v)", inv)
	require.Equal(t, inviteStateExpired, inv.State, "invite state (%v)", inv)
	_, ok = m.getSession("inv-stale")
	require.False(t, ok, "expired invite must drop its EAP session")
	id, _ := m.clusterIdentity()
	require.Equal(t, "", id, "invite-created solo cluster must leave after expiry")
}

// TestExpirePendingInviteClearsBothSides verifies the reconciled two-sided expiry
// while an older invite is being superseded: when the receiver's inbound invite
// times out it expires locally and signals the inviter (phase:"expire") so the
// inviter's outbound invite clears too. This replaces the older inviter->joiner
// cancel-on-expiry path — the receiver now owns its own teardown, and each side's
// own TTL remains a local fallback.
func TestExpirePendingInviteClearsBothSides(t *testing.T) {
	inviter := newTestManager(t)
	receiver := newTestManager(t)
	const inviteID = "inv-expire-remote"

	// The inviter holds a pending outbound invite. Its TTL is not overridden
	// here, so the only thing that can clear it in this test is the receiver's
	// phase:"expire" signal (proving the cross-node teardown, not a local timer).
	inviter.putInvite(&Invite{
		InviteID:     inviteID,
		FromNodeUUID: inviter.identity.NodeUUID,
		State:        inviteStatePending,
		CreatedAt:    time.Now().UnixMilli(),
	})
	inviter.putSession(&pairingSession{inviteID: inviteID, role: roleInviter})

	// Route the receiver's terminal signal to the inviter's pairing handler.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env pairingEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, "decode pairing envelope", http.StatusBadRequest)
			return
		}
		inviter.handlePairingExpired(w, &env)
	}))
	defer server.Close()

	// The receiver holds the matching pending inbound invite + joiner session
	// (addr points at the inviter's pairing endpoint) + tentative member.
	receiver.setClusterIdentity("", "")
	peer := &PairingInfo{
		NodeID:   inviter.identity.NodeID,
		NodeUUID: inviter.identity.NodeUUID,
		Name:     inviter.identity.Name,
	}
	toID := receiver.identity.NodeID
	receiver.putInvite(&Invite{
		InviteID:     inviteID,
		FromNodeID:   inviter.identity.NodeID,
		FromNodeUUID: inviter.identity.NodeUUID,
		ToNodeID:     &toID,
		State:        inviteStatePending,
		CreatedAt:    time.Now().Add(-time.Hour).UnixMilli(),
	})
	receiver.putSession(&pairingSession{
		inviteID:    inviteID,
		role:        roleJoiner,
		addr:        strings.TrimPrefix(server.URL, "http://"),
		peerPairing: peer,
	})
	receiver.recordMember(peer, statePendingInbound, nil)

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })
	receiver.expirePendingInvites(time.Now())

	// The receiver cleared itself as expired.
	inv, ok := receiver.getInvite(inviteID)
	require.True(t, ok, "receiver invite (%v)", inv)
	require.Equal(t, inviteStateExpired, inv.State, "receiver invite (%v)", inv)
	_, ok = receiver.getSession(inviteID)
	require.False(t, ok, "receiver retained session after expiry")
	_, ok = receiver.memberByNodeID(inviter.identity.NodeUUID)
	require.False(t, ok, "receiver retained pending member after expiry")

	// The inviter cleared its outbound invite via the receiver's phase:"expire".
	deadline := time.Now().Add(2 * time.Second)
	for {
		inv, ok := inviter.getInvite(inviteID)
		if ok && inv.State == inviteStateExpired {
			break
		}
		require.False(t, time.Now().After(deadline), "inviter invite (%v)", inv)
		time.Sleep(10 * time.Millisecond)
	}
	_, ok = inviter.getSession(inviteID)
	require.False(t, ok, "inviter retained session after receiver expiry signal")
}

// TestExpirePendingInvitePreservesIntentionalSolo verifies TTL expiry does not
// dissolve a user-founded cluster of one.
func TestExpirePendingInvitePreservesIntentionalSolo(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	// inviteCreated remains false (intentional).

	created := time.Now().Add(-time.Hour).UnixMilli()
	m.putInvite(&Invite{
		InviteID:     "inv-stale",
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStatePending,
		CreatedAt:    created,
	})

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })

	m.expirePendingInvites(time.Now())

	inv, ok := m.getInvite("inv-stale")
	require.True(t, ok, "invite state (%v)", inv)
	require.Equal(t, inviteStateExpired, inv.State, "invite state (%v)", inv)
	id, _ := m.clusterIdentity()
	require.NotEqual(t, "", id, "intentional solo cluster must survive invite expiry")
}

// TestInviteCreatedProvenanceSurvivesRestart covers the restart half of the
// lost-decline fallback. Pairing sessions are intentionally in-memory, so after
// restart a durably marked invite-created solo cluster can no longer complete
// and must be cleaned up when its identity is restored.
func TestInviteCreatedProvenanceSurvivesRestart(t *testing.T) {
	configDir := t.TempDir()
	newManager := func() *Manager {
		t.Helper()
		codec := NewCodec(struct {
			io.Reader
			io.Writer
		}{strings.NewReader(""), io.Discard})
		mgr, err := NewManager(codec, configDir, 14998)
		require.NoError(t, err, "new manager")
		return mgr
	}

	first := newManager()
	first.setClusterIdentity("invite-cluster", "Invite Lab")
	first.addSelfMember()
	first.setInviteCreatedCluster(true)

	restarted := newManager()
	restarted.setClusterIdentity("invite-cluster", "Invite Lab")
	require.True(t, restarted.restoreInviteCreatedCluster("invite-cluster"), "restart lost invite-created cluster provenance")
	require.True(t, restarted.maybeLeaveInviteCreatedCluster(), "restarted inviter must clean up orphaned invite-created cluster")
	id, _ := restarted.clusterIdentity()
	require.Equal(t, "", id, "restarted inviter remained clustered")
}
