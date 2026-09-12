// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"eapnoob"
)

// completionAuthError signals the Completion Exchange failed EAP-NOOB MAC
// verification — i.e. the joiner entered the wrong PIN — as distinct from a
// transport/protocol failure (peer unreachable, malformed). The joiner maps it
// to reason:"incorrect-pin" and forwards that reason to the inviter.
type completionAuthError struct{ err error }

func (e *completionAuthError) Error() string {
	return "completion failed: incorrect pin: " + e.err.Error()
}

func (e *completionAuthError) Unwrap() error { return e.err }

type respondParams struct {
	InviteID string  `json:"inviteId"`
	Accept   *bool   `json:"accept"`
	Pin      *string `json:"pin"`
}

// handleRespondToInvite submits the PIN (or a decline) for a pending-inbound
// invite (§7.0). On accept it feeds the PIN to the EAP-NOOB Peer, drives the
// Completion Exchange as HTTP client, pins the inviter, records membership, and
// adopts the inviter's cluster identity.
func (m *Manager) handleRespondToInvite(msg *Message) {
	var p respondParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
		return
	}
	if p.InviteID == "" {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "inviteId is required", map[string]string{"field": "inviteId"})
		return
	}
	if p.Accept == nil {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "accept is required", map[string]string{"field": "accept"})
		return
	}

	inv, ok := m.getInvite(p.InviteID)
	if !ok {
		m.codec.RespondErrorData(msg.ID, codeUnknownInvite, "unknown inviteId", map[string]string{"inviteId": p.InviteID})
		return
	}
	sess, ok := m.getSession(p.InviteID)
	if !ok {
		m.codec.RespondErrorData(msg.ID, codeUnknownInvite, "invite session evicted", map[string]string{"inviteId": p.InviteID})
		return
	}
	if sess.role != roleJoiner {
		m.codec.RespondErrorData(msg.ID, codeInvalidState, "invite is outbound, cannot respond locally",
			map[string]any{"inviteId": p.InviteID, "state": inv.State})
		return
	}
	if inv.State != inviteStatePending {
		m.codec.RespondErrorData(msg.ID, codeInvalidState, "invite is not pending",
			map[string]any{"inviteId": p.InviteID, "state": inv.State})
		return
	}

	// Serialize the user's response with same-sender supersession. Re-read the
	// state after taking sess.mu because a newer invite may have canceled this
	// one after the optimistic checks above.
	sess.mu.Lock()
	inv, ok = m.getInvite(p.InviteID)
	if !ok || inv.State != inviteStatePending {
		sess.mu.Unlock()
		state := inviteStateCanceled
		if ok {
			state = inv.State
		}
		m.codec.RespondErrorData(msg.ID, codeInvalidState, "invite is not pending",
			map[string]any{"inviteId": p.InviteID, "state": state})
		return
	}

	if !*p.Accept {
		// Capture the inviter address before tearing the session down so we can
		// tell the inviter its outbound invite is declined (with retries). Local
		// teardown is authoritative even if the notify ultimately fails — the
		// inviter's invite TTL then expires the pending invite as a fallback.
		inviterAddr := sess.addr
		signalTag := sessionSignalTag(sess, p.InviteID, "decline")
		m.finishInvite(p.InviteID, inviteStateDeclined)
		if sess.peerPairing != nil {
			m.removeMemberByUUID(sess.peerPairing.NodeUUID)
		}
		m.deleteSession(p.InviteID)
		sess.mu.Unlock()
		m.emitNodesChanged()
		go m.notifyInviterTerminal(inviterAddr, p.InviteID, "decline", "", signalTag)
		log.Printf("invite %s: declined by local user", p.InviteID)
		m.respondInvite(msg, p.InviteID)
		return
	}

	if p.Pin == nil || !pinPattern.MatchString(*p.Pin) {
		sess.mu.Unlock()
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "pin must be six digits", map[string]string{"field": "pin"})
		return
	}

	// Re-check cluster identity at accept time. The handlePairingInitial guard
	// only rejects invites whose joiner session is created after we joined a
	// cluster; a session opened while we were still standalone survives into this
	// Completion path. Without this second check, a node holding two pending
	// invites from different clusters could accept one (becoming clustered) and
	// then accept the other, adopting a second cluster and leaving both trust
	// relationships inconsistent. If we are now clustered, refuse and clean up the
	// pending invite instead of running the Completion Exchange.
	if cid, _ := m.clusterIdentity(); cid != "" {
		log.Printf("invite %s: accept rejected, node is already clustered (%s)", p.InviteID, cid)
		m.finishInvite(p.InviteID, inviteStateRejected)
		if sess.peerPairing != nil {
			m.removeMemberByUUID(sess.peerPairing.NodeUUID)
		}
		m.deleteSession(p.InviteID)
		sess.mu.Unlock()
		m.emitNodesChanged()
		m.respondInvite(msg, p.InviteID)
		return
	}

	if err := m.runCompletionExchangeLocked(sess, *p.Pin); err != nil {
		log.Printf("invite %s: completion failed: %v", p.InviteID, err)
		// Every completion failure is mirrored to the inviter (phase:"fail"),
		// the same way a decline is — so a failed pairing tears down on both
		// sides immediately instead of stranding the inviter until its invite
		// TTL. Only a definitive wrong-PIN failure (a *completionAuthError from
		// an EAP-NOOB MAC/NoobId mismatch) carries reason:"incorrect-pin";
		// transport/other causes send an empty reason so the UI falls back to
		// generic copy.
		reason := ""
		var authErr *completionAuthError
		if errors.As(err, &authErr) {
			reason = reasonIncorrectPIN
		}
		// Capture the inviter address before teardown so we can still reach it
		// after the session is dropped. Local teardown is authoritative even if
		// the notify fails — the inviter's invite TTL is the remaining backstop.
		inviterAddr := sess.addr
		signalTag := sessionSignalTag(sess, p.InviteID, "fail")
		m.finishInviteReason(p.InviteID, inviteStateFailed, reason)
		if sess.peerPairing != nil {
			m.removeMemberByUUID(sess.peerPairing.NodeUUID)
		}
		m.deleteSession(p.InviteID)
		sess.mu.Unlock()
		m.emitNodesChanged()
		go m.notifyInviterTerminal(inviterAddr, p.InviteID, "fail", reason, signalTag)
		m.respondInvite(msg, p.InviteID)
		return
	}

	// Paired: persist the complete provisional roster and pin first, then make
	// admission.json authoritative as the final durable commit. A crash before
	// activation leaves a nonzero provisional self member that startup rolls
	// back; a crash after activation has every required member/pin already.
	pi := sess.peerPairing
	now := time.Now().UnixMilli()
	var pinErr error
	m.withClusterComposition(func() {
		if m.teardownPending.Load() {
			pinErr = fmt.Errorf("cluster teardown is pending")
			return
		}
		if m.removalProofBlocksAdmission(pi.NodeUUID, inv.ClusterID, pi.AdmissionEpoch) {
			pinErr = fmt.Errorf("peer admission %d was already removed", pi.AdmissionEpoch)
			return
		}
		admitted := *pi
		admitted.ClusterID = inv.ClusterID
		end := m.endorsePeerForAdmission(admitted.NodeUUID,
			certFingerprintFromDER(sess.peerCert.Raw), inv.ClusterID,
			admitted.AdmissionEpoch, sess.localAdmissionEpoch)
		rollback := func(cause error) error {
			var rollbackErr error
			if err := m.trust.Remove(admitted.NodeUUID); err != nil {
				m.trust.Forget(admitted.NodeUUID)
				rollbackErr = errors.Join(rollbackErr, err)
			}
			m.removeMemberByUUID(admitted.NodeUUID)
			m.removeMemberByUUID(m.identity.NodeUUID)
			rollbackErr = errors.Join(rollbackErr, m.persistMembersErr())
			m.setClusterIdentity("", "")
			return errors.Join(cause, rollbackErr)
		}
		m.recordMember(&admitted, stateMember, &now)
		m.addSelfMemberForAdmission(inv.ClusterID, sess.localAdmissionEpoch)
		if pinErr = m.persistMembersErr(); pinErr != nil {
			pinErr = rollback(pinErr)
			return
		}
		if pinErr = m.pinPeer(&admitted, sess.peerCert, []Endorsement{end}); pinErr != nil {
			pinErr = rollback(pinErr)
			return
		}
		if pinErr = m.activateAdmission(inv.ClusterID, sess.localAdmissionEpoch); pinErr != nil {
			pinErr = rollback(pinErr)
			return
		}
		m.setClusterIdentity(inv.ClusterID, inv.ClusterFriendlyName)
		if clearErr := m.clearRemovalProofAfterAdmission(admitted.NodeUUID, admitted.AdmissionEpoch); clearErr != nil {
			log.Printf("pairing %s: clear superseded removal proof for %s: %v", p.InviteID, admitted.NodeUUID, clearErr)
		}
	})
	if pinErr != nil {
		inviterAddr := sess.addr
		inviterDER := append([]byte(nil), sess.peerCert.Raw...)
		m.finishInvite(p.InviteID, inviteStateFailed)
		m.deleteSession(p.InviteID)
		sess.mu.Unlock()
		m.emitNodesChanged()
		go m.notifyInviterAuthenticated(inviterAddr, inviterDER, p.InviteID, "fail")
		m.codec.RespondErrorData(msg.ID, codeInternalError, "pin inviter: "+pinErr.Error(), nil)
		return
	}
	inviterAddr := sess.addr
	inviterDER := append([]byte(nil), sess.peerCert.Raw...)
	m.finishInvite(p.InviteID, inviteStatePaired)
	m.deleteSession(p.InviteID)
	sess.mu.Unlock()
	m.emitIdentityChanged()
	m.emitNodesChanged()
	m.respondInvite(msg, p.InviteID)
	go m.notifyInviterAuthenticated(inviterAddr, inviterDER, p.InviteID, "ack")
	// Reconcile with the inviter to immediately learn the rest of the cluster
	// (its roster reply carries every other member, endorsed for us to pin).
	go m.reconcileAllMembers()
	log.Printf("invite %s: joined cluster %s via %s", p.InviteID, inv.ClusterID, pi.NodeID)
}

