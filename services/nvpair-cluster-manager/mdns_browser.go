// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mdns_browser.go resolves a cluster peer's host:port from a nodeUuid / nodeId,
// used to resolve an invite target given only a UUID or hostname and to
// corroborate a member's advertised addr (§7.5). It exposes no RPC; node
// selection is the Broker's job and a Broker-supplied address always wins.
//
// Post-cutover this no longer runs its own _nvpair-cluster-manager
// browse. This node's cl=<port> is carried in the single _nvpair-node record the
// node-scanner daemon advertises; the Manager feeds the discovery:nodes snapshot
// here via setRelay, and Resolve answers from that relay-fed map. The map DOES
// evict (a removed node stops resolving), the deliberate behavior change from the
// former accumulate-only browse.

import (
	"sync"

	"nvpair-shared/discovery"
	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

// Browser resolves relay records by discovery record ID and host UUID.
type Browser struct {
	mu    sync.RWMutex
	relay map[string]discovery.Node
}

func newBrowser() *Browser {
	return &Browser{relay: make(map[string]discovery.Node)}
}

// setRelay replaces the resolver map from a discovery:nodes snapshot. Records
// with a cluster service and dialable addresses remain selectable by record ID
// for pairing. Verified records take precedence when resolving a host UUID.
// Departed records disappear with the next snapshot. Synthesized TXT preserves
// address ranking for callers that need address failover.
func (b *Browser) setRelay(nodes []noderec.DirectoryNode) {
	m := make(map[string]discovery.Node, len(nodes))
	verified := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		proved := n.IdentityVerification == noderec.IdentityLocal || n.IdentityVerification == noderec.IdentityPinned
		svc, ok := n.Services[noderec.ServiceCluster]
		if !ok {
			continue
		}
		// Gate on having somewhere to dial, not on the canonical field being set:
		// a node whose ranking is entirely in ips= is perfectly resolvable, and
		// dropping it would make it unpairable for no reason.
		hosts := n.CandidateIPs()
		if len(hosts) == 0 {
			continue
		}
		node := discovery.Node{
			ID:        n.Name,
			Addresses: hosts,
			Port:      svc.Port,
			TXT:       n.AddressTXT(),
		}
		// A selected discovery remains addressable for pairing, independently
		// of the preferred endpoint for an already verified host.
		if n.DiscoveryRecordID != "" {
			m[n.DiscoveryRecordID] = node
		}
		if verified[n.HostUUID] && !proved {
			continue
		}
		m[n.HostUUID] = node
		verified[n.HostUUID] = proved
	}
	b.mu.Lock()
	b.relay = m
	b.mu.Unlock()
}

// Resolve returns ranked addresses and the cluster-manager port. Exact record
// ID and host UUID lookups take precedence over hostname matching. Callers try
// the addresses in order because the publisher's ranking does not establish
// which addresses this host can reach.
func (b *Browser) Resolve(idOrUUID string) (hosts []string, port int, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	// setRelay and seed index every UUID directly. Only hostname lookups need
	// a scan, so adding another writer must preserve that indexing invariant.
	if node, present := b.relay[idOrUUID]; present && node.Port > 0 {
		if addresses := pickHosts(node); len(addresses) > 0 {
			return addresses, node.Port, true
		}
	}
	for _, n := range b.relay {
		if n.ID != idOrUUID || n.Port == 0 {
			continue
		}
		if addresses := pickHosts(n); len(addresses) > 0 {
			return addresses, n.Port, true
		}
	}
	return nil, 0, false
}

// pickHosts returns the node's dialable addresses in ranked order, falling back
// to whatever it advertised when none of them rank.
func pickHosts(n discovery.Node) []string {
	if h := netpick.Candidates(n.TXT, n.Addresses); len(h) > 0 {
		return h
	}
	return n.Addresses
}

// seed primes a known peer into the resolver map without a live event (tests).
func (b *Browser) seed(uuid, host string, port int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.relay[uuid] = discovery.Node{
		ID:        uuid,
		Addresses: []string{host},
		Port:      port,
	}
}
