// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	{
		got, ok := trust.DER("uuid-peer")
		require.True(t, ok, "first load: ok")
		assert.Equal(t, string(der), string(got), "first load: ok")
	}

	trusted := filepath.Join(clusterDir, "trusted")
	require.NoError(t, os.Chmod(trusted, 0o000), "chmod trusted")
	t.Cleanup(func() { _ = os.Chmod(trusted, 0o700) })

	trust.Reload()
	{
		_, ok := trust.DER("uuid-peer")
		require.True(t, ok, "an unreadable trusted/ dir dropped a pin we already held")
	}
	assert.Equal(t, 1, trust.Count(), "pin count")

	// Readable again with the pin genuinely gone: that IS a revocation and must
	// take effect, which is what keeps a removal propagating.
	require.NoError(t, os.Chmod(trusted, 0o700), "restore chmod")
	require.NoError(t, os.Remove(filepath.Join(trusted, "uuid-peer.json")), "remove pin")
	trust.Reload()
	{
		_, ok := trust.DER("uuid-peer")
		require.False(t, ok, "a removed pin survived a successful reload")
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
	assert.Equal(t, 1, trust.Count(), "first load count")

	require.NoError(t, os.RemoveAll(filepath.Join(clusterDir, "trusted")), "remove trusted dir")
	trust.Reload()
	assert.Equal(t, 0, trust.Count(), "count after teardown")
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
	assert.Equal(t, 1, trust.Count(), "first load count")

	pin := filepath.Join(clusterDir, "trusted", "uuid-peer.json")
	require.NoError(t, os.WriteFile(pin, []byte("{not json"), 0o600), "corrupt pin")
	trust.Reload()
	{
		_, ok := trust.DER("uuid-peer")
		require.False(t, ok, "a pin that no longer parses was carried forward")
	}
}
