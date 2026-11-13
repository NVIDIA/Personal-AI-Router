// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"nvpair-shared/discovery"
	"nvpair-shared/noderec"
	"strconv"
)

// A roster address enters the same candidate authentication path as mDNS.
// It cannot supply trusted identity, service ports, models, or hardware.
func clusterPeerNode(peer noderec.ClusterPeer) RawNode {
	return RawNode{ID: "cluster-roster:" + peer.HostUUID, Host: peer.Address, Addresses: []string{peer.Address}, TXT: []string{
		"uuid=" + peer.HostUUID, "cluster-uuid=" + peer.HostUUID, "ni=" + strconv.Itoa(peer.NodeInfoPort),
	}}
}

func (d *daemon) setClusterPeers(peers []noderec.ClusterPeer) error {
	next := make(map[string]noderec.ClusterPeer, len(peers))
	for _, peer := range peers {
		if peer.HostUUID == "" || peer.Address == "" || peer.NodeInfoPort < 1 || peer.NodeInfoPort > 65535 {
			return errors.New("invalid cluster discovery peer")
		}
		if peer.HostUUID != d.reg.record().HostUUID {
			next[peer.HostUUID] = peer
		}
	}
	d.clusterPeersMu.Lock()
	defer d.clusterPeersMu.Unlock()
	for id, old := range d.clusterPeers {
		if current, keep := next[id]; !keep || current != old {
			d.onBrowse(DiscoveryEvent{Type: discovery.Removed, Node: clusterPeerNode(old)})
		}
	}
	d.clusterPeers = next
	return nil
}

// Serialize hint replacement with candidate publication. Authentication runs
// outside this lock in the existing bounded, cancellable candidate workers.
func (d *daemon) refreshClusterPeers() {
	d.clusterPeersMu.Lock()
	defer d.clusterPeersMu.Unlock()
	for _, peer := range d.clusterPeers {
		d.onBrowse(DiscoveryEvent{Type: discovery.Discovered, Node: clusterPeerNode(peer)})
	}
}
