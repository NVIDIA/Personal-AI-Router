// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// unknownNodeLabel stands in for a node reference with no value at all.
const unknownNodeLabel = "-"

// shortNodeIDLen is how much of an unresolved UUID to show. Enough to tell two
// nodes apart and to match against a full id elsewhere, without letting one
// column eat the row.
const shortNodeIDLen = 8

// nodeNamer resolves the stable node UUIDs the backend stamps onto records into
// names an operator recognises.
//
// Several payloads identify a node by UUID rather than by name: a workload's
// originatedFrom and scheduledOn (the broker stamps its resolveLocalNodeID,
// which is nodeid.Resolve), and a service error's nodeId. Rendering those raw
// shows the operator a random-looking string. Discovery already carries the
// mapping — hostUuid alongside name — so nothing extra has to be fetched, only
// remembered.
//
// Names are retained once learned rather than dropped when a node leaves the
// discovery snapshot: a completed job outlives the reachability of the machine
// that ran it, and "the node formerly known as 3f2a…" is not an improvement.
type nodeNamer struct {
	names    map[string]string
	selfUUID string
}

func newNodeNamer() *nodeNamer {
	return &nodeNamer{names: map[string]string{}}
}

// identityCmd resolves this machine's own UUID and name, which discovery does
// not necessarily report for the local host.
func nodeIdentityCmd(client *rpc.Client, finish func(clusterIdentity, error) tea.Msg) tea.Cmd {
	return call(client, "cluster:get-node-id", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return finish(clusterIdentity{}, err)
		}
		var id clusterIdentity
		_ = decodeParams(msg.Result, &id)
		return finish(id, nil)
	})
}

func (n *nodeNamer) learnDiscovered(nodes []availableNode) {
	for _, d := range nodes {
		n.learn(d.HostUUID, d.Name)
	}
}

func (n *nodeNamer) learnMembers(nodes []clusterNode) {
	for _, m := range nodes {
		n.learn(m.NodeUUID, m.Name)
	}
}

// setSelf records this machine, so its own jobs read as a name rather than as
// the one UUID the operator is guaranteed to see most often.
func (n *nodeNamer) setSelf(id clusterIdentity) {
	n.selfUUID = id.NodeUUID
	name := id.Name
	if name == "" {
		name = id.NodeID
	}
	n.learn(id.NodeUUID, name)
}

func (n *nodeNamer) learn(uuid, name string) {
	if uuid == "" || name == "" {
		return
	}
	n.names[uuid] = name
}

// name renders a node reference for display.
func (n *nodeNamer) name(uuid string) string {
	if uuid == "" {
		return unknownNodeLabel
	}
	if known, ok := n.names[uuid]; ok {
		return known
	}
	// An unresolved id still has to be distinguishable, and truncating marks it
	// as an id rather than passing a fragment off as a name.
	return truncate(uuid, shortNodeIDLen)
}
