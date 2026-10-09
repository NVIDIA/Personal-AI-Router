// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// putPendingInviterSession registers a pending outbound invite plus its
// inviter-role EAP session, the state the inviter holds while it waits for the
// joiner to complete pairing. Mirrors what runInitialExchange records before a
// joiner-driven Completion (or, here, a terminal fail signal) lands.
func putPendingInviterSession(t *testing.T, m *Manager, inviteID string) {
	t.Helper()
	m.putInvite(&Invite{
		InviteID:     inviteID,
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStatePending,
		CreatedAt:    time.Now().UnixMilli(),
	})
	m.putSession(&pairingSession{inviteID: inviteID, role: roleInviter})
}

// TestHandlePairingFailedReasonGuard verifies the inviter only honors a reason
// it recognizes when a joiner signals phase:"fail" over the un-pinned pairing
// channel. A wrong-PIN reason is stamped through; any other (attacker-chosen)
// text is sanitized to empty so a peer can't inject arbitrary copy onto the
// invite, and an empty reason (e.g. a transport-failure fail) still cleans up.
// Every case ends the invite in state:"failed".
func TestHandlePairingFailedReasonGuard(t *testing.T) {
	test := func(name, sentReason, wantReason string) {
		t.Run(name, func(t *testing.T) {
			m := newTestManager(t) // clustered as cluster-1, inviteCreated=false
			m.addSelfMember()
			putPendingInviterSession(t, m, "inv-fail")

			m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
				InviteID: "inv-fail", Phase: "fail", Reason: sentReason,
			})

			inv, ok := m.getInvite("inv-fail")
			require.True(t, ok, "invite missing after fail signal; want a failed record")
			require.Equal(t, inviteStateFailed, inv.State, "invite state")
			require.Equal(t, wantReason, inv.Reason, "invite reason")
			// The EAP session is always torn down (PIN invalidated), whatever
			// the reason.
			_, ok = m.getSession("inv-fail")
			require.False(t, ok, "fail signal must drop the inviter's EAP session")
			// An intentional (non-invite-created) cluster is preserved.
			id, _ := m.clusterIdentity()
			require.NotEqual(t, "", id, "intentional cluster erased by a fail signal")
		})
	}
	test("wrong pin is honored", reasonIncorrectPIN, reasonIncorrectPIN)
	test("arbitrary reason is sanitized", "evil-arbitrary-text", "")
	test("empty reason is preserved", "", "")
}

// TestHandlePairingFailedIdempotent verifies a duplicate or late fail signal is
// a silent no-op: once the invite is terminal and the session evicted, a second
// delivery (even one carrying a different reason) neither panics nor rewrites
// the already-stamped outcome.
func TestHandlePairingFailedIdempotent(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	putPendingInviterSession(t, m, "inv-dup")

	first := &pairingEnvelope{InviteID: "inv-dup", Phase: "fail", Reason: reasonIncorrectPIN}
	m.handlePairingFailed(httptest.NewRecorder(), first)

	inv, ok := m.getInvite("inv-dup")
	require.True(t, ok, "after first fail: invite (%v)", inv)
	require.Equal(t, inviteStateFailed, inv.State, "after first fail: invite (%v)", inv)
	require.Equal(t, reasonIncorrectPIN, inv.Reason, "after first fail: invite (%v)", inv)

	// Redeliver with a different reason; the session is gone so it must no-op.
	m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: "inv-dup", Phase: "fail", Reason: "something-else",
	})

	inv, ok = m.getInvite("inv-dup")
	require.True(t, ok, "after duplicate fail: invite (%v)", inv)
	require.Equal(t, inviteStateFailed, inv.State, "after duplicate fail: invite (%v)", inv)
	require.Equal(t, reasonIncorrectPIN, inv.Reason, "after duplicate fail: invite (%v)", inv)
}

// TestHandlePairingFailedNonInviterIgnored verifies the guard on session role:
// a fail signal that correlates to a non-inviter (or unknown) session is
// ignored, so it can't flip an unrelated or inbound invite to failed.
func TestHandlePairingFailedNonInviterIgnored(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()

	// Unknown invite/session: no-op (no panic, nothing recorded).
	m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: "inv-unknown", Phase: "fail", Reason: reasonIncorrectPIN,
	})
	_, ok := m.getInvite("inv-unknown")
	require.False(t, ok, "fail signal for an unknown invite must not create a record")

	// A joiner-role session must not be torn down by an inviter fail signal.
	m.putInvite(&Invite{
		InviteID:     "inv-inbound",
		FromNodeUUID: "some-peer",
		State:        inviteStatePending,
		CreatedAt:    time.Now().UnixMilli(),
	})
	m.putSession(&pairingSession{inviteID: "inv-inbound", role: roleJoiner})

	m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: "inv-inbound", Phase: "fail", Reason: reasonIncorrectPIN,
	})

	inv, ok := m.getInvite("inv-inbound")
	require.True(t, ok, "inbound invite state (%v)", inv)
	require.Equal(t, inviteStatePending, inv.State, "inbound invite state (%v)", inv)
	_, ok = m.getSession("inv-inbound")
	require.True(t, ok, "joiner session wrongly evicted by an inviter fail signal")
}
