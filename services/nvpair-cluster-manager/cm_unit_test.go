// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIdentityMintAndReload(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrMintIdentity(dir)
	require.NoError(t, err, "mint")
	require.NotEqual(t, "", first.NodeUUID, "expected a non-empty UUID and fingerprint")
	require.NotEqual(t, "", first.CertFingerprint, "expected a non-empty UUID and fingerprint")

	second, err := loadOrMintIdentity(dir)
	require.NoError(t, err, "reload")
	require.Equal(t, first.NodeUUID, second.NodeUUID, "UUID changed across reload")
	require.Equal(t, first.CertFingerprint, second.CertFingerprint, "fingerprint changed across reload")
}

func TestIdentityLostKeyFailsLoud(t *testing.T) {
	dir := t.TempDir()
	_, err := loadOrMintIdentity(dir)
	require.NoError(t, err, "mint")
	require.NoError(t, os.Remove(filepath.Join(dir, "node.key")), "remove key")
	_, err = loadOrMintIdentity(dir)
	require.Error(t, err, "expected a loud failure when identity.json exists but the key is gone")
}

// makePin builds a valid TrustedPin for a fresh node identity.
func makePin(t *testing.T) *TrustedPin {
	t.Helper()
	uuid, err := newUUIDv4()
	require.NoError(t, err, "uuid")
	certPEM, _, err := generateLeaf(uuid, "peer-host")
	require.NoError(t, err, "leaf")
	fp, _ := certFingerprintFromPEM(certPEM)
	return &TrustedPin{
		NodeUUID:        uuid,
		NodeID:          "peer-host",
		Name:            "peer-host",
		ClusterID:       "cluster-1",
		CertPem:         string(certPEM),
		CertFingerprint: fp,
		PinnedAt:        time.Now().UnixMilli(),
	}
}

func TestTrustStorePinRemoveReload(t *testing.T) {
	dir := t.TempDir()
	ts, err := newTrustStore(dir)
	require.NoError(t, err, "open")
	pin := makePin(t)
	require.NoError(t, ts.Pin(pin), "pin")
	der, ok := ts.DER(pin.NodeUUID)
	require.True(t, ok, "expected the pinned DER to match itself")
	require.True(t, ts.MatchDER(pin.NodeUUID, der), "expected the pinned DER to match itself")

	// Reopen: the pin must reload from disk.
	reopened, err := newTrustStore(dir)
	require.NoError(t, err, "reopen")
	_, ok = reopened.Get(pin.NodeUUID)
	require.True(t, ok, "pin did not survive reload")

	require.NoError(t, reopened.Remove(pin.NodeUUID), "remove")
	_, ok = reopened.Get(pin.NodeUUID)
	require.False(t, ok, "pin still present after removal")
}

func TestTrustStoreRePinGuard(t *testing.T) {
	dir := t.TempDir()
	ts, _ := newTrustStore(dir)
	pin := makePin(t)
	require.NoError(t, ts.Pin(pin), "pin")
	// Identical re-pin is an idempotent no-op.
	require.NoError(t, ts.Pin(pin), "identical re-pin should be a no-op")
	// A different cert for the same UUID is rejected.
	other := makePin(t)
	other.NodeUUID = pin.NodeUUID // same uuid, different cert
	require.Error(t, ts.Pin(other), "expected re-pinning a different cert for the same UUID to fail")
}

func TestTrustStoreAntiTamper(t *testing.T) {
	dir := t.TempDir()
	// Pre-create a tampered file: filename UUID != the inner nodeUuid / cert.
	trustedDir := filepath.Join(dir, "trusted")
	require.NoError(t, os.MkdirAll(trustedDir, 0o700), "mkdir")
	pin := makePin(t)
	data, _ := json.MarshalIndent(pin, "", "  ")
	wrongName := filepath.Join(trustedDir, "00000000-0000-4000-8000-000000000000.json")
	require.NoError(t, os.WriteFile(wrongName, data, 0o600), "write tampered")

	ts, err := newTrustStore(dir)
	require.NoError(t, err, "open")
	_, ok := ts.Get(pin.NodeUUID)
	require.False(t, ok, "tampered (renamed) pin should have been skipped on load")
	require.Empty(t, ts.List(), "expected no valid pins")
}

func TestPINNoobRoundTrip(t *testing.T) {
	cases := []string{"000000", "000123", "402199", "999999"}
	for _, pin := range cases {
		noob := noobFromPIN(pin)
		require.Len(t, noob, 16, "noob length")
		got := new(big.Int).SetBytes(noob).String()
		want := new(big.Int)
		want.SetString(pin, 10)
		require.Equal(t, want.String(), got, "noob decodes incorrectly")
	}

	gp, noob, err := generatePIN()
	require.NoError(t, err, "generatePIN")
	require.Regexp(t, pinPattern, gp, "generated PIN")
	require.Equal(t, string(noobFromPIN(gp)), string(noob), "generatePIN's noob does not match noobFromPIN of its PIN")
}
