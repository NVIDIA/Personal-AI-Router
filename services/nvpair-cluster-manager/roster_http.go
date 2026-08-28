// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"
)

// reconcileEvery is the heartbeat cadence for catch-up roster reconciles — the
// backstop that converges nodes which were offline during a pair/removal.
// Membership churns far less than this; convergence on the happy path comes from
// the immediate post-pair / post-removal pushes, not the heartbeat.
const reconcileEvery = 30 * time.Second

// initialReconcileDelay is how long after startup the first reconcile pass
// runs. Short enough to converge a just-restarted or just-woken node quickly,
// but long enough to let the mDNS browser populate first (its first browse
// takes a few seconds).
const initialReconcileDelay = 5 * time.Second

// rosterPath is the mTLS trusted endpoint for cluster-trust fan-out: paired
// peers exchange and reconcile their rosters here so every member transitively
// learns (and pins) every other member, and learns of removals.
const rosterPath = "/v1/cluster/roster"

// handleRoster serves an inbound roster reconcile. The caller is authenticated
// by mTLS (verifyClientPin), so it is already a trusted member; we merge its
// roster into our state and reply with our own so convergence is symmetric in
// one round trip (mirrors nvpair-errors peer-sync).
func (m *Manager) handleRoster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	senderUUID, ok := m.verifyClientPin(r)
	if !ok {
		m.rejectRosterReconcile(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var remote Roster
	if err := json.Unmarshal(body, &remote); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	changed := m.mergeRoster(&remote, senderUUID)
	if changed {
		log.Printf("roster: reconciled from peer %s; membership changed", senderUUID)
	}
	// Learn the peer's current address from the authenticated connection so a
	// member that moved to a new IP becomes reachable again without waiting on
	// mDNS. verifyClientPin already authenticated the sender against its pinned
	// cert, so the TCP source IP is trustworthy enough to dial back on the
	// member's known listening port (we pass port 0 to keep the stored
	// listening port, never the ephemeral source port).
	if host := hostOnly(r.RemoteAddr); host != "" && m.noteObservedPeerHost(senderUUID, host) {
		log.Printf("roster: refreshed peer %s address to %s (from connection)", senderUUID, host)
		changed = true
	}
	if changed {
		m.emitNodesChanged()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m.buildLocalRoster())
}

// rosterRejection is the body a roster reconcile returns alongside a 403 when the
// caller is not (or is no longer) a pinned member. It carries any signed removal
// tombstone this node holds that names the caller — authenticated,
// cluster-scoped proof that the caller was removed. A node removed while it was
// offline verifies this against a signer it still trusts and treats it as
// grounds to self-remove; a bare 403 with no such proof (a peer that itself left,
// lost its pin, or has not finished pinning the caller during trust fan-out) is
// NOT proof of removal, so the caller must not self-evict on it.
type rosterRejection struct {
	Tombstones    []Tombstone    `json:"tombstones,omitempty"` // legacy compatibility; never self-removal proof
	RemovalProofs []RemovalProof `json:"removalProofs,omitempty"`
}

// rejectRosterReconcile answers an unauthenticated / no-longer-pinned roster
// caller with 403, attaching any tombstone we hold that names the caller's
// certificate UUID so a node removed while offline gets authenticated proof of
// its removal (see rosterRejection). The tombstone is independently Ed25519
// signed, so the caller verifies it against its own trusted-signer set rather
// than trusting this transport.
func (m *Manager) rejectRosterReconcile(w http.ResponseWriter, r *http.Request) {
	var body rosterRejection
	if uuid := clientCertUUID(r); uuid != "" {
		if t, ok := m.tombstoneFor(uuid); ok {
			body.Tombstones = []Tombstone{t}
		}
		for _, p := range m.snapshotRemovalProofs() {
			if p.Tombstone.NodeUUID == uuid {
				body.RemovalProofs = []RemovalProof{p}
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(body)
}

// clientCertUUID returns the node UUID carried in the request's client
// certificate (URI SAN / CN), or "" when there is no client cert. Unlike
// verifyClientPin it does not require the cert to be pinned — a removed caller is
// by definition no longer pinned, but we still key its removal proof by the UUID
// it presents.
func clientCertUUID(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return uuidFromCert(r.TLS.PeerCertificates[0])
}

// rejectionProvesRemoval reports whether a 403 roster-reconcile response body
// carries authenticated, cluster-scoped proof that THIS node was removed: a
// tombstone naming us, scoped to the cluster we currently believe we are in,
// signed by a node we still trust. This is what distinguishes "you were removed"
// (self-eviction is warranted) from "this peer left or lost its pin" (a bare 403
// — the surviving node must stay). The tombstone's own Ed25519 signature is what
// we verify, so a removal signed by any trusted member and relayed by this peer
// counts.
func (m *Manager) rejectionProvesRemoval(body []byte, relayUUID string) bool {
	if len(body) == 0 {
		return false
	}
	var rej rosterRejection
	if err := json.Unmarshal(body, &rej); err != nil {
		return false
	}
	cid, _ := m.clusterIdentity()
	if cid == "" {
		return false
	}
	admissionCID, admissionEpoch := m.currentAdmission()
	if admissionCID != cid || admissionEpoch == 0 {
		return false
	}
	for _, p := range rej.RemovalProofs {
		t := p.Tombstone
		if t.NodeUUID != m.identity.NodeUUID || t.AdmissionEpoch != admissionEpoch {
			continue
		}
		// The response transport is pinned to relayUUID. Require that the proof
		// chain reaches a currently trusted member; verifyRemovalProof checks the
		// exact relay/remover admissions and signer certificate without mutating
		// trust. A bare/legacy tombstone is deliberately insufficient.
		if !m.verifyRemovalProof(p, cid) {
			continue
		}
		if t.By == relayUUID {
			return true
		}
		for _, end := range p.Endorsements {
			if end.By == relayUUID {
				return true
			}
		}
	}
	return false
}

// reconcileOutcome is the result of a single reconcileWith attempt, used by the
// periodic pass to decide whether every peer has dropped our pin.
type reconcileOutcome int

const (
	reconcileUnreachable reconcileOutcome = iota // dial error, no addr, non-200/403 status, or unusable body
	reconcileAccepted                            // 200: the peer still holds our pin
	reconcileRejected                            // 403: the peer no longer holds our pin (removed us, left, or lost its pin)
)

// reconcileWith pushes our roster to a peer over mTLS and merges the roster it
// returns, reporting whether the peer accepted us (200), rejected us (403), or
// was unreachable. The second return reports whether a 403 carried authenticated
// proof that this node was removed (a signed tombstone naming us); it is false
// for every non-403 outcome. Best-effort: a failure to reach one peer never
// blocks the others.
//
// addrs are the peer's candidate endpoints, best-ranked first. The reconcile
// starts at the one already confirmed to accept connections and walks the rest on
// a transport failure, so a peer whose recorded address stopped working is still
// converged in this pass instead of waiting for the address to be corrected from
// some other direction.
//
// pairingHTTPTimeout bounds the whole walk, not each address in it. Failover is
// worth spending a round trip on, but not multiplying one: the walk runs on a
// heartbeat, and the self-removal verdict in reconcilePeersAndMaybeSelfRemove
// waits on every peer's goroutine, so a multi-homed peer whose addresses all
// blackhole would otherwise stall the whole pass for as many timeouts as it
// published addresses.
func (m *Manager) reconcileWith(addrs []string, peerUUID string) (reconcileOutcome, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), pairingHTTPTimeout)
	defer cancel()
	return m.reconcileWithin(ctx, addrs, peerUUID)
}

// reconcileWithin is reconcileWith under a caller-supplied budget.
func (m *Manager) reconcileWithin(ctx context.Context, addrs []string, peerUUID string) (reconcileOutcome, bool) {
	if len(addrs) == 0 {
		return reconcileUnreachable, false
	}
	client, err := m.peerClient(peerUUID)
	if err != nil {
		log.Printf("roster: no pin for peer %s: %v", peerUUID, err)
		return reconcileUnreachable, false
	}
	body, err := json.Marshal(m.buildLocalRoster())
	if err != nil {
		log.Printf("roster: marshal local roster: %v", err)
		return reconcileUnreachable, false
	}
	for _, addr := range confirmedFirst(addrs, m.peerAddrs.ChooseWithin(ctx, peerUUID, addrs)) {
		outcome, proven, answered := m.reconcileOnce(ctx, client, body, addr, peerUUID)
		if answered {
			return outcome, proven
		}
		// Nothing answered at this address, so the remembered choice is stale;
		// the next pass re-confirms rather than starting here again.
		m.peerAddrs.Forget(peerUUID)
		if ctx.Err() != nil {
			log.Printf("roster: reconcile with %s spent its address budget; the remaining addresses wait for the next pass",
				peerUUID)
			break
		}
	}
	return reconcileUnreachable, false
}

// confirmedFirst returns addrs with confirmed moved to the front, preserving the
// peer's ranking for the rest.
func confirmedFirst(addrs []string, confirmed string) []string {
	if confirmed == "" || len(addrs) == 0 || addrs[0] == confirmed {
		return addrs
	}
	ordered := make([]string, 0, len(addrs))
	ordered = append(ordered, confirmed)
	for _, a := range addrs {
		if a != confirmed {
			ordered = append(ordered, a)
		}
	}
	return ordered
}

// reconcileOnce performs one reconcile round trip against a single address.
// answered is false only when the peer never responded there, which is what tells
// reconcileWith to try the next candidate; every actual response — including a
// rejection — is that peer's answer and ends the walk.
func (m *Manager) reconcileOnce(ctx context.Context, client *http.Client, body []byte, addr, peerUUID string) (outcome reconcileOutcome, provenRemoval, answered bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+rosterPath, bytes.NewReader(body))
	if err != nil {
		log.Printf("roster: build reconcile request for %s (%s): %v", addr, peerUUID, err)
		return reconcileUnreachable, false, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("roster: reconcile with %s (%s): %v", addr, peerUUID, err)
		return reconcileUnreachable, false, false
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	// A 403 is an authenticated signal (we pinned the peer's server cert, so we
	// know it is really that peer) that it no longer holds our pin. But a dropped
	// pin alone does not prove *we* were removed — a peer that itself left drops
	// every pin too. So we also look for an authenticated removal tombstone in
	// the 403 body; the caller requires that proof before treating a unanimous
	// rejection as grounds to self-remove.
	if resp.StatusCode == http.StatusForbidden {
		proven := m.rejectionProvesRemoval(rb, peerUUID)
		if proven {
			log.Printf("roster: reconcile with %s (%s): 403 with a signed removal tombstone for us (we were removed)", addr, peerUUID)
		} else {
			log.Printf("roster: reconcile with %s (%s): 403 without removal proof (peer left or dropped our pin without removing us)", addr, peerUUID)
		}
		return reconcileRejected, proven, true
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("roster: reconcile with %s (%s): status %d", addr, peerUUID, resp.StatusCode)
		return reconcileUnreachable, false, true
	}
	var remote Roster
	if err := json.Unmarshal(rb, &remote); err != nil {
		log.Printf("roster: decode reconcile response from %s: %v", peerUUID, err)
		return reconcileAccepted, false, true // 200: the peer accepted us; only the response body was unusable
	}
	if m.mergeRoster(&remote, peerUUID) {
		log.Printf("roster: reconciled with peer %s; membership changed", peerUUID)
		m.emitNodesChanged()
	}
	return reconcileAccepted, false, true
}

// reconcileAllMembers pushes a roster reconcile to every confirmed peer member,
// each in its own goroutine. Used after a pairing, after a removal, and on the
// periodic heartbeat to converge the cluster's trust fabric.
func (m *Manager) reconcileAllMembers() {
	for _, n := range m.snapshotNodes() {
		if n.NodeUUID == m.identity.NodeUUID || n.State != stateMember {
			continue
		}
		addrs := m.resolvePeerAddrs(n)
		uuid := n.NodeUUID
		go m.reconcileWith(addrs, uuid)
	}
}

// reconcilePeersAndMaybeSelfRemove reconciles with every confirmed peer, waits
// for all of them, and self-removes only if EVERY peer rejected us with a 403
// AND at least one of those rejections carried authenticated proof that we were
// removed (a signed tombstone naming us; see rejectionProvesRemoval).
//
// A 403 alone means the peer no longer holds our pin — but that is NOT proof the
// cluster removed us. handleRoster also 403s a caller whose pin a peer dropped
// for other reasons: most importantly a peer that itself ran cluster:leave drops
// every pin (including ours), so a node that was offline when its only peer left
// comes back to a unanimous 403 even though it is the surviving cluster member.
// Requiring an authenticated, cluster-scoped removal tombstone distinguishes
// "you were removed" from "this peer left or lost its pin," and also excludes the
// trust-fan-out window (a peer that has not finished pinning us yet 403s but
// holds no tombstone for us). An unreachable peer counts as neither accept nor
// reject and blocks teardown rather than triggering it. This is the backstop for
// a node removed while it was offline — it never received the direct
// members/remove notify and only learns of its eviction here. Called from the
// periodic pass only (never the post-pair/removal push), so a pairing still
// converging cannot trip it.
//
// The verdict is only valid for the cluster we were in when we sent those
// reconciles. The network fan-out can take up to pairingHTTPTimeout per peer, and
// a pairing/rejoin can complete on another goroutine during that wait — adopting
// a new clusterId, or re-pinning us back into the same one. That would establish
// valid new cluster state that the now-stale verdict must not erase. So we
// snapshot the cluster identity and a generation counter before fanning out and
// revalidate both under rosterMu (the teardown boundary) before tearing down: if
// either changed, the verdict is stale and we skip.
func (m *Manager) reconcilePeersAndMaybeSelfRemove() {
	cid0, _ := m.clusterIdentity()
	if cid0 == "" {
		return
	}
	gen0 := m.clusterGen.Load()

	var wg sync.WaitGroup
	var mu sync.Mutex
	peers, rejected, proven := 0, 0, 0
	var bareRejected []ClusterNode
	for _, n := range m.snapshotNodes() {
		if n.NodeUUID == m.identity.NodeUUID || n.State != stateMember {
			continue
		}
		peers++
		addrs := m.resolvePeerAddrs(n)
		uuid := n.NodeUUID
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcome, provenRemoval := m.reconcileWith(addrs, uuid)
			if outcome != reconcileRejected {
				return
			}
			mu.Lock()
			rejected++
			if provenRemoval {
				proven++
			} else {
				bareRejected = append(bareRejected, n)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if proven == 0 && len(bareRejected) > 0 {
		m.removeBareRejectingPeers(cid0, gen0, bareRejected)
	}
	if peers == 0 || rejected != peers {
		return
	}
	// A unanimous 403 is necessary but not sufficient. Require at least one peer
	// to have furnished authenticated, cluster-scoped proof of our removal;
	// absent any proof, a peer merely left or lost its pin and we are the
	// surviving member, so we must stay.
	if proven == 0 {
		log.Printf("roster: all %d peer(s) rejected our pin but none furnished a signed removal tombstone; not self-removing (a peer likely left or lost its pin and this node is the surviving member)", peers)
		return
	}

	// Unanimous rejection. Revalidate and tear down in one rosterMu critical
	// section. Because every cluster-composition commit (adopt, create,
	// set-identity, pairing, merge) also holds rosterMu, this recheck-then-
	// teardown is linearizable against them: a rejoin either committed before we
	// take the lock — so we observe the new identity/generation and skip — or it
	// blocks until our teardown finishes and then cleanly re-establishes the
	// cluster, never interleaving with it. A changed clusterId means we joined a
	// different cluster; a bumped generation with an unchanged clusterId means we
	// were re-pinned back into the same one — both invalidate the stale verdict.
	m.rosterMu.Lock()
	cidNow, _ := m.clusterIdentity()
	stale := cidNow == "" || cidNow != cid0 || m.clusterGen.Load() != gen0
	var teardownErr error
	if !stale {
		teardownErr = m.teardownClusterLocalLocked()
	}
	m.rosterMu.Unlock()

	if stale {
		log.Printf("roster: all %d peer(s) rejected our pin, but local cluster state changed during the pass; skipping self-remove (stale verdict)", peers)
		return
	}
	if teardownErr != nil {
		log.Printf("roster: proven self-removal durable teardown incomplete (will retry on restart): %v", teardownErr)
		return
	}
	log.Printf("roster: all %d cluster peer(s) rejected our pin; self-removing (removed while offline)", peers)
	m.emitIdentityChanged()
	m.emitNodesChanged()
}

func (m *Manager) removeBareRejectingPeers(clusterID string, generation uint64, peers []ClusterNode) {
	m.rosterMu.Lock()
	if cid, _ := m.clusterIdentity(); cid != clusterID || m.clusterGen.Load() != generation {
		m.rosterMu.Unlock()
		return
	}
	changed := false
	for _, rejected := range peers {
		current, ok := m.memberByNodeID(rejected.NodeUUID)
		pin, pinned := m.trust.Get(rejected.NodeUUID)
		if !ok || !pinned || current.ClusterID != clusterID || pin.ClusterID != clusterID ||
			current.AdmissionEpoch != rejected.AdmissionEpoch ||
			pin.AdmissionEpoch != rejected.AdmissionEpoch {
			continue
		}
		if err := m.trust.Remove(rejected.NodeUUID); err != nil {
			log.Printf("roster: remove departed peer %s after bare 403: %v", rejected.NodeUUID, err)
			continue
		}
		if m.removeMemberByUUID(rejected.NodeUUID) {
			changed = true
		}
	}
	if changed {
		if err := m.persistMembersErr(); err != nil {
			log.Printf("roster: persist departed-peer cleanup: %v", err)
		}
	}
	m.rosterMu.Unlock()
	if changed {
		m.emitNodesChanged()
	}
}

// reconcileLoop runs the periodic catch-up reconcile and tombstone GC until the
// context is cancelled. Each pass first refreshes member addresses from the
// mDNS browse map, so a member that moved to a new IP (sleep/wake, DHCP)
// re-converges within a heartbeat without a dedicated discovery callback. An
// initial pass shortly after startup converges a node that just (re)started or
// woke at a new address without waiting a full heartbeat.
func (m *Manager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()
	initial := time.NewTimer(initialReconcileDelay)
	defer initial.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-initial.C:
			m.reconcilePass()
		case <-ticker.C:
			m.reconcilePass()
		}
	}
}

// reconcilePass GCs expired tombstones, refreshes member addresses from the
// discovery resolver, and pushes a roster reconcile to every confirmed peer.
func (m *Manager) reconcilePass() {
	m.gcTombstones()
	if cid, _ := m.clusterIdentity(); cid == "" {
		return
	}
	m.mesh.Refresh()
	m.clients.DropUnpinned()
	m.refreshMemberAddrsFromMDNS()
	m.reconcilePeersAndMaybeSelfRemove()
}

// refreshMemberAddrsFromMDNS updates each confirmed peer's stored reachable
// address from the mDNS browse map when the peer has stopped advertising the
// address we hold. Inter-node calls are mTLS cert-pinned, so trusting mDNS only
// to *locate* a peer is safe — a wrong address simply fails the pinned handshake
// and the next pass corrects it. This is the backstop that re-converges a moved
// member even when it can't reach us to announce its new address itself.
//
// A peer that still advertises the address we hold keeps it. That address was
// observed working from here, whereas the peer's own first choice is ranked from
// its vantage point and may be a link only its cabled neighbour can use, so
// overwriting on every pass would trade a known-good endpoint for a guess.
func (m *Manager) refreshMemberAddrsFromMDNS() {
	if m.browser == nil {
		return
	}
	changed := false
	for _, n := range m.snapshotNodes() {
		if n.NodeUUID == m.identity.NodeUUID || n.State != stateMember {
			continue
		}
		hosts, port, ok := m.browser.Resolve(n.NodeUUID)
		if !ok {
			hosts, port, ok = m.browser.Resolve(n.ID)
		}
		if !ok || len(hosts) == 0 {
			continue
		}
		host := hosts[0]
		if slices.Contains(hosts, n.IPAddress) {
			host = n.IPAddress
		}
		if m.setMemberAddr(n.NodeUUID, host, port) {
			log.Printf("roster: refreshed peer %s address to %s (from discovery)", n.NodeUUID, joinHostPort(host, port, m.port))
			changed = true
		}
	}
	if changed {
		m.emitNodesChanged()
	}
}

// resolvePeerAddrs returns a member's host:port candidates in the order to try
// them: the address recorded at pairing time first (authoritative — it is the
// source IP the peer connected from plus the listening port it advertised), then
// everything the peer currently advertises through mDNS.
//
// The recorded address leads but is not the only option, because it can go stale
// or name a link this host cannot reach while the peer is plainly reachable at
// another of its addresses. Sitting on the recorded one is how a reconcile came
// to time out against a peer whose working address was already in hand.
func (m *Manager) resolvePeerAddrs(n ClusterNode) []string {
	addrs := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	add := func(addr string) {
		if addr == "" || seen[addr] {
			return
		}
		seen[addr] = true
		addrs = append(addrs, addr)
	}
	if n.IPAddress != "" {
		add(joinHostPort(n.IPAddress, n.Port, m.port))
	}
	if m.browser != nil {
		hosts, port, ok := m.browser.Resolve(n.NodeUUID)
		if !ok {
			hosts, port, ok = m.browser.Resolve(n.ID)
		}
		if ok {
			for _, h := range hosts {
				add(joinHostPort(h, port, m.port))
			}
		}
	}
	return addrs
}
