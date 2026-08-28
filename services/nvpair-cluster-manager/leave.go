// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"
)

// leaveNotifyTimeout bounds how long handleLeave waits for the best-effort
// departure-tombstone push to reachable members before it tears down local
// cluster state. The pushes run concurrently, so an offline or unreachable
// member can no longer stall the leave for the full pairingHTTPTimeout (10s)
// each — it learns we left later via the retained, gossiped tombstone. Kept
// short so leaving remains responsive even when peers are down.
const leaveNotifyTimeout = 2 * time.Second

// handleLeave makes this node cleanly unjoin its cluster (§7.0) — the
// self-initiated counterpart to nodes:remove (which removes a *peer*). It
// broadcasts its departure, then tears down all local cluster state so the node
// ends up genuinely unclustered rather than a zombie that everyone else has
// already evicted. Idempotent: leaving when already unclustered is a no-op.
func (m *Manager) handleLeave(msg *Message) {
	left, err := m.leaveCluster()
	if err != nil {
		m.codec.RespondErrorData(msg.ID, codeInternalError, "leave cluster: "+err.Error(), nil)
		return
	}
	m.codec.Respond(msg.ID, map[string]any{"left": left})
}

// maybeLeaveInviteCreatedCluster dissolves a throwaway solo cluster that exists
// solely to back invite pairing. It leaves only when:
//   - the cluster was auto-created by invite-node while unclustered, and
//   - there is no confirmed peer member, and
//   - there is no other pending outbound invite (a sibling session must keep the
//     cluster alive so a late Completion cannot join an already-torn-down id).
//
// An intentionally created solo cluster is preserved.
func (m *Manager) maybeLeaveInviteCreatedCluster() bool {
	m.inviteMu.Lock()
	defer m.inviteMu.Unlock()
	return m.maybeLeaveInviteCreatedClusterLocked()
}

// maybeLeaveInviteCreatedClusterLocked is the atomic form; caller holds
// inviteMu across the terminal invite transition and this cleanup decision.
func (m *Manager) maybeLeaveInviteCreatedClusterLocked() bool {
	if !m.isInviteCreatedCluster() {
		return false
	}
	if m.hasConfirmedPeer() {
		// A peer joined — this is a real cluster now.
		m.setInviteCreatedCluster(false)
		return false
	}
	if m.hasPendingOutboundInvite() {
		return false
	}
	left, err := m.leaveCluster()
	if err != nil {
		log.Printf("leave invite-created cluster: %v", err)
		return false
	}
	if !left {
		return false
	}
	log.Printf("left invite-created solo cluster (no pending outbound invites remain)")
	return true
}

// hasConfirmedPeer reports whether any non-self member is in stateMember.
func (m *Manager) hasConfirmedPeer() bool {
	for _, n := range m.snapshotNodes() {
		if n.NodeUUID != m.identity.NodeUUID && n.State == stateMember {
			return true
		}
	}
	return false
}

// hasPendingOutboundInvite reports whether any invite this node sent is still
// pending. Sibling pending invites must keep an invite-created cluster alive.
func (m *Manager) hasPendingOutboundInvite() bool {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	self := m.identity.NodeUUID
	for _, inv := range m.invites {
		if inv.State == inviteStatePending && inv.FromNodeUUID == self {
			return true
		}
	}
	return false
}

