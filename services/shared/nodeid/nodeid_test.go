// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveMintsAndPersists(t *testing.T) {
	base := t.TempDir()
	first := Resolve(base)
	if first == "" {
		t.Fatal("Resolve returned empty UUID")
	}
	if _, err := os.Stat(filepath.Join(base, "node-id.json")); err != nil {
		t.Fatalf("node-id.json not persisted: %v", err)
	}
	// A second resolve must return the same persisted UUID.
	if second := Resolve(base); second != first {
		t.Errorf("Resolve not stable: first %q, second %q", first, second)
	}
}

func TestResolvePrefersClusterIdentity(t *testing.T) {
	base := t.TempDir()
	clusterDir := filepath.Join(base, "cluster")
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const want = "11111111-2222-4333-8444-555555555555"
	if err := os.WriteFile(filepath.Join(clusterDir, "identity.json"),
		[]byte(`{"node_uuid":"`+want+`","created_at":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Resolve(base); got != want {
		t.Errorf("Resolve = %q, want cluster identity %q", got, want)
	}
	// It should not have written its own node-id.json when reusing the
	// cluster identity.
	if _, err := os.Stat(filepath.Join(base, "node-id.json")); !os.IsNotExist(err) {
		t.Errorf("node-id.json should not exist when cluster identity is present (err=%v)", err)
	}
}
