// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sync"
	"time"

	"eapnoob"
)

// pairingInfoVersion is the schema version of the PairingInfo object embedded in
// EAP-NOOB ServerInfo/PeerInfo (§7.2).
const pairingInfoVersion = 2

// legacyAdmissionEpoch is the deterministic incarnation assigned to peers from
// the v1 protocol and pre-admission on-disk state. Those peers had no admission
// counter and therefore all represent their first admission to the cluster.
const legacyAdmissionEpoch uint64 = 1

// pinPattern matches a well-formed six-digit PIN (§7.6).
var pinPattern = regexp.MustCompile(`^[0-9]{6}$`)

// PairingInfo is the object each side puts in EAP-NOOB ServerInfo (inviter) /
// PeerInfo (joiner). It binds the node's cert and identity into the Hoob and the
// Completion MACs (§7.2). Identical schema both directions.
type PairingInfo struct {
	V                   int    `json:"v"`
	NodeUUID            string `json:"nodeUuid"`
	NodeID              string `json:"nodeId"`
	Name                string `json:"name"`
	ClusterID           string `json:"clusterId"`
	AdmissionEpoch      uint64 `json:"admissionEpoch,omitempty"`
	ClusterFriendlyName string `json:"clusterFriendlyName"`
	Addr                string `json:"addr,omitempty"`
	Cert                string `json:"cert"`
	// PinSalt is the inviter's per-invite PBKDF2 salt for stretching the PIN
	// into the OOB Noob (see pinNoob), base64-encoded. It rides inside the
	// EAP-NOOB transcript (ServerInfo is MAC-covered), so tampering with it
	// breaks the transcript handshake rather than substituting a chosen Noob.
	PinSalt string `json:"pinSalt,omitempty"`
}

// localPairingInfo builds this node's PairingInfo. addr is the inviter's
// reachable host:port for the joiner-driven Completion Exchange; it is required
// on the inviter's ServerInfo and optional on the joiner's PeerInfo.
func (m *Manager) localPairingInfo(addr string, admissionEpoch ...uint64) *PairingInfo {
	cid, friendly := m.clusterIdentity()
	epoch := uint64(0)
	if len(admissionEpoch) > 0 {
		epoch = admissionEpoch[0]
	} else if admissionCID, current := m.currentAdmission(); admissionCID == cid {
		epoch = current
	}
	return &PairingInfo{
		V:                   pairingInfoVersion,
		NodeUUID:            m.identity.NodeUUID,
		NodeID:              m.identity.NodeID,
		Name:                m.identity.Name,
		ClusterID:           cid,
		AdmissionEpoch:      epoch,
		ClusterFriendlyName: friendly,
		Addr:                addr,
		Cert:                string(m.identity.CertPEM),
	}
}

// localPairingInfoWithPinSalt is localPairingInfo for inviter pairing sessions,
// additionally carrying the PIN stretching salt so the joiner can reconstruct
// the Noob from the user's typed PIN.
func (m *Manager) localPairingInfoWithPinSalt(addr string, salt []byte) (*PairingInfo, error) {
	pi := m.localPairingInfo(addr)
	pi.PinSalt = base64.StdEncoding.EncodeToString(salt)
	return pi, nil
}

