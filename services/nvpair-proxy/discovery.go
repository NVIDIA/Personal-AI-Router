// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Discovery is the proxy's routing-target set. Post-cutover the proxy
// no longer runs its own _nvpair-ollama browse: routing targets are pushed down
// from the broker's discovery relay (discovery:nodes snapshots for the ol
// service) into the subscribed overlay, merged with user-added manual nodes. The
// proxy is
// itself advertised — as an ol service — by the node-scanner daemon's single
// _nvpair-node record, keyed off the engine port the broker's poller registers.
//
// The routable Node projection (IP / withPrimaryIP) and the manual-node
// overlay live here; request-path reachability (TCP-probe + failover) lives in
// proxy.go.

import (
	"slices"
	"sync"

	"nvpair-shared/discovery"
	"nvpair-shared/netpick"
)

// uuidFromTXT extracts a node's stable uuid= from its TXT records. Kept as a
// thin re-export of the shared helper (it was triplicated across the two proxies
// and the scanner before consolidation).
var uuidFromTXT = discovery.UUIDFromTXT

// Node is the proxy's routable view of a node. It adds a canonical dialable IP
// field over the discovered node shape.
type Node struct {
	ID        string   `json:"id"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	TXT       []string `json:"txt"`
	// Models is the latest model inventory carried by the broker's discovery
	// snapshot. Model-bearing inference is eligible only when this list
	// advertises the requested model; an empty list stays in discovery but is
	// not an inference candidate until a later inventory update.
	Models []string `json:"models,omitempty"`
	// IP is the single canonical LAN address a consumer should dial/display for
	// this node, resolved via the shared netpick ranker: the node's
	// own ip= TXT if present, else the best-scored advertised IPv4. It is
	// stamped onto outbound node/* notifications only (see withPrimaryIP) so a
	// downstream consumer agrees on the same address the proxy routes to.
	IP string `json:"ip,omitempty"`
	// APIKey is the user-configured credential for an external
	// OpenAI-API-compatible endpoint, carried per candidate so the dial path
	// can inject it. Internal routing plumbing, like ClusterUUID: it never
	// belongs in a log line, a telemetry payload, or an outward node/*
	// notification, which is what Node.redacted is for. The tag exists only
	// so the broker's local overlay push and node/add-manual params can
	// carry it; it is stripped before anything leaves the process.
	APIKey string `json:"apiKey,omitempty"`
	// Scheme overrides the dial scheme for a manual endpoint ("https");
	// empty means plain http. It selects the TLS dial but never pinning:
	// an external endpoint rides the plain shared transport, so it gets
	// system-root verification without a pin, unlike a clustered peer.
	Scheme string `json:"scheme,omitempty"`
	// ClusterUUID is the relay peer's cluster principal (its mTLS cert UUID),
	// carried from the discovery DirectoryNode. Non-empty only for a clustered
	// peer; it is the key used to pin the peer's server cert when dialing its
	// promoted proxy over cluster mTLS. Whether we actually hold that pin is
	// resolved against the live mesh at routing time (resolveCandidates), never
	// cached here: a cached answer goes stale the moment a peer is paired or
	// removed. Internal routing metadata, not part of the proxy's outward node
	// contract.
	ClusterUUID string `json:"-"`
}

// withPrimaryIP returns a copy of the node with IP resolved by the shared ranker
// (netpick.Primary over its TXT + Addresses).
func (n Node) withPrimaryIP() Node {
	n.IP = netpick.Primary(n.TXT, n.Addresses)
	return n
}

// redacted returns a copy with the per-candidate credential cleared. Every
// outward projection of a Node must pass through it: the endpoint key is
// dial-time plumbing, and a "temporary" passthrough on a notification path
// is exactly how a secret reaches a UI, a feed, or a paired peer.
func (n Node) redacted() Node {
	n.APIKey = ""
	return n
}

// Discovery holds the proxy's routing targets: the relay-fed subscribed overlay
// and the user-added manual overlay.
type Discovery struct {
	mu          sync.RWMutex
	manualNodes map[string]Node
	// subscribedNodes are routing targets pushed down by the broker's discovery
	// relay (discovery:nodes snapshots for the ol service), keyed by node ID (the
	// directory instance name).
	subscribedNodes map[string]Node
	// probeMisses counts consecutive authorization refusals (HTTP 401/403)
	// per manual endpoint. It lives here because AddManual and RemoveManual
	// own the lifetime of an endpoint's routing episode, and the reset rule
	// is that both of them clear the count.
	probeMisses map[string]int
}

func NewDiscovery() *Discovery {
	return &Discovery{
		manualNodes:     make(map[string]Node),
		subscribedNodes: make(map[string]Node),
		probeMisses:     make(map[string]int),
	}
}

// Nodes returns the merged subscribed + manual node set (manual entries that
// aren't also present via the relay are appended).
func (d *Discovery) Nodes() []Node {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Node, 0, len(d.manualNodes)+len(d.subscribedNodes))
	seen := make(map[string]struct{}, len(d.subscribedNodes))
	for id, n := range d.subscribedNodes {
		out = append(out, n)
		seen[id] = struct{}{}
	}
	for id, n := range d.manualNodes {
		if _, exists := seen[id]; !exists {
			out = append(out, n)
		}
	}
	return out
}

// Has reports whether a node id is still present in either overlay. Used to
// watch an in-flight request's target: a node that has dropped out of discovery
// is not coming back to answer the attempt already sent to it.
func (d *Discovery) Has(id string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if _, ok := d.subscribedNodes[id]; ok {
		return true
	}
	_, ok := d.manualNodes[id]
	return ok
}

// SetSubscribed replaces the relay-fed routing overlay with the given set and
// reports what changed versus the previous set (keyed by node ID): nodes newly
// present, nodes whose routable details changed, and nodes that dropped out. The
// broker pushes the full filtered snapshot on every change, so the overlay is
// replaced wholesale; the returned diff lets the caller emit node/discovered|
// updated|removed so a consumer (the UI) learns which peers currently run this
// engine — the signal the proxy's own _nvpair-ollama browse provided before
// discovery was consolidated onto the relay. Manual nodes are a separate overlay
// and are untouched.
func (d *Discovery) SetSubscribed(nodes []Node) (discovered, updated, removed []Node) {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		next[n.ID] = n
		switch prev, ok := d.subscribedNodes[n.ID]; {
		case !ok:
			discovered = append(discovered, n)
		case !nodeEqual(prev, n):
			updated = append(updated, n)
		}
	}
	for id, prev := range d.subscribedNodes {
		if _, ok := next[id]; !ok {
			removed = append(removed, prev)
		}
	}
	d.subscribedNodes = next
	return discovered, updated, removed
}

// nodeEqual reports whether two routable Nodes carry the same routing/display
// identity — the fields a consumer dials or renders. A change in any of them
// warrants a node/updated.
func nodeEqual(a, b Node) bool {
	return a.ID == b.ID && a.Host == b.Host && a.Port == b.Port && a.IP == b.IP &&
		slices.Equal(a.Addresses, b.Addresses) && slices.Equal(a.TXT, b.TXT) &&
		slices.Equal(a.Models, b.Models)
}

func (d *Discovery) AddManual(node Node) (added bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// A (re-)add begins a fresh probe-failure episode: an endpoint removed
	// after repeated authorization refusals may be re-added once a probe
	// succeeds, and it then gets its full budget back rather than being
	// removed on its first refused request.
	delete(d.probeMisses, node.ID)
	_, exists := d.manualNodes[node.ID]
	d.manualNodes[node.ID] = node
	return !exists
}

func (d *Discovery) RemoveManual(id string) (removed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.probeMisses, id)
	_, exists := d.manualNodes[id]
	if exists {
		delete(d.manualNodes, id)
	}
	return exists
}

func (d *Discovery) IsManual(id string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, exists := d.manualNodes[id]
	return exists
}

// probeFailThreshold matches the nvpair-manual-nodes probe rule: three
// consecutive failures remove an endpoint. A wrong API key answers
// identically on every try, so three is enough to call an endpoint dead,
// while one transient 401 never drops a working server.
const probeFailThreshold = 3

// recordProbeMiss records one authorization refusal against a manual
// endpoint and reports whether it has now reached the probe-failure
// threshold. Only manual endpoints count misses: a 401 from a clustered
// peer or a managed engine is that engine's own business, while the
// manual-plane counter exists precisely because a user-entered credential
// can be wrong in a way that makes every retry pointless.
func (d *Discovery) recordProbeMiss(id string) (failed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.manualNodes[id]; !ok {
		return false
	}
	d.probeMisses[id]++
	return d.probeMisses[id] >= probeFailThreshold
}

// recordProbeSuccess clears the authorization-refusal count for a manual
// endpoint: one accepted request proves the configured credential works,
// which is the whole question the counter asks.
func (d *Discovery) recordProbeSuccess(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.probeMisses, id)
}
