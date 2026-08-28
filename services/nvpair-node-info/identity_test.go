// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"nvpair-shared/nodeid"
)

// TestResolveHostUUIDFlagWins: an explicit --node-id (the
// broker's already-resolved b.nodeID) is reported verbatim, so node-info stays
// on the fleet's identity even when a fallback resolve of a custom data root
// would return something else.
func TestResolveHostUUIDFlagWins(t *testing.T) {
	base := t.TempDir()
	seeded := nodeid.Resolve(base) // a different identity persisted under base
	if seeded == "broker-uuid" {
		t.Skip("astronomically unlikely uuid collision with the flag literal")
	}
	if got := resolveHostUUID("broker-uuid", filepath.Join(base, "cluster")); got != "broker-uuid" {
		t.Fatalf("flag should win over any resolved identity: got %q, want broker-uuid", got)
	}
}

// TestResolveHostUUIDFallsBackToClusterRoot: standalone (no --node-id) node-info
// resolves the identity under the --cluster-dir parent — the same custom root
// the broker's resolveLocalNodeID uses — so both agree. This is the custom-root
// equality the broker relies on when it passes b.nodeID.
func TestResolveHostUUIDFallsBackToClusterRoot(t *testing.T) {
	base := t.TempDir()
	want := nodeid.Resolve(base)
	if got := resolveHostUUID("", filepath.Join(base, "cluster")); got != want {
		t.Fatalf("fallback resolve = %q, want %q (custom-root equality)", got, want)
	}
}
