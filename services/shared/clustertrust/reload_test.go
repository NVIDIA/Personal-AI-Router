// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestReloadKeepsPinsWhenDirUnreadable is the anti-flap guard. Consumers poll
// this set every couple of seconds and act on every answer — the proxies decide
// routing from it, the scanner annotates its directory from it — so treating one
// unreadable read as "this node trusts nobody" would drop every peer out of the
// cluster for a tick and restore them on the next, emitting the churn in
// between. A dir we cannot read is missing information, not a revocation.
func TestReloadKeepsPinsWhenDirUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable dir is not portable to Windows ACLs")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	clusterDir := t.TempDir()
	certPEM, _, der := genLeaf(t, "uuid-peer")
	writePin(t, clusterDir, "uuid-peer", string(certPEM))

	trust := newTrust(clusterDir)
	trust.Reload()
	if got, ok := trust.DER("uuid-peer"); !ok || string(got) != string(der) {
		t.Fatalf("first load: ok=%v, want the pinned DER", ok)
	}

	trusted := filepath.Join(clusterDir, "trusted")
	if err := os.Chmod(trusted, 0o000); err != nil {
		t.Fatalf("chmod trusted: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(trusted, 0o700) })

	trust.Reload()
	if _, ok := trust.DER("uuid-peer"); !ok {
		t.Fatal("an unreadable trusted/ dir dropped a pin we already held")
	}
	if trust.Count() != 1 {
		t.Fatalf("pin count = %d, want the previous set retained", trust.Count())
	}

	// Readable again with the pin genuinely gone: that IS a revocation and must
	// take effect, which is what keeps a removal propagating.
	if err := os.Chmod(trusted, 0o700); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	if err := os.Remove(filepath.Join(trusted, "uuid-peer.json")); err != nil {
		t.Fatalf("remove pin: %v", err)
	}
	trust.Reload()
	if _, ok := trust.DER("uuid-peer"); ok {
		t.Fatal("a removed pin survived a successful reload")
	}
}

// TestReloadEmptiesWhenDirAbsent keeps the other half honest: an absent
// trusted/ dir is the durable statement that this node trusts no peer (it is
// unclustered, or its cluster was torn down), so it must publish the empty set
// rather than preserving whatever it last held.
func TestReloadEmptiesWhenDirAbsent(t *testing.T) {
	clusterDir := t.TempDir()
	certPEM, _, _ := genLeaf(t, "uuid-peer")
	writePin(t, clusterDir, "uuid-peer", string(certPEM))

	trust := newTrust(clusterDir)
	trust.Reload()
	if trust.Count() != 1 {
		t.Fatalf("first load count = %d, want 1", trust.Count())
	}

	if err := os.RemoveAll(filepath.Join(clusterDir, "trusted")); err != nil {
		t.Fatalf("remove trusted dir: %v", err)
	}
	trust.Reload()
	if trust.Count() != 0 {
		t.Fatalf("count after teardown = %d, want 0", trust.Count())
	}
}

// TestReloadDropsUnparseablePin separates the transient case from the content
// case: a pin that reads but does not validate is dropped rather than carried,
// because honoring a certificate whose provenance no longer checks out is the
// one failure this store exists to prevent.
func TestReloadDropsUnparseablePin(t *testing.T) {
	clusterDir := t.TempDir()
	certPEM, _, _ := genLeaf(t, "uuid-peer")
	writePin(t, clusterDir, "uuid-peer", string(certPEM))

	trust := newTrust(clusterDir)
	trust.Reload()
	if trust.Count() != 1 {
		t.Fatalf("first load count = %d, want 1", trust.Count())
	}

	pin := filepath.Join(clusterDir, "trusted", "uuid-peer.json")
	if err := os.WriteFile(pin, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt pin: %v", err)
	}
	trust.Reload()
	if _, ok := trust.DER("uuid-peer"); ok {
		t.Fatal("a pin that no longer parses was carried forward")
	}
}
