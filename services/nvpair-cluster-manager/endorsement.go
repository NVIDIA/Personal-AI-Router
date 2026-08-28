// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strconv"
)

// Endorsement is a trusted member's signed vouching that an introduced node's
// certificate was authenticated by a human PIN pairing. It is the trust
// primitive behind transitive (fan-out) cluster trust: a node accepts an
// introduced peer only if some node it already trusts has endorsed that peer's
// exact cert for the same cluster (§ cluster fan-out design). Because the
// signature is end-to-end it stays verifiable across any number of gossip hops,
// unlike the per-hop authentication mTLS provides.
type Endorsement struct {
	By               string `json:"by"`                         // endorser nodeUuid (the trusted member who signed)
	Fingerprint      string `json:"fingerprint"`                // endorsed cert fingerprint ("sha256:...") this signs over
	ClusterID        string `json:"clusterId"`                  // cluster the endorsement is scoped to
	AdmissionEpoch   uint64 `json:"admissionEpoch,omitempty"`   // exact admission of the endorsed node
	ByAdmissionEpoch uint64 `json:"byAdmissionEpoch,omitempty"` // exact admission of the endorser
	IssuedAt         int64  `json:"issuedAt"`                   // epoch ms (audit metadata; not ordering authority)
	Sig              string `json:"sig"`                        // legacy v1 signature
	SigV2            string `json:"sigV2,omitempty"`            // admission-bound v2 signature
}

// Tombstone is a trusted member's signed assertion that a node was removed from
// the cluster as of RemovedAt. It fans removals out across the mesh so a removed
// node is de-pinned cluster-wide, not just by the remover. Merge is
// last-writer-wins by timestamp against the corresponding add (§ removal fan-out).
type Tombstone struct {
	NodeUUID         string `json:"nodeUuid"`                   // the removed node
	ClusterID        string `json:"clusterId"`                  // cluster the removal is scoped to
	AdmissionEpoch   uint64 `json:"admissionEpoch,omitempty"`   // exact admission being removed
	By               string `json:"by"`                         // remover nodeUuid
	ByAdmissionEpoch uint64 `json:"byAdmissionEpoch,omitempty"` // remover's admission when it issued the proof
	RemovedAt        int64  `json:"removedAt"`                  // epoch ms (audit metadata; not ordering authority)
	Sig              string `json:"sig"`                        // legacy v1 signature
	SigV2            string `json:"sigV2,omitempty"`            // admission-bound v2 signature
}

// Domain-separation tags so an endorsement signature can never be replayed as a
// tombstone signature (or vice versa) and neither can collide with any other
// signing context in the system.
const (
	endorseDomain     = "nvpair-endorse:v1"
	endorseDomainV2   = "nvpair-endorse:v2"
	tombstoneDomain   = "nvpair-remove:v1"
	tombstoneDomainV2 = "nvpair-remove:v2"
)

// endorsePayload is the exact byte string an endorsement signs over. Both signer
// and verifier MUST build it identically. introducedUUID is bound in here (it is
// not a field of Endorsement) so the signature is tied to a specific peer entry.
func endorsePayload(introducedUUID, fingerprint, clusterID string, issuedAt int64) []byte {
	return []byte(endorseDomain + "\n" + introducedUUID + "\n" + fingerprint + "\n" + clusterID + "\n" + strconv.FormatInt(issuedAt, 10))
}

// tombstonePayload is the exact byte string a tombstone signs over.
func tombstonePayload(nodeUUID, clusterID string, removedAt int64) []byte {
	return []byte(tombstoneDomain + "\n" + nodeUUID + "\n" + clusterID + "\n" + strconv.FormatInt(removedAt, 10))
}

func endorsePayloadV2(introducedUUID, fingerprint, clusterID string, admissionEpoch, byAdmissionEpoch uint64, issuedAt int64) []byte {
	return []byte(endorseDomainV2 + "\n" + introducedUUID + "\n" + fingerprint + "\n" + clusterID + "\n" +
		strconv.FormatUint(admissionEpoch, 10) + "\n" + strconv.FormatUint(byAdmissionEpoch, 10) + "\n" +
		strconv.FormatInt(issuedAt, 10))
}

func tombstonePayloadV2(nodeUUID, clusterID string, admissionEpoch, byAdmissionEpoch uint64, removedAt int64) []byte {
	return []byte(tombstoneDomainV2 + "\n" + nodeUUID + "\n" + clusterID + "\n" +
		strconv.FormatUint(admissionEpoch, 10) + "\n" + strconv.FormatUint(byAdmissionEpoch, 10) + "\n" +
		strconv.FormatInt(removedAt, 10))
}

