// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func TestCandidateTransportReusesPlainTransport(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	a := p.candidateTransport(candidate{})
	b := p.candidateTransport(candidate{id: "manual"})
	require.True(t, a == b, "plain candidates returned distinct Transports")
	require.NotNil(t, a, "plain Transport is nil")
}

func TestCandidateTransportReusesPeerTransport(t *testing.T) {
	const peerUUID = "principal-peer"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)

	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	p.mesh = clustertrust.Open(clusterDir)

	a := p.candidateTransport(candidate{peerUUID: peerUUID})
	b := p.candidateTransport(candidate{peerUUID: peerUUID})
	require.True(t, a == b, "same peerUUID returned distinct Transports")
	require.NotNil(t, a.TLSClientConfig, "peer Transport missing TLSClientConfig")

	other := p.candidateTransport(candidate{peerUUID: "principal-other"})
	require.True(t, other != a, "unpinned peer reused pinned peer Transport")
}

func TestDropUnpinnedPeerTransportsRemovesEntry(t *testing.T) {
	const peerUUID = "principal-peer"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)

	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	p.mesh = clustertrust.Open(clusterDir)

	tr := p.candidateTransport(candidate{peerUUID: peerUUID})
	p.transportMu.Lock()
	if _, ok := p.peerTransports[peerUUID]; !ok {
		p.transportMu.Unlock()
		require.FailNow(t, "test expectation failed", "peer Transport was not cached")
	}
	p.transportMu.Unlock()

	clustertrusttest.RemovePeerPin(t, clusterDir, peerUUID)
	p.mesh.Refresh()
	p.dropUnpinnedPeerTransports()

	p.transportMu.Lock()
	_, still := p.peerTransports[peerUUID]
	p.transportMu.Unlock()
	require.False(t, still, "peer Transport remained after pin removal")
	_ = tr
}
