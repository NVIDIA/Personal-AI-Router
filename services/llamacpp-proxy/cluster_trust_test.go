// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// TestResolveCandidatesFollowsLivePinSet reproduces the join ordering that made
// a freshly-joined node route to nobody. The peer is discovered BEFORE this node
// has any cluster identity, and its discovery record never changes again — which
// is the normal steady state, because the relay only re-sends a node when its
// mDNS record actually moves. Routing must still pick the peer up the moment the
// pin lands, and drop it again the moment the pin is removed, because it reads
// the live pin set rather than a trust flag cached at discovery time.
func TestResolveCandidatesFollowsLivePinSet(t *testing.T) {
	const peerUUID = "principal-peer"
	clusterDir := filepath.Join(t.TempDir(), "cluster")

	disc := NewDiscovery()
	disc.SetSubscribed([]Node{{
		ID: "peer-a", Host: "peer-a", Port: 8081,
		Addresses:   []string{"192.0.2.10"},
		IP:          "192.0.2.10",
		ClusterUUID: peerUUID,
	}})
	p := testProxy(disc, 8080)
	p.mesh = clustertrust.Open(clusterDir)

	// Pre-join: no identity, no pins, so the peer is not a routable target.
	if got := p.resolveCandidates(""); len(got) != 0 {
		t.Fatalf("pre-join candidates = %+v, want none", got)
	}

	// The cluster-manager lands the join on disk while the proxy is running. No
	// new discovery snapshot arrives — the peer's mDNS record has not changed.
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)

	cands := p.resolveCandidates("")
	if len(cands) != 1 {
		t.Fatalf("post-join candidates = %+v, want the peer", cands)
	}
	if cands[0].id != "peer-a" || cands[0].peerUUID != peerUUID || cands[0].url.Scheme != "https" {
		t.Fatalf("post-join candidate = %+v, want peer-a over https pinned to %s", cands[0], peerUUID)
	}

	// Removing the peer from the cluster retires it as a target just as promptly,
	// again with no discovery event involved.
	clustertrusttest.RemovePeerPin(t, clusterDir, peerUUID)
	if got := p.resolveCandidates(""); len(got) != 0 {
		t.Fatalf("post-removal candidates = %+v, want none", got)
	}
}

// TestResolveCandidatesRejectsUnpinnedClusteredPeer keeps the isolation property
// honest now that trust is read locally: a peer that advertises a cluster
// principal we hold no pin for is not routable, even though this node is itself
// a healthy cluster member. Two clusters on one LAN must not route to each other.
func TestResolveCandidatesRejectsUnpinnedClusteredPeer(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "cluster-ours", "principal-self", "principal-ourpeer")

	disc := NewDiscovery()
	disc.SetSubscribed([]Node{{
		ID: "stranger", Host: "stranger", Port: 8081,
		Addresses:   []string{"192.0.2.30"},
		IP:          "192.0.2.30",
		ClusterUUID: "principal-stranger",
	}})
	p := testProxy(disc, 8080)
	p.mesh = clustertrust.Open(clusterDir)

	if got := p.resolveCandidates(""); len(got) != 0 {
		t.Fatalf("candidates = %+v, want none for a peer in another cluster", got)
	}
}
