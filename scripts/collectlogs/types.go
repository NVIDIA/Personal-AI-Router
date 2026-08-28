// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"strings"
)

// Record mirrors the desktop LogEntry contract in
// desktop/src/shared/types/log.ts. Field order here is the on-disk field order,
// so re-marshalling a record produces a line shaped like the original.
//
// Data holds arbitrary decoded JSON. Message frequently contains a serialized
// JSON-RPC frame as a string; parseEmbedded reads through that.
type Record struct {
	Level    string `json:"level"`
	Time     string `json:"time"`
	Source   string `json:"source,omitempty"`
	Sublevel string `json:"sublevel"`
	Message  string `json:"message,omitempty"`
	Data     any    `json:"data,omitempty"`
}

// Kind classifies a discovered identifier. Classification is by value shape,
// never by the field name that carried the value: the same logical identifier
// appears under id, name, host, node_id, nodeId, uuid, hostUuid, ip, ipAddress,
// addr, target and remote, and also inside free-text log messages.
type Kind int

const (
	KindHostname Kind = iota
	KindUUID
	KindIPv4
	KindIPv6
	KindMAC
	KindUsername
	// KindPath is an installation or application root directory. A custom
	// install location can carry a name the owner chose, and the root itself is
	// not diagnostic — the tail of the path is.
	KindPath
	// KindLabel is free text the user typed, such as a cluster's friendly name.
	// No shape test can recognise it, so it is found by the key that carried it.
	KindLabel
	// KindModel is a model identifier. Left alone unless asked for, because a
	// private model name can describe what someone is working on.
	KindModel
)

func (k Kind) String() string {
	switch k {
	case KindHostname:
		return "hostname"
	case KindUUID:
		return "uuid"
	case KindIPv4:
		return "ipv4"
	case KindIPv6:
		return "ipv6"
	case KindMAC:
		return "mac"
	case KindUsername:
		return "username"
	case KindPath:
		return "path"
	case KindLabel:
		return "label"
	case KindModel:
		return "model"
	}
	return "unknown"
}

// pathLike reports kinds whose value contains path separators. Those cannot be
// matched as a plain literal: a path reaches the log at more than one escape
// depth, so after decoding the same root exists both as C:\dir and C:\\dir.
func (k Kind) pathLike() bool { return k == KindPath }

// caseInsensitive reports kinds the producing platform does not treat as
// case-sensitive, and which therefore appear in mixed case across sources.
func (k Kind) caseInsensitive() bool {
	switch k {
	case KindHostname, KindUsername, KindPath, KindLabel:
		return true
	}
	return false
}

// Entity is one discovered identifier and the token that replaces it.
type Entity struct {
	Kind  Kind   `json:"kind"`
	Value string `json:"value"`
	Token string `json:"token"`
	Count int    `json:"occurrences"`
	// Class is the diagnostic category retained in the output for addresses
	// (lan, cgnat, public, ...). Knowing an address was corp-routable rather
	// than link-local matters for routing bugs and identifies nobody.
	Class string `json:"class,omitempty"`
	// Node is the owning node label when this identifier was linked to a node.
	Node string `json:"node,omitempty"`
	// FirstSeen is the record timestamp of the first observation, used to make
	// token assignment deterministic across runs on the same input.
	FirstSeen string `json:"firstSeen,omitempty"`
}

// Node groups the identifiers that describe a single machine. Linking is what
// lets the output say node-a consistently for a hostname, its UUID, its address
// and its account instead of four unrelated-looking tokens.
type Node struct {
	Label     string
	Hostnames map[string]bool
	UUIDs     map[string]bool
	IPs       map[string]bool
	// Users holds the accounts seen in this machine's own file paths. A service
	// logs paths under the account it runs as and has no knowledge of a peer's
	// home directory, so an account found while reading one machine's log
	// belongs to that machine.
	Users     map[string]bool
	FirstSeen string
	// seq is the order this group was created in. Labels are assigned by it
	// rather than by position in the node list, because a merge removes whichever
	// group did not survive and that would otherwise shift the ordering.
	seq int
	// merged points at the group this one was folded into. Two inputs produced
	// by the same machine each create a group, and the second is merged away
	// once a shared identifier is seen. Anything holding the discarded group
	// follows this to reach the surviving one, which is what keeps an output file
	// named after its node instead of falling back to a source number.
	merged *Node
}

// resolve follows the merge chain to the group that survived.
func (n *Node) resolve() *Node {
	current := n
	for current != nil && current.merged != nil {
		current = current.merged
	}
	return current
}

func newNode() *Node {
	return &Node{
		Hostnames: map[string]bool{},
		UUIDs:     map[string]bool{},
		IPs:       map[string]bool{},
		Users:     map[string]bool{},
	}
}

func (n *Node) size() int {
	return len(n.Hostnames) + len(n.UUIDs) + len(n.IPs) + len(n.Users)
}

// classifyIP returns the diagnostic class of an address, and whether the
// address should be left in the clear. Loopback and the unspecified address
// carry no identifying information and are far more readable untouched.
func classifyIP(raw string) (class string, keepLiteral bool) {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return "", false
	}
	switch {
	case ip.IsLoopback(), ip.IsUnspecified():
		return "loopback", true
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local", true
	case ip.IsMulticast():
		return "multicast", true
	case ip.IsPrivate():
		return "lan", false
	case isCGNAT(ip):
		return "cgnat", false
	default:
		return "public", false
	}
}

var cgnatBlock = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && cgnatBlock.Contains(v4)
}
