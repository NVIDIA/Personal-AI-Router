// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
)

func TestLegacyPairingInfoMapsToFirstAdmission(t *testing.T) {
	peer := newTestManagerPort(t, 15123)
	raw, err := json.Marshal(PairingInfo{
		V:         1,
		NodeUUID:  peer.identity.NodeUUID,
		NodeID:    peer.identity.NodeID,
		Name:      peer.identity.Name,
		ClusterID: "cluster-1",
		Cert:      string(peer.identity.CertPEM),
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _, err := parsePairingInfo(raw)
	if err != nil {
		t.Fatalf("parse legacy pairing info: %v", err)
	}
	if info.AdmissionEpoch != legacyAdmissionEpoch {
		t.Fatalf("legacy admission epoch = %d, want %d", info.AdmissionEpoch, legacyAdmissionEpoch)
	}

	info.AdmissionEpoch = 0
	info.V = pairingInfoVersion
	raw, _ = json.Marshal(info)
	if _, _, err := parsePairingInfo(raw); err == nil {
		t.Fatal("v2 pairing info without admission epoch was accepted")
	}
}

func TestRestartMigratesLegacyPinnedMemberAdmission(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15124)
	activateTestCluster(t, m, "cluster-1")
	peer := newTestManagerPort(t, 15125)
	if err := m.trust.Pin(&TrustedPin{
		NodeUUID:        peer.identity.NodeUUID,
		NodeID:          peer.identity.NodeID,
		Name:            peer.identity.Name,
		CertPem:         string(peer.identity.CertPEM),
		CertFingerprint: peer.identity.CertFingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	m.upsertMember(&ClusterNode{
		ID: peer.identity.NodeID, NodeUUID: peer.identity.NodeUUID,
		State: stateMember,
	})
	if err := m.persistMembersErr(); err != nil {
		t.Fatal(err)
	}

	restarted := testManagerAt(t, dir, 15124)
	pin, ok := restarted.trust.Get(peer.identity.NodeUUID)
	if !ok || pin.ClusterID != "cluster-1" || pin.AdmissionEpoch != legacyAdmissionEpoch {
		t.Fatalf("migrated pin = %+v", pin)
	}
	hasLocalV2 := false
	_, selfEpoch := restarted.currentAdmission()
	for _, end := range pin.Endorsements {
		if end.By == restarted.identity.NodeUUID && end.ByAdmissionEpoch == selfEpoch &&
			end.AdmissionEpoch == legacyAdmissionEpoch && end.SigV2 != "" {
			hasLocalV2 = true
		}
	}
	if !hasLocalV2 {
		t.Fatal("migrated pin has no local admission-bound endorsement")
	}
	member, ok := restarted.memberByNodeID(peer.identity.NodeUUID)
	if !ok || member.ClusterID != "cluster-1" || member.AdmissionEpoch != legacyAdmissionEpoch {
		t.Fatalf("migrated member = %+v", member)
	}
	proof, err := restarted.newRemovalProof(peer.identity.NodeUUID, member.AdmissionEpoch)
	if err != nil {
		t.Fatalf("migrated offline member is not removable: %v", err)
	}
	if _, err := restarted.putRemovalProof(proof); err != nil {
		t.Fatalf("persist removal for migrated offline member: %v", err)
	}
}
