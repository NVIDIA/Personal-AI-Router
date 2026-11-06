// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Resolve invite and member addresses from broker discovery snapshots (§7.5).
// Scanner supplies records advertising cl; this resolver opens no browse socket
// or RPC surface. Each snapshot replaces the map, so departed records stop
// resolving. A supplied invite address is tried before resolved candidates.

import (
	"sync"

	"nvpair-shared/discovery"
	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

type resolverKeyKind uint8

const (
	byDiscoveryRecord resolverKeyKind = iota
	byHostUUID
)

// resolverKey keeps advertised UUID text separate from scanner-assigned record
// IDs, so a claim cannot overwrite another record's pairing lookup.
type resolverKey struct {
	kind resolverKeyKind
	id   string
}

// Browser resolves relay records by discovery record ID and host UUID.
type Browser struct {
	mu    sync.RWMutex
	relay map[resolverKey]discovery.Node
}

func newBrowser() *Browser {
	return &Browser{relay: make(map[resolverKey]discovery.Node)}
}

// setRelay replaces the resolver map from a discovery:nodes snapshot. Records
// with a cluster service and dialable addresses remain selectable by record ID
// for pairing. Verified records take precedence when resolving a host UUID.
// Departed records disappear with the next snapshot. Synthesized TXT preserves
// address ranking for callers that need address failover.
func (b *Browser) setRelay(nodes []noderec.DirectoryNode) {
	m := make(map[resolverKey]discovery.Node, len(nodes))
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
			m[resolverKey{kind: byDiscoveryRecord, id: n.DiscoveryRecordID}] = node
		}
		if verified[n.HostUUID] && !proved {
			continue
		}
		m[resolverKey{kind: byHostUUID, id: n.HostUUID}] = node
		verified[n.HostUUID] = proved
	}
	b.mu.Lock()
	b.relay = m
	b.mu.Unlock()
}

// Resolve returns ranked addresses and the cluster-manager port. Exact record
// IDs take precedence over host UUIDs, followed by hostname matching. Callers try
// the addresses in order because the publisher's ranking does not establish
// which addresses this host can reach.
func (b *Browser) Resolve(idOrUUID string) (hosts []string, port int, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, kind := range []resolverKeyKind{byDiscoveryRecord, byHostUUID} {
		if node, present := b.relay[resolverKey{kind: kind, id: idOrUUID}]; present && node.Port > 0 {
			if addresses := pickHosts(node); len(addresses) > 0 {
				return addresses, node.Port, true
			}
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
	b.relay[resolverKey{kind: byHostUUID, id: uuid}] = discovery.Node{
		ID:        uuid,
		Addresses: []string{host},
		Port:      port,
	}
}
