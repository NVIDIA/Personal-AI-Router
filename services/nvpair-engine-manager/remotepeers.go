// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// remotepeers.go maintains this node's view of cluster peers that expose the ec
// remote-control surface. Like nvpair-errors / nvpair-workload-manager, engine-manager
// learns peers from the broker's discovery relay: it subscribes for the ec
// service and folds each discovery:nodes snapshot into a directory keyed by the
// peer's stable per-host UUID (hostUuid), resolving a remote-control target's
// address, port, and cluster UUID (for pin selection). Keying by UUID (not the
// hostname) means a remote target survives a PC rename and two same-named peers
// stay distinct; callers address a target by its hostUuid. The remote
// engine:remote-* methods look targets up here.

import (
	"sync"

	"nvpair-shared/noderec"
)

// ecPeer is a resolved remote-control target.
type ecPeer struct {
	nodeID      string
	addresses   []string
	port        int
	clusterUUID string
}

// peerDirectory is the relay-fed set of ec-capable peers, replaced wholesale on
// each discovery:nodes snapshot (stateless + self-correcting, matching the relay
// contract).
type peerDirectory struct {
	mu    sync.RWMutex
	peers map[string]ecPeer
}

func newPeerDirectory() *peerDirectory {
	return &peerDirectory{peers: make(map[string]ecPeer)}
}

// set replaces the directory from a discovery:nodes snapshot, keeping only nodes
// that advertise the ec service and a dialable address.
func (d *peerDirectory) set(nodes []noderec.DirectoryNode) {
	next := make(map[string]ecPeer, len(nodes))
	for _, n := range nodes {
		svc, ok := n.Services[noderec.ServiceEngineControl]
		addresses := n.CandidateIPs()
		if !ok || len(addresses) == 0 || svc.Port == 0 {
			continue
		}
		// Key by the stable hostUuid so a target survives a rename and same-named
		// peers don't collide. A relay DirectoryNode always carries a hostUuid
		// (the scanner guarantees it), so there is no name fallback.
		next[n.HostUUID] = ecPeer{
			nodeID:      n.HostUUID,
			addresses:   addresses,
			port:        svc.Port,
			clusterUUID: n.ClusterUUID,
		}
	}
	d.mu.Lock()
	d.peers = next
	d.mu.Unlock()
}

// lookup resolves a target node id to its ec peer entry.
func (d *peerDirectory) lookup(nodeID string) (ecPeer, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.peers[nodeID]
	return p, ok
}
