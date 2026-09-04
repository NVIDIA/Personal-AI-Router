// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"eapnoob"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postPairing drives one plain-HTTP pairing envelope through handlePairing via
// a real server mux, so status-code gates are exercised the way a remote peer
// hits them.
func postPairing(t *testing.T, m *Manager, env pairingEnvelope) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(pairingPath, m.handlePairing)
	req := httptest.NewRequest(http.MethodPost, pairingPath, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// signalTagFor computes the terminal-signal MAC the same way the joiner would.
func signalTagFor(t *testing.T, sess *pairingSession, inviteID, phase string) string {
	t.Helper()
	key, err := sess.ephemeralKey()
	if err != nil {
		t.Fatalf("session ephemeral key: %v", err)
	}
	return pairingSignalMAC(key, inviteID, phase)
}

// putPendingInviterSessionWithEAP registers a pending outbound invite plus an
// inviter-role session carrying a live EAP-NOOB Server that has run a Key
// Exchange (so the ephemeral secret exists and signal MACs can be derived),
// mirroring the state runInitialExchange records once the joiner's Initial
// Exchange completes its Key Exchange round.
func putPendingInviterSessionWithEAP(t *testing.T, m *Manager, inviteID string) *pairingSession {
	t.Helper()
	info, err := m.localPairingInfo("127.0.0.1:1").toMap()
	if err != nil {
		t.Fatalf("pairing info: %v", err)
	}
	server := newPairingServer(info)
	// Drive a real Initial Exchange against a Peer so the server holds the
	// ECDH shared secret (z) the signal MAC derives from. PeerInfo mirrors a
	// joiner's pre-adoption identity (no cluster yet).
	peer := eapnoob.NewPeer(eapnoob.PeerConfig{PreferDir: 2, PeerInfo: map[string]any{"role": "peer"}}, nil)
	msg, err := server.Start()
	if err != nil {
		t.Fatalf("server start: %v", err)
	}
	peerTurn := true
	for {
		var out eapnoob.Outcome
		if peerTurn {
			out, err = peer.Receive(msg)
		} else {
			out, err = server.Receive(msg)
		}
		if err != nil || out.Err != nil {
			t.Fatalf("initial exchange round: err=%v protocolErr=%v", err, out.Err)
		}
		if len(out.Send) == 0 {
			break
		}
		msg = out.Send
		peerTurn = !peerTurn
	}
	if server.State() != eapnoob.StateWaiting {
		t.Fatalf("server state %s after Initial Exchange, want waiting", server.State())
	}
	sess := &pairingSession{
		inviteID:  inviteID,
		role:      roleInviter,
		createdAt: time.Now().UnixMilli(),
		server:    server,
	}
	m.putInvite(&Invite{
		InviteID:     inviteID,
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStatePending,
		CreatedAt:    time.Now().UnixMilli(),
	})
	m.putSession(sess)
	return sess
}

// TestPairingSignalGateRejectsUnauthenticated tests the 401 gate added in front
// of the cancel/decline/expire signal phases: a missing tag, a garbage tag, and
// a tag MACed for the wrong phase or wrong invite all get rejected, and none of
// them may tear down the invite or session they target. A correctly MACed tag
// passes the same gate.
func TestPairingSignalGateRejectsUnauthenticated(t *testing.T) {
	for _, phase := range []string{"cancel", "decline", "expire"} {
		t.Run(phase, func(t *testing.T) {
			m := newTestManager(t)
			m.addSelfMember()

			cases := []struct {
				name     string
				tag      func(freshSess *pairingSession) string
				inviteID string
			}{
				// Every tag is computed against the fresh session re-put for
				// the case, so the wrong-phase and wrong-invite negatives
				// fail on exactly the dimension under test — not on a stale
				// key from an earlier session.
				{"missing tag", func(*pairingSession) string { return "" }, "inv-gate"},
				{"garbage tag", func(*pairingSession) string { return "not-a-mac" }, "inv-gate"},
				{"wrong phase tag", func(s *pairingSession) string { return signalTagFor(t, s, "inv-gate", "fail") }, "inv-gate"},
				{"wrong invite tag", func(s *pairingSession) string { return signalTagFor(t, s, "inv-other", phase) }, "inv-gate"},
				{"valid tag", func(s *pairingSession) string { return signalTagFor(t, s, "inv-gate", phase) }, "inv-gate"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					// Re-put a pending invite/session for every case so a
					// preceding teardown never masks the gate under test.
					freshSess := putPendingInviterSessionWithEAP(t, m, "inv-gate")
					rec := postPairing(t, m, pairingEnvelope{
						InviteID: tc.inviteID, Phase: phase, SignalTag: tc.tag(freshSess),
					})
					// The valid tag is admitted past the 401 gate (the phase
					// handler then answers with its own status); every other
					// case must hit the gate and leave state untouched.
					if tc.name == "valid tag" {
						if rec.Code == http.StatusUnauthorized {
							t.Fatalf("valid %s tag rejected with 401; gate is too strict", phase)
						}
						return
					}
					if rec.Code != http.StatusUnauthorized {
						t.Fatalf("%s: status = %d, want 401", tc.name, rec.Code)
					}
					if inv, ok := m.getInvite("inv-gate"); !ok || inv.State != inviteStatePending {
						t.Fatalf("%s: unauthenticated signal tore down the invite (state=%v, present=%v)", tc.name, inv.State, ok)
					}
					if _, ok := m.getSession("inv-gate"); !ok {
						t.Fatalf("%s: unauthenticated signal dropped the session", tc.name)
					}
				})
			}
		})
	}
}

