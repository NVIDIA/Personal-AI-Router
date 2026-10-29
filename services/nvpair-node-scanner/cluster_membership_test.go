// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func newMembershipTestDaemon(clusterDir string) *daemon {
	mesh := clustertrust.Open(clusterDir)
	return &daemon{
		mesh:          mesh,
		modelsClients: clustertrust.NewPeerClientPool(mesh, modelsFetchTimeout),
		clusterDir:    clusterDir,
		baseDir:       filepath.Dir(clusterDir),
		reg:           newRegistry("host-uuid", "", selfAddrs("127.0.0.1")),
		dir:           newDirectory(),
		lastInfoAt:    make(map[string]time.Time),
		// codec/responder nil: emit is nil-safe; reloadIdentity skips re-advertise.
	}
}

// Remote model inventories require pinned mTLS. The owned engine-manager keeps
// its loopback HTTP path before and after joining a cluster.
func TestModelFetchTransport_FollowsMembership(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	d := newMembershipTestDaemon(clusterDir)
	d.modelsHTTP = &http.Client{}

	// Unclustered: loopback (self) still resolves a plain client...
	_, scheme, ok := d.modelsClient("127.0.0.1", "")
	require.True(t, ok, "unclustered loopback: scheme (%v, %v)", scheme, ok)
	require.Equal(t, "http", scheme, "unclustered loopback: scheme (%v, %v)", scheme, ok)
	// ...but no LAN peer is reachable at all, in either transport.
	_, _, ok = d.modelsClient("192.168.1.42", "principal-peer")
	require.False(t, ok, "an unclustered node must not fetch a peer's model inventory")
	_, _, ok = d.modelsClient("192.168.1.42", "")
	require.False(t, ok, "a peer advertising no cluster principal must not be fetched")

	// Join a cluster that pins one peer.
	clustertrusttest.Join(t, clusterDir, "cluster-models", "principal-self", "principal-peer")
	d.mesh.Refresh()

	_, scheme, ok = d.modelsClient("192.168.1.42", "principal-peer")
	require.True(t, ok, "pinned peer: scheme (%v, %v)", scheme, ok)
	require.Equal(t, "https", scheme, "pinned peer: scheme (%v, %v)", scheme, ok)
	_, _, ok = d.modelsClient("192.168.1.99", "principal-stranger")
	require.False(t, ok, "an unpinned peer must not be fetched")
	// Self keeps its loopback plaintext path once clustered too.
	_, scheme, ok = d.modelsClient("127.0.0.1", "")
	require.True(t, ok)
	assert.Equal(t, "http", scheme)
}

// A retained keypair alone must not advertise active cluster membership.
func TestClusterUUID_ReflectsLiveMembership(t *testing.T) {
	// A left/removed node: keypair present, but no active admission and no pins.
	// It must NOT advertise a cluster-uuid= (so peers see it as invitable).
	leftover := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.WriteKeypair(t, leftover, "principal-A")
	d := newMembershipTestDaemon(leftover)
	d.reloadIdentity()
	require.Equal(t, "", d.reg.record().ClusterUUID, "leftover-keypair node advertised cluster-uuid")

	// An already-clustered node on restart: keypair + live admission on disk. It
	// must advertise its cluster principal so peers gate correctly.
	clustered := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.WriteKeypair(t, clustered, "principal-B")
	clustertrusttest.WriteAdmission(t, clustered, "cluster-xyz", 1)
	d2 := newMembershipTestDaemon(clustered)
	d2.reloadIdentity()
	require.Equal(t, "principal-B", d2.reg.record().ClusterUUID, "clustered node advertised cluster-uuid")
}

// A running scanner must advertise admission changes without a process restart.
func TestClusterUUID_AppearsOnJoinWithoutRestart(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	d := newMembershipTestDaemon(clusterDir)
	d.reloadIdentity()
	require.Equal(t, "", d.reg.record().ClusterUUID, "pre-join cluster-uuid")

	// The cluster-manager mints the identity and activates the admission while
	// the scanner is already running.
	clustertrusttest.WriteKeypair(t, clusterDir, "principal-C")
	clustertrusttest.WriteAdmission(t, clusterDir, "cluster-xyz", 1)

	d.reloadIdentity()
	require.Equal(t, "principal-C", d.reg.record().ClusterUUID, "post-join cluster-uuid")
}

// Membership reconciliation updates the local registry after joining a cluster.
func TestReconcileIdentity_FollowsMembership(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	d := newMembershipTestDaemon(clusterDir)

	d.reconcileIdentity()
	require.Equal(t, "", d.reg.record().ClusterUUID, "pre-join cluster-uuid")

	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self")
	d.mesh.Refresh()
	d.reconcileIdentity()
	require.Equal(t, "principal-self", d.reg.record().ClusterUUID, "post-join cluster-uuid")
}

// An unverified announcement's membership claim is display data. Clearing it
// must make the row available for pairing again.
func TestPeerLeavingItsCluster_ClearsInviteGate(t *testing.T) {
	d := newMembershipTestDaemon(filepath.Join(t.TempDir(), "cluster"))
	const peerUUID = "peer-uuid"

	// The peer belongs to a cluster this node is not part of: not invitable.
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{
		ID:  "peer",
		TXT: []string{"v=1", "uuid=" + peerUUID, "cluster-uuid=their-cluster", "ip=192.168.1.9"},
	}})
	n, ok := d.dir.get(nodeDiscoveryRecordID(RawNode{ID: "peer", TXT: []string{"uuid=" + peerUUID, "ip=192.168.1.9"}}))
	require.True(t, ok, "peer advertising a cluster principal was not recorded")
	require.True(t, n.Clustered())

	// It leaves, so it re-advertises without a cluster principal: invitable again.
	d.onBrowse(DiscoveryEvent{Type: "updated", Node: RawNode{
		ID:  "peer",
		TXT: []string{"v=1", "uuid=" + peerUUID, "ip=192.168.1.9"},
	}})
	n, ok = d.dir.get(nodeDiscoveryRecordID(RawNode{ID: "peer", TXT: []string{"uuid=" + peerUUID, "ip=192.168.1.9"}}))
	require.True(t, ok, "peer dropped from the directory when it left its cluster")
	assert.False(t, n.Clustered())
}
