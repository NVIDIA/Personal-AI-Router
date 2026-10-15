// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"maps"
	"sort"
	"sync"
	"time"

	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

// toDirectoryNode converts a browsed _nvpair-node instance into a directory entry
// — identity + advertised services — without enrichment or liveness (those
// decorate it later in the daemon). The directory is keyed by hostUuid (the
// stable identity invite/attribution correlate on); name is the mDNS instance
// (hostname). trusted is supplied by the caller from the cluster pin set.
//
// Addresses are carried through whole rather than reduced to one. The node ranked
// its own addresses from evidence no observer has — which of its interfaces the
// kernel routes off-link, which carry live peers, which ones peers have connected
// to — so IPs keeps that order and IP is simply its head. Collapsing here is what
// left every consumer with a single address and no recourse when it turned out to
// be a link only one other machine could reach.
//
// It returns ok=false for a record without a uuid= TXT: a conformant
// _nvpair-node record always carries one (the daemon stamps it from
// nodeid.Resolve), so a record without it can't be keyed by a stable identity.
// We skip it (and warn) rather than key it by the mutable, non-unique hostname —
// node identity is the UUID everywhere, with no hostname fallback.
func toDirectoryNode(raw RawNode, trusted bool) (noderec.DirectoryNode, bool) {
	rec := noderec.ParseTXT(raw.TXT)
	if rec.HostUUID == "" {
		slog.Warn("skipping _nvpair-node record without uuid=; cannot key it by a stable identity",
			"instance", raw.ID, "addrs", raw.Addresses)
		return noderec.DirectoryNode{}, false
	}
	// netpick.Candidates unions the record's own ip= / ips= with whatever the
	// browse resolved, preserving the node's order and appending the rest.
	ips := netpick.Candidates(raw.TXT, raw.Addresses)
	var ip string
	if len(ips) > 0 {
		ip = ips[0]
	}
	svcs := make(map[noderec.ServiceKey]noderec.ServiceStatus, len(rec.Services))
	for k, port := range rec.Services {
		svcs[k] = noderec.ServiceStatus{Port: port}
	}
	return noderec.DirectoryNode{
		HostUUID:    rec.HostUUID,
		Name:        raw.ID,
		IP:          ip,
		IPs:         ips,
		ClusterUUID: rec.ClusterUUID,
		Trusted:     trusted,
		Services:    svcs,
		// Models is enriched separately from engine-manager's em /v1/models
		// (daemon.enrichModelsAt), not carried in the record's TXT.
		LastSeen: time.Now().Unix(),
	}, true
}

// directory is the daemon's view of all LAN nodes (this node included), keyed by
// hostUuid. Mutations come from the browse-event goroutine; reads come from
// discovery:get-nodes handlers, so it's an RWMutex map.
type directory struct {
	mu    sync.RWMutex
	nodes map[string]noderec.DirectoryNode
}

func newDirectory() *directory {
	return &directory{nodes: make(map[string]noderec.DirectoryNode)}
}

// upsert stores a node and reports whether it was new (vs. an update).
func (d *directory) upsert(n noderec.DirectoryNode) (isNew bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, existed := d.nodes[n.HostUUID]
	d.nodes[n.HostUUID] = n
	return !existed
}

// supersedeMinAge is how much older a record must be than the one arriving at
// its address before it is worth asking whether it has been replaced.
//
// Note what it is NOT: evidence. LastSeen is not a liveness clock — the browser
// reports a node only when its record CHANGES (shared/discovery, reconcile), so
// a healthy peer with a stable advertisement produces no events and its LastSeen
// freezes at first discovery exactly like a ghost's does. A frozen timestamp
// says nothing about whether the node is alive, and anything willing to wait out
// the window satisfies it. Nothing may be deleted on the strength of it; it
// earns its place only as a cheap filter that keeps the identity probe in
// daemon.supersedingUpsert off the common path.
const supersedeMinAge int64 = 300 // seconds, matching DirectoryNode.LastSeen

