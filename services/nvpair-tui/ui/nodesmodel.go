// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net"
	"sort"
	"strings"

	"nvpair-shared/noderec"
)

// Being listed and being reachable are different facts: a cluster member that
// has gone away stays in the roster, and showing it without that distinction is
// what made departed peers look present. What decides reachability is the
// backend's own eviction — see discoveredPresence, which deliberately keeps no
// timer of its own.

// nodePresence is whether PAIR can currently reach a node.
type nodePresence int

const (
	// presenceUnknown is a node we have no liveness evidence for either way —
	// a roster member that discovery has never reported and no probe covers.
	presenceUnknown nodePresence = iota
	presenceOnline
	presenceOffline
)

func (p nodePresence) String() string {
	switch p {
	case presenceOnline:
		return "Online"
	case presenceOffline:
		return "Offline"
	default:
		return "Unknown"
	}
}

// nodeMembership is a node's relationship to our cluster. It is deliberately
// separate from presence: the old single STATUS column conflated "we trust
// this node" with "this node is up", so a departed member read as Connected.
type nodeMembership int

const (
	// membershipNone is a standalone node — the only kind that can be invited.
	membershipNone nodeMembership = iota
	membershipMember
	membershipPending
	// membershipForeign is a node that belongs to some other cluster. It must
	// leave that one before it can pair with us.
	membershipForeign
)

func (m nodeMembership) String() string {
	switch m {
	case membershipMember:
		return "Member"
	case membershipPending:
		return "Pending"
	case membershipForeign:
		return "Other cluster"
	default:
		return "-"
	}
}

// invitable reports whether an invite to this node could succeed. An existing
// relationship — ours or another cluster's — must be removed first, and the
// peer rejects the attempt either way, so the UI declines to send it.
func (m nodeMembership) invitable() bool {
	return m == membershipNone
}

// relationship describes a membership in a sentence.
//
// String is a table cell — "Member", "Other cluster" — and lowercasing it into
// prose produced "peer is already other cluster". A column label and a clause
// are not the same text.
func (m nodeMembership) relationship() string {
	switch m {
	case membershipMember:
		return "a member of this cluster"
	case membershipPending:
		return "part-way through pairing"
	case membershipForeign:
		return "in another cluster"
	default:
		return "unrelated to this cluster"
	}
}

// nodeRow is one machine as the Nodes tab presents it, merged from the three
// feeds that each hold part of the picture:
//
//   - discovery — liveness, address, and model inventory (AvailableNode)
//   - cluster   — membership state for peers we have paired with
//   - manual    — user-added entries discovery cannot see, plus their probes
//
// Verified host identities join matching feeds. Unverified discoveries retain
// separate record keys, and unverified manual configurations retain manual keys.
type nodeRow struct {
	identityVerification noderec.IdentityVerification
	key                  string
	name                 string
	address              string
	// addresses is every address the node published, ranked by the node itself
	// with address first. Anything that dials the node walks this rather than
	// assuming the first one is reachable from here.
	addresses []string
	port      int

	presence   nodePresence
	membership nodeMembership
	self       bool

	// manualID is the handle node/remove needs. Empty unless the entry was
	// added by hand.
	manualID string
	// nodeInfoTLS marks a manual TLS configuration. The TUI uses the worker's
	// verified reading for this endpoint because the TUI does not hold the
	// cluster certificate needed for a TLS request.
	nodeInfoTLS     bool
	probedTelemetry *nodeTelemetry

	models         []string
	modelsByEngine map[string][]string
	loadedByEngine map[string][]string
}

// modelCount is the number of distinct models the node advertises.
func (n nodeRow) modelCount() int { return len(n.models) }

// nodeFeeds is the raw input to a merge: one snapshot from each source.
type nodeFeeds struct {
	discovered []availableNode
	members    []clusterNode
	manual     []manualNode
	selfUUID   string
}

