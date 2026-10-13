// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	info, _, err := parsePairingInfo(raw)
	require.NoError(t, err, "parse legacy pairing info")
	require.Equal(t, legacyAdmissionEpoch, info.AdmissionEpoch, "legacy admission epoch")

	info.AdmissionEpoch = 0
	info.V = pairingInfoVersion
	raw, _ = json.Marshal(info)
	_, _, err = parsePairingInfo(raw)
	require.Error(t, err, "v2 pairing info without admission epoch was accepted")
}

func TestRestartMigratesLegacyPinnedMemberAdmission(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15124)
	activateTestCluster(t, m, "cluster-1")
	peer := newTestManagerPort(t, 15125)
	require.NoError(t, m.trust.Pin(&TrustedPin{
		NodeUUID:        peer.identity.NodeUUID,
		NodeID:          peer.identity.NodeID,
		Name:            peer.identity.Name,
		CertPem:         string(peer.identity.CertPEM),
		CertFingerprint: peer.identity.CertFingerprint,
	}))
	m.upsertMember(&ClusterNode{
		ID: peer.identity.NodeID, NodeUUID: peer.identity.NodeUUID,
		State: stateMember,
	})
	require.NoError(t, m.persistMembersErr())

	restarted := testManagerAt(t, dir, 15124)
	pin, ok := restarted.trust.Get(peer.identity.NodeUUID)
	require.True(t, ok, "migrated pin (%v)", pin)
	require.Equal(t, "cluster-1", pin.ClusterID, "migrated pin (%v)", pin)
	require.Equal(t, legacyAdmissionEpoch, pin.AdmissionEpoch, "migrated pin (%v)", pin)
	hasLocalV2 := false
	_, selfEpoch := restarted.currentAdmission()
	for _, end := range pin.Endorsements {
		if end.By == restarted.identity.NodeUUID && end.ByAdmissionEpoch == selfEpoch &&
			end.AdmissionEpoch == legacyAdmissionEpoch && end.SigV2 != "" {
			hasLocalV2 = true
		}
	}
	require.True(t, hasLocalV2, "migrated pin has no local admission-bound endorsement")
	member, ok := restarted.memberByNodeID(peer.identity.NodeUUID)
	require.True(t, ok, "migrated member (%v)", member)
	require.Equal(t, "cluster-1", member.ClusterID, "migrated member (%v)", member)
	require.Equal(t, legacyAdmissionEpoch, member.AdmissionEpoch, "migrated member (%v)", member)
	proof, err := restarted.newRemovalProof(peer.identity.NodeUUID, member.AdmissionEpoch)
	require.NoError(t, err, "migrated offline member is not removable")
	_, err = restarted.putRemovalProof(proof)
	require.NoError(t, err, "persist removal for migrated offline member")
}
