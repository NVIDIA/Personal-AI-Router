// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func TestCandidateTransportReusesPlainTransport(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	a := p.candidateTransport(candidate{})
	b := p.candidateTransport(candidate{id: "manual"})
	if a != b {
		t.Fatalf("plain candidates returned distinct Transports")
	}
	if a == nil {
		t.Fatal("plain Transport is nil")
	}
}

func TestCandidateTransportReusesPeerTransport(t *testing.T) {
	const peerUUID = "principal-peer"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)

	p := testProxy(NewDiscovery(), 11435)
	p.mesh = clustertrust.Open(clusterDir)

	a := p.candidateTransport(candidate{peerUUID: peerUUID})
	b := p.candidateTransport(candidate{peerUUID: peerUUID})
	if a != b {
		t.Fatalf("same peerUUID returned distinct Transports")
	}
	if a.TLSClientConfig == nil {
		t.Fatal("peer Transport missing TLSClientConfig")
	}

	other := p.candidateTransport(candidate{peerUUID: "principal-other"})
	if other == a {
		t.Fatal("unpinned peer reused pinned peer Transport")
	}
}

func TestDropUnpinnedPeerTransportsRemovesEntry(t *testing.T) {
	const peerUUID = "principal-peer"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)

	p := testProxy(NewDiscovery(), 11435)
	p.mesh = clustertrust.Open(clusterDir)

	tr := p.candidateTransport(candidate{peerUUID: peerUUID})
	p.transportMu.Lock()
	if _, ok := p.peerTransports[peerUUID]; !ok {
		p.transportMu.Unlock()
		t.Fatal("peer Transport was not cached")
	}
	p.transportMu.Unlock()

	clustertrusttest.RemovePeerPin(t, clusterDir, peerUUID)
	p.mesh.Refresh()
	p.dropUnpinnedPeerTransports()

	p.transportMu.Lock()
	_, still := p.peerTransports[peerUUID]
	p.transportMu.Unlock()
	if still {
		t.Fatal("peer Transport remained after pin removal")
	}
	_ = tr
}
