// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"eapnoob"
)

const (
	pairingPath          = "/v1/cluster/pairing"
	maxBodyBytes         = 1 << 20
	maxPreInviteSessions = 64
)

var errPairingCommitStale = errors.New("pairing commit refused after teardown")

// pairingEnvelope wraps exactly one EAP-NOOB message on the pairing channel
// (§7.2). Initial/Completion use plain HTTP; post-commit ack/fail use the same
// path over mTLS. msg is the base64 of the opaque eap-noob Outcome.Send blob, or
// "" for the Completion kickoff.
type pairingEnvelope struct {
	InviteID string `json:"inviteId"`
	Phase    string `json:"phase"`
	Msg      string `json:"msg"`
	// Rejected + Reason carry an explicit pairing refusal from the joiner back to
	// the inviter (e.g. the joiner is already clustered). Sent with HTTP 409 and
	// distinct from a transport/protocol error so the inviter can report
	// state:"rejected" rather than state:"failed". Empty on normal messages.
	Rejected bool   `json:"rejected,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// runHTTP starts the inter-node listener on the configured port and blocks until
// ctx is cancelled. The single port carries two transport modes distinguished
// by the first byte of each connection (§7.2): the plain-HTTP pairing channel
// and the mTLS trusted endpoints. See serveSplit (mtls.go).
func (m *Manager) runHTTP(ctx context.Context) error {
	pairingMux := http.NewServeMux()
	pairingMux.HandleFunc(pairingPath, m.handlePairing)

	trustedMux := http.NewServeMux()
	trustedMux.HandleFunc(pairingPath, m.handlePairing)
	trustedMux.HandleFunc(membersRemovePath, m.handleMembersRemove)
	trustedMux.HandleFunc(rosterPath, m.handleRoster)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", m.port))
	if err != nil {
		return fmt.Errorf("listen on :%d: %w", m.port, err)
	}
	log.Printf("inter-node server listening on :%d (plain pairing + mTLS trusted)", m.port)
	return m.serveSplit(ctx, listener, pairingMux, trustedMux)
}

func (m *Manager) handlePairing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var env pairingEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if env.InviteID == "" {
		http.Error(w, "missing inviteId", http.StatusBadRequest)
		return
	}
	var msg []byte
	if env.Msg != "" {
		if msg, err = base64.StdEncoding.DecodeString(env.Msg); err != nil {
			http.Error(w, "invalid msg encoding", http.StatusBadRequest)
			return
		}
	}

	switch env.Phase {
	case "initial":
		m.handlePairingInitial(w, &env, msg, hostOnly(r.RemoteAddr))
	case "completion":
		m.handlePairingCompletion(w, &env, msg, hostOnly(r.RemoteAddr))
	case "cancel":
		m.handlePairingCancel(w, &env)
	case "decline":
		m.handlePairingDecline(w, &env)
	case "fail":
		callerUUID := ""
		if r.TLS != nil {
			var trusted bool
			callerUUID, trusted = m.verifyClientPin(r)
			if !trusted {
				http.Error(w, "not a trusted peer", http.StatusForbidden)
				return
			}
		}
		m.handlePairingFailedFrom(w, &env, callerUUID)
	case "ack":
		callerUUID, trusted := m.verifyClientPin(r)
		if !trusted {
			http.Error(w, "not a trusted peer", http.StatusForbidden)
			return
		}
		m.handlePairingAck(w, &env, callerUUID)
	case "expire":
		m.handlePairingExpired(w, &env)
	default:
		http.Error(w, "unknown phase", http.StatusBadRequest)
	}
}

// handlePairingInitial runs the joiner side (EAP-NOOB Peer) of the Initial
// Exchange. The first message creates the joiner session; when the Peer reaches
// Waiting the invite is recorded and cluster:invite-received is emitted.
func (m *Manager) handlePairingInitial(w http.ResponseWriter, env *pairingEnvelope, msg []byte, inviterIP string) {
	sess, ok := m.getSession(env.InviteID)
	if !ok {
		if m.teardownPending.Load() {
			http.Error(w, "cluster teardown is pending", http.StatusConflict)
			return
		}
		var first struct {
			Type *int `json:"Type"`
		}
		if json.Unmarshal(msg, &first) != nil || first.Type == nil || *first.Type != 1 {
			http.Error(w, "invalid EAP-NOOB message", http.StatusBadRequest)
			return
		}
		if m.joinerSessionCount() >= maxPreInviteSessions {
			http.Error(w, "too many pending pairing sessions", http.StatusTooManyRequests)
			return
		}
		// Refuse to join a new pairing while already clustered: an already-
		// clustered node must leave (or be removed) before it can pair again.
		// This is the authoritative guard — we reject before creating a session,
		// minting a PIN, or emitting cluster:invite-received, so the inviter
		// learns state:"rejected". A well-behaved inviter also skips the invite
		// when it can see our cluster-uuid over mDNS, but this backstops any
		// client that tries anyway.
		if cid, _ := m.clusterIdentity(); cid != "" {
			respondPairingRejected(w, reasonAlreadyClustered)
			return
		}
		// Advertise our reachable address (the local IP that routes toward the
		// inviter, plus our listening port) in PeerInfo so the inviter can later
		// reconcile rosters back to us. This is folded into the EAP-NOOB MAC.
		myAddr := net.JoinHostPort(outboundIP(inviterIP), strconv.Itoa(m.port))
		admissionEpoch, err := m.reserveAdmissionEpoch()
		if err != nil {
			http.Error(w, "reserve admission", http.StatusInternalServerError)
			return
		}
		info, err := m.localPairingInfo(myAddr, admissionEpoch).toMap()
		if err != nil {
			http.Error(w, "build pairing info", http.StatusInternalServerError)
			return
		}
		sess = &pairingSession{
			inviteID:            env.InviteID,
			role:                roleJoiner,
			createdAt:           time.Now().UnixMilli(),
			peer:                newPairingPeer(info),
			localAdmissionEpoch: admissionEpoch,
		}
		registered, atCapacity := m.registerJoinerSession(sess)
		if !registered {
			if atCapacity {
				http.Error(w, "too many pending pairing sessions", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "cluster state changed during pairing", http.StatusConflict)
			return
		}
	}
	if sess.role != roleJoiner {
		http.Error(w, "phase does not match session state", http.StatusConflict)
		return
	}

	sess.mu.Lock()
	out, err := sess.peer.Receive(msg)
	if err != nil {
		m.deleteSession(env.InviteID)
		sess.mu.Unlock()
		http.Error(w, "eap-noob: "+err.Error(), http.StatusBadRequest)
		return
	}
	var completed *Invite
	if out.State == eapnoob.StateWaiting && !sess.invitedEmitted {
		completed = m.prepareJoinerInitialComplete(env.InviteID, sess)
	}
	sess.mu.Unlock()
	if completed != nil {
		m.onJoinerInitialComplete(completed, sess)
	}
	respondPairing(w, out.Send)
}

// handlePairingCompletion runs the inviter side (EAP-NOOB Server) of the
// joiner-driven Completion Exchange. An empty msg is the kickoff (Server.Start);
// on reaching Registered the joiner's cert is pinned and the invite flips to
// paired.
func (m *Manager) handlePairingCompletion(w http.ResponseWriter, env *pairingEnvelope, msg []byte, srcIP string) {
	sess, ok := m.getSession(env.InviteID)
	if !ok {
		http.Error(w, "unknown or expired inviteId", http.StatusConflict)
		return
	}
	if sess.role != roleInviter {
		http.Error(w, "phase does not match session state", http.StatusConflict)
		return
	}

	m.inviteMu.Lock()
	defer m.inviteMu.Unlock()
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// Authoritative serialization point against cancel and decline, which take
	// the same sess.mu and re-read state inside it. If either terminal path wins,
	// refuse to advance Completion so it cannot produce a one-sided join.
	// Re-checked on every round trip because cancellation/decline can arrive
	// between them.
	inv, ok := m.getInvite(env.InviteID)
	if ok && inv.State == inviteStatePaired && sess.awaitingAck && len(sess.completionResponse) > 0 {
		respondPairing(w, sess.completionResponse)
		return
	}
	if !ok || inv.State != inviteStatePending {
		http.Error(w, "invite no longer pending", http.StatusConflict)
		return
	}
	if sess.peerSrcIP == "" {
		sess.peerSrcIP = srcIP
	}
	if len(msg) == 0 {
		blob, err := sess.server.Start()
		if err != nil {
			http.Error(w, "eap-noob start: "+err.Error(), http.StatusBadRequest)
			return
		}
		respondPairing(w, blob)
		return
	}
	out, err := sess.server.Receive(msg)
	if err != nil {
		http.Error(w, "eap-noob: "+err.Error(), http.StatusBadRequest)
		return
	}
	if out.Done && out.Success {
		if err := m.onInviterPaired(env.InviteID, sess); err != nil {
			respondPairingCommit(w, out.Send, err)
			return
		}
		sess.awaitingAck = true
		sess.createdAt = time.Now().UnixMilli()
		sess.completionResponse = append([]byte(nil), out.Send...)
	}
	respondPairing(w, out.Send)
}

// respondPairingCommit centralizes the fail-closed terminal response. In
// particular, it never writes successBlob when local commit failed, even though
// EAP-NOOB already computed an EAP-Success frame.
func respondPairingCommit(w http.ResponseWriter, successBlob []byte, commitErr error) {
	if commitErr == nil {
		respondPairing(w, successBlob)
		return
	}
	if errors.Is(commitErr, errPairingCommitStale) {
		http.Error(w, commitErr.Error(), http.StatusConflict)
	} else {
		http.Error(w, "commit pairing: "+commitErr.Error(), http.StatusInternalServerError)
	}
}

// handlePairingCancel runs the joiner side of an inviter-initiated cancel: it
// drops the pending-inbound invite/session and member for inviteId and emits
// cluster:invite-canceled so the joiner's UI can dismiss its PIN prompt. It is
// idempotent and always answers 200 — this is a best-effort teardown signal,
// not part of the cryptographic handshake.
func (m *Manager) handlePairingCancel(w http.ResponseWriter, env *pairingEnvelope) {
	defer respondPairing(w, nil)

	sess, ok := m.getSession(env.InviteID)
	if !ok || sess.role != roleJoiner {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	inv, ok := m.getInvite(env.InviteID)
	if !ok || inv.State != inviteStatePending {
		return
	}
	m.finishInvite(env.InviteID, inviteStateCanceled)
	if sess.peerPairing != nil {
		m.removeMemberByUUID(sess.peerPairing.NodeUUID)
	}
	m.deleteSession(env.InviteID)
	m.emitNodesChanged()
	canceled, ok := m.getInvite(env.InviteID)
	if !ok {
		return
	}
	canceled.Pin = nil
	if err := m.codec.Notify("cluster:invite-canceled", canceled); err != nil {
		log.Printf("emit cluster:invite-canceled: %v", err)
	}
	log.Printf("invite %s: canceled by inviter (inbound cleared)", env.InviteID)
}

// handlePairingDecline runs the inviter side of a joiner-initiated decline. It
// flips the outbound invite to declined and runs the shared terminal-signal
// teardown (emitting cluster:invite-declined). See handlePairingTerminalSignal.
func (m *Manager) handlePairingDecline(w http.ResponseWriter, env *pairingEnvelope) {
	defer respondPairing(w, nil)
	m.handlePairingTerminalSignal(env, inviteStateDeclined, "", "cluster:invite-declined", "")
}

// handlePairingFailed runs the inviter side of a joiner-signaled completion
// failure (today only a wrong PIN). It is the failure mirror of decline: without
// it a wrong PIN would strand the inviter on a stale pending invite, because the
// joiner (EAP Peer) detects the bad MAC first and never drives the Completion
// round that would otherwise surface the failure here. Only a reason we
// recognize is honored, so a peer can't stamp arbitrary text onto the invite;
// any other cause collapses to a generic failure. It flips the outbound invite
// to failed and runs the shared teardown (emitting cluster:invite-failed). See
// handlePairingTerminalSignal.
func (m *Manager) handlePairingFailed(w http.ResponseWriter, env *pairingEnvelope) {
	m.handlePairingFailedFrom(w, env, "")
}

func (m *Manager) handlePairingFailedFrom(w http.ResponseWriter, env *pairingEnvelope, callerUUID string) {
	defer respondPairing(w, nil)
	reason := ""
	if env.Reason == reasonIncorrectPIN {
		reason = reasonIncorrectPIN
	}
	m.handlePairingTerminalSignal(env, inviteStateFailed, reason, "cluster:invite-failed", callerUUID)
}

func (m *Manager) handlePairingAck(w http.ResponseWriter, env *pairingEnvelope, callerUUID string) {
	defer respondPairing(w, nil)
	sess, ok := m.getSession(env.InviteID)
	if !ok || sess.role != roleInviter {
		return
	}
	m.inviteMu.Lock()
	sess.mu.Lock()
	inv, ok := m.getInvite(env.InviteID)
	if !ok || inv.State != inviteStatePaired || !sess.awaitingAck ||
		sess.peerPairing == nil || sess.peerPairing.NodeUUID != callerUUID {
		sess.mu.Unlock()
		m.inviteMu.Unlock()
		return
	}
	sess.awaitingAck = false
	m.deleteSession(env.InviteID)
	sess.mu.Unlock()
	m.setInviteCreatedCluster(false)
	m.inviteMu.Unlock()
	m.emitNodesChanged()
	go m.reconcileAllMembers()
	log.Printf("pairing %s: joiner acknowledged durable commit", env.InviteID)
}

// handlePairingExpired runs the inviter side of a joiner-signaled inbound
// expiry: the receiving node's user never answered within the invite TTL, so the
// joiner expired its inbound invite and signals us to tear down the matching
// outbound invite immediately instead of waiting for our own outbound TTL (which
// remains the fallback when the joiner is offline or unreachable). It flips the
// outbound invite to expired and runs the shared teardown (emitting
// cluster:invite-expired). See handlePairingTerminalSignal.
func (m *Manager) handlePairingExpired(w http.ResponseWriter, env *pairingEnvelope) {
	defer respondPairing(w, nil)
	m.handlePairingTerminalSignal(env, inviteStateExpired, "", "cluster:invite-expired", "")
}

// handlePairingTerminalSignal is the shared inviter-side teardown for a
// joiner-signaled terminal outcome delivered over the pairing channel (a
// decline, a completion failure, or an inbound-invite expiry). It flips the
// pending outbound invite to state (stamping reason), tears down the EAP-NOOB
// Server session (invalidating the PIN), emits notifyMethod carrying the
// updated Invite, and — when this is an
// invite-created solo cluster with no sibling pending outbound invites — leaves
// the throwaway cluster while preserving intentional solo clusters. A failure
// for a paired invite still awaiting the
// joiner's durable-commit acknowledgment also rolls that provisional peer back.
// Idempotent: a duplicate or late signal for an unknown, already-finalized, or
// non-inviter invite is a silent no-op. Callers answer 200 unconditionally.
func (m *Manager) handlePairingTerminalSignal(env *pairingEnvelope, state InviteState, reason, notifyMethod, callerUUID string) {
	sess, ok := m.getSession(env.InviteID)
	if !ok || sess.role != roleInviter {
		return
	}

	m.inviteMu.Lock()
	sess.mu.Lock()
	cur, ok := m.getInvite(env.InviteID)
	rolledBackPair := false
	if ok && cur.State == inviteStatePaired && state == inviteStateFailed && sess.awaitingAck {
		peerUUID := ""
		if sess.peerPairing != nil {
			peerUUID = sess.peerPairing.NodeUUID
		}
		if callerUUID == "" || callerUUID != peerUUID {
			sess.mu.Unlock()
			m.inviteMu.Unlock()
			return
		}
		m.withClusterComposition(func() {
			if peerUUID == "" {
				return
			}
			member, memberOK := m.memberByNodeID(peerUUID)
			pin, pinOK := m.trust.Get(peerUUID)
			if !memberOK || !pinOK ||
				member.ClusterID != sess.clusterID || pin.ClusterID != sess.clusterID ||
				member.AdmissionEpoch != sess.peerPairing.AdmissionEpoch ||
				pin.AdmissionEpoch != sess.peerPairing.AdmissionEpoch {
				return
			}
			if err := m.trust.Remove(peerUUID); err != nil {
				m.trust.Forget(peerUUID)
				log.Printf("pairing %s: rollback durable pin: %v", env.InviteID, err)
			}
			m.removeMemberByUUID(peerUUID)
			if err := m.persistMembersErr(); err != nil {
				log.Printf("pairing %s: persist rollback after joiner failure: %v", env.InviteID, err)
			}
			rolledBackPair = true
		})
		m.finishInviteReason(env.InviteID, state, reason)
		m.deleteSession(env.InviteID)
		sess.mu.Unlock()
		inv, _ := m.getInvite(env.InviteID)
		if inv != nil {
			inv.Pin = nil
			if err := m.codec.Notify(notifyMethod, inv); err != nil {
				log.Printf("emit %s: %v", notifyMethod, err)
			}
		}
		m.maybeLeaveInviteCreatedClusterLocked()
		m.inviteMu.Unlock()
		if rolledBackPair {
			m.emitNodesChanged()
		}
		return
	}
	if !ok || cur.State != inviteStatePending {
		sess.mu.Unlock()
		m.inviteMu.Unlock()
		return
	}
	m.finishInviteReason(env.InviteID, state, reason)
	m.deleteSession(env.InviteID)
	sess.mu.Unlock()

	inv, ok := m.getInvite(env.InviteID)
	if !ok {
		m.inviteMu.Unlock()
		return
	}
	inv.Pin = nil
	if err := m.codec.Notify(notifyMethod, inv); err != nil {
		log.Printf("emit %s: %v", notifyMethod, err)
	}
	log.Printf("invite %s: %s by joiner (outbound cleared, reason %q)", env.InviteID, state, reason)

	m.maybeLeaveInviteCreatedClusterLocked()
	m.inviteMu.Unlock()
}

// prepareJoinerInitialComplete captures the inviter's authenticated PairingInfo
// while the caller holds sess.mu. The returned invite is published after that
// lock is released so same-sender supersession can safely lock older sessions.
func (m *Manager) prepareJoinerInitialComplete(inviteID string, sess *pairingSession) *Invite {
	pi, cert, err := parsePairingInfo(sess.peer.ServerInfo())
	if err != nil {
		log.Printf("pairing %s: cannot read inviter PairingInfo: %v", inviteID, err)
		return nil
	}
	sess.peerPairing = pi
	sess.peerCert = cert
	sess.addr = pi.Addr
	sess.invitedEmitted = true

	now := time.Now().UnixMilli()
	toID := m.identity.NodeID
	return &Invite{
		InviteID:            inviteID,
		FromNodeID:          pi.NodeID,
		FromNodeUUID:        pi.NodeUUID,
		FromNodeName:        pi.Name,
		ToNodeID:            &toID,
		ClusterID:           pi.ClusterID,
		ClusterFriendlyName: pi.ClusterFriendlyName,
		State:               inviteStatePending,
		CreatedAt:           now,
	}
}

type supersededInboundInvite struct {
	invite      *Invite
	inviterAddr string
}

// onJoinerInitialComplete publishes the newest pending invite from a sender and
// atomically supersedes that sender's older pending inbound sessions. Invites
// from other senders remain independent.
func (m *Manager) onJoinerInitialComplete(inv *Invite, sess *pairingSession) {
	if current, ok := m.getSession(inv.InviteID); !ok || current != sess {
		return
	}
	m.inviteMu.Lock()
	superseded := m.supersedePendingInboundLocked(inv.FromNodeUUID, inv.InviteID)
	published := false
	m.withClusterComposition(func() {
		cid, _ := m.clusterIdentity()
		current, live := m.getSession(inv.InviteID)
		if m.teardownPending.Load() || cid != "" || !live || current != sess || sess.peerPairing == nil {
			return
		}
		m.putInvite(inv)
		if sess.peerPairing != nil {
			m.recordMember(sess.peerPairing, statePendingInbound, nil)
		}
		published = true
	})
	if !published {
		m.deleteSession(inv.InviteID)
	}
	m.inviteMu.Unlock()
	if !published {
		return
	}

	for _, old := range superseded {
		if err := m.codec.Notify("cluster:invite-canceled", old.invite); err != nil {
			log.Printf("emit cluster:invite-canceled: %v", err)
		}
		go m.notifyInviterTerminal(old.inviterAddr, old.invite.InviteID, "decline", "")
		log.Printf("invite %s: superseded by newer invite %s from %s",
			old.invite.InviteID, inv.InviteID, inv.FromNodeUUID)
	}
	if err := m.codec.Notify("cluster:invite-received", inv); err != nil {
		log.Printf("emit cluster:invite-received: %v", err)
	}
}

// supersedePendingInboundLocked cancels older live joiner sessions from the
// same authenticated sender. The caller holds inviteMu, which serializes two
// Initial Exchanges completing concurrently.
func (m *Manager) supersedePendingInboundLocked(fromNodeUUID, keepInviteID string) []supersededInboundInvite {
	m.memMu.Lock()
	var candidateIDs []string
	for id, inv := range m.invites {
		if id != keepInviteID && inv.State == inviteStatePending && inv.FromNodeUUID == fromNodeUUID {
			candidateIDs = append(candidateIDs, id)
		}
	}
	m.memMu.Unlock()

	var superseded []supersededInboundInvite
	for _, id := range candidateIDs {
		sess, ok := m.getSession(id)
		if !ok || sess.role != roleJoiner {
			continue
		}
		sess.mu.Lock()
		current, ok := m.getInvite(id)
		if !ok || current.State != inviteStatePending {
			sess.mu.Unlock()
			continue
		}
		m.finishInvite(id, inviteStateCanceled)
		m.deleteSession(id)
		inviterAddr := sess.addr
		sess.mu.Unlock()

		canceled, ok := m.getInvite(id)
		if !ok {
			continue
		}
		canceled.Pin = nil
		superseded = append(superseded, supersededInboundInvite{
			invite:      canceled,
			inviterAddr: inviterAddr,
		})
	}
	return superseded
}

// onInviterPaired pins the joiner, promotes it to a member, flips the invite to
// paired, and evicts the session.
// onInviterPaired must be called with sess.mu held (from handlePairingCompletion)
// so its pending-state check and the pin/record/flip below are atomic with
// respect to concurrent cancel/decline, which serialize on the same mutex. The
// check is belt-and-suspenders behind handlePairingCompletion's top-of-lock
// guard.
func (m *Manager) onInviterPaired(inviteID string, sess *pairingSession) error {
	if inv, ok := m.getInvite(inviteID); !ok || inv.State != inviteStatePending {
		log.Printf("pairing %s: completion ignored, invite no longer pending", inviteID)
		return errPairingCommitStale
	}
	testCommitDelay()
	pi, cert, err := parsePairingInfo(sess.server.PeerInfo())
	if err != nil {
		log.Printf("pairing %s: cannot read joiner PairingInfo: %v", inviteID, err)
		m.finishInvite(inviteID, inviteStateFailed)
		m.deleteSession(inviteID)
		return err
	}
	// Reconcile address: take the joiner's advertised listening port but prefer
	// the source IP we actually observed (more reliable than the joiner's own
	// guess under NAT / multi-homing) for the host. Recording it as an observation
	// keeps that preference standing: a later roster entry carrying the joiner's
	// own claim, or another peer's relay of it, must not replace a verified host.
	host, port := splitAddr(pi.Addr, m.port)
	if sess.peerSrcIP != "" {
		host = sess.peerSrcIP
		m.noteObservedPeerHost(pi.NodeUUID, host)
	}
	if host != "" {
		pi.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return m.finalizePairing(inviteID, sess, pi, cert)
}

// finalizePairing commits the joiner's pin + membership through the cluster-epoch
// / session gate and does the post-commit bookkeeping. Split out from
// onInviterPaired so the commit and its abort path are unit-testable without a
// full EAP-NOOB handshake (onInviterPaired's only extra job is parsing the
// joiner PairingInfo off the completed eap-noob server).
func (m *Manager) finalizePairing(inviteID string, sess *pairingSession, pi *PairingInfo, cert *x509.Certificate) error {
	now := time.Now().UnixMilli()
	committed, pinErr := m.commitPairing(sess, pi, cert, now)
	if pinErr != nil {
		log.Printf("pairing %s: pin joiner: %v", inviteID, pinErr)
		m.finishInvite(inviteID, inviteStateFailed)
		m.deleteSession(inviteID)
		return pinErr
	}
	if !committed {
		// The Completion landed after this node left or was removed from cluster
		// sess.clusterID (or the pairing was abandoned by a teardown). Fail the
		// invite and drop the session rather than resurrect the joiner's
		// pin/member into an emptied cluster or leave a phantom pending invite
		// (a broker-driven cluster switch does not clear invites, so the abort
		// must fail this one itself).
		log.Printf("pairing %s: completion landed after leaving cluster %s; discarding (no pin/member resurrected)", inviteID, sess.clusterID)
		m.finishInvite(inviteID, inviteStateFailed)
		m.deleteSession(inviteID)
		return errPairingCommitStale
	}
	if inv, ok := m.getInvite(inviteID); ok {
		inv.State = inviteStatePaired
		inv.RespondedAt = &now
		m.putInvite(inv)
	}
	sess.awaitingAck = true
	sess.createdAt = now
	log.Printf("pairing %s: inviter commit complete for %s (%s), awaiting joiner ack", inviteID, pi.NodeID, pi.NodeUUID)
	return nil
}

// commitPairing pins a freshly-paired peer and records it as a member, but only
// if this node is still in the cluster the pairing was scoped to AND the pairing
// session is still live. The whole commit runs under the cluster-composition
// boundary (rosterMu), so it is atomic against a concurrent teardown, and it
// rechecks both gates inside that boundary before writing anything:
//
//   - Epoch gate: a Completion that lands after this node left or was removed
//     (clusterId cleared) — or after it rejoined a *different* cluster — observes
//     the changed identity and commits nothing.
//   - Liveness gate: teardown clears the sessions map under this same boundary,
//     so a Completion for a pairing that a removal abandoned commits nothing even
//     if the node has since rejoined the *same* cluster (where the epoch alone
//     would match again).
//
// Either way it never resurrects a pin/member the removal was meant to drop.
// Returns whether it committed and any pin error.
func (m *Manager) commitPairing(sess *pairingSession, pi *PairingInfo, cert *x509.Certificate, now int64) (committed bool, err error) {
	m.withClusterComposition(func() {
		if m.teardownPending.Load() {
			err = fmt.Errorf("cluster teardown is pending")
			return
		}
		if cid, _ := m.clusterIdentity(); cid == "" || cid != sess.clusterID {
			return
		}
		if cur, ok := m.getSession(sess.inviteID); !ok || cur != sess {
			return
		}
		if m.removalProofBlocksAdmission(pi.NodeUUID, sess.clusterID, pi.AdmissionEpoch) {
			err = fmt.Errorf("peer admission %d was already removed", pi.AdmissionEpoch)
			return
		}
		admitted := *pi
		// The joiner's pre-adoption PeerInfo legitimately carries an empty
		// clusterId. The inviter admits it into the session's authenticated
		// cluster, never the untrusted pre-adoption value.
		admitted.ClusterID = sess.clusterID
		end := m.endorsePeer(admitted.NodeUUID, certFingerprintFromDER(cert.Raw), admitted.AdmissionEpoch)

		// Persist membership before granting mTLS authority. A crash in between
		// can leave only an untrusted member row, which startup prunes; it cannot
		// leave an authorized pin that was never durably committed as a member.
		m.recordMember(&admitted, stateMember, &now)
		if err = m.persistMembersErr(); err != nil {
			m.removeMemberByUUID(admitted.NodeUUID)
			return
		}
		if err = m.pinPeer(&admitted, cert, []Endorsement{end}); err != nil {
			m.removeMemberByUUID(admitted.NodeUUID)
			if rollbackErr := m.persistMembersErr(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("persist pairing rollback: %w", rollbackErr))
			}
			return
		}
		if clearErr := m.clearRemovalProofAfterAdmission(admitted.NodeUUID, admitted.AdmissionEpoch); clearErr != nil {
			log.Printf("pairing: clear superseded removal proof for %s: %v", admitted.NodeUUID, clearErr)
		}
		sess.peerPairing = &admitted
		committed = true
	})
	return
}

// testCommitDelay optionally pauses inside onInviterPaired's critical section —
// holding sess.mu, after the invite is confirmed pending and before the pin /
// membership commit — to widen the cancel-vs-Completion race window so
// TestClusterCancelInviteRace can drive it deterministically. It is a no-op
// unless NVPAIR_TEST_PAIR_COMMIT_DELAY_MS is set; no production path sets it.
func testCommitDelay() {
	v := os.Getenv("NVPAIR_TEST_PAIR_COMMIT_DELAY_MS")
	if v == "" {
		return
	}
	if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

// pinPeer durably pins a peer's certificate in the trusted-node store, carrying
// the endorsements that vouch for it (this node's own endorsement at pair time,
// plus any gathered via gossip).
func (m *Manager) pinPeer(pi *PairingInfo, cert *x509.Certificate, endorsements []Endorsement) error {
	return m.trust.Pin(&TrustedPin{
		NodeUUID:        pi.NodeUUID,
		NodeID:          pi.NodeID,
		Name:            pi.Name,
		ClusterID:       pi.ClusterID,
		AdmissionEpoch:  pi.AdmissionEpoch,
		CertPem:         pi.Cert,
		CertFingerprint: certFingerprintFromDER(cert.Raw),
		PinnedAt:        time.Now().UnixMilli(),
		Endorsements:    endorsements,
	})
}

// recordMember upserts a ClusterNode for a peer described by its PairingInfo.
func (m *Manager) recordMember(pi *PairingInfo, state MembershipState, joinedAt *int64) {
	host, port := splitAddr(pi.Addr, m.port)
	m.upsertMember(&ClusterNode{
		ID:             pi.NodeID,
		NodeUUID:       pi.NodeUUID,
		Name:           pi.Name,
		IPAddress:      host,
		Port:           port,
		ClusterID:      pi.ClusterID,
		AdmissionEpoch: pi.AdmissionEpoch,
		State:          state,
		JoinedAt:       joinedAt,
	})
}

// respondPairing writes the pairing envelope response carrying the receiver's
// next EAP-NOOB blob (possibly empty for a terminal message).
func respondPairing(w http.ResponseWriter, send []byte) {
	out := pairingEnvelope{}
	if len(send) > 0 {
		out.Msg = base64.StdEncoding.EncodeToString(send)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// respondPairingRejected tells the inviter this node explicitly refuses the
// pairing (HTTP 409 + a rejected envelope carrying the reason), as distinct from
// a transport/protocol failure. The inviter's postPairingBlob recognizes this
// shape and surfaces it as an invite result of state:"rejected".
func respondPairingRejected(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(pairingEnvelope{Rejected: true, Reason: reason})
}

// hostOnly returns the host portion of a host:port, or the input unchanged if
// it has no port.
func hostOnly(hostPort string) string {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return host
}

// joinHostPort renders host:port, substituting defPort when port is not a valid
// TCP port. The inverse of splitAddr.
func joinHostPort(host string, port, defPort int) string {
	if host == "" {
		return ""
	}
	if port < 1 || port > 65535 {
		port = defPort
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// splitAddr parses a host:port, falling back to defPort when the port is absent
// or unparseable, and to "" host when addr is empty.
func splitAddr(addr string, defPort int) (string, int) {
	if addr == "" {
		return "", defPort
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, defPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, defPort
	}
	return host, port
}
