// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"sync"

	"nvpair-shared/noderec"
)

// registry is the promoted daemon's view of THIS node's advertised services. It
// is fed by relayed discovery:register / unregister / update-txt (each local
// worker registers its service + port; the broker's engine poller registers
// ol/lm at the engine port), and it builds the single _nvpair-node._tcp TXT
// record this node advertises. Transport is never stored — it's policy-derived
// from the service key (see noderec.ServiceKey.Transport).
//
// The node's identity (hostUuid / clusterUuid / addresses) is stamped by the
// daemon, not by the registering workers: hostUuid comes from cluster-manager's
// identity when present (see nodeid.Resolve), clusterUuid from the cluster trust
// dir, addresses from the local address ranker.
type registry struct {
	mu       sync.Mutex
	hostUUID string
	cluUUID  string
	// ips is this host's addresses in the ranker's order. The first is canonical
	// (published as ip=) and the whole list is published as ips=, because a
	// multi-homed host has no single address every peer can reach: a
	// direct-connect link works only for the machine on its far end.
	ips      []string
	services map[noderec.ServiceKey]int
}

func newRegistry(hostUUID, clusterUUID string, ips []string) *registry {
	return &registry{
		hostUUID: hostUUID,
		cluUUID:  clusterUUID,
		ips:      trimAddresses(ips),
		services: make(map[noderec.ServiceKey]int),
	}
}

// trimAddresses copies and caps a ranked address list to what the record
// publishes, so the registry never holds addresses the wire would drop.
func trimAddresses(ips []string) []string {
	if len(ips) > noderec.MaxAdvertisedIPs {
		ips = ips[:noderec.MaxAdvertisedIPs]
	}
	return slices.Clone(ips)
}

// register adds or updates a service's port. It reports whether the advertised
// record changed (so the caller only re-advertises on a real change). update-txt
// is the same operation (any per-service TXT a future service contributes would
// be merged here); today no service carries TXT — the model list moved to HTTP.
func (r *registry) register(p noderec.RegisterParams) bool {
	if p.Service == "" || p.Port == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.services[p.Service] == p.Port {
		return false
	}
	r.services[p.Service] = p.Port
	return true
}

// unregister drops a service. Reports whether the record changed.
func (r *registry) unregister(s noderec.ServiceKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.services[s]; !ok {
		return false
	}
	delete(r.services, s)
	return true
}

// setIdentity updates the node's stamped identity (e.g. after cluster-manager
// registers or its identity changes). Reports whether the record changed.
func (r *registry) setIdentity(hostUUID, clusterUUID string, ips []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ips = trimAddresses(ips)
	if r.hostUUID == hostUUID && r.cluUUID == clusterUUID && slices.Equal(r.ips, ips) {
		return false
	}
	r.hostUUID, r.cluUUID, r.ips = hostUUID, clusterUUID, ips
	return true
}

// setAddresses updates only the ranked address list, for the periodic re-rank
// that folds in evidence which arrives after startup (a peer appearing on a link,
// an interface that stops sending). It deliberately skips identity so the common
// case — nothing changed — costs no identity file read or trust-store re-read.
//
// Reports whether the record changed. Order is significant: a different order is
// a different canonical address, which is exactly the change worth republishing.
func (r *registry) setAddresses(ips []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ips = trimAddresses(ips)
	if slices.Equal(r.ips, ips) {
		return false
	}
	r.ips = ips
	return true
}

// record snapshots the current node record.
func (r *registry) record() noderec.NodeRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	svcs := make(map[noderec.ServiceKey]int, len(r.services))
	for k, v := range r.services {
		svcs[k] = v
	}
	var ip string
	if len(r.ips) > 0 {
		ip = r.ips[0]
	}
	return noderec.NodeRecord{
		SchemaVersion: noderec.SchemaVersion,
		HostUUID:      r.hostUUID,
		ClusterUUID:   r.cluUUID,
		IP:            ip,
		IPs:           slices.Clone(r.ips),
		Services:      svcs,
	}
}

// txt builds the current _nvpair-node TXT strings.
func (r *registry) txt() []string { return r.record().TXT() }
