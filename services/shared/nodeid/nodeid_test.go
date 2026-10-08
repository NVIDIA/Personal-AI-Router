// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveMintsAndPersists(t *testing.T) {
	base := t.TempDir()
	first := Resolve(base)
	assert.NotEqual(t, "", first, "Resolve returned empty UUID")
	require.FileExists(t, filepath.Join(base, "node-id.json"), "node-id.json not persisted")
	// A second resolve must return the same persisted UUID.
	{
		second := Resolve(base)
		assert.Equal(t, first, second, "Resolve not stable: first")
	}
}

func TestResolvePrefersClusterIdentity(t *testing.T) {
	base := t.TempDir()
	clusterDir := filepath.Join(base, "cluster")
	require.NoError(t, os.MkdirAll(clusterDir, 0o700))
	const want = "11111111-2222-4333-8444-555555555555"
	require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "identity.json"),
		[]byte(`{"node_uuid":"`+want+`","created_at":1}`), 0o600))
	{
		got := Resolve(base)
		assert.Equal(t, want, got)
	}
	// It should not have written its own node-id.json when reusing the
	// cluster identity.
	{
		_, err := os.Stat(filepath.Join(base, "node-id.json"))
		assert.ErrorIs(t, err, os.ErrNotExist, "node-id.json should not exist when cluster identity is present")
	}
}
