// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"
)

// RosterEntry is one member's identity + cert + the endorsements that admit it,
// as carried in a roster reconcile. The endorsements are what let a receiver
// transitively pin a peer it has never paired with directly (§ fan-out trust).
type RosterEntry struct {
	NodeUUID        string        `json:"nodeUuid"`
	NodeID          string        `json:"nodeId"`
	Name            string        `json:"name"`
	Addr            string        `json:"addr"` // reachable host:port for inter-node calls
	AdmissionEpoch  uint64        `json:"admissionEpoch,omitempty"`
	CertPem         string        `json:"certPem"`
	CertFingerprint string        `json:"certFingerprint"`
	Endorsements    []Endorsement `json:"endorsements,omitempty"`
}

// Roster is a node's full view of the cluster exchanged on the reconcile
// endpoint: every member it trusts (plus itself) and the removals it knows of.
type Roster struct {
	ClusterID     string         `json:"clusterId"`
	Members       []RosterEntry  `json:"members"`
	Tombstones    []Tombstone    `json:"tombstones,omitempty"` // legacy compatibility
	RemovalProofs []RemovalProof `json:"removalProofs,omitempty"`
}

// selfPub returns this node's own Ed25519 public key.
func (m *Manager) selfPub() ed25519.PublicKey {
	return m.identity.Signer.Public().(ed25519.PublicKey)
}

// trustedPub resolves the public key of a node this manager currently trusts:
// itself, or any pinned peer. The bool reports whether it is trusted at all.
func (m *Manager) trustedPub(uuid string) (ed25519.PublicKey, bool) {
	if uuid == m.identity.NodeUUID {
		return m.selfPub(), true
	}
	return m.trust.PubKey(uuid)
}

// buildLocalRoster assembles this node's outbound roster: a self entry plus one
// entry per pinned peer (carrying that peer's stored endorsements), and the live
// tombstone set. Addresses are filled from the membership map when known.
func (m *Manager) buildLocalRoster() *Roster {
	cid, _ := m.clusterIdentity()
	r := &Roster{
		ClusterID:     cid,
		Tombstones:    m.snapshotTombstones(),
		RemovalProofs: m.snapshotRemovalProofs(),
	}

	// Self entry — consumed by the already-trusting mTLS peer to refresh our
	// metadata; it carries no endorsement (the peer already trusts us) and no
	// addr (the peer has our source IP from the connection, and transitive
	// learners get our addr from whichever node pinned us).
	r.Members = append(r.Members, RosterEntry{
		NodeUUID:        m.identity.NodeUUID,
		NodeID:          m.identity.NodeID,
		Name:            m.identity.Name,
		AdmissionEpoch:  func() uint64 { _, epoch := m.currentAdmission(); return epoch }(),
		CertPem:         string(m.identity.CertPEM),
		CertFingerprint: m.identity.CertFingerprint,
	})

	for _, pin := range m.trust.List() {
		entry := RosterEntry{
			NodeUUID:        pin.NodeUUID,
			NodeID:          pin.NodeID,
			Name:            pin.Name,
			AdmissionEpoch:  pin.AdmissionEpoch,
			CertPem:         pin.CertPem,
			CertFingerprint: pin.CertFingerprint,
			Endorsements:    pin.Endorsements,
		}
		if n, ok := m.memberByNodeID(pin.NodeUUID); ok && n.IPAddress != "" {
			entry.Addr = net.JoinHostPort(n.IPAddress, strconv.Itoa(n.Port))
		}
		r.Members = append(r.Members, entry)
	}
	return r
}

// mergeRoster reconciles a peer's roster into local state: it applies signed
// removals (newest-wins tombstones) and transitively pins every member whose
// cert is endorsed by a node we already trust, iterating to a fixpoint. It
// returns whether local membership/trust changed. senderUUID is the
// mTLS-authenticated peer the roster came from (already a trusted member).
// Serialized with teardownClusterLocal via rosterMu.
//
// NOTE: this acquires rosterMu itself, so it must never be called from inside a
// withClusterComposition closure (which already holds it) — rosterMu is not
// reentrant and that would deadlock.
func (m *Manager) mergeRoster(remote *Roster, senderUUID string) bool {
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	if m.teardownPending.Load() {
		return false
	}
	cid, _ := m.clusterIdentity()
	if cid == "" || remote == nil || remote.ClusterID != cid {
		return false
	}
	changed := false
	if m.applyRemovalProofs(remote.RemovalProofs, cid) {
		changed = true
	}
	if m.applyTombstones(remote.Tombstones, cid) {
		changed = true
	}
	if m.applyMembers(remote.Members, cid, senderUUID) {
		changed = true
	}
	return changed
}

