// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log"
	"net/http"
)

type cancelInviteParams struct {
	InviteID string `json:"inviteId"`
}

// handleCancelInvite aborts a pending outbound (inviter-side) invite: it tears
// down the EAP-NOOB Server session — which invalidates the PIN, so a later
// joiner-driven Completion POST now hits the 409 unknown-invite branch and can
// no longer complete the join — flips the invite to canceled, and best-effort
// notifies the joiner so its pending PIN prompt clears. Valid only for a
// pending outbound invite (mirroring respond-to-invite's guards): an inbound or
// non-pending invite returns -32002, an unknown/evicted invite -32001 (§7.0).
func (m *Manager) handleCancelInvite(msg *Message) {
	var p cancelInviteParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
		return
	}
	if p.InviteID == "" {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "inviteId is required", map[string]string{"field": "inviteId"})
		return
	}

	inv, ok := m.getInvite(p.InviteID)
	if !ok {
		m.codec.RespondErrorData(msg.ID, codeUnknownInvite, "unknown inviteId", map[string]string{"inviteId": p.InviteID})
		return
	}
	sess, ok := m.getSession(p.InviteID)
	if !ok {
		// No live session means the pairing already reached a terminal state
		// (paired/failed/declined/canceled) — nothing left to cancel.
		m.codec.RespondErrorData(msg.ID, codeInvalidState, "invite is not pending",
			map[string]any{"inviteId": p.InviteID, "state": inv.State})
		return
	}
	if sess.role != roleInviter {
		m.codec.RespondErrorData(msg.ID, codeInvalidState, "invite is inbound, cannot cancel locally",
			map[string]any{"inviteId": p.InviteID, "state": inv.State})
		return
	}

	// Authoritative section. The joiner-driven Completion holds sess.mu across
	// its verify → pin → record → flip-to-paired, and handlePairingCompletion
	// re-reads the invite state under the same lock. Taking sess.mu here and
	// re-reading the state makes cancel and Completion mutually exclusive: a
	// Completion that already committed is honored (we report the terminal state
	// rather than tearing down a finished pairing and adding a member behind it),
	// and a cancel that wins deterministically prevents the join.
	m.inviteMu.Lock()
	sess.mu.Lock()
	cur, ok := m.getInvite(p.InviteID)
	if !ok || cur.State != inviteStatePending {
		sess.mu.Unlock()
		m.inviteMu.Unlock()
		state := inviteStateFailed
		if ok {
			state = cur.State
		}
		m.codec.RespondErrorData(msg.ID, codeInvalidState, "invite is not pending",
			map[string]any{"inviteId": p.InviteID, "state": state})
		return
	}
	// Deleting the Server session invalidates the PIN; a later Completion POST
	// then hits handlePairingCompletion's unknown-invite / not-pending branches.
	// The signal MAC is derived first: it needs the session's ephemeral Key
	// Exchange secret, which dies with the session. (sess.mu is already held.)
	signalKey, keyErr := sess.ephemeralKey()
	if keyErr != nil {
		// No Key Exchange secret (initial exchange never completed) — the joiner
		// has no session to clear either, so skipping the notify is safe.
		m.finishInvite(p.InviteID, inviteStateCanceled)
		m.deleteSession(p.InviteID)
		m.maybeLeaveInviteCreatedClusterLocked()
		m.inviteMu.Unlock()
		log.Printf("invite %s: canceled by inviter (no joiner signal key; skipping notify: %v)", p.InviteID, keyErr)
		m.codec.RespondErrorData(msg.ID, codeInvalidState, "invite is not pending",
			map[string]any{"inviteId": p.InviteID, "state": inviteStateCanceled})
		return
	}
	signalTag := pairingSignalMAC(signalKey, p.InviteID, "cancel")
	m.finishInvite(p.InviteID, inviteStateCanceled)
	m.deleteSession(p.InviteID)
	joinerAddr := sess.joinerAddr
	sess.mu.Unlock()
	m.maybeLeaveInviteCreatedClusterLocked()
	m.inviteMu.Unlock()

	// Best-effort: tell the joiner so its pending prompt clears. If the joiner is
	// unreachable, the teardown above still prevents the join.
	go m.notifyJoinerCanceled(joinerAddr, p.InviteID, signalTag)

	log.Printf("invite %s: canceled by inviter", p.InviteID)
	m.respondInvite(msg, p.InviteID)
}

// notifyJoinerCanceled POSTs a best-effort "cancel" pairing envelope to the
// joiner so it can drop its pending-inbound invite and dismiss the PIN prompt.
// signalTag authenticates the signal (see pairingSignalMAC). Failures are
// logged and ignored — the inviter-side teardown is authoritative.
func (m *Manager) notifyJoinerCanceled(joinerAddr, inviteID, signalTag string) {
	if joinerAddr == "" {
		return
	}
	client := &http.Client{Timeout: pairingHTTPTimeout}
	if _, err := postPairingBlobTagged(client, joinerAddr, inviteID, "cancel", nil, signalTag); err != nil {
		log.Printf("invite %s: notify joiner cancel: %v", inviteID, err)
	}
}
