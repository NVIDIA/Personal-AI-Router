// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

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
	if a == "" {
		t.Fatal("nodeid.Resolve returned empty")
	}

	// cluster-manager then mints identity.json on first launch.
	clusterDir := filepath.Join(base, "cluster")
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatalf("mkdir cluster dir: %v", err)
	}
	id, err := loadOrMintIdentity(clusterDir)
	if err != nil {
		t.Fatalf("loadOrMintIdentity: %v", err)
	}

	// It must have adopted the already-minted UUID, not minted a fresh one.
	if id.NodeUUID != a {
		t.Fatalf("cluster-manager minted a divergent UUID: identity=%q, node-id.json=%q", id.NodeUUID, a)
	}

	// And every subsequent resolution (now preferring identity.json) agrees:
	// this is the empty-config equality invariant the whole fleet relies on.
	if got := nodeid.Resolve(base); got != a {
		t.Fatalf("post-mint nodeid.Resolve = %q, want %q (no A/B divergence)", got, a)
	}
}

// TestFirstMintWhenClusterManagerIsFirst verifies the other ordering: when
// cluster-manager mints before any node-id.json exists, later nodeid.Resolve
// calls converge on the identity.json UUID it wrote.
func TestFirstMintWhenClusterManagerIsFirst(t *testing.T) {
	base := t.TempDir()
	clusterDir := filepath.Join(base, "cluster")
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatalf("mkdir cluster dir: %v", err)
	}
	id, err := loadOrMintIdentity(clusterDir)
	if err != nil {
		t.Fatalf("loadOrMintIdentity: %v", err)
	}
	if id.NodeUUID == "" {
		t.Fatal("minted empty UUID")
	}
	if got := nodeid.Resolve(base); got != id.NodeUUID {
		t.Fatalf("nodeid.Resolve = %q, want the minted identity %q", got, id.NodeUUID)
	}
}