// toMap renders the PairingInfo as the map[string]any the eap-noob config
// expects for ServerInfo/PeerInfo.
func (pi *PairingInfo) toMap() (map[string]any, error) {
	b, err := json.Marshal(pi)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// parsePairingInfo decodes a peer's PairingInfo from the authenticated EAP-NOOB
// transcript and validates that the embedded cert's principal matches its
// nodeUuid (§7.2: a PairingInfo whose cert CN/URI != nodeUuid is rejected).
func parsePairingInfo(raw []byte) (*PairingInfo, *x509.Certificate, error) {
	var pi PairingInfo
	if err := json.Unmarshal(raw, &pi); err != nil {
		return nil, nil, fmt.Errorf("decode PairingInfo: %w", err)
	}
	if pi.NodeUUID == "" {
		return nil, nil, fmt.Errorf("PairingInfo missing nodeUuid")
	}
	block, _ := pem.Decode([]byte(pi.Cert))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("PairingInfo has no CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse PairingInfo cert: %w", err)
	}
	if got := uuidFromCert(cert); got != pi.NodeUUID {
		return nil, nil, fmt.Errorf("PairingInfo cert principal %q != nodeUuid %q", got, pi.NodeUUID)
	}
	switch {
	case pi.V >= pairingInfoVersion && pi.AdmissionEpoch == 0:
		return nil, nil, fmt.Errorf("PairingInfo v%d missing admissionEpoch", pi.V)
	case pi.V < pairingInfoVersion && pi.AdmissionEpoch == 0:
		// v1 peers did not carry admission epochs. Their first and only
		// pre-upgrade incarnation maps to epoch 1 on every upgraded member.
		pi.AdmissionEpoch = legacyAdmissionEpoch
	}
	return &pi, cert, nil
}

// pinPBKDF2Iterations is the stretching work factor applied when deriving the
// PIN Noob. The NoobId the joiner transmits is H("NoobId", Noob) with no key,
// so a passive observer of the plaintext pairing channel could otherwise check
// all 10^6 PIN candidates against a captured NoobId in milliseconds. A PBKDF2
// pass per candidate makes offline enumeration ~10^6 × iteration cost instead,
// and the online cap in handlePairingCompletion complements it for active
// guessing. Tune upward with hardware.
const pinPBKDF2Iterations = 50000

// newPinSalt returns a fresh per-invite PBKDF2 salt for stretching the PIN
// into the OOB Noob. It is generated before the Initial Exchange so it can ride
// inside the inviter's ServerInfo — which the EAP-NOOB transcript MACs —
// giving the joiner an integrity-protected copy without a second channel.
func newPinSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate pin salt: %w", err)
	}
	return salt, nil
}

// mintPIN draws the random six-digit PIN and derives its Noob under the
// session's salt. Identical PINs across invites produce unrelated Noobs, and a
// captured NoobId cannot be attacked offline without redoing the full stretched
// search over all 10^6 candidates.
func mintPIN(salt []byte) (string, []byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", nil, fmt.Errorf("generate pin: %w", err)
	}
	pin := fmt.Sprintf("%06d", n.Int64())
	return pin, pinNoob(pin, salt), nil
}

// pinNoob derives the 16-byte Noob from a PIN and its per-invite salt:
// PBKDF2-HMAC-SHA256(PIN, salt), first 16 bytes. Stretching is the
// compensating control for the low-entropy PIN Noob (documented security
// debt, §4). An empty salt falls back to the legacy big-endian encoding so a
// pre-salt inviter remains pairable; the salt itself is not secret (it is
// MAC-covered against tampering), the work factor is the point.
func pinNoob(pin string, salt []byte) []byte {
	if len(salt) == 0 {
		return noobFromPIN(pin)
	}
	k, err := pbkdf2.Key(sha256.New, pin, salt, pinPBKDF2Iterations, 16)
	if err != nil {
		// sha256.New never fails to construct; an error here is a programming
		// fault, so fail closed rather than pairing on a weak Noob.
		panic(fmt.Sprintf("pbkdf2: %v", err))
	}
	return k
}

// deriveNoobFromPINAndSalt reconstructs the Noob on the joiner side from the
// PIN the user typed and the salt from the inviter's MAC-covered ServerInfo.
func deriveNoobFromPINAndSalt(pin string, salt []byte) []byte {
	return pinNoob(pin, salt)
}

// noobFromPIN encodes a six-digit PIN as a 16-byte big-endian Noob. This is the
// legacy unsalted derivation, kept only for tests asserting the migration
// reference; production flows use pinNoob with a per-invite salt.
func noobFromPIN(pin string) []byte {
	v := new(big.Int)
	v.SetString(pin, 10)
	b := make([]byte, 16)
	v.FillBytes(b) // big-endian, left-padded with zeros
	return b
}

