// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
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

	gp, noob, err := generatePIN()
	if err != nil {
		t.Fatalf("generatePIN: %v", err)
	}
	if !pinPattern.MatchString(gp) {
		t.Fatalf("generated PIN %q is not six digits", gp)
	}
	if string(noob) != string(noobFromPIN(gp)) {
		t.Fatal("generatePIN's noob does not match noobFromPIN of its PIN")
	}
}