// mergeNodes folds the three feeds into the unified list the Nodes tab renders.
//
// Discovery is the base layer because it carries the richest record. The
// cluster roster then contributes membership and, critically, keeps a member
// listed even when discovery has dropped it — that node is offline, not gone.
// Manual entries join matching authenticated host identities. Names and shared
// addresses never join unverified discoveries to those records.
func mergeNodes(in nodeFeeds) []nodeRow {
	byKey := make(map[string]*nodeRow, len(in.discovered)+len(in.members)+len(in.manual))
	order := make([]string, 0, len(byKey))

	get := func(key string) *nodeRow {
		if row, ok := byKey[key]; ok {
			return row
		}
		row := &nodeRow{key: key}
		byKey[key] = row
		order = append(order, key)
		return row
	}

	for _, d := range in.discovered {
		key := d.HostUUID
		if d.IdentityVerification == noderec.IdentityUnverified {
			key = d.DiscoveryRecordID
		}
		if key == "" {
			key = "name:" + d.Name
		}
		row := get(key)
		row.identityVerification = d.IdentityVerification
		row.name = d.Name
		row.address = d.IPAddress
		row.addresses = candidateAddresses(d)
		row.port = d.Port
		row.models = d.Models
		row.modelsByEngine = d.ModelsByEngine
		row.loadedByEngine = d.LoadedByEngine
		row.presence = discoveredPresence(d)
		switch {
		case d.Trusted && d.IdentityVerification != noderec.IdentityUnverified:
			row.membership = membershipMember
		case d.Clustered:
			row.membership = membershipForeign
		}
	}

	for _, m := range in.members {
		key := m.NodeUUID
		if key == "" {
			key = m.ID
		}
		row := get(key)
		if row.name == "" {
			row.name = m.Name
		}
		if row.address == "" {
			row.address = m.IPAddress
			row.port = m.Port
		}
		row.membership = memberMembership(m.State)
		// A member discovery has not reported is listed but unreachable; one it
		// did report keeps the presence computed above. Unknown is precisely
		// "no feed has graded this row", since every discovered node gets a
		// verdict and so does every probed manual entry.
		if row.presence == presenceUnknown {
			row.presence = presenceOffline
		}
	}

	for _, m := range in.manual {
		row := matchManual(byKey, m)
		if row == nil {
			row = get("manual:" + m.ID)
			row.name = m.Name
			row.address = m.Address
		}
		row.manualID = m.ID
		if row.name == "" {
			row.name = m.Address
		}
		// Where the probe actually reached node-info, for a row discovery has
		// not already given a port to.
		if row.port == 0 {
			row.port = m.NodeInfoPort
		}
		if m.TLSEnabled {
			row.nodeInfoTLS = true
			if m.IdentityVerification == noderec.IdentityPinned && m.NodeInfoUp {
				row.probedTelemetry = &nodeTelemetry{
					GPUs: m.GPUs, CPU: m.CPU, Memory: m.Memory,
					TelemetryValid: m.TelemetryValid,
				}
			}
		}
		// A probe is direct evidence and outranks discovery silence: a node on
		// a network that filters multicast is reachable but never announced.
		// Any service answering counts — a host running only LM Studio is as
		// reachable as one running Ollama.
		if m.answered() {
			row.presence = presenceOnline
		} else if row.presence == presenceUnknown {
			row.presence = presenceOffline
		}
	}

	if in.selfUUID != "" {
		if row, ok := byKey[in.selfUUID]; ok {
			row.self = true
			row.presence = presenceOnline
		}
	}

	rows := make([]nodeRow, 0, len(order))
	for _, key := range order {
		rows = append(rows, *byKey[key])
	}
	sortNodeRows(rows)
	return rows
}

// filterNodeRows narrows a node list to those matching a case-insensitive
// substring of the name or any of the node's addresses.
//
// Addresses are included because half of what an operator knows a machine by is
// its IP — especially the ones added by address in the first place, which may
// carry a name they have never seen.
func filterNodeRows(rows []nodeRow, filter string) []nodeRow {
	needle := strings.ToLower(strings.TrimSpace(filter))
	if needle == "" {
		return rows
	}
	out := make([]nodeRow, 0, len(rows))
	for _, n := range rows {
		if nodeMatchesFilter(n, needle) {
			out = append(out, n)
		}
	}
	return out
}

