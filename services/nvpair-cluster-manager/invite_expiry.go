// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"
)

// inviteTTL is how long a pending invite may wait for accept/decline before the
// inviter expires it locally. This is the bounded fallback when a joiner's
// decline POST is lost (one-way outage / inviter restart mid-flight): without it
// an invite-created solo cluster would stay "Joined" forever.
const inviteTTL = 5 * time.Minute

// inviteExpiryInterval is how often the inviter sweeps for TTL-elapsed invites.
const inviteExpiryInterval = 15 * time.Second

// inviteTTLOverride / inviteExpiryIntervalOverride, when > 0, replace inviteTTL
// / inviteExpiryInterval. Unit tests set them directly; the binary sets them from
// env (applyInviteExpiryTestOverrides). Production leaves both at 0.
var (
	inviteTTLOverride            time.Duration
	inviteExpiryIntervalOverride time.Duration
)

func (m *Manager) effectiveInviteTTL() time.Duration {
	if inviteTTLOverride > 0 {
		return inviteTTLOverride
	}
	return inviteTTL
}

func (m *Manager) effectiveInviteExpiryInterval() time.Duration {
	if inviteExpiryIntervalOverride > 0 {
		return inviteExpiryIntervalOverride
	}
	return inviteExpiryInterval
}

// applyInviteExpiryTestOverrides shortens the invite TTL and sweep cadence from
// env so integration tests can exercise expiry without waiting minutes, mirroring
// the NVPAIR_TEST_PAIR_COMMIT_DELAY_MS fault-injection hook. No production path
// sets these; a unit test that already set the vars directly wins (env is only
// applied when the corresponding var is still unset).
func applyInviteExpiryTestOverrides() {
	if inviteTTLOverride == 0 {
		if ms, err := strconv.Atoi(os.Getenv("NVPAIR_TEST_INVITE_TTL_MS")); err == nil && ms > 0 {
			inviteTTLOverride = time.Duration(ms) * time.Millisecond
		}
	}
	if inviteExpiryIntervalOverride == 0 {
		if ms, err := strconv.Atoi(os.Getenv("NVPAIR_TEST_INVITE_EXPIRY_INTERVAL_MS")); err == nil && ms > 0 {
			inviteExpiryIntervalOverride = time.Duration(ms) * time.Millisecond
		}
	}
}

// inviteExpiryLoop periodically expires pending invites past their TTL (both the
// outbound self-heal and the inbound receiver-side teardown) and runs the
// invite-created solo cleanup. Cancelled with the manager context.
func (m *Manager) inviteExpiryLoop(ctx context.Context) {
	applyInviteExpiryTestOverrides()
	ticker := time.NewTicker(m.effectiveInviteExpiryInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.expirePendingInvites(time.Now())
		}
	}
}

// expirePendingInvites flips every pending invite older than the TTL to expired
// and tears down its pairing state, on both sides of a stranded pairing:
//
//   - Outbound (this node invited): flip to expired, drop the EAP session, emit
//     cluster:invite-expired, and maybe leave an invite-created solo cluster.
//     This is the inviter's self-healing backstop — it fires on the TTL even if
//     the joiner went offline or never signaled its own expiry.
//   - Inbound (this node was invited): the local user never entered the PIN, so
//     drop the tentative pending-inbound member + EAP session, emit
//     cluster:invite-expired so the UI dismisses the prompt, and best-effort
//     signal the inviter (phase:"expire") so it tears its outbound invite down
//     immediately instead of waiting for its own TTL.
//
// Safe to call from tests with a synthetic now.
func (m *Manager) expirePendingInvites(now time.Time) {
	m.inviteMu.Lock()
	defer m.inviteMu.Unlock()

	m.expirePreInviteSessions(now)
	cutoff := now.Add(-m.effectiveInviteTTL()).UnixMilli()
	var outboundIDs, inboundIDs []string

	m.memMu.Lock()
	self := m.identity.NodeUUID
	for id, inv := range m.invites {
		if inv.State != inviteStatePending || inv.CreatedAt > cutoff {
			continue
		}
		if inv.FromNodeUUID == self {
			cp := *inv
			cp.State = inviteStateExpired
			ts := now.UnixMilli()
			cp.RespondedAt = &ts
			cp.Pin = nil
			m.invites[id] = &cp
			outboundIDs = append(outboundIDs, id)
		} else {
			inboundIDs = append(inboundIDs, id)
		}
	}
	m.memMu.Unlock()

	for _, id := range outboundIDs {
		// Outbound expiry is a local fallback: the inviter clears its own invite
		// on the TTL. It does not signal the joiner — the receiver expires its
		// own inbound invite and signals us via phase:"expire", so both
		// converge on "expired" rather than a timing-dependent expired/canceled.
		m.deleteSession(id)
		m.emitInviteExpired(id)
		log.Printf("invite %s: expired (TTL elapsed with no accept/decline)", id)
	}

	m.expireInboundInvitesLocked(inboundIDs)

	if len(outboundIDs) > 0 {
		m.maybeLeaveInviteCreatedClusterLocked()
	}
}

