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

// Browser holds the relay-fed peer map, keyed by the directory hostUuid.
type Browser struct {
	mu    sync.RWMutex
	relay map[string]discovery.Node
}

func newBrowser() *Browser {
	return &Browser{relay: make(map[string]discovery.Node)}
}

// setRelay replaces the resolver map from a discovery:nodes snapshot: every node
// advertising cl with an IP becomes a resolvable entry keyed by hostUuid (the
// rest are dropped). The broker sends the full filtered set on every change, so
// this is a wholesale replace, not a per-node apply — a departed node is simply
// absent from the next snapshot and stops resolving. The synthesized TXT carries
// uuid=<hostUuid> so Resolve's UUID match works (hostUuid is the same principal
// the node advertises as uuid=), plus the node's own address ranking so callers
// can fail over past an address they cannot reach.
func (b *Browser) setRelay(nodes []noderec.DirectoryNode) {
	m := make(map[string]discovery.Node, len(nodes))
	for _, n := range nodes {
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
		m[n.HostUUID] = discovery.Node{
			ID:        n.Name,
			Addresses: hosts,
			Port:      svc.Port,
			TXT:       append([]string{"uuid=" + n.HostUUID}, n.AddressTXT()...),
		}
	}
	b.mu.Lock()
	b.relay = m
	b.mu.Unlock()
}

// Resolve returns a node's addresses in the order it ranked them, plus its
// cluster-manager port, for a UUID or nodeId (hostname); a UUID match wins. The
// list is what a caller dials down until one address answers — the publisher's
// ranking is evidence from its vantage point, not a promise about this host's.
func (b *Browser) Resolve(idOrUUID string) (hosts []string, port int, ok bool) {
	b.mu.RLock()
	nodes := make([]discovery.Node, 0, len(b.relay))
	for _, n := range b.relay {
		nodes = append(nodes, n)
	}
	b.mu.RUnlock()

	var byNode discovery.Node
	haveNode := false
	for _, n := range nodes {
		if n.Port == 0 {
			continue
		}
		if discovery.UUIDFromTXT(n.TXT) == idOrUUID {
			if h := pickHosts(n); len(h) > 0 {
				return h, n.Port, true
			}
		}
		if n.ID == idOrUUID && !haveNode {
			byNode, haveNode = n, true
		}
	}
	if haveNode {
		if h := pickHosts(byNode); len(h) > 0 {
			return h, byNode.Port, true
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
		TXT:       []string{"uuid=" + uuid},
	}
}
