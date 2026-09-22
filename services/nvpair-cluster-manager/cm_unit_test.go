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
)

func TestIdentityMintAndReload(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrMintIdentity(dir)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if first.NodeUUID == "" || first.CertFingerprint == "" {
		t.Fatal("expected a non-empty UUID and fingerprint")
	}

	second, err := loadOrMintIdentity(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.NodeUUID != first.NodeUUID {
		t.Fatalf("UUID changed across reload: %q -> %q", first.NodeUUID, second.NodeUUID)
	}
	if second.CertFingerprint != first.CertFingerprint {
		t.Fatalf("fingerprint changed across reload: %q -> %q", first.CertFingerprint, second.CertFingerprint)
	}
}

func TestIdentityLostKeyFailsLoud(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrMintIdentity(dir); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "node.key")); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	if _, err := loadOrMintIdentity(dir); err == nil {
		t.Fatal("expected a loud failure when identity.json exists but the key is gone")
	}
}

// makePin builds a valid TrustedPin for a fresh node identity.
func makePin(t *testing.T) *TrustedPin {
	t.Helper()
	uuid, err := newUUIDv4()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	certPEM, _, err := generateLeaf(uuid, "peer-host")
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
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
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pin := makePin(t)
	if err := ts.Pin(pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	der, ok := ts.DER(pin.NodeUUID)
	if !ok || !ts.MatchDER(pin.NodeUUID, der) {
		t.Fatal("expected the pinned DER to match itself")
	}

	// Reopen: the pin must reload from disk.
	reopened, err := newTrustStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.Get(pin.NodeUUID); !ok {
		t.Fatal("pin did not survive reload")
	}

	if err := reopened.Remove(pin.NodeUUID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := reopened.Get(pin.NodeUUID); ok {
		t.Fatal("pin still present after removal")
	}
}

func TestTrustStoreRePinGuard(t *testing.T) {
	dir := t.TempDir()
	ts, _ := newTrustStore(dir)
	pin := makePin(t)
	if err := ts.Pin(pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// Identical re-pin is an idempotent no-op.
	if err := ts.Pin(pin); err != nil {
		t.Fatalf("identical re-pin should be a no-op: %v", err)
	}
	// A different cert for the same UUID is rejected.
	other := makePin(t)
	other.NodeUUID = pin.NodeUUID // same uuid, different cert
	if err := ts.Pin(other); err == nil {
		t.Fatal("expected re-pinning a different cert for the same UUID to fail")
	}
}

func TestTrustStoreAntiTamper(t *testing.T) {
	dir := t.TempDir()
	// Pre-create a tampered file: filename UUID != the inner nodeUuid / cert.
	trustedDir := filepath.Join(dir, "trusted")
	if err := os.MkdirAll(trustedDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pin := makePin(t)
	data, _ := json.MarshalIndent(pin, "", "  ")
	wrongName := filepath.Join(trustedDir, "00000000-0000-4000-8000-000000000000.json")
	if err := os.WriteFile(wrongName, data, 0o600); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	ts, err := newTrustStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, ok := ts.Get(pin.NodeUUID); ok {
		t.Fatal("tampered (renamed) pin should have been skipped on load")
	}
	if len(ts.List()) != 0 {
		t.Fatalf("expected no valid pins, got %d", len(ts.List()))
	}
}

func TestPINNoobRoundTrip(t *testing.T) {
	cases := []string{"000000", "000123", "402199", "999999"}
	for _, pin := range cases {
		noob := noobFromPIN(pin)
		if len(noob) != 16 {
			t.Fatalf("noob length %d, want 16", len(noob))
		}
		got := new(big.Int).SetBytes(noob).String()
		want := new(big.Int)
		want.SetString(pin, 10)
		if got != want.String() {
			t.Fatalf("noob decodes to %s, want %s", got, want.String())
		}
	}
}

func TestSaltedPINNoob(t *testing.T) {
	salt, err := newPinSalt()
	if err != nil {
		t.Fatalf("newPinSalt: %v", err)
	}
	if len(salt) != 16 {
		t.Fatalf("salt length %d, want 16", len(salt))
	}

	pin, noob, err := mintPIN(salt)
	if err != nil {
		t.Fatalf("mintPIN: %v", err)
	}
	if !pinPattern.MatchString(pin) {
		t.Fatalf("generated PIN %q is not six digits", pin)
	}

	// Joiner reconstruction must match the inviter's Noob, and the same PIN
	// under different salts must produce unrelated Noobs.
	joinerNoob := deriveNoobFromPINAndSalt(pin, salt)
	if string(joinerNoob) != string(noob) {
		t.Fatal("joiner-derived noob does not match inviter noob")
	}
	otherNoob := pinNoob(pin, append([]byte("x"), salt...))
	if string(otherNoob) == string(noob) {
		t.Fatal("different salt produced identical noob")
	}

	// Legacy escape hatch: empty salt must equal the unsalted derivation.
	if string(pinNoob(pin, nil)) != string(noobFromPIN(pin)) {
		t.Fatal("empty salt must fall back to legacy noob derivation")
	}
}
