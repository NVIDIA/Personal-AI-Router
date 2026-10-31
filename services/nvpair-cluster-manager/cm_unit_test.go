// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