// runCompletionExchangeLocked feeds the PIN to the Peer and drives the
// Completion Exchange as HTTP client until Registered. The caller holds
// sess.mu through the terminal invite transition.
func (m *Manager) runCompletionExchangeLocked(sess *pairingSession, pin string) error {
	if sess.addr == "" {
		return fmt.Errorf("inviter address unknown")
	}
	// The salt arrived inside the inviter's MAC-covered ServerInfo (captured
	// into sess.peerPairing at Initial completion), so the PIN Noob we derive
	// matches what the inviter injected. Empty salt means a legacy pre-salt
	// inviter: derive the legacy unsalted Noob (see pinNoob).
	salt := sess.pinSalt
	if len(salt) == 0 && sess.peerPairing != nil && sess.peerPairing.PinSalt != "" {
		s, err := base64.StdEncoding.DecodeString(sess.peerPairing.PinSalt)
		if err != nil {
			return fmt.Errorf("decode pin salt: %w", err)
		}
		salt = s
	}
	if err := sess.peer.OOBInputNoob(deriveNoobFromPINAndSalt(pin, salt)); err != nil {
		return fmt.Errorf("feed pin: %w", err)
	}

	client := &http.Client{Timeout: pairingHTTPTimeout}
	// Kickoff: empty msg so the inviter's Server.Start() emits the first blob.
	respBlob, err := postPairingBlob(client, sess.addr, sess.inviteID, "completion", nil)
	if err != nil {
		return err
	}
	for {
		if len(respBlob) == 0 {
			return fmt.Errorf("inviter ended completion unexpectedly")
		}
		out, rerr := sess.peer.Receive(respBlob)
		if rerr != nil {
			return fmt.Errorf("peer receive: %w", rerr)
		}
		if out.Done {
			// A bad PIN surfaces here as out.Err (an EAP-NOOB protocol failure
			// carried in the Outcome, not the returned error). A wrong PIN
			// yields a different OOB secret (Noob), so the joiner's derived
			// NoobId won't match the inviter's (ErrUnrecognizedOOBMsgID, which
			// is checked first) — and, were the NoobId to match, the Completion
			// MAC would fail to verify (ErrHMACVerificationFailed). Either is a
			// definitive "wrong PIN"; classify it so the joiner can stamp
			// reason:"incorrect-pin". Any other terminal error is a generic
			// completion failure. out.Err is already a concrete
			// *eapnoob.ProtocolError, so read its Code directly. NOTE: this
			// classification assumes the joiner (Peer) detects the mismatch
			// first; if a future EAP-NOOB change surfaced a wrong PIN as an
			// EAP-Failure (out.Err == nil) instead, the reason would silently
			// degrade to empty — TestPeerWrongPinYieldsProtocolError in the
			// eap-noob package guards that ordering.
			if out.Err != nil {
				if out.Err.Code == eapnoob.ErrUnrecognizedOOBMsgID || out.Err.Code == eapnoob.ErrHMACVerificationFailed {
					return &completionAuthError{err: out.Err}
				}
				return fmt.Errorf("completion failed: %w", out.Err)
			}
			break
		}
		respBlob, err = postCompletionBlob(client, sess.addr, sess.inviteID, out.Send)
		if err != nil {
			return err
		}
	}
	if sess.peer.State() != eapnoob.StateRegistered {
		return fmt.Errorf("completion ended in state %s, want Registered", sess.peer.State())
	}
	return nil
}