// supersedeCandidates lists the records a newly browsed node MIGHT have replaced,
// for the caller to confirm or reject with proof. It decides nothing and deletes
// nothing.
//
// This is the peer-side counterpart to dropSelf: when a node's appdata is wiped
// it mints a fresh hostUuid, so the fleet sees the same machine arrive as a
// stranger while the pre-wipe record lingers under the old key. dropSelf handles
// that re-stamp for the local node; nothing handled it for peers, so a reset
// node showed up twice everywhere downstream.
//
// A record is nominated — and no more than nominated — when it shares the
// arriving node's Name, carries a different hostUuid, and is at least
// supersedeMinAge older:
//
//   - Same Name. A wipe clears identity but not the hostname.
//   - Older than supersedeMinAge (a filter, not evidence — see above).
//
// A shared address is deliberately NOT part of this. It reads as "one machine"
// only if an address belongs to one machine at a time, and it does not: a
// direct-connect link between two machines uses the same small subnet on every
// such pair, so identically-provisioned hosts genuinely advertise the same
// address on it, and matching there nominated live peers as each other's ghosts.
// Nor is it needed — a wiped machine keeps its hostname whether or not it comes
// back on the same address.
//
// Both remaining signals come from an unauthenticated mDNS record that anything
// on the LAN can claim, so together they describe the SHAPE of a wipe and equally
// the shape of a spoof — and, now that the address is gone, also the shape of two
// hosts that merely share a hostname. daemon.supersedingUpsert must confirm a
// candidate is really gone before deleting it.
//
// The arriving record still needs an address of its own, since that is where the
// confirming read is made — not because matching it against anything would mean
// something.
//
// selfUUID is never nominated, and never nominates: the local record is
// registry-driven rather than browse-driven, and its LastSeen advances only when
// this node republishes, so it must not be displaceable by a peer that happens
// to claim its name.
//
// Candidates are returned sorted by hostUuid so the resulting removals are
// emitted in a deterministic order rather than map-iteration order.
func (d *directory) supersedeCandidates(n noderec.DirectoryNode, selfUUID string) []noderec.DirectoryNode {
	if n.IP == "" || n.Name == "" || n.HostUUID == selfUUID {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	var candidates []noderec.DirectoryNode
	for uuid, other := range d.nodes {
		if uuid == n.HostUUID || uuid == selfUUID {
			continue
		}
		if other.Name != n.Name {
			continue
		}
		if n.LastSeen-other.LastSeen < supersedeMinAge {
			continue
		}
		candidates = append(candidates, other)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].HostUUID < candidates[j].HostUUID })
	return candidates
}

// upsertEvicting stores a node and deletes the records the caller has PROVEN it
// replaced, returning the entries actually removed so the caller can emit
// removals and drop their caches.
//
// The proof is a network read, so it is gathered without this lock held and each
// eviction is re-validated here against the entry the caller actually judged. A
// record that was removed, re-addressed, renamed, or refreshed while the probe
// was in flight is left alone: the state the verdict was formed against is gone,
// and it must be judged again from the new one.
func (d *directory) upsertEvicting(n noderec.DirectoryNode, proven []noderec.DirectoryNode) (evicted []noderec.DirectoryNode) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, old := range proven {
		if old.HostUUID == n.HostUUID {
			continue
		}
		current, present := d.nodes[old.HostUUID]
		if !present || current.IP != old.IP || current.Name != old.Name || current.LastSeen != old.LastSeen {
			continue
		}
		delete(d.nodes, old.HostUUID)
		evicted = append(evicted, current)
	}
	sort.Slice(evicted, func(i, j int) bool { return evicted[i].HostUUID < evicted[j].HostUUID })
	d.nodes[n.HostUUID] = n
	return evicted
}

// remove drops a node by hostUuid and reports whether it existed.
func (d *directory) remove(hostUUID string) (existed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, existed = d.nodes[hostUUID]
	delete(d.nodes, hostUUID)
	return existed
}

// get returns a node by hostUuid.
func (d *directory) get(hostUUID string) (noderec.DirectoryNode, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n, ok := d.nodes[hostUUID]
	return n, ok
}