// newPairingServer builds the inviter-side EAP-NOOB Server, server-to-peer OOB
// only (Dirs=2), carrying this node's PairingInfo as ServerInfo.
func newPairingServer(info map[string]any) *eapnoob.Server {
	return eapnoob.NewServer(eapnoob.ServerConfig{Dirs: 2, ServerInfo: info}, nil)
}

// newPairingPeer builds the joiner-side EAP-NOOB Peer preferring server-to-peer
// OOB (PreferDir=2), carrying this node's PairingInfo as PeerInfo.
func newPairingPeer(info map[string]any) *eapnoob.Peer {
	return eapnoob.NewPeer(eapnoob.PeerConfig{PreferDir: 2, PeerInfo: info}, nil)
}

// pairingRole fixes which EAP-NOOB role a session plays for its whole lifetime.
type pairingRole int

const (
	roleInviter pairingRole = iota // EAP-NOOB Server
	roleJoiner                     // EAP-NOOB Peer
)

// pairingSignalLabel domain-separates the terminal-signal MAC derived from the
// session's ephemeral Key Exchange secret.
const pairingSignalLabel = "nvpair-pairing-signal-v1"

// pairingSignalMAC derives the authentication tag for a terminal pairing
// signal (cancel/decline/expire/fail) from the session's EphemeralKey — the
// ECDH secret both sides already share after the Initial Exchange. A passive
// on-path observer of the plaintext pairing channel can read the inviteId but
// cannot compute this tag, so terminal signals are no longer forgeable. The
// tag is base64-encoded for transport; verification decodes before comparing.
func pairingSignalMAC(key []byte, inviteID, phase string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(pairingSignalLabel))
	mac.Write([]byte{0})
	mac.Write([]byte(inviteID))
	mac.Write([]byte{0})
	mac.Write([]byte(phase))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verifyPairingSignal reports whether env carries a valid tag for the given
