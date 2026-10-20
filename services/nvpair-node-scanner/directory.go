// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

// toDirectoryNode projects announcement claims for display and authentication.
// A claimed UUID or principal never grants trust.
func toDirectoryNode(raw RawNode) (noderec.DirectoryNode, bool) {
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
		DiscoveryRecordID:    nodeDiscoveryRecordID(raw),
		IdentityVerification: noderec.IdentityUnverified,
		HostUUID:             rec.HostUUID,
		Name:                 raw.ID,
		IP:                   ip,
		IPs:                  ips,
		ClusterUUID:          rec.ClusterUUID,
		Services:             svcs,
		// Models are absent from announcements. Only the verified peer record
		// receives them through the authenticated engine-manager refresh.
		LastSeen: time.Now().Unix(),
	}, true
}

// nodeDiscoveryRecordID identifies a connection candidate, not its claimed owner.
// Address ordering and UUID/principal claims do not change that identity.
func nodeDiscoveryRecordID(raw RawNode) string {
	rec := noderec.ParseTXT(raw.TXT)
	addresses := append([]string(nil), netpick.Candidates(raw.TXT, raw.Addresses)...)
	sort.Strings(addresses)
	fields := []string{raw.ID, raw.Host, strconv.Itoa(rec.Services[noderec.ServiceNodeInfo])}
	fields = append(fields, addresses...)
	return fmt.Sprintf("observation:%x", sha256.Sum256([]byte(fmt.Sprintf("%q", fields))))
}

// directory keeps canonical hosts by UUID and unverified discoveries by their
// scanner-assigned record ID. A network claim cannot select a canonical key.
// The mutex covers whole changes, such as removing a discovery record and storing
// its verified peer. sync.Map alone only protects individual map operations.
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
	_, existed := d.nodes[directoryKey(n)]
	d.nodes[directoryKey(n)] = n
	return !existed
}

// directoryKey keeps unverified claims outside the canonical host namespace.
func directoryKey(n noderec.DirectoryNode) string {
	if n.IdentityVerification == noderec.IdentityUnverified {
		return n.DiscoveryRecordID
	}
	return n.HostUUID
}

// promote atomically replaces the authenticated discovery with its peer record.
// Inventory refreshes preserve model fields written concurrently by their owner.
func (d *directory) promote(candidateID string, n noderec.DirectoryNode) (removed noderec.DirectoryNode, discovered, changed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	removed = d.nodes[candidateID]
	delete(d.nodes, candidateID)
	old, present := d.nodes[n.HostUUID]
	if present {
		n.Models, n.ModelsByEngine, n.LoadedByEngine = old.Models, old.ModelsByEngine, old.LoadedByEngine
		n.LastSeen = old.LastSeen
	}
	changed = !present || !old.Equals(n)
	if changed {
		n.LastSeen = time.Now().Unix()
	}
	d.nodes[n.HostUUID] = n
	return removed, !present, changed
}

// remove drops a canonical host or a discovery by its storage key.
func (d *directory) remove(hostUUID string) (existed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, existed = d.nodes[hostUUID]
	delete(d.nodes, hostUUID)
	return existed
}

// get returns a canonical host by UUID or an unverified discovery by record ID.
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostUUID == out[j].HostUUID {
			return out[i].DiscoveryRecordID < out[j].DiscoveryRecordID
		}
		return out[i].HostUUID < out[j].HostUUID
	})
	return out
}

type localInfoResult uint8

const (
	localInfoMissing localInfoResult = iota
	localInfoUnchanged
	localInfoUpdated
)

// applyLocalInfo changes only hardware under one directory lock. Background
// model refreshes do not take selfMu, so separate get/upsert calls could overwrite
// their newer model inventory with the hardware handler's earlier copy.
func (d *directory) applyLocalInfo(info noderec.NodeInfo) (noderec.DirectoryNode, localInfoResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	node, present := d.nodes[info.HostUUID]
	if !present {
		return node, localInfoMissing
	}
	if slices.Equal(node.GPUs, info.GPUs) && equalOptional(node.CPU, info.CPU) && equalOptional(node.Memory, info.Memory) {
		return node, localInfoUnchanged
	}
	node.GPUs, node.CPU, node.Memory = info.GPUs, info.CPU, info.Memory
	node.LastSeen = time.Now().Unix()
	d.nodes[info.HostUUID] = node
	return node, localInfoUpdated
}

// equalOptional distinguishes an absent value from a present zero value.
func equalOptional[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// applyModels atomically reconciles one node's model inventory from a completed
// /v1/models fetch, guarded against a node that was removed or re-addressed
// while the fetch was in flight. Both the post-authentication model sweep and
// periodic refresh use this write path through refreshNodeModelsCandidates.
//
// It confirms the node still exists AND still advertises the same em endpoint
// (ip + port) the fetch targeted, so an in-flight result can never resurrect a
// removed node or overwrite one that has since been re-addressed. It then
// compares the fetched flat, per-engine, and loaded inventories
// order-insensitively with the stored entry and, only when any changed, updates
// just the model fields in place, preserving identity, services, hardware, and
// trust from local registry publications or authenticated peer inventory.
//
// Returns the (possibly updated) node, whether the inventory changed, and
// whether the guarded apply was valid. ok == false means the result is stale
// (node gone or re-addressed) and the caller must not cache or emit it.
func (d *directory) applyModels(hostUUID, ip string, emPort int, models []string, byEngine, loadedByEngine map[string][]string) (node noderec.DirectoryNode, changed, ok bool) {
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
	if sameStringSet(n.Models, models) && sameByEngine(n.ModelsByEngine, byEngine) && sameByEngine(n.LoadedByEngine, loadedByEngine) {
		return n, false, true
	}
	n.Models = models
	n.ModelsByEngine = byEngine
	n.LoadedByEngine = loadedByEngine
	d.nodes[hostUUID] = n
	return n, true, true
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
