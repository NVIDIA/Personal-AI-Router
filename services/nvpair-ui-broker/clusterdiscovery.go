// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"nvpair-shared/noderec"
	"slices"
	"time"
)

func (b *Broker) updateClusterPeers(raw json.RawMessage) {
	b.updateClusterPeersAt(raw, nil)
}

func (b *Broker) updateClusterPeersAt(raw json.RawMessage, expected *uint64) {
	var p struct {
		Nodes []struct {
			UUID    string `json:"nodeUuid"`
			Name    string `json:"name"`
			Address string `json:"ipAddress"`
			State   string `json:"state"`
		} `json:"nodes"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	peers := make([]noderec.ClusterPeer, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		if n.State != "member" || n.UUID == "" || n.UUID == b.nodeID || n.Address == "" {
			continue
		}
		peers = append(peers, noderec.ClusterPeer{HostUUID: n.UUID, Name: n.Name, Address: n.Address, NodeInfoPort: nodeInfoHTTPPort})
	}
	b.clusterPeersMu.Lock()
	if expected != nil && b.clusterPeersGeneration != *expected {
		b.clusterPeersMu.Unlock()
		return
	}
	b.clusterPeersGeneration++
	b.clusterPeers = peers
	b.clusterPeersMu.Unlock()
	b.pushClusterPeers(b.getScanner())
}
func (b *Broker) pushClusterPeers(scanner *scannerProcess) {
	if scanner == nil {
		return
	}
	b.clusterPeersMu.Lock()
	peers := slices.Clone(b.clusterPeers)
	// Keep snapshot publication ordered with roster replacement.
	defer b.clusterPeersMu.Unlock()
	if err := scanner.peer.Notify(noderec.MethodSetClusterPeers, noderec.ClusterPeersParams{Peers: peers}); err != nil {
		slog.Warn("failed to seed member discovery", "err", err)
	}
}
func (b *Broker) refreshClusterPeers() {
	manager := b.getClusterMgr()
	if manager == nil {
		return
	}
	b.clusterPeersMu.Lock()
	generation := b.clusterPeersGeneration
	b.clusterPeersMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, rpcErr, err := manager.Call(ctx, "nodes:get-initial", nil)
	if err == nil && rpcErr == nil {
		b.updateClusterPeersAt(raw, &generation)
	}
}