// phase under the session's ephemeral key. Unknown/absent keys verify nothing.
func verifyPairingSignal(sess *pairingSession, env *pairingEnvelope, phase string) bool {
	if sess == nil || env.SignalTag == "" {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	key, err := sess.ephemeralKey()
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(pairingSignalMAC(key, env.InviteID, phase))
	if err != nil {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(env.SignalTag)
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}

// pairingSession holds the live EAP-NOOB crypto state for one in-flight pairing,
// kept alive across the separate HTTP requests of the Initial and Completion
// exchanges and the human PIN step in between (§7.2). Keyed by inviteId.
type pairingSession struct {
	inviteID  string
	role      pairingRole
	createdAt int64
	server    *eapnoob.Server // set for roleInviter
	peer      *eapnoob.Peer   // set for roleJoiner
	addr      string          // inviter listening host:port (joiner drives Completion here)
	// joinerAddr is the target host:port the inviter POSTed the Initial Exchange
	// to (set for roleInviter). It lets the inviter best-effort notify the joiner
	// on cancel so the joiner's pending prompt clears.
	joinerAddr string

	// clusterID is the cluster this pairing is scoped to, captured when the
	// inviter session is created (set for roleInviter; empty for roleJoiner,
	// which is adopting a cluster rather than adding to one). commitPairing
	// rechecks it against the live identity under the teardown boundary so a
	// Completion that lands after this node left/was removed does not resurrect
	// the joiner's pin/member into an emptied cluster.
	clusterID string

	// peerPairing / peerCert hold the other side's authenticated PairingInfo,
	// captured when the Initial Exchange completes (joiner) or at Registered
	// (inviter), for pinning and cluster adoption.
	peerPairing *PairingInfo
	peerCert    *x509.Certificate
	// localAdmissionEpoch is this node's reserved admission incarnation carried
	// in its authenticated PairingInfo. It is activated only if the joiner
	// successfully commits the inviter's cluster.
	localAdmissionEpoch uint64
	// peerSrcIP is the joiner's source IP observed on its Completion POST, used
	// by the inviter to record a reachable address when the joiner did not
	// advertise one in its PeerInfo.
	peerSrcIP string
	// invitedEmitted guards the one-shot cluster:invite-received notification.
	invitedEmitted bool
	// The inviter keeps a successful Completion session until the joiner
	// acknowledges its own durable commit. This makes a lost EAP-Success response
	// retryable and lets a post-success joiner failure roll the inviter back.
	awaitingAck        bool
	completionResponse []byte
	// completionAttempts counts Completion Exchange messages fed to the EAP
	// server, bounding online PIN guessing against the six-digit PIN Noob
	// (see handlePairingCompletion).
	completionAttempts int
	// pinSalt is the per-invite PBKDF2 salt (inviter: generated with the PIN;
	// joiner: learned from the inviter's authenticated ServerInfo) used to
	// stretch the PIN into the OOB Noob.
	pinSalt []byte

	mu sync.Mutex
}

// maxCompletionAttempts bounds PIN-entry attempts per invite: enough for
// honest typos, far below the 10^6 PIN keyspace.
const maxCompletionAttempts = 5

// ephemeralKey returns this session's Key Exchange secret (see
// eapnoob.EphemeralKey). The caller must hold sess.mu (the eapnoob Server/Peer
// objects are not synchronized). Sessions whose EAP object was never created
// (or whose Initial Exchange never completed) report an error — callers treat
// the signal as unauthenticated/unsendable.
func (s *pairingSession) ephemeralKey() ([]byte, error) {
	if s.role == roleInviter {
		if s.server == nil {
			return nil, errors.New("no inviter EAP session")
		}
		return s.server.EphemeralKey()
	}
	if s.peer == nil {
		return nil, errors.New("no joiner EAP session")
	}
	return s.peer.EphemeralKey()
}

func (m *Manager) putSession(s *pairingSession) {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	m.sessions[s.inviteID] = s
}

func (m *Manager) getSession(inviteID string) (*pairingSession, bool) {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	s, ok := m.sessions[inviteID]
	return s, ok
}

func (m *Manager) deleteSession(inviteID string) {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	delete(m.sessions, inviteID)
}

func (m *Manager) joinerSessionCount() int {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	count := 0
	for _, session := range m.sessions {
		if session.role == roleJoiner {
			count++
		}
	}
	return count
}

func (m *Manager) registerJoinerSession(session *pairingSession) (registered, atCapacity bool) {
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	if m.teardownPending.Load() {
		return false, false
	}
	if cid, _ := m.clusterIdentity(); cid != "" {
		return false, false
	}
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	if _, exists := m.sessions[session.inviteID]; exists {
		return false, false
	}
	count := 0
	for _, current := range m.sessions {
		if current.role == roleJoiner {
			count++
		}
	}
	if count >= maxPreInviteSessions {
		return false, true
	}
	m.sessions[session.inviteID] = session
	return true, false
}

// resetSessions drops every live EAP-NOOB pairing session. Used by teardown so
// an in-flight pairing cannot complete against a cluster this node has already
// left; combined with the cluster-epoch recheck in commitPairing it guarantees
// a Completion that lands after removal resurrects nothing. Bumping sessGen lets
// an in-flight Initial Exchange (registerInviterSession) detect that a teardown
// cleared sessions since it began — even across a rejoin into the same cluster.
func (m *Manager) resetSessions() {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	m.sessions = make(map[string]*pairingSession)
	m.sessGen.Add(1)
}

// registerInviterSession installs the inviter's pairing session only while its
// invite is still pending, this node is still in the cluster the invite was
// scoped to (cid0), and no teardown has cleared the session map since the invite
// began (sessGen0). Holding inviteMu across the state check and installation
// makes registration atomic against cancel: either this session is installed and
// cancel removes it, or cancel records its terminal state first and registration
// is refused. rosterMu provides the equivalent boundary against cluster teardown.
func (m *Manager) registerInviterSession(sess *pairingSession, cid0 string, sessGen0 uint64) bool {
	m.inviteMu.Lock()
	defer m.inviteMu.Unlock()
	if inv, ok := m.getInvite(sess.inviteID); !ok || inv.State != inviteStatePending {
		return false
	}
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	if cid, _ := m.clusterIdentity(); cid == "" || cid != cid0 || m.sessGen.Load() != sessGen0 {
		return false
	}
	m.putSession(sess)
	return true
}

// withLivePairing runs fn under the teardown boundary iff sess is still the live
// session for its inviteId. A teardown clears the session map under the same
// rosterMu, so a dropped (or replaced) session means a leave / inbound removal /
// self-remove abandoned this pairing; fn — which republishes the Broker-facing
// invite, stamps its PIN, or records its terminal state — then does not run,
// leaving the invite as the teardown left it (cleared) rather than resurrecting
// it into an emptied cluster. Reports whether fn ran. The caller holds inviteMu;
// rosterMu nests under it consistently with foundCluster.
//
// A nil session is not live: an exchange that failed before it could register one
// has nothing for fn to be scoped to.
func (m *Manager) withLivePairing(sess *pairingSession, fn func()) bool {
	if sess == nil {
		return false
	}
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	if cur, ok := m.getSession(sess.inviteID); !ok || cur != sess {
		return false
	}
	fn()
	return true
}

// withPendingPairing applies fn to the invite as currently recorded, but only
// while sess is still the live session for that invite and the invite is still
// pending. It reports the state the invite was left in ("" if it is gone
// entirely) and whether fn ran. The caller holds inviteMu.
//
// Both guards are needed and neither implies the other. A teardown clears the
// session map, which is withLivePairing's question. A cancel instead writes its
// own terminal state and drops only the session of the attempt that was running
// — so the next address in the walk registers a session of its own, passes the
// liveness test, and would write a fresh PIN over an invite the user has already
// canceled. The invite state is the authoritative gate on that, the same one
// handlePairingCompletion re-reads under this mutex on every Completion round
// trip.
//
// fn is handed the recorded invite rather than the copy captured before the
// Initial Exchange began, because a copy from before the network I/O is precisely
// what cannot be trusted to still describe the invite.
//
// A pairing whose invite is no longer pending loses its session here as well: a
// live pairing server with an unpublished PIN is exactly what a cancel exists to
// remove.
func (m *Manager) withPendingPairing(inviteID string, sess *pairingSession, fn func(inv *Invite)) (InviteState, bool) {
	applied := false
	m.withLivePairing(sess, func() {
		if cur, ok := m.getInvite(inviteID); ok && cur.State == inviteStatePending {
			fn(cur)
			applied = true
			return
		}
		m.deleteSession(inviteID)
	})
	// The state the invite was left in, whichever writer left it there: when this
	// attempt lost, a cancel's terminal state is the truthful answer to report to
	// the invite request that lost to it.
	if cur, ok := m.getInvite(inviteID); ok {
		return cur.State, applied
	}
	return "", applied
}

// endorsePeer produces this node's endorsement of a freshly-paired peer's cert,
// scoped to the current cluster. Gossiping this with the peer's pin is what lets
// other members transitively trust the peer (§ fan-out trust).
func (m *Manager) endorsePeer(peerUUID, fingerprint string, peerAdmissionEpoch ...uint64) Endorsement {
	cid, _ := m.clusterIdentity()
	_, selfEpoch := m.currentAdmission()
	targetEpoch := uint64(0)
	if len(peerAdmissionEpoch) > 0 {
		targetEpoch = peerAdmissionEpoch[0]
	}
	return m.endorsePeerForAdmission(peerUUID, fingerprint, cid, targetEpoch, selfEpoch)
}

func (m *Manager) endorsePeerForAdmission(peerUUID, fingerprint, clusterID string, peerEpoch, selfEpoch uint64) Endorsement {
	return signEndorsement(m.identity.Signer, m.identity.NodeUUID, peerUUID, fingerprint, clusterID,
		time.Now().UnixMilli(), peerEpoch, selfEpoch)
}