func (m *Manager) expirePreInviteSessions(now time.Time) {
	cutoff := now.Add(-m.effectiveInviteTTL()).UnixMilli()
	m.sessMu.Lock()
	candidates := make([]*pairingSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		candidates = append(candidates, session)
	}
	m.sessMu.Unlock()

	for _, session := range candidates {
		if !session.mu.TryLock() {
			continue
		}
		if session.createdAt == 0 || session.createdAt > cutoff {
			session.mu.Unlock()
			continue
		}
		if session.role == roleJoiner {
			if _, published := m.getInvite(session.inviteID); published {
				session.mu.Unlock()
				continue
			}
		} else if session.role != roleInviter || !session.awaitingAck {
			session.mu.Unlock()
			continue
		}
		current, live := m.getSession(session.inviteID)
		if live && current == session {
			m.deleteSession(session.inviteID)
			if session.awaitingAck {
				session.awaitingAck = false
				m.setInviteCreatedCluster(false)
				log.Printf("pairing %s: acknowledgment window expired after durable commits", session.inviteID)
			} else {
				log.Printf("pairing %s: expired before invite publication", session.inviteID)
			}
		}
		session.mu.Unlock()
	}
}

// emitInviteExpired pushes cluster:invite-expired for a now-expired invite, with
// the PIN stripped. Shared by the outbound and inbound expiry paths.
func (m *Manager) emitInviteExpired(id string) {
	inv, ok := m.getInvite(id)
	if !ok {
		return
	}
	inv.Pin = nil
	if err := m.codec.Notify("cluster:invite-expired", inv); err != nil {
		log.Printf("emit cluster:invite-expired: %v", err)
	}
}

// expireInboundInvitesLocked tears down each inbound (this node was invited)
// pairing whose id was scanned as TTL-elapsed and pending. Called with
// m.inviteMu held. When a live joiner session is present it best-effort acquires
// the session lock (TryLock) and re-checks the invite is still pending: a
// just-in-time accept — whose Completion Exchange holds the same sess.mu across
// its network round-trips and re-checks pending itself — either commits first
// (and this skips) or has not yet taken the lock (this wins and it aborts
// cleanly). If the accept currently holds sess.mu, TryLock fails and the invite
// is left for the next sweep rather than parking m.inviteMu on the accept's
// network I/O. On a win it flips the invite to expired, drops the tentative
// pending-inbound member + EAP session, emits cluster:invite-expired so the
// receiver's UI clears its PIN prompt, and best-effort signals the inviter
// (phase:"expire") to tear its outbound invite down immediately; the inviter's
// own outbound TTL is the fallback when it is offline or unreachable. A pending
// inbound invite with no live session (their lifecycles are coupled, so this is
// defensive — but the "nothing lingers" guarantee must not hinge on that
// invariant) is still flipped to expired, has its tentative member dropped
// (keyed by the invite's FromNodeUUID), and is pushed; there is simply no session
// to signal from.
func (m *Manager) expireInboundInvitesLocked(ids []string) {
	membersChanged := false
	for _, id := range ids {
		sess, hasSess := m.getSession(id)
		if !hasSess || sess.role != roleJoiner {
			if inv, ok := m.getInvite(id); ok && inv.State == inviteStatePending {
				m.finishInvite(id, inviteStateExpired)
				if m.removeMemberByUUID(inv.FromNodeUUID) {
					membersChanged = true
				}
				m.emitInviteExpired(id)
				log.Printf("invite %s: expired inbound (no live session)", id)
			}
			continue
		}
		if !sess.mu.TryLock() {
			// An accept is mid-completion under sess.mu; don't park m.inviteMu on
			// its network round-trips. The next sweep reaps it if still pending.
			continue
		}
		cur, ok := m.getInvite(id)
		if !ok || cur.State != inviteStatePending {
			sess.mu.Unlock()
			continue
		}
		m.finishInvite(id, inviteStateExpired)
		if sess.peerPairing != nil && m.removeMemberByUUID(sess.peerPairing.NodeUUID) {
			membersChanged = true
		}
		inviterAddr := sess.addr
		m.deleteSession(id)
		sess.mu.Unlock()

		m.emitInviteExpired(id)
		go m.notifyInviterTerminal(inviterAddr, id, "expire", "")
		log.Printf("invite %s: expired inbound (TTL elapsed with no local response)", id)
	}
	if membersChanged {
		m.emitNodesChanged()
	}
}
