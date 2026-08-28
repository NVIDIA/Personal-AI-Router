// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const removalProofsFile = "removal-proofs.json"

// RemovalProof is durable, relay-verifiable evidence that one exact admission
// of a node was removed. The tombstone preserves the original remover's
// signature; SignerCert supplies that key to a victim that never pinned the
// remover; Endorsements establish the signer through a member the verifier
// still trusts.
type RemovalProof struct {
	Tombstone         Tombstone     `json:"tombstone"`
	SignerCertPem     string        `json:"signerCertPem"`
	SignerFingerprint string        `json:"signerFingerprint"`
	Endorsements      []Endorsement `json:"endorsements,omitempty"`
}

func (m *Manager) removalProofsPath() string {
	return filepath.Join(m.clusterDir, removalProofsFile)
}

func (m *Manager) newRemovalProof(nodeUUID string, admissionEpoch uint64) (RemovalProof, error) {
	cid, selfEpoch := m.currentAdmission()
	if cid == "" || selfEpoch == 0 || admissionEpoch == 0 {
		return RemovalProof{}, fmt.Errorf("removal proof requires active remover and victim admissions")
	}
	t := signTombstone(m.identity.Signer, m.identity.NodeUUID, nodeUUID, cid,
		time.Now().UnixMilli(), admissionEpoch, selfEpoch)
	return RemovalProof{
		Tombstone:         t,
		SignerCertPem:     string(m.identity.CertPEM),
		SignerFingerprint: m.identity.CertFingerprint,
	}, nil
}

func proofKey(p RemovalProof) string {
	return p.Tombstone.NodeUUID
}

func cloneRemovalProof(p RemovalProof) RemovalProof {
	p.Endorsements = append([]Endorsement(nil), p.Endorsements...)
	return p
}

// targetAdmissionAuthenticated reports whether local authenticated state binds
// uuid to exactly epoch. A remover may only persist a new proof while it still
// has that exact admission pinned; otherwise a trusted but malicious peer could
// invent a huge victim epoch and permanently suppress every real readmission.
// Self-removal is bound to this node's durable active admission instead.
func (m *Manager) targetAdmissionAuthenticated(uuid string, epoch uint64) bool {
	if uuid == "" || epoch == 0 {
		return false
	}
	if uuid == m.identity.NodeUUID {
		_, current := m.currentAdmission()
		return current == epoch
	}
	pin, ok := m.trust.Get(uuid)
	return ok && pin.AdmissionEpoch == epoch
}

// putRemovalProof persists p before making it visible. A proof for a newer
// admission supersedes an older one; proof material for the same admission is
// unioned so relay chains become richer over time.
func (m *Manager) putRemovalProof(p RemovalProof) (bool, error) {
	if p.Tombstone.NodeUUID == "" || p.Tombstone.AdmissionEpoch == 0 {
		return false, fmt.Errorf("removal proof has no targeted admission")
	}
	p = m.withLocalRelayEndorsement(p)
	m.proofMu.Lock()
	defer m.proofMu.Unlock()
	key := proofKey(p)
	if cur, ok := m.removalProofs[key]; ok {
		switch {
		case cur.Tombstone.AdmissionEpoch > p.Tombstone.AdmissionEpoch:
			return false, nil
		case cur.Tombstone.AdmissionEpoch == p.Tombstone.AdmissionEpoch:
			p.Endorsements = mergeProofEndorsements(cur.Endorsements, p.Endorsements)
			if removalProofEqual(cur, p) {
				return false, nil
			}
		}
	}
	if !m.targetAdmissionAuthenticated(p.Tombstone.NodeUUID, p.Tombstone.AdmissionEpoch) {
		return false, fmt.Errorf("removal proof target admission is not authenticated")
	}
	old, hadOld := m.removalProofs[key]
	m.removalProofs[key] = cloneRemovalProof(p)
	if err := m.persistRemovalProofsLocked(); err != nil {
		if hadOld {
			m.removalProofs[key] = old
		} else {
			delete(m.removalProofs, key)
		}
		return false, err
	}
	return true, nil
}

func removalProofEqual(a, b RemovalProof) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

