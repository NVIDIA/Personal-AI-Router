// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"nvpair-shared/nodeid"
)

// TestFirstMintAdoptsExistingNodeUUID verifies that on an empty cluster base,
// a worker that resolves its identity first (via nodeid, minting node-id.json =
// A) and cluster-manager, which mints identity.json later, must land on the SAME
// UUID — cluster-manager adopts A instead of minting an independent B. Otherwise
// b.nodeID / node-info / errors stay on A while the scanner/cluster move to B.
func TestFirstMintAdoptsExistingNodeUUID(t *testing.T) {
	base := t.TempDir()

	// A worker resolves the per-host UUID first (mints <base>/node-id.json).
	a := nodeid.Resolve(base)
	require.NotEqual(t, "", a, "nodeid.Resolve returned empty")

	// cluster-manager then mints identity.json on first launch.
	clusterDir := filepath.Join(base, "cluster")
	require.NoError(t, os.MkdirAll(clusterDir, 0o700), "mkdir cluster dir")
	id, err := loadOrMintIdentity(clusterDir)
	require.NoError(t, err, "loadOrMintIdentity")

	// It must have adopted the already-minted UUID, not minted a fresh one.
	require.True(t, id.NodeUUID == a, "cluster-manager minted a divergent UUID: identity (%v)", a)

	// And every subsequent resolution (now preferring identity.json) agrees:
	// this is the empty-config equality invariant the whole fleet relies on.
	{
		got := nodeid.Resolve(base)
		require.True(t, got == a, "post-mint nodeid.Resolve (%v, %v)", got, a)
	}
}

// TestFirstMintWhenClusterManagerIsFirst verifies the other ordering: when
// cluster-manager mints before any node-id.json exists, later nodeid.Resolve
// calls converge on the identity.json UUID it wrote.
func TestFirstMintWhenClusterManagerIsFirst(t *testing.T) {
	base := t.TempDir()
	clusterDir := filepath.Join(base, "cluster")
	require.NoError(t, os.MkdirAll(clusterDir, 0o700), "mkdir cluster dir")
	id, err := loadOrMintIdentity(clusterDir)
	require.NoError(t, err, "loadOrMintIdentity")
	require.NotEqual(t, "", id.NodeUUID, "minted empty UUID")
	{
		got := nodeid.Resolve(base)
		require.True(t, got == id.NodeUUID, "nodeid.Resolve (%v)", got)
	}
}
