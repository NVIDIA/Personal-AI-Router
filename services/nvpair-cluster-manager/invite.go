// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"eapnoob"
	"nvpair-shared/reach"
)

// pairingHTTPTimeout bounds each inter-node pairing round trip.
const pairingHTTPTimeout = 10 * time.Second

// pairingRejectedError signals the peer explicitly refused the pairing (rather
// than it failing on transport/protocol). The inviter maps it to an invite
// result of state:"rejected" instead of "failed". reason is the peer-supplied
// machine reason (e.g. "already-clustered"), empty if none was given.
type pairingRejectedError struct{ reason string }

func (e *pairingRejectedError) Error() string {
	if e.reason == "" {
		return "pairing rejected by peer"
	}
	return "pairing rejected by peer: " + e.reason
}

type inviteNodeParams struct {
	Address string  `json:"address"`
	Port    *int    `json:"port"`
	NodeID  *string `json:"nodeId"`
}

// handleInviteNode starts a pairing: it runs the EAP-NOOB Initial Exchange as
// the HTTP client + EAP Server, derives the six-digit PIN, and returns
// {inviteId, state, pin}. The Server is kept alive (keyed by inviteId) to serve
// the joiner-driven Completion later (§7.0/§7.2).
func (m *Manager) handleInviteNode(msg *Message) {
	var p inviteNodeParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
		return
	}
	port := m.port
	if p.Port != nil {
		if *p.Port < 1 || *p.Port > 65535 {
			m.codec.RespondErrorData(msg.ID, codeInvalidParams, "port out of range", map[string]string{"field": "port"})
			return
		}
		port = *p.Port
	}
	// A Broker-supplied address is tried first; the peer's own remaining
	// addresses follow it as failover candidates, because the address any
	// observer holds for a multi-homed node can be one only its cabled
	// neighbour can reach. With no supplied address, resolve the nodeId/UUID
	// through the mDNS browse map (§7.5).
	//
	// Each candidate carries its own port. The discovered ones use the port the
	// peer advertises for its cluster service, which is the only authority on
	// where that peer listens; a caller-supplied port still overrides everything,
	// and this node's own port is the last resort.
	endpoints := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	addEndpoint := func(host string, p int) {
		if host == "" || p < 1 || p > 65535 {
			return
		}
		ep := net.JoinHostPort(host, strconv.Itoa(p))
		if seen[ep] {
			return
		}
		seen[ep] = true
		endpoints = append(endpoints, ep)
	}
	addEndpoint(p.Address, port)
	if p.NodeID != nil && m.browser != nil {
		if resolved, mport, ok := m.browser.Resolve(*p.NodeID); ok {
			discovered := port
			if p.Port == nil && mport > 0 {
				discovered = mport
			}
			for _, host := range resolved {
				addEndpoint(host, discovered)
			}
		}
	}
	if len(endpoints) == 0 {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "address is required (or a discoverable nodeId)",
			map[string]string{"field": "address"})
		return
	}
	// Probe before taking inviteMu. That mutex also serializes cancellation,
	// decline, expiry and pairing commit, and a handful of connect timeouts to
	// unreachable NICs must not hold any of them up.
	endpoints = reachableEndpointFirst(endpoints)

	// Serialize cluster founding + pending-invite registration with terminal
	// cleanup. The pending record is installed before network I/O so a decline or
	// expiry on a sibling cannot observe "no pending invites" in this window.
	m.inviteMu.Lock()

	// Auto-found a cluster of one when this node isn't clustered yet, so a caller
	// can invite straight away with no separate cluster:create step.
	// This runs the same founding path as cluster:create, in-process, so the
	// former check-then-create round trip (and its cross-wire race) is gone. Mark
	// this provenance as invite-created so cleanup may dissolve it when
	// every outbound invite terminates without a peer.
	cid, friendly := m.clusterIdentity()
	if cid == "" {
		newID, name, err := m.foundCluster("")
		if err != nil {
			m.inviteMu.Unlock()
			m.codec.RespondErrorData(msg.ID, codeInternalError, "found cluster: "+err.Error(), nil)
			return
		}
		cid, friendly = newID, name
		m.setInviteCreatedCluster(true)
	}
	inviteID, err := newInviteID()
	if err != nil {
		m.inviteMu.Unlock()
		m.codec.RespondErrorData(msg.ID, codeInternalError, "mint inviteId: "+err.Error(), nil)
		return
	}
	now := time.Now().UnixMilli()
	inv := &Invite{
		InviteID:            inviteID,
		FromNodeID:          m.identity.NodeID,
		FromNodeUUID:        m.identity.NodeUUID,
		FromNodeName:        m.identity.Name,
		ToNodeID:            p.NodeID,
		ClusterID:           cid,
		ClusterFriendlyName: friendly,
		State:               inviteStatePending,
		CreatedAt:           now,
	}
	// Publish the first pending invite and capture the session generation in one
	// teardown-boundary critical section. If teardown already won, publish
	// nothing; if it wins after this returns, it clears this invite and bumps the
	// captured generation together.
	sessGen0, published := m.publishInitialInvite(inv, cid)
	m.inviteMu.Unlock()
	if !published {
		log.Printf("invite %s: cluster was torn down before initial publication; abandoning", inviteID)
		m.codec.Respond(msg.ID, map[string]any{"inviteId": inviteID, "state": inviteStateFailed})
		return
	}

	sess, pin, err := m.pairAtFirstWorkingEndpoint(inviteID, endpoints, cid, sessGen0)
	if err != nil {
		var rej *pairingRejectedError
		rejected := errors.As(err, &rej)
		state := inviteStateFailed
		if rejected {
			state = inviteStateRejected
		}
		m.inviteMu.Lock()
		// Terminal bookkeeping only while this pairing is still live and its
		// invite still pending. A teardown (leave / inbound removal /
		// self-remove) clears the session and the pending invite together, and a
		// cancel writes its own terminal state; if either already did — or
		// registration was refused because the cluster was gone — leave that
		// record alone rather than overwriting it with this attempt's outcome.
		recorded, applied := m.withPendingPairing(inviteID, sess, func(cur *Invite) {
			m.deleteSession(inviteID)
			cur.State = state
			m.putInvite(cur)
		})
		if applied {
			// A terminated invite establishes no peer, so drop a solo cluster
			// this invite auto-founded.
			m.maybeLeaveInviteCreatedClusterLocked()
		}
		m.inviteMu.Unlock()

		switch {
		case !applied:
			log.Printf("invite %s: initial exchange ended (%v) after the pairing was abandoned; leaving the recorded state %q alone",
				inviteID, err, recorded)
			m.codec.Respond(msg.ID, map[string]any{"inviteId": inviteID, "state": abandonedState(recorded)})
		case rejected:
			log.Printf("invite %s: rejected by peer (reason %q)", inviteID, rej.reason)
			resp := map[string]any{"inviteId": inviteID, "state": inviteStateRejected}
			if rej.reason != "" {
				resp["reason"] = rej.reason
			}
			m.codec.Respond(msg.ID, resp)
		default:
			log.Printf("invite %s: initial exchange failed: %v", inviteID, err)
			m.codec.Respond(msg.ID, map[string]any{"inviteId": inviteID, "state": inviteStateFailed})
		}
		return
	}

	m.inviteMu.Lock()
	recorded, applied := m.withPendingPairing(inviteID, sess, func(cur *Invite) {
		cur.Pin = &pin
		m.putInvite(cur)
	})
	m.inviteMu.Unlock()
	if !applied {
		// A teardown or a cancel abandoned this pairing during the Initial
		// Exchange; don't resurrect a pending invite (and PIN) into a cluster
		// this node has left, or over a record the user has already terminated.
		log.Printf("invite %s: abandoned during the initial exchange (recorded state %q); not publishing the PIN",
			inviteID, recorded)
		m.codec.Respond(msg.ID, map[string]any{"inviteId": inviteID, "state": abandonedState(recorded)})
		return
	}
	m.codec.Respond(msg.ID, map[string]any{"inviteId": inviteID, "state": inviteStatePending, "pin": pin})
}