func mergeProofEndorsements(a, b []Endorsement) []Endorsement {
	out := append([]Endorsement(nil), a...)
	seen := make(map[string]struct{}, len(out))
	for _, e := range out {
		seen[e.By+"|"+e.SigV2] = struct{}{}
	}
	for _, e := range b {
		key := e.By + "|" + e.SigV2
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}

func (m *Manager) removalProofFor(uuid string) (RemovalProof, bool) {
	m.proofMu.Lock()
	defer m.proofMu.Unlock()
	p, ok := m.removalProofs[uuid]
	return cloneRemovalProof(p), ok
}

func (m *Manager) removalProofBlocksAdmission(uuid, clusterID string, admissionEpoch uint64) bool {
	p, ok := m.removalProofFor(uuid)
	return ok && p.Tombstone.ClusterID == clusterID &&
		(admissionEpoch == 0 || admissionEpoch <= p.Tombstone.AdmissionEpoch)
}

func (m *Manager) withLocalRelayEndorsement(p RemovalProof) RemovalProof {
	cid, selfEpoch := m.currentAdmission()
	if cid == "" || selfEpoch == 0 || p.Tombstone.ClusterID != cid {
		return p
	}
	for _, end := range p.Endorsements {
		if end.By == m.identity.NodeUUID && end.ByAdmissionEpoch == selfEpoch &&
			end.AdmissionEpoch == p.Tombstone.ByAdmissionEpoch &&
			end.Fingerprint == p.SignerFingerprint && end.SigV2 != "" {
			return p
		}
	}
	relay := signEndorsement(m.identity.Signer, m.identity.NodeUUID, p.Tombstone.By,
		p.SignerFingerprint, cid, time.Now().UnixMilli(),
		p.Tombstone.ByAdmissionEpoch, selfEpoch)
	p.Endorsements = mergeProofEndorsements(p.Endorsements, []Endorsement{relay})
	return p
}

// snapshotRemovalProofs returns every durable proof. putRemovalProof stores this
// node's relay endorsement with the proof so restart never has to endorse
// unvalidated disk content.
func (m *Manager) snapshotRemovalProofs() []RemovalProof {
	m.proofMu.Lock()
	out := make([]RemovalProof, 0, len(m.removalProofs))
	for _, stored := range m.removalProofs {
		out = append(out, cloneRemovalProof(stored))
	}
	m.proofMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Tombstone.NodeUUID < out[j].Tombstone.NodeUUID
	})
	return out
}

// clearRemovalProofAfterAdmission deletes proof only after a strictly newer
// authenticated admission has already been durably pinned.
func (m *Manager) clearRemovalProofAfterAdmission(uuid string, admissionEpoch uint64) error {
	m.proofMu.Lock()
	defer m.proofMu.Unlock()
	p, ok := m.removalProofs[uuid]
	if !ok || admissionEpoch <= p.Tombstone.AdmissionEpoch {
		return nil
	}
	delete(m.removalProofs, uuid)
	if err := m.persistRemovalProofsLocked(); err != nil {
		m.removalProofs[uuid] = p
		return err
	}
	return nil
}

func (m *Manager) clearAllRemovalProofs() error {
	m.proofMu.Lock()
	defer m.proofMu.Unlock()
	if len(m.removalProofs) == 0 {
		return nil
	}
	old := m.removalProofs
	m.removalProofs = make(map[string]RemovalProof)
	if err := m.persistRemovalProofsLocked(); err != nil {
		m.removalProofs = old
		return err
	}
	return nil
}

func (m *Manager) persistRemovalProofsLocked() error {
	out := make([]RemovalProof, 0, len(m.removalProofs))
	for _, p := range m.removalProofs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Tombstone.NodeUUID < out[j].Tombstone.NodeUUID
	})
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.removalProofsPath(), data, 0o600)
}