// snapshot returns the directory, optionally filtered to nodes advertising a
// given service, sorted by hostUuid for stable rendering. An empty filter
// returns every node.
func (d *directory) snapshot(filter noderec.ServiceKey) []noderec.DirectoryNode {
	d.mu.RLock()
	out := make([]noderec.DirectoryNode, 0, len(d.nodes))
	for _, n := range d.nodes {
		if filter != "" && !n.HasService(filter) {
			continue
		}
		out = append(out, n)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out
}

// applyModels atomically reconciles one node's model inventory from a completed
// /v1/models fetch, guarded against a node that was removed or re-addressed
// while the fetch was in flight. It is the periodic refresh loop's write path
// (daemon.refreshNodeModels); the event-driven enrichment writes the whole node
// via upsert instead.
//
// It confirms the node still exists AND still advertises the same em endpoint
// (ip + port) the fetch targeted, so an in-flight result can never resurrect a
// removed node or overwrite one that has since been re-addressed. It then
// compares the fetched flat, per-engine, and loaded inventories
// order-insensitively with the stored entry and, only when any changed, updates
// just the model fields in place — preserving every other field (identity,
// services, node-info, trust) the browse path owns.
//
// Returns the (possibly updated) node, whether the inventory changed, and
// whether the guarded apply was valid. ok == false means the result is stale
// (node gone or re-addressed) and the caller must not cache or emit it.
func (d *directory) applyModels(hostUUID, ip string, emPort int, models []string, byEngine, loadedByEngine map[string][]string, loadedVRAMByEngine map[string]map[string]uint64) (node noderec.DirectoryNode, changed, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, present := d.nodes[hostUUID]
	if !present {
		return noderec.DirectoryNode{}, false, false
	}
	em, has := n.Services[noderec.ServiceEngineManager]
	if !has || n.IP != ip || em.Port != emPort {
		return n, false, false
	}
	if sameStringSet(n.Models, models) && sameByEngine(n.ModelsByEngine, byEngine) && sameByEngine(n.LoadedByEngine, loadedByEngine) && maps.EqualFunc(n.LoadedVRAMByEngine, loadedVRAMByEngine, maps.Equal[map[string]uint64, map[string]uint64]) {
		return n, false, true
	}
	n.Models = models
	n.ModelsByEngine = byEngine
	n.LoadedByEngine = loadedByEngine
	n.LoadedVRAMByEngine = loadedVRAMByEngine
	d.nodes[hostUUID] = n
	return n, true, true
}

// applyClusterIdentity sets one node's cluster principal and the trust annotation
// derived from it, reporting whether either changed. It is the write path for both
// membership reconciles — the pin-set pass (daemon.reconcileTrust) and the
// node-info pass (daemon.refreshClusterIdentityOnce) — and mirrors applyModels: it
// updates just those two fields in place, preserving everything the browse and
// enrichment paths own, and reports no change for an entry that already agrees so
// the caller stays silent in the steady state.
//
// The two fields move together because trust is derived from the principal: a peer
// that leaves its cluster is no longer pinned under it, so leaving one stale while
// updating the other would describe a node that cannot exist. For the same reason
// trust is computed here, from the principal this call settles on, rather than
// supplied by the caller: a caller computing it from a snapshot would race the
// other reconcile and could write back a principal that had already moved.
//
// A nil clusterUUID means "keep whatever is stored" — the pin-set pass re-derives
// trust without claiming to know the principal, since only the peer's own record
// or report can say what it is.
//
// A node that has since been evicted reports no change — there is nothing to
// annotate, and a later rediscovery evaluates both from scratch.
func (d *directory) applyClusterIdentity(hostUUID string, clusterUUID *string, trustFor func(clusterUUID string) bool) (node noderec.DirectoryNode, changed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, present := d.nodes[hostUUID]
	if !present {
		return n, false
	}
	settled := n.ClusterUUID
	if clusterUUID != nil {
		settled = *clusterUUID
	}
	trusted := false
	if settled != "" {
		trusted = trustFor(settled)
	}
	if n.ClusterUUID == settled && n.Trusted == trusted {
		return n, false
	}
	n.ClusterUUID = settled
	n.Trusted = trusted
	d.nodes[hostUUID] = n
	return n, true
}

// sameByEngine reports whether two per-engine model maps are semantically equal:
// the same set of engine keys, each mapping to the same set of model names
// (order-insensitive). A nil and an empty map compare equal (both mean "no
// per-engine attribution").
func sameByEngine(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameStringSet(av, bv) {
			return false
		}
	}
	return true
}