func postCompletionBlob(client *http.Client, addr, inviteID string, msg []byte) ([]byte, error) {
	attempts := 1
	var wire struct {
		Type *int `json:"Type"`
	}
	if json.Unmarshal(msg, &wire) == nil && wire.Type != nil && *wire.Type == 6 {
		attempts = inviterNotifyAttempts
	}
	var blob []byte
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		blob, err = postPairingBlob(client, addr, inviteID, "completion", msg)
		if err == nil {
			return blob, nil
		}
		if attempt < attempts {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	return nil, err
}

// inviterNotifyAttempts is how many times the joiner retries an out-of-band
// pairing signal before giving up. The inviter's invite/session TTL is the
// remaining safety net.
const inviterNotifyAttempts = 3

func (m *Manager) notifyInviterAuthenticated(inviterAddr string, inviterDER []byte, inviteID, phase string) {
	if inviterAddr == "" {
		return
	}
	tlsConfig, err := m.buildPairingClientTLSConfig(inviterDER)
	if err != nil {
		log.Printf("invite %s: build authenticated %s client: %v", inviteID, phase, err)
		return
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: pairingHTTPTimeout}
	for attempt := 1; attempt <= inviterNotifyAttempts; attempt++ {
		err = postPairingSignalScheme(client, "https", inviterAddr, inviteID, phase, "")
		if err == nil {
			return
		}
		log.Printf("invite %s: notify inviter authenticated %s attempt %d/%d: %v",
			inviteID, phase, attempt, inviterNotifyAttempts, err)
		if attempt < inviterNotifyAttempts {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
}

// notifyInviterTerminal POSTs a pre-commit pairing outcome signal
// (decline/fail/expire), authenticated with signalTag over the session's
// ephemeral Key Exchange secret (see pairingSignalMAC). Retries a few times;
// remaining failures are logged and periodic reconciliation/TTL cleanup is the
// backstop.
func (m *Manager) notifyInviterTerminal(inviterAddr, inviteID, phase, reason, signalTag string) {
	if inviterAddr == "" {
		return
	}
	client := &http.Client{Timeout: pairingHTTPTimeout}
	var err error
	for attempt := 1; attempt <= inviterNotifyAttempts; attempt++ {
		err = postPairingSignalTagged(client, "http", inviterAddr, inviteID, phase, reason, signalTag)
		if err == nil {
			return
		}
		log.Printf("invite %s: notify inviter %s attempt %d/%d: %v", inviteID, phase, attempt, inviterNotifyAttempts, err)
		if attempt < inviterNotifyAttempts {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
}

// sessionSignalTag computes the terminal-signal MAC for a live joiner session
// (caller holds sess.mu) and must be called before the session is deleted.
// An empty tag is returned when no Key Exchange secret exists yet — the signal
// is then sent unauthenticated and a hardened inviter falls back to its TTL.
func sessionSignalTag(sess *pairingSession, inviteID, phase string) string {
	key, err := sess.ephemeralKey()
	if err != nil {
		return ""
	}
	return pairingSignalMAC(key, inviteID, phase)
}

// finishInvite transitions an invite to a terminal state and stamps respondedAt.
func (m *Manager) finishInvite(inviteID string, state InviteState) {
	m.finishInviteReason(inviteID, state, "")
}

// finishInviteReason is finishInvite that also stamps a machine-readable reason
// (e.g. "incorrect-pin"). An empty reason leaves the field cleared.
func (m *Manager) finishInviteReason(inviteID string, state InviteState, reason string) {
	if inv, ok := m.getInvite(inviteID); ok {
		now := time.Now().UnixMilli()
		inv.State = state
		inv.Reason = reason
		inv.RespondedAt = &now
		m.putInvite(inv)
	}
}

// respondInvite writes the current Invite as a JSON-RPC result (pin omitted).
func (m *Manager) respondInvite(msg *Message, inviteID string) {
	inv, ok := m.getInvite(inviteID)
	if !ok {
		m.codec.RespondErrorData(msg.ID, codeUnknownInvite, "unknown inviteId", map[string]string{"inviteId": inviteID})
		return
	}
	inv.Pin = nil
	m.codec.Respond(msg.ID, inv)
}