func (m *Manager) publishInitialInvite(inv *Invite, clusterID string) (uint64, bool) {
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	cid, _ := m.clusterIdentity()
	admissionCID, admissionEpoch := m.currentAdmission()
	if cid == "" || cid != clusterID || admissionCID != cid || admissionEpoch == 0 {
		return 0, false
	}
	gen := m.sessGen.Load()
	m.putInvite(inv)
	return gen, true
}

// errPairingClusterGone means a teardown cleared the cluster this invite was
// scoped to (or a rejoin bumped the session generation) before the Initial
// Exchange could register its session, so the pairing is abandoned rather than
// run against an emptied cluster.
var errPairingClusterGone = errors.New("pairing abandoned: cluster torn down during initial exchange")

// errPairingAbandoned means the invite stopped being pending while the walk was
// between addresses — the user canceled it, or a teardown cleared it — so the
// remaining addresses are not tried.
var errPairingAbandoned = errors.New("pairing abandoned: invite is no longer pending")

// runInitialExchange drives the Initial Exchange to target as the HTTP client,
// keeps the Server alive in the session map, and on success derives + returns
// the six-digit PIN (the Server is now at Waiting with the PIN-Noob injected).
// cid0/sessGen0 are the cluster epoch captured by handleInviteNode; the session
// is only registered while the node is still in that cluster and no teardown has
// intervened (see registerInviterSession). The session is returned in every case
// (even on error) so the caller can gate the invite's terminal/PIN bookkeeping on
// its liveness.
func (m *Manager) runInitialExchange(inviteID, target, cid0 string, sessGen0 uint64) (*pairingSession, string, error) {
	myAddr := net.JoinHostPort(outboundIP(target), strconv.Itoa(m.port))
	info, err := m.localPairingInfo(myAddr).toMap()
	if err != nil {
		return nil, "", fmt.Errorf("build pairing info: %w", err)
	}
	server := newPairingServer(info)
	// Scope the session to the cluster we are inviting into (cid0, captured by
	// handleInviteNode) so a Completion that lands after we leave/are removed is
	// discarded by commitPairing's epoch recheck.
	sess := &pairingSession{
		inviteID: inviteID, role: roleInviter, createdAt: time.Now().UnixMilli(),
		server: server, addr: myAddr, clusterID: cid0, joinerAddr: target,
	}
	// Register under the teardown boundary: if the cluster was torn down (or
	// rejoined, bumping the session generation) since the invite began, abandon
	// the pairing rather than resurrecting a session an inbound removal cleared.
	if !m.registerInviterSession(sess, cid0, sessGen0) {
		return sess, "", errPairingClusterGone
	}

	client := &http.Client{Timeout: pairingHTTPTimeout}
	blob, err := server.Start()
	if err != nil {
		return sess, "", fmt.Errorf("server start: %w", err)
	}
	for {
		respBlob, err := postPairingBlob(client, target, inviteID, "initial", blob)
		if err != nil {
			return sess, "", err
		}
		if len(respBlob) == 0 {
			break // joiner reached a terminal state with nothing to send
		}
		out, rerr := server.Receive(respBlob)
		if rerr != nil {
			return sess, "", fmt.Errorf("server receive: %w", rerr)
		}
		blob = out.Send
		if out.Done {
			if len(blob) > 0 {
				// Deliver the final EAP-Failure so the joiner also reaches Waiting.
				_, _ = postPairingBlob(client, target, inviteID, "initial", blob)
			}
			break
		}
		if len(blob) == 0 {
			break
		}
	}

	if server.State() != eapnoob.StateWaiting {
		return sess, "", fmt.Errorf("initial exchange ended in state %s, want Waiting", server.State())
	}
	pin, noob, err := generatePIN()
	if err != nil {
		return sess, "", fmt.Errorf("generate pin: %w", err)
	}
	if _, err := server.OOBOutputWith(noob); err != nil {
		return sess, "", fmt.Errorf("inject pin noob: %w", err)
	}
	return sess, pin, nil
}

