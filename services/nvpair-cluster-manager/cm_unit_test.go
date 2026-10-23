// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIdentity_MintsAndReloadsStableCredentials(t *testing.T) {
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

func TestIdentity_RejectsReloadWhenPrivateKeyIsMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := loadOrMintIdentity(dir)
	require.NoError(t, err, "mint")
	require.NoError(t, os.Remove(filepath.Join(dir, "node.key")), "remove key")
	_, err = loadOrMintIdentity(dir)
	require.Error(t, err, "expected a loud failure when identity.json exists but the key is gone")
}

func TestPIN_RoundTripsThroughNoobEncoding(t *testing.T) {
	test := func(name, pin string) {
		t.Run(name, func(t *testing.T) {
			noob := noobFromPIN(pin)
			require.Len(t, noob, 16, "noob length")
			want := new(big.Int)
			want.SetString(pin, 10)
			require.Equal(t, want.String(), new(big.Int).SetBytes(noob).String(), "noob decodes incorrectly")
		})
	}
	test("all zeroes", "000000")
	test("leading zeroes", "000123")
	test("ordinary PIN", "402199")
	test("maximum PIN", "999999")

	gp, noob, err := generatePIN()
	require.NoError(t, err, "generatePIN")
	require.Regexp(t, pinPattern, gp, "generated PIN")
	require.Equal(t, string(noobFromPIN(gp)), string(noob), "generatePIN's noob does not match noobFromPIN of its PIN")
}