// applyRemovalProofs verifies and durably stores admission-targeted removals.
// Persistence precedes de-pinning so an offline victim can always recover the
// proof later. Self-targeted proofs are handled only on an authenticated 403;
// roster gossip cannot directly evict this node from its own view.
func (m *Manager) applyRemovalProofs(proofs []RemovalProof, cid string) bool {
	changed := false
	for _, p := range proofs {
		t := p.Tombstone
		if t.NodeUUID == m.identity.NodeUUID || !m.verifyRemovalProof(p, cid) {
			continue
		}
		_, err := m.putRemovalProof(p)
		if err != nil {
			log.Printf("roster: persist removal proof for %s: %v", t.NodeUUID, err)
			continue
		}
		// Keep the bare mirror for old peers, but admission-aware ordering is
		// driven exclusively by the proof.
		m.putTombstone(t)
		if pin, pinned := m.trust.Get(t.NodeUUID); pinned && pin.AdmissionEpoch != t.AdmissionEpoch {
			if pin.AdmissionEpoch > t.AdmissionEpoch {
				if err := m.clearRemovalProofAfterAdmission(t.NodeUUID, pin.AdmissionEpoch); err != nil {
					log.Printf("roster: clear superseded proof for %s: %v", t.NodeUUID, err)
				}
			}
			continue
		}
		if member, ok := m.memberByNodeID(t.NodeUUID); ok && member.AdmissionEpoch != t.AdmissionEpoch {
			continue
		}
		if _, pinned := m.trust.Get(t.NodeUUID); pinned {
			if err := m.trust.Remove(t.NodeUUID); err != nil {
				log.Printf("roster: enforce removal proof for %s: %v", t.NodeUUID, err)
				continue
			}
			changed = true
		}
		if m.removeMemberByUUID(t.NodeUUID) {
			changed = true
		}
	}
	return changed
}

// applyTombstones retains verified v1 removals only for legacy gossip. They are
// never admission authority locally: a v2 tombstone also carries a v1
// compatibility signature, and accepting a stripped relay would let an old
// removal evict a newer readmission. Only RemovalProof can de-pin a member.
func (m *Manager) applyTombstones(tombstones []Tombstone, cid string) bool {
	for _, t := range tombstones {
		if t.NodeUUID == m.identity.NodeUUID {
			continue
		}
		if t.SigV2 != "" {
			continue
		}
		pub, ok := m.trustedPub(t.By)
		if !ok || !verifyTombstone(pub, cid, t) {
			continue
		}
		m.putTombstone(t)
	}
	return false
}

// applyMembers pins every entry endorsed by a currently-trusted node, iterating
// to a fixpoint so a newly-pinned node can in turn vouch for the nodes it
// endorsed (the endorsement graph is the human-authorized pairing graph).
// senderUUID is the mTLS-authenticated peer this roster came from; its own entry
// is authoritative for its current display name so host renames propagate.
func (m *Manager) applyMembers(entries []RosterEntry, cid, senderUUID string) bool {
	changed := false
	for {
		progressed := false
		for i := range entries {
			e := &entries[i]
			if e.NodeUUID == m.identity.NodeUUID {
				continue
			}
			// Already trusted: enrich endorsements + refresh address, and — for
			// the sender describing itself — refresh its display name so a PC
			// rename propagates instead of stranding the old hostname.
			if pin, pinned := m.trust.Get(e.NodeUUID); pinned {
				if e.AdmissionEpoch > pin.AdmissionEpoch {
					if m.supersededByTombstone(e) {
						continue
					}
					der, err := validateEntryCert(e)
					if err != nil {
						continue
					}
					if !m.entryEndorsed(e, cid) {
						continue
					}
					if err := m.pinEntry(e, der); err != nil {
						continue
					}
					if err := m.clearRemovalProofAfterAdmission(e.NodeUUID, e.AdmissionEpoch); err != nil {
						log.Printf("roster: clear superseded proof for %s: %v", e.NodeUUID, err)
					}
					m.clearTombstone(e.NodeUUID)
					changed = true
					progressed = true
					continue
				}
				if err := m.trust.AddEndorsements(e.NodeUUID, e.Endorsements); err != nil {
					log.Printf("roster: persist endorsements for %s: %v", e.NodeUUID, err)
				}
				m.refreshMemberAddr(e)
				if e.NodeUUID == senderUUID && m.refreshMemberIdentity(e) {
					changed = true
				}
				continue
			}
			if m.supersededByTombstone(e) {
				continue
			}
			der, err := validateEntryCert(e)
			if err != nil {
				continue
			}
			if !m.entryEndorsed(e, cid) {
				continue // no endorsement from a node we currently trust
			}
			if err := m.pinEntry(e, der); err != nil {
				continue
			}
			if err := m.clearRemovalProofAfterAdmission(e.NodeUUID, e.AdmissionEpoch); err != nil {
				log.Printf("roster: clear superseded proof for %s: %v", e.NodeUUID, err)
			}
			m.clearTombstone(e.NodeUUID)
			changed = true
			progressed = true
		}
		if !progressed {
			return changed
		}
	}
}

// entryEndorsed reports whether any endorsement on the entry was made by a node
// this manager currently trusts, over the entry's exact cert and cluster.
func (m *Manager) entryEndorsed(e *RosterEntry, cid string) bool {
	if e.AdmissionEpoch == 0 {
		return false
	}
	for _, end := range e.Endorsements {
		if end.SigV2 == "" || end.AdmissionEpoch != e.AdmissionEpoch {
			continue
		}
		byEpoch, ok := m.trustedAdmissionEpoch(end.By)
		if !ok || end.ByAdmissionEpoch != byEpoch {
			continue
		}
		pub, ok := m.trustedPub(end.By)
		if !ok {
			continue
		}
		if verifyEndorsement(pub, e.NodeUUID, e.CertFingerprint, cid, end) {
			return true
		}
	}
	return false
}