// postPairingBlob POSTs one pairing envelope and returns the peer's response
// blob (nil if the peer had nothing to send). Shared by the inviter (Initial)
// and joiner (Completion) HTTP-client loops.
func postPairingBlob(client *http.Client, target, inviteID, phase string, blob []byte) ([]byte, error) {
	env := pairingEnvelope{InviteID: inviteID, Phase: phase}
	if len(blob) > 0 {
		env.Msg = base64.StdEncoding.EncodeToString(blob)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	resp, err := client.Post("http://"+target+pairingPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode == http.StatusConflict {
		// A 409 may be an explicit pairing refusal (rejected envelope with a
		// reason) rather than a protocol error. Decode it so the inviter can
		// report state:"rejected"; a plain-text 409 (session/phase mismatch)
		// won't set Rejected and falls through to the generic error below.
		var out pairingEnvelope
		if json.Unmarshal(rb, &out) == nil && out.Rejected {
			return nil, &pairingRejectedError{reason: out.Reason}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pairing %s: status %d: %s", phase, resp.StatusCode, string(rb))
	}
	var out pairingEnvelope
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("decode pairing response: %w", err)
	}
	if out.Msg == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(out.Msg)
}

// postPairingSignal POSTs a best-effort terminal pairing signal (phase
// "decline" or "fail") to the inviter so it can tear down its pending outbound
// invite. Unlike postPairingBlob it carries no EAP blob (the pairing is already
// terminal) but does carry the Reason so the inviter can surface a specific
// cause (e.g. "incorrect-pin"). Returns an error only on transport / non-200 so
// the caller can retry.
func postPairingSignal(client *http.Client, target, inviteID, phase, reason string) error {
	return postPairingSignalScheme(client, "http", target, inviteID, phase, reason)
}

func postPairingSignalScheme(client *http.Client, scheme, target, inviteID, phase, reason string) error {
	env := pairingEnvelope{InviteID: inviteID, Phase: phase, Reason: reason}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	resp, err := client.Post(scheme+"://"+target+pairingPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pairing %s: status %d", phase, resp.StatusCode)
	}
	return nil
}

// reachableEndpointFirst moves the first endpoint whose pairing port accepts a
// connection to the front, keeping the others behind it in their published order.
//
// Pairing is one user-initiated exchange, so confirming costs a single handshake
// and spares the user an attempt against an address that was never reachable from
// this host — a direct-connect link on the far machine, for instance. It only
// reorders: a handshake proves a port is open, not that the machine behind it is
// the peer being invited, so nothing is discarded on its evidence.
func reachableEndpointFirst(endpoints []string) []string {
	if len(endpoints) < 2 {
		return endpoints
	}
	confirmed, ok := reach.First(endpoints, reach.DefaultTimeout)
	if !ok {
		log.Printf("invite: no published address accepted a pairing connection; trying all %d in published order",
			len(endpoints))
		return endpoints
	}
	return confirmedFirst(endpoints, confirmed)
}

// pairAtFirstWorkingEndpoint runs the Initial Exchange against each endpoint until
// one pairs, and returns that pairing's session and PIN.
//
// A handshake is not a pairing: an endpoint can accept a connection and still be
// the wrong machine, a stale listener, or a host that fails partway through the
// exchange. Identically-wired machines genuinely share a direct-connect address,
// so "something answered here" is exactly the case where the remaining addresses
// matter most. The session is dropped between attempts so each one registers
// cleanly under the same inviteId, and the last attempt's session is returned even
// on failure so the caller's terminal bookkeeping can gate on its liveness.
// A cancel or a teardown between attempts ends the walk: no other address of the
// peer's can make a terminated invite live again, and continuing would run a full
// exchange against a peer the user has already stopped inviting.
//
// The previous attempt's session is deliberately not dropped between attempts.
// The next attempt's registration replaces it, and while it stands a cancel
// arriving between two addresses still finds the live inviter session it requires
// — without it, the user's cancel is refused as "not pending" in exactly the
// window this walk introduced, while the walk carries on to pair.
func (m *Manager) pairAtFirstWorkingEndpoint(inviteID string, endpoints []string, cid0 string, sessGen0 uint64) (*pairingSession, string, error) {
	var lastSess *pairingSession
	lastErr := errors.New("no pairing endpoint")
	for i, target := range endpoints {
		if !m.invitePending(inviteID) {
			return lastSess, "", errPairingAbandoned
		}
		sess, pin, err := m.runInitialExchange(inviteID, target, cid0, sessGen0)
		if err == nil {
			return sess, pin, nil
		}
		lastSess, lastErr = sess, err
		if !pairingWorthAnotherAddress(err) || i == len(endpoints)-1 {
			break
		}
		log.Printf("invite %s: %s did not complete the initial exchange (%v); trying the next published address",
			inviteID, target, err)
	}
	return lastSess, "", lastErr
}

// invitePending reports whether inviteID is still a pending invite. inviteMu is
// held only for the read: it also serializes cancellation, decline, expiry and
// pairing commit, none of which may wait on an exchange's network I/O.
func (m *Manager) invitePending(inviteID string) bool {
	m.inviteMu.Lock()
	defer m.inviteMu.Unlock()
	inv, ok := m.getInvite(inviteID)
	return ok && inv.State == inviteStatePending
}

// abandonedState is the state to report for an invite whose pairing was abandoned
// mid-exchange. The recorded terminal state is reported when there is one — a
// cancel's "canceled" is the truthful answer to an invite request that lost to it
// — and "failed" when a teardown left no record at all.
func abandonedState(recorded InviteState) InviteState {
	if recorded == "" {
		return inviteStateFailed
	}
	return recorded
}

// pairingWorthAnotherAddress reports whether a failure describes the address
// rather than the pairing. A peer that answered and refused refuses at every
// address it has, and a teardown of our own cluster is not something a different
// address of the peer's can fix; anything else is a transport or protocol failure
// that the next address may not have.
func pairingWorthAnotherAddress(err error) bool {
	var rejected *pairingRejectedError
	if errors.As(err, &rejected) {
		return false
	}
	return !errors.Is(err, errPairingClusterGone)
}

// newInviteID returns a fresh correlation id of the form "inv-<hex>".
func newInviteID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "inv-" + hex.EncodeToString(b), nil
}

// outboundIP returns the local IP that routes to target, so the inviter can tell
// the joiner where to POST the Completion Exchange. Returns "" if it can't be
// determined (the joiner then falls back to the source address).
func outboundIP(target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return ""
	}
	defer conn.Close()
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	return ""
}