// leaveCluster tears down local cluster membership and identity. It first
// best-effort announces a self-tombstone to confirmed peers, then drops pins,
// clears membership, and emits identity-changed + nodes:changed. Returns false
// when already unclustered.
func (m *Manager) leaveCluster() (bool, error) {
	cid, _ := m.clusterIdentity()
	if cid == "" {
		return false, nil
	}

	// Announce departure as a signed self-tombstone and push it to every
	// reachable member *before* dropping their pins (which would fail the mTLS
	// handshake). Each peer applies it (dropping us) and retains it to gossip
	// onward, so members that are offline right now still learn we left.
	//
	// The pushes run concurrently with an overall deadline (leaveNotifyTimeout)
	// rather than serially: a single unreachable member used to block the whole
	// leave for the full pairingHTTPTimeout (10s) before teardown — the
	// "cluster leave timed out" hang. Reachable peers are still
	// notified before we un-pin them; an offline one just misses the live push
	// and converges later from the gossiped tombstone.
	_, selfEpoch := m.currentAdmission()
	if selfEpoch == 0 {
		var err error
		selfEpoch, err = m.ensureAdmission(cid)
		if err != nil {
			log.Printf("cluster leave: establish admission for legacy state: %v", err)
			return false, err
		}
	}
	proof, err := m.newRemovalProof(m.identity.NodeUUID, selfEpoch)
	if err != nil {
		log.Printf("cluster leave: create departure proof: %v", err)
		return false, err
	}
	if _, err := m.putRemovalProof(proof); err != nil {
		log.Printf("cluster leave: persist departure proof: %v", err)
		return false, err
	}
	m.putTombstone(proof.Tombstone)
	var wg sync.WaitGroup
	for _, n := range m.snapshotNodes() {
		if n.NodeUUID == m.identity.NodeUUID || n.State != stateMember {
			continue
		}
		addrs := m.resolvePeerAddrs(n)
		uuid := n.NodeUUID
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.reconcileWith(addrs, uuid)
		}()
	}
	notified := make(chan struct{})
	go func() {
		wg.Wait()
		close(notified)
	}()
	select {
	case <-notified:
	case <-time.After(leaveNotifyTimeout):
		slog.Warn("cluster leave: departure notification timed out; proceeding with teardown (offline peers converge via the gossiped tombstone)",
			"timeout", leaveNotifyTimeout)
	}

	if err := m.teardownClusterLocal(); err != nil {
		log.Printf("cluster leave: durable teardown incomplete (will retry on restart): %v", err)
		return false, err
	}
	return true, nil
}

// teardownClusterLocal drops every pin, clears membership, tombstones, and
// in-flight pairing state, and resets this node to unclustered — emitting
// cluster:identity-changed and nodes:changed so the Broker persists the empty
// state. Shared by
// cluster:leave (after announcing departure) and the inbound peer-removal
// notify (POST /v1/cluster/members/remove), which must leave the victim
// genuinely clusterless rather than only de-pinning the remover.
//
// Held under rosterMu with mergeRoster so a concurrent roster reply cannot
// resurrect pins/members after we clear them (the online-remove race that left
// the victim with a stale one-member roster and empty clusterId).
//
// NOTE: this acquires rosterMu itself, so it must never be called from inside a
// withClusterComposition closure (which already holds it) — that would deadlock.
// A caller already under rosterMu wants teardownClusterLocalLocked instead.
func (m *Manager) teardownClusterLocal() error {
	m.rosterMu.Lock()
	err := m.teardownClusterLocalLocked()
	m.rosterMu.Unlock()
	if err != nil {
		return err
	}

	m.emitIdentityChanged()
	m.emitNodesChanged()
	return nil
}

// teardownClusterLocalLocked performs the teardown assuming rosterMu is already
// held and does not emit notifications (the caller does that after releasing
// the lock). It exists so the periodic self-remove guard can make its teardown
// *decision* and the teardown itself atomic under rosterMu: revalidating the
// cluster identity/generation and clearing state in one critical section, so an
// adopt/rejoin that raced in cannot slip between the check and the teardown.
//
// It also abandons every in-flight pairing session and invite. An outbound
// pairing Completion can land on another goroutine after we clear cluster state;
// dropping the session/invite here (under rosterMu) together with commitPairing's
// cluster-epoch and session-liveness rechecks (also under rosterMu) makes such a
// late Completion a no-op instead of resurrecting a pin/member into the now-empty
// cluster — and clearing the session (not just the identity) is what closes the
// window where this node rejoins the same cluster before the Completion lands.
func (m *Manager) teardownClusterLocalLocked() error {
	if err := m.beginDurableTeardown(); err != nil {
		return fmt.Errorf("start teardown journal: %w", err)
	}
	return m.finishDurableTeardown()
}