func (m *Manager) loadRemovalProofs() error {
	data, err := os.ReadFile(m.removalProofsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []RemovalProof
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	m.proofMu.Lock()
	defer m.proofMu.Unlock()
	for _, p := range list {
		if p.Tombstone.NodeUUID == "" || p.Tombstone.AdmissionEpoch == 0 {
			continue
		}
		cur, ok := m.removalProofs[p.Tombstone.NodeUUID]
		if !ok || p.Tombstone.AdmissionEpoch > cur.Tombstone.AdmissionEpoch {
			m.removalProofs[p.Tombstone.NodeUUID] = p
		}
	}
	return nil
}

// replayRemovalProofs finishes a removal that crashed after durable proof
// persistence but before de-pinning. It runs during construction, before any
// endpoint is served.
func (m *Manager) replayRemovalProofs() error {
	cid, _ := m.currentAdmission()
	if cid == "" {
		return nil
	}
	changed := false
	for _, p := range m.snapshotRemovalProofs() {
		t := p.Tombstone
		if t.NodeUUID == m.identity.NodeUUID || !m.verifyRemovalProof(p, cid) {
			continue
		}
		pin, pinned := m.trust.Get(t.NodeUUID)
		if pinned && pin.AdmissionEpoch == t.AdmissionEpoch {
			if err := m.trust.Remove(t.NodeUUID); err != nil {
				return fmt.Errorf("replay removal proof for %s: %w", t.NodeUUID, err)
			}
			changed = true
		}
		if n, ok := m.memberByNodeID(t.NodeUUID); ok && n.AdmissionEpoch == t.AdmissionEpoch {
			if m.removeMemberByUUID(t.NodeUUID) {
				changed = true
			}
		}
	}
	if changed {
		if err := m.persistMembersErr(); err != nil {
			return fmt.Errorf("persist replayed removals: %w", err)
		}
	}
	return nil
}

func parseProofSigner(p RemovalProof) ([]byte, error) {
	block, _ := pem.Decode([]byte(p.SignerCertPem))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("proof signer has no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	if uuidFromCert(cert) != p.Tombstone.By {
		return nil, fmt.Errorf("proof signer principal mismatch")
	}
	if certFingerprintFromDER(block.Bytes) != p.SignerFingerprint {
		return nil, fmt.Errorf("proof signer fingerprint mismatch")
	}
	return block.Bytes, nil
}

func (m *Manager) trustedAdmissionEpoch(uuid string) (uint64, bool) {
	if uuid == m.identity.NodeUUID {
		_, epoch := m.currentAdmission()
		return epoch, epoch != 0
	}
	pin, ok := m.trust.Get(uuid)
	if !ok || pin.AdmissionEpoch == 0 {
		return 0, false
	}
	return pin.AdmissionEpoch, true
}

// verifyRemovalProof validates the original signer and an admission-bound trust
// path to that signer. It never mutates trust or membership.
func (m *Manager) verifyRemovalProof(p RemovalProof, clusterID string) bool {
	t := p.Tombstone
	if t.ClusterID != clusterID || t.NodeUUID == "" || t.AdmissionEpoch == 0 ||
		t.By == "" || t.ByAdmissionEpoch == 0 || t.SigV2 == "" {
		return false
	}
	der, err := parseProofSigner(p)
	if err != nil {
		return false
	}
	pub, err := pubFromDER(der)
	if err != nil || !verifyTombstone(pub, clusterID, t) {
		return false
	}

	// A signer we still trust directly needs no relay endorsement, but the exact
	// certificate and admission incarnation must match the current pin.
	if t.By == m.identity.NodeUUID {
		_, selfEpoch := m.currentAdmission()
		return selfEpoch == t.ByAdmissionEpoch && bytes.Equal(der, m.identity.Cert.Certificate[0])
	}
	if pin, ok := m.trust.Get(t.By); ok && pin.AdmissionEpoch == t.ByAdmissionEpoch {
		if pinnedDER, ok := m.trust.DER(t.By); ok && bytes.Equal(pinnedDER, der) {
			return true
		}
	}

	// Otherwise require an endorsement over the remover's exact certificate and
	// admission from a member we still trust in its exact current admission.
	for _, end := range p.Endorsements {
		if end.SigV2 == "" || end.AdmissionEpoch != t.ByAdmissionEpoch {
			continue
		}
		byEpoch, ok := m.trustedAdmissionEpoch(end.By)
		if !ok || byEpoch != end.ByAdmissionEpoch {
			continue
		}
		pub, ok := m.trustedPub(end.By)
		if !ok {
			continue
		}
		if verifyEndorsement(pub, t.By, p.SignerFingerprint, clusterID, end) {
			return true
		}
	}
	return false
}