// nodeMatchesFilter reports whether one row matches an already-lowercased needle.
func nodeMatchesFilter(n nodeRow, needle string) bool {
	if strings.Contains(strings.ToLower(n.name), needle) {
		return true
	}
	if strings.Contains(strings.ToLower(n.address), needle) {
		return true
	}
	for _, a := range n.addresses {
		if strings.Contains(strings.ToLower(a), needle) {
			return true
		}
	}
	return false
}

// candidateAddresses is the node's addresses in the order it ranked them, with
// its primary first and duplicates removed. The broker omits the list entirely
// when a node published a single address, so that case falls back to it.
func candidateAddresses(d availableNode) []string {
	out := make([]string, 0, len(d.IPAddresses)+1)
	seen := make(map[string]bool, len(d.IPAddresses)+1)
	for _, a := range append([]string{d.IPAddress}, d.IPAddresses...) {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// matchManual joins only authenticated host identities. Configuration keys
// otherwise remain independent of names, addresses, and unverified UUID claims.
func matchManual(byKey map[string]*nodeRow, m manualNode) *nodeRow {
	if m.IdentityVerification == noderec.IdentityPinned && m.HostUUID != "" {
		return byKey[m.HostUUID]
	}
	return byKey["manual:"+m.ID]
}

// hasHostIdentity reports whether a row key is a machine's own UUID, rather
// than a stand-in for a node that has not reported one: a discovered node
// keyed by name, or a manual entry keyed by its id. mergeNodes builds both
// stand-ins; everything else it keys by UUID.
func hasHostIdentity(key string) bool {
	return !strings.HasPrefix(key, "name:") && !strings.HasPrefix(key, "manual:")
}

// normalizeHost reduces an address to a comparable host: trimmed, lowercased,
// and without a port.
func normalizeHost(address string) string {
	host := strings.TrimSpace(address)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

// discoveredPresence grades a node that appears in the broker's discovery
// snapshot: reachable if it has somewhere to be reached.
//
// Deliberately no clock. Eviction is the backend's job and it does it properly —
// roughly sixty seconds of unbroken mDNS silence, with a TCP probe to stop a
// node flapping out on one missed announcement, and inference traffic from the
// node counting as evidence too. A node that survives all that is still in the
// snapshot, and a client second-guessing it with a timer can only be wrong.
// The record's timestamp is no substitute: nvpair-node-scanner writes it only
// when a node's advertisement changes, so a healthy peer's stays at first
// discovery.
//
// This is also what the desktop app does, which matters because the two should
// not disagree about whether a machine is up. Its rule is
// `anyEngineUp(node) || node.nodeInfoUp`, where nodeInfoUp begins as
// `Boolean(ipAddress) && port > 0` from the same snapshot and goes false only
// when the broker drops the node. Its own /v1/node-info poll never demotes a
// node — a failed poll keeps the last metrics and backs off.
func discoveredPresence(n availableNode) nodePresence {
	if n.IPAddress == "" || n.Port <= 0 {
		return presenceOffline
	}
	return presenceOnline
}

// memberMembership maps a cluster roster state onto the membership shown. The
// manager reports intermediate states while a join is settling; anything that
// is not clearly established reads as pending rather than as a full member.
func memberMembership(state string) nodeMembership {
	switch strings.ToLower(state) {
	case "joined", "active", "connected", "trusted", "member":
		return membershipMember
	case "":
		return membershipMember
	default:
		return membershipPending
	}
}

// sortNodeRows orders the table: this machine first, then reachable nodes, then
// by name. Rows are keyed rather than indexed by the view, so re-ordering as
// state changes does not move the operator's selection.
func sortNodeRows(rows []nodeRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.self != b.self {
			return a.self
		}
		if (a.presence == presenceOnline) != (b.presence == presenceOnline) {
			return a.presence == presenceOnline
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
}
