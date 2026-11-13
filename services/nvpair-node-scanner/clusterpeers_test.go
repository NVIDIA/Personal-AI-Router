// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"sync"
	"testing"
)

func rosterPeer(f *discoveryPeerFixture) noderec.ClusterPeer {
	return noderec.ClusterPeer{HostUUID: f.info.HostUUID, Name: "roster hint", Address: "127.0.0.1", NodeInfoPort: f.info.Peer.Services[noderec.ServiceNodeInfo]}
}
func seedRosterPeer(t *testing.T, f *discoveryPeerFixture) {
	t.Helper()
	require.NoError(t, f.daemon.setClusterPeers([]noderec.ClusterPeer{rosterPeer(f)}))
	f.daemon.refreshClusterPeers()
}
func TestClusterDiscovery_AuthenticatesARosterPeerWithoutMulticast(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	seedRosterPeer(t, f)
	_, present := f.daemon.dir.get("peer-uuid")
	require.False(t, present, "a roster hint alone must not create a trusted host")
	f.daemon.refreshCandidatesOnce(t.Context())
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
	assert.Equal(t, "owned peer", node.Name, "use the authenticated name, not the hint")
	assert.Equal(t, fakePeerPort, node.Services[noderec.ServiceOllama].Port)
}
func TestClusterDiscovery_RejectsInvalidPeerEvidence(t *testing.T) {
	for _, name := range []string{"different host", "different principal", "invalid service port", "unpinned peer"} {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			peer := rosterPeer(f)
			switch name {
			case "different host":
				f.info.HostUUID = "stranger"
			case "different principal":
				other := "stranger"
				f.info.ClusterUUID = &other
			case "invalid service port":
				f.info.Peer.Services[noderec.ServiceOllama] = 70000
			case "unpinned peer":
				peer.HostUUID = "stranger"
			}
			require.NoError(t, f.daemon.setClusterPeers([]noderec.ClusterPeer{peer}))
			f.daemon.refreshClusterPeers()
			f.daemon.refreshCandidatesOnce(t.Context())
			_, present := f.daemon.dir.get(peer.HostUUID)
			assert.False(t, present)
		})
	}
}
func TestClusterDiscovery_RefreshesAuthenticatedServicePorts(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	seedRosterPeer(t, f)
	f.daemon.refreshCandidatesOnce(t.Context())
	delete(f.info.Peer.Services, noderec.ServiceOllama)
	f.info.Peer.Services[noderec.ServiceVLLM] = 18432
	f.daemon.refreshTelemetryOnce(t.Context())
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.NotContains(t, node.Services, noderec.ServiceOllama)
	assert.Equal(t, 18432, node.Services[noderec.ServiceVLLM].Port)
}
func TestClusterDiscovery_RemovingAHintPreservesAnAuthenticatedPeer(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	seedRosterPeer(t, f)
	f.daemon.refreshCandidatesOnce(t.Context())
	require.NoError(t, f.daemon.setClusterPeers(nil))
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
	_, hintPresent := f.daemon.candidates[nodeDiscoveryRecordID(clusterPeerNode(rosterPeer(f)))]
	assert.False(t, hintPresent)
}
func TestClusterDiscovery_DiscardsInflightReadsAfterHintOrPinRemoval(t *testing.T) {
	for _, name := range []string{"hint removed", "pin removed"} {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			f.respond = func(http.ResponseWriter, *http.Request) bool { close(entered); <-release; return false }
			seedRosterPeer(t, f)
			done := make(chan struct{})
			go func() { f.daemon.refreshCandidatesOnce(context.Background()); close(done) }()
			<-entered
			if name == "hint removed" {
				require.NoError(t, f.daemon.setClusterPeers(nil))
			} else {
				clustertrusttest.RemovePeerPin(t, f.selfDir, "peer-uuid")
				f.daemon.mesh.Refresh()
			}
			unblock()
			<-done
			_, present := f.daemon.dir.get("peer-uuid")
			assert.False(t, present)
		})
	}
}