// signEndorsement produces this node's endorsement of an introduced peer's cert.
func signEndorsement(signer ed25519.PrivateKey, by, introducedUUID, fingerprint, clusterID string, issuedAt int64, epochs ...uint64) Endorsement {
	sig := ed25519.Sign(signer, endorsePayload(introducedUUID, fingerprint, clusterID, issuedAt))
	e := Endorsement{
		By:          by,
		Fingerprint: fingerprint,
		ClusterID:   clusterID,
		IssuedAt:    issuedAt,
		Sig:         base64.StdEncoding.EncodeToString(sig),
	}
	if len(epochs) >= 2 && epochs[0] != 0 && epochs[1] != 0 {
		e.AdmissionEpoch = epochs[0]
		e.ByAdmissionEpoch = epochs[1]
		e.SigV2 = base64.StdEncoding.EncodeToString(ed25519.Sign(signer,
			endorsePayloadV2(introducedUUID, fingerprint, clusterID, epochs[0], epochs[1], issuedAt)))
	}
	return e
}

// verifyEndorsement reports whether e is a valid endorsement of introducedUUID
// (with the given fingerprint, in the given cluster) by the holder of pub.
// pub is the endorser's public key, which the caller looks up from the trusted
// store (the endorser must itself be a currently-trusted member).
func verifyEndorsement(pub ed25519.PublicKey, introducedUUID, fingerprint, clusterID string, e Endorsement) bool {
	if e.Fingerprint != fingerprint || e.ClusterID != clusterID {
		return false
	}
	if e.SigV2 != "" {
		if e.AdmissionEpoch == 0 || e.ByAdmissionEpoch == 0 {
			return false
		}
		sig, err := base64.StdEncoding.DecodeString(e.SigV2)
		if err != nil {
			return false
		}
		return ed25519.Verify(pub, endorsePayloadV2(introducedUUID, fingerprint, clusterID,
			e.AdmissionEpoch, e.ByAdmissionEpoch, e.IssuedAt), sig)
	}
	sig, err := base64.StdEncoding.DecodeString(e.Sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, endorsePayload(introducedUUID, fingerprint, clusterID, e.IssuedAt), sig)
}

// signTombstone produces this node's signed removal of a peer.
func signTombstone(signer ed25519.PrivateKey, by, nodeUUID, clusterID string, removedAt int64, epochs ...uint64) Tombstone {
	sig := ed25519.Sign(signer, tombstonePayload(nodeUUID, clusterID, removedAt))
	t := Tombstone{
		NodeUUID:  nodeUUID,
		ClusterID: clusterID,
		RemovedAt: removedAt,
		By:        by,
		Sig:       base64.StdEncoding.EncodeToString(sig),
	}
	if len(epochs) >= 2 && epochs[0] != 0 && epochs[1] != 0 {
		t.AdmissionEpoch = epochs[0]
		t.ByAdmissionEpoch = epochs[1]
		t.SigV2 = base64.StdEncoding.EncodeToString(ed25519.Sign(signer,
			tombstonePayloadV2(nodeUUID, clusterID, epochs[0], epochs[1], removedAt)))
	}
	return t
}

// verifyTombstone reports whether t is a valid removal signed by the holder of
// pub (the remover, who must be a currently-trusted member).
func verifyTombstone(pub ed25519.PublicKey, clusterID string, t Tombstone) bool {
	if t.ClusterID != clusterID || t.NodeUUID == "" {
		return false
	}
	if t.SigV2 != "" {
		if t.AdmissionEpoch == 0 || t.ByAdmissionEpoch == 0 {
			return false
		}
		sig, err := base64.StdEncoding.DecodeString(t.SigV2)
		if err != nil {
			return false
		}
		return ed25519.Verify(pub, tombstonePayloadV2(t.NodeUUID, clusterID,
			t.AdmissionEpoch, t.ByAdmissionEpoch, t.RemovedAt), sig)
	}
	sig, err := base64.StdEncoding.DecodeString(t.Sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, tombstonePayload(t.NodeUUID, clusterID, t.RemovedAt), sig)
}

// pubKeyFromCert extracts the Ed25519 public key from a parsed leaf certificate.
func pubKeyFromCert(cert *x509.Certificate) (ed25519.PublicKey, error) {
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cert public key is %T, want ed25519.PublicKey", cert.PublicKey)
	}
	return pub, nil
}