// supersededByTombstone reports whether a removal newer than every endorsement
// on the entry exists, in which case the entry must not be (re-)pinned. A later
// endorsement (re-invite) wins over an older tombstone.
func (m *Manager) supersededByTombstone(e *RosterEntry) bool {
	if p, ok := m.removalProofFor(e.NodeUUID); ok {
		return e.AdmissionEpoch == 0 || e.AdmissionEpoch <= p.Tombstone.AdmissionEpoch
	}
	t, ok := m.tombstoneFor(e.NodeUUID)
	if !ok || t.SigV2 == "" || t.AdmissionEpoch == 0 {
		return false
	}
	return e.AdmissionEpoch == 0 || e.AdmissionEpoch <= t.AdmissionEpoch
}

// pinEntry pins an accepted roster entry's cert and records it as a member.
func (m *Manager) pinEntry(e *RosterEntry, der []byte) error {
	now := time.Now().UnixMilli()
	host, port := splitAddr(e.Addr, m.port)
	cid, _ := m.clusterIdentity()
	previous, hadPrevious := m.memberByNodeID(e.NodeUUID)
	m.upsertMember(&ClusterNode{
		ID:             e.NodeID,
		NodeUUID:       e.NodeUUID,
		Name:           e.Name,
		IPAddress:      host,
		Port:           port,
		ClusterID:      cid,
		AdmissionEpoch: e.AdmissionEpoch,
		State:          stateMember,
		JoinedAt:       &now,
	})
	restoreMember := func() error {
		if hadPrevious {
			m.upsertMember(previous)
		} else {
			m.removeMemberByUUID(e.NodeUUID)
		}
		return m.persistMembersErr()
	}
	if err := m.persistMembersErr(); err != nil {
		return errors.Join(err, restoreMember())
	}
	if err := m.trust.Pin(&TrustedPin{
		NodeUUID:        e.NodeUUID,
		NodeID:          e.NodeID,
		Name:            e.Name,
		ClusterID:       func() string { c, _ := m.clusterIdentity(); return c }(),
		AdmissionEpoch:  e.AdmissionEpoch,
		CertPem:         e.CertPem,
		CertFingerprint: certFingerprintFromDER(der),
		PinnedAt:        time.Now().UnixMilli(),
		Endorsements:    e.Endorsements,
	}); err != nil {
		return errors.Join(err, restoreMember())
	}
	return nil
}

// refreshMemberAddr updates a known member's reachable address from a roster
// entry without disturbing its other fields.
//
// A host this node has seen the member connect from outranks the entry's claim,
// which may be the member's own guess about which of its addresses works from
// here — or another peer's relayed copy of that guess. The port is taken from the
// entry either way: a listening port is a fact the member knows and an observer
// cannot infer, since the source port of an inbound connection is ephemeral.
func (m *Manager) refreshMemberAddr(e *RosterEntry) {
	if e.Addr == "" {
		return
	}
	host, port := splitAddr(e.Addr, m.port)
	if observed := m.observedPeerHost(e.NodeUUID); observed != "" {
		host = observed
	}
	m.setMemberAddr(e.NodeUUID, host, port)
}

// refreshMemberIdentity updates a still-trusted peer's display id/name — in both
// the member record and the persisted pin — from its own roster entry, keyed by
// the stable nodeUuid. This tracks a peer's PC rename so the roster and UI show
// its current name instead of the hostname frozen at pairing. Returns
// whether anything changed.
func (m *Manager) refreshMemberIdentity(e *RosterEntry) bool {
	changed := m.updateMemberIdentity(e.NodeUUID, e.NodeID, e.Name)
	if ok, err := m.trust.UpdateIdentity(e.NodeUUID, e.NodeID, e.Name); err != nil {
		log.Printf("roster: update pin identity for %s: %v", e.NodeUUID, err)
	} else if ok {
		changed = true
	}
	return changed
}

// validateEntryCert parses and sanity-checks a roster entry's certificate,
// returning its DER. The cert principal must equal the entry UUID and the
// embedded fingerprint must match the cert, so an endorsement (which signs over
// the fingerprint) is bound to this exact cert.
func validateEntryCert(e *RosterEntry) ([]byte, error) {
	block, _ := pem.Decode([]byte(e.CertPem))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("entry %s: no CERTIFICATE block", e.NodeUUID)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("entry %s: parse cert: %w", e.NodeUUID, err)
	}
	if got := uuidFromCert(cert); got != e.NodeUUID {
		return nil, fmt.Errorf("entry cert principal %q != nodeUuid %q", got, e.NodeUUID)
	}
	if certFingerprintFromDER(block.Bytes) != e.CertFingerprint {
		return nil, fmt.Errorf("entry %s: fingerprint mismatch", e.NodeUUID)
	}
	return block.Bytes, nil
}
