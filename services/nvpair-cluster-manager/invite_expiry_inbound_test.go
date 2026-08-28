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
)

// putPendingJoinerSession registers a pending INBOUND invite (one this node
// received) plus its joiner-role EAP session and the tentative pending-inbound
// member, the state a receiver holds after the Initial Exchange while it waits
// for the local user to enter the PIN. fromUUID is the inviter's uuid (never
// this node's, so the invite classifies as inbound). inviterAddr, when set, is
// where the receiver will POST its phase:"expire" signal on timeout.
func putPendingJoinerSession(t *testing.T, m *Manager, inviteID, fromUUID, inviterAddr string, createdAt int64) {
	t.Helper()
	toID := m.identity.NodeID
	m.putInvite(&Invite{
		InviteID:     inviteID,
		FromNodeID:   "inviter-node",
		FromNodeUUID: fromUUID,
		ToNodeID:     &toID,
		ClusterID:    "cluster-remote",
		State:        inviteStatePending,
		CreatedAt:    createdAt,
	})
	pi := &PairingInfo{NodeUUID: fromUUID, NodeID: "inviter-node", ClusterID: "cluster-remote"}
	m.putSession(&pairingSession{
		inviteID:    inviteID,
		role:        roleJoiner,
		addr:        inviterAddr,
		peerPairing: pi,
	})
	m.upsertMember(&ClusterNode{NodeUUID: fromUUID, ID: "inviter-node", State: statePendingInbound})
}

// TestExpireInboundInviteTearsDownReceiver verifies that a pending
// inbound invite the local user never answered expires after the TTL, dropping
// the tentative member and EAP session and flipping the invite to expired — the
// receiver's own reaper, not just the inviter's outbound sweep, returns it to a
// usable state.
func TestExpireInboundInviteTearsDownReceiver(t *testing.T) {
	m := newTestManager(t)
	// A receiver deciding on an inbound invite is not itself clustered; clear the
	// test manager's default identity so the setup matches a real joiner.
	m.setClusterIdentity("", "")

	const inviteID = "inv-inbound-stale"
	const inviterUUID = "inviter-uuid"
	created := time.Now().Add(-time.Hour).UnixMilli()
	// No inviter address: notifyInviterTerminal short-circuits, so the test does
	// not depend on network I/O to observe the local teardown.
	putPendingJoinerSession(t, m, inviteID, inviterUUID, "", created)

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })

	m.expirePendingInvites(time.Now())

	inv, ok := m.getInvite(inviteID)
	if !ok || inv.State != inviteStateExpired {
		t.Fatalf("invite state = %+v, want expired", inv)
	}
	if inv.Pin != nil {
		t.Fatal("expired invite must not carry a PIN")
	}
	if _, ok := m.getSession(inviteID); ok {
		t.Fatal("expired inbound invite must drop its joiner EAP session")
	}
	if _, ok := m.memberByNodeID(inviterUUID); ok {
		t.Fatal("expired inbound invite must drop the tentative pending-inbound member")
	}
}

// TestExpireInboundInviteSignalsInviter verifies the receiver best-effort POSTs
// a phase:"expire" terminal signal to the inviter on timeout, so the inviter can
// tear its outbound invite down immediately rather than waiting for its own TTL.
func TestExpireInboundInviteSignalsInviter(t *testing.T) {
	m := newTestManager(t)
	m.setClusterIdentity("", "")

	got := make(chan pairingEnvelope, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pairingPath {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var env pairingEnvelope
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		_ = json.Unmarshal(body, &env)
		respondPairing(w, nil)
		select {
		case got <- env:
		default:
		}
	}))
	defer srv.Close()

	const inviteID = "inv-inbound-signal"
	created := time.Now().Add(-time.Hour).UnixMilli()
	putPendingJoinerSession(t, m, inviteID, "inviter-uuid", strings.TrimPrefix(srv.URL, "http://"), created)

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })

	m.expirePendingInvites(time.Now())

	select {
	case env := <-got:
		if env.Phase != "expire" {
			t.Fatalf("inviter signal phase = %q, want %q", env.Phase, "expire")
		}
		if env.InviteID != inviteID {
			t.Fatalf("inviter signal inviteId = %q, want %q", env.InviteID, inviteID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receiver did not signal the inviter on inbound expiry")
	}
}

// TestExpireInboundInvitePreservesFresh verifies an inbound invite still inside
// its TTL is left pending — the reaper only reaps genuinely stale prompts.
func TestExpireInboundInvitePreservesFresh(t *testing.T) {
	m := newTestManager(t)
	m.setClusterIdentity("", "")

	const inviteID = "inv-inbound-fresh"
	putPendingJoinerSession(t, m, inviteID, "inviter-uuid", "", time.Now().UnixMilli())

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })

	m.expirePendingInvites(time.Now())

	inv, ok := m.getInvite(inviteID)
	if !ok || inv.State != inviteStatePending {
		t.Fatalf("fresh inbound invite state = %+v, want still pending", inv)
	}
	if _, ok := m.getSession(inviteID); !ok {
		t.Fatal("fresh inbound invite must keep its joiner session")
	}
}

