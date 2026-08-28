// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"

	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// relaySource.go is the PeerSource fed by the broker's discovery relay.
// The Manager routes discovery:nodes snapshots into set(), and Nodes()
// returns the current peer snapshot. Post-cutover it is the sole peer source:
// this node is advertised by the node-scanner daemon's single _nvpair-node record
// and peers are pushed down from the broker relay, replacing the per-service
// _nvpair-workload-manager advertise+browse this service used to run itself.

// relayPeerSource maintains the peer set pushed down by the broker relay, keyed
// by the directory hostUuid. It drops our own _nvpair-node record from the peer
// set — the relay directory includes the local node (the client node list wants
// it), but a broadcast target set must not, mirroring the mDNS browser's
// WithSelfFilter. selfUUID is this node's stable per-host UUID; the self-filter
// keys on it, so a peer that merely shares our hostname is NOT dropped.
// A relay DirectoryNode always carries a hostUuid (the scanner
// guarantees it), so there is no hostname fallback.
type relayPeerSource struct {
	selfUUID string

	mu    sync.RWMutex
	peers map[string]PeerNode
}

func newRelayPeerSource(selfUUID string) *relayPeerSource {
	return &relayPeerSource{selfUUID: selfUUID, peers: make(map[string]PeerNode)}
}

// isSelf reports whether a directory node is this node's own record, matching
// on the stable hostUuid.
func (r *relayPeerSource) isSelf(n noderec.DirectoryNode) bool {
	return n.HostUUID == r.selfUUID
}

// Nodes returns the current relay-sourced peers.
func (r *relayPeerSource) Nodes(context.Context) ([]PeerNode, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PeerNode, 0, len(r.peers))
	for _, n := range r.peers {
		out = append(out, n)
	}
	return out, nil
}

// set replaces the relay peer set from a discovery:nodes snapshot: it projects
// every node advertising wl with a dialable IP (dropping the rest) and applies
// the self-filter. The broker sends the full filtered set on every change, so
// this is a wholesale replace, not a per-node apply — a departed peer is simply
// absent from the next snapshot.
func (r *relayPeerSource) set(nodes []noderec.DirectoryNode) {
	m := make(map[string]PeerNode, len(nodes))
	for _, node := range nodes {
		// Never treat our own node as a broadcast peer (self-filter), keyed by
		// the stable UUID so a same-named peer isn't excluded.
		if r.isSelf(node) {
			continue
		}
		pn, ok := directoryToPeerNode(node)
		if !ok {
			continue
		}
		m[node.HostUUID] = pn
	}
	r.mu.Lock()
	r.peers = m
	r.mu.Unlock()
}

// directoryToPeerNode projects a relay DirectoryNode onto a PeerNode for the wl
// service, returning false when the node doesn't advertise wl or has no dialable
// address. ID is the stable hostUuid — peerSet.Replace keys the broadcast set by
// PeerNode.ID, so two peers sharing a hostname must key on distinct UUIDs or
// they'd collapse into one and one would silently miss every broadcast.
// Host stays the hostname (display / dial fallback); the peer is
// dialed by its ip=/Addresses, not its ID. cluster-uuid= is reconstructed into
// TXT so the broadcaster's mTLS pin lookup is unchanged.
func directoryToPeerNode(n noderec.DirectoryNode) (PeerNode, bool) {
	svc, ok := n.Services[noderec.ServiceWorkload]
	addresses := n.CandidateIPs()
	if !ok || len(addresses) == 0 {
		return PeerNode{}, false
	}
	txt := n.AddressTXT()
	if n.ClusterUUID != "" {
		txt = append(txt, clustertrust.ClusterUUIDTXTKey+"="+n.ClusterUUID)
	}
	return PeerNode{
		ID:        n.HostUUID,
		Host:      n.Name,
		Addresses: addresses,
		Port:      svc.Port,
		TXT:       txt,
	}, true
}