// TestCompletionAttemptsRateLimit verifies the PIN brute-force cap: attempts 1
// through maxCompletionAttempts are not limited, the next one returns 429, and
// the over-limit attempt tears down both the invite (failed/incorrect-pin) and
// the EAP session so a resumed attack needs a fresh invite.
func TestCompletionAttemptsRateLimit(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	sess := putPendingInviterSessionWithEAP(t, m, "inv-rate")
	if sess.server == nil {
		t.Fatal("inviter session with EAP server missing")
	}

	for i := 1; i <= maxCompletionAttempts; i++ {
		// A non-empty (garbage) EAP message reaches the attempt counter; an
		// empty msg is the kickoff POST and returns before it.
		rec := postPairing(t, m, pairingEnvelope{InviteID: "inv-rate", Phase: "completion", Msg: base64.StdEncoding.EncodeToString([]byte("guess"))})
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d (of %d allowed) was rate limited", i, maxCompletionAttempts)
		}
		if rec.Code >= 500 {
			t.Fatalf("attempt %d: unexpected server error %d", i, rec.Code)
		}
	}

	rec := postPairing(t, m, pairingEnvelope{InviteID: "inv-rate", Phase: "completion", Msg: base64.StdEncoding.EncodeToString([]byte("guess"))})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit completion status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if inv, ok := m.getInvite("inv-rate"); !ok || inv.State != inviteStateFailed || inv.Reason != reasonIncorrectPIN {
		t.Fatalf("invite after over-limit = %+v (present %v), want failed/incorrect-pin", inv, ok)
	}
	if _, ok := m.getSession("inv-rate"); ok {
		t.Fatal("over-limit completion left the EAP session alive; a resumed attack keeps its transcript")
	}
}

// TestCancelInviteNoSignalKey exercises the cancel branch that runs before the
// Initial Exchange completed: the session has no ephemeral key, so the notify
// is skipped, but the invite must still be canceled and the session deleted,
// with the terminal write staying serialized under sess.mu. The codec writer is
// a bytes.Buffer, so the response frame is captured and its error payload
// asserted too.
func TestCancelInviteNoSignalKey(t *testing.T) {
	var out bytes.Buffer
	codec := NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), &out})
	dir := t.TempDir()
	mgr, err := NewManager(codec, dir, 14999)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := mgr.ensureAdmission("cluster-1"); err != nil {
		t.Fatalf("establish admission: %v", err)
	}
	mgr.setClusterIdentity("cluster-1", "Lab")
	m := mgr
	m.addSelfMember()

	// A pending inviter session whose EAP server never started holds no Key
	// Exchange secret — exactly the state a cancel racing the Initial Exchange
	// observes.
	m.putInvite(&Invite{
		InviteID:     "inv-nokey",
		FromNodeUUID: m.identity.NodeUUID,
		State:        inviteStatePending,
		CreatedAt:    time.Now().UnixMilli(),
	})
	m.putSession(&pairingSession{inviteID: "inv-nokey", role: roleInviter})

	id := json.RawMessage(`"cancel-nokey-1"`)
	params, err := json.Marshal(map[string]string{"inviteId": "inv-nokey"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	m.handleCancelInvite(&Message{ID: &id, Method: "cluster:cancel-invite", Params: params})

	if inv, ok := m.getInvite("inv-nokey"); !ok || inv.State != inviteStateCanceled {
		t.Fatalf("invite state = %+v (present %v), want canceled", inv, ok)
	}
	if _, ok := m.getSession("inv-nokey"); ok {
		t.Fatal("session survived a no-signal-key cancel")
	}
	resp, err := io.ReadAll(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(resp), "invite is not pending") {
		t.Fatalf("cancel response %q does not report the invalid-state error", string(resp))
	}
	if !strings.Contains(string(resp), string(inviteStateCanceled)) {
		t.Fatalf("cancel response %q does not carry the canceled state", string(resp))
	}
}