// TestExpireInboundInviteNoSession verifies the reaper's defensive path: a
// pending inbound invite whose joiner session is (unexpectedly) gone must still
// be expired rather than lingering forever, since without a session it can no
// longer be accepted either.
func TestExpireInboundInviteNoSession(t *testing.T) {
	m := newTestManager(t)
	m.setClusterIdentity("", "")

	const inviteID = "inv-inbound-nosession"
	toID := m.identity.NodeID
	m.putInvite(&Invite{
		InviteID:     inviteID,
		FromNodeUUID: "inviter-uuid",
		ToNodeID:     &toID,
		State:        inviteStatePending,
		CreatedAt:    time.Now().Add(-time.Hour).UnixMilli(),
	})
	// Deliberately no session registered for this invite.

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })

	m.expirePendingInvites(time.Now())

	inv, ok := m.getInvite(inviteID)
	if !ok || inv.State != inviteStateExpired {
		t.Fatalf("sessionless inbound invite state = %+v, want expired", inv)
	}
}

// TestExpireInboundSkipsInFlightAccept guards the accept-vs-expiry race: while an
// accept holds sess.mu across its Completion Exchange, the reaper must not expire
// the invite out from under it. The reaper best-effort acquires sess.mu (TryLock)
// and, failing that, leaves the invite pending for the next sweep — so a stale
// TTL never tears down a pairing the local user is actively completing.
func TestExpireInboundSkipsInFlightAccept(t *testing.T) {
	m := newTestManager(t)
	m.setClusterIdentity("", "")

	const inviteID = "inv-inflight"
	const inviterUUID = "inviter-uuid"
	created := time.Now().Add(-time.Hour).UnixMilli()
	putPendingJoinerSession(t, m, inviteID, inviterUUID, "", created)

	inviteTTLOverride = time.Minute
	t.Cleanup(func() { inviteTTLOverride = 0 })

	// Simulate an accept mid-completion: hold sess.mu the way
	// runCompletionExchangeLocked does across its network round-trips.
	sess, ok := m.getSession(inviteID)
	if !ok {
		t.Fatal("setup did not register the joiner session")
	}
	sess.mu.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.expirePendingInvites(time.Now())
	}()

	// The reaper must not expire the invite while the accept holds sess.mu.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expirePendingInvites parked on sess.mu instead of skipping the busy session")
	}
	if inv, _ := m.getInvite(inviteID); inv == nil || inv.State != inviteStatePending {
		t.Fatalf("invite state = %+v, want still pending during in-flight accept", inv)
	}
	if _, ok := m.getSession(inviteID); !ok {
		t.Fatal("in-flight accept must keep its joiner session")
	}
	if _, ok := m.memberByNodeID(inviterUUID); !ok {
		t.Fatal("in-flight accept must keep its tentative pending-inbound member")
	}

	sess.mu.Unlock()
}

// TestHandlePairingExpiredTearsDownInviter verifies the inviter side of the
// two-sided teardown: a phase:"expire" signal from a receiver whose inbound
// invite timed out flips the matching outbound invite to expired and drops the
// EAP session, instead of leaving the inviter stranded on a pending invite until
// its own TTL. An intentional (non-invite-created) cluster is preserved.
func TestHandlePairingExpiredTearsDownInviter(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	putPendingInviterSession(t, m, "inv-expire")

	m.handlePairingExpired(httptest.NewRecorder(), &pairingEnvelope{InviteID: "inv-expire", Phase: "expire"})

	inv, ok := m.getInvite("inv-expire")
	if !ok || inv.State != inviteStateExpired {
		t.Fatalf("invite state = %+v, want expired", inv)
	}
	if _, ok := m.getSession("inv-expire"); ok {
		t.Fatal("expire signal must drop the inviter's EAP session")
	}
	if id, _ := m.clusterIdentity(); id == "" {
		t.Fatal("intentional cluster erased by an expire signal")
	}
}
