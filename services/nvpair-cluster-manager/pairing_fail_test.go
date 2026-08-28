// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http/httptest"
	"testing"
	"time"
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
	cases := []struct {
		name       string
		sentReason string
		wantReason string
	}{
		{"wrong pin is honored", reasonIncorrectPIN, reasonIncorrectPIN},
		{"arbitrary reason is sanitized", "evil-arbitrary-text", ""},
		{"empty reason is preserved", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t) // clustered as cluster-1, inviteCreated=false
			m.addSelfMember()
			putPendingInviterSession(t, m, "inv-fail")

			m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
				InviteID: "inv-fail", Phase: "fail", Reason: tc.sentReason,
			})

			inv, ok := m.getInvite("inv-fail")
			if !ok {
				t.Fatal("invite missing after fail signal; want a failed record")
			}
			if inv.State != inviteStateFailed {
				t.Fatalf("invite state = %q, want %q", inv.State, inviteStateFailed)
			}
			if inv.Reason != tc.wantReason {
				t.Fatalf("invite reason = %q, want %q", inv.Reason, tc.wantReason)
			}
			// The EAP session is always torn down (PIN invalidated), whatever
			// the reason.
			if _, ok := m.getSession("inv-fail"); ok {
				t.Fatal("fail signal must drop the inviter's EAP session")
			}
			// An intentional (non-invite-created) cluster is preserved.
			if id, _ := m.clusterIdentity(); id == "" {
				t.Fatal("intentional cluster erased by a fail signal")
			}
		})
	}
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
	if !ok || inv.State != inviteStateFailed || inv.Reason != reasonIncorrectPIN {
		t.Fatalf("after first fail: invite = %+v, want failed + incorrect-pin", inv)
	}

	// Redeliver with a different reason; the session is gone so it must no-op.
	m.handlePairingFailed(httptest.NewRecorder(), &pairingEnvelope{
		InviteID: "inv-dup", Phase: "fail", Reason: "something-else",
	})

	inv, ok = m.getInvite("inv-dup")
	if !ok || inv.State != inviteStateFailed || inv.Reason != reasonIncorrectPIN {
		t.Fatalf("after duplicate fail: invite = %+v, want unchanged failed + incorrect-pin", inv)
	}
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
	if _, ok := m.getInvite("inv-unknown"); ok {
		t.Fatal("fail signal for an unknown invite must not create a record")
	}

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
	if !ok || inv.State != inviteStatePending {
		t.Fatalf("inbound invite state = %+v, want still pending (fail ignored for non-inviter)", inv)
	}
	if _, ok := m.getSession("inv-inbound"); !ok {
		t.Fatal("joiner session wrongly evicted by an inviter fail signal")
	}
}
