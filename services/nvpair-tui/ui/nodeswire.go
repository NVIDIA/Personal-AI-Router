// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import "nvpair-shared/noderec"

// The wire shapes behind the Nodes tab. One machine is described by three
// different services, so their payloads are collected here and folded into the
// single nodeRow the view renders (see mergeNodes).

// availableNode mirrors the broker's discovery boundary shape (the
// discovery:get-nodes element and discovery:nodes-changed payload entry).
type availableNode struct {
	ID        string `json:"id"`
	HostUUID  string `json:"hostUuid"`
	Name      string `json:"name"`
	IPAddress string `json:"ipAddress"`
	// IPAddresses is every address the node published, in its own ranked order
	// with IPAddress first. A multi-homed node has no single address every peer
	// can reach — a direct-connect link only works from the machine on its far
	// end — so a client that dials the node must walk this list rather than
	// treating IPAddress as the only answer. Omitted when there is just one.
	IPAddresses []string `json:"ipAddresses"`
	Port        int      `json:"port"`
	// LastSeen is when the scanner last WROTE this record — not when anything
	// last confirmed the node was alive. nvpair-node-scanner is explicit that it
	// is not a liveness clock: the mDNS browser reports a node only when its
	// record changes, so a healthy peer's value freezes at first discovery, and
	// the local node's advances only when it republishes.
	//
	// Decoded because it is on the wire, and deliberately unused. Presence comes
	// from whether the backend still lists the node (see discoveredPresence),
	// never from this value's age.
	LastSeen int64 `json:"lastSeen"` // Unix seconds
	// Trusted: this node is a paired cluster peer of ours. Clustered: it belongs
	// to some cluster (advertises a cluster-uuid), whether or not we're paired
	// with it. Either one makes it non-invitable — an already-clustered peer
	// rejects a fresh pairing (it must leave/be removed first).
	Trusted   bool `json:"trusted"`
	Clustered bool `json:"clustered"`

	// The broker enriches each discovered node with the model inventory it
	// learned from that peer's engine-manager, so a remote node's models need
	// no extra call: Models is the flat de-duplicated union, ModelsByEngine
	// attributes each to the engine serving it, and LoadedByEngine names those
	// currently resident in memory.
	Models         []string            `json:"models"`
	ModelsByEngine map[string][]string `json:"modelsByEngine"`
	LoadedByEngine map[string][]string `json:"loadedByEngine"`
}

// clusterIdentity is this node's principal, from cluster:get-node-id.
type clusterIdentity struct {
	NodeUUID  string `json:"nodeUuid"`
	NodeID    string `json:"nodeId"`
	Name      string `json:"name"`
	ClusterID string `json:"clusterId"`
}

// clusterNode mirrors nvpair-cluster-manager's ClusterNode (a member or
// pending invitee), the element of nodes:get-initial / nodes:changed.
type clusterNode struct {
	ID        string `json:"id"`
	NodeUUID  string `json:"nodeUuid"`
	Name      string `json:"name"`
	IPAddress string `json:"ipAddress"`
	Port      int    `json:"port"`
	State     string `json:"state"`
}

// clusterInvite is the broker-facing view of a pairing session
// (cluster:invite-received push / cluster:invite-node result).
type clusterInvite struct {
	InviteID     string  `json:"inviteId"`
	FromNodeName string  `json:"fromNodeName"`
	Pin          *string `json:"pin"`
	State        string  `json:"state"`
}

// manualNode is the subset of nvpair-manual-nodes' ManualNodeStatus
// (services/nvpair-manual-nodes/manager.go), the element of the broker's
// nodes/list reply, that the Nodes tab uses.
type manualNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	// HostUUID is the machine's own identity, read from its node-info. It is
	// what joins a hand-typed hostname to the same machine discovered by IP.
	HostUUID   string `json:"hostUuid"`
	OllamaUp   bool   `json:"ollama_up"`
	LMStudioUp bool   `json:"lmstudio_up"`
	NodeInfoUp bool   `json:"node_info_up"`
	// NodeInfoPort is the port the probe reached node-info on, and TLSEnabled
	// whether that was over TLS.
	NodeInfoPort int  `json:"node_info_port"`
	TLSEnabled   bool `json:"tls_enabled"`
	// The worker's own hardware reading from that probe. For a TLS node it is
	// the only one to be had: the worker holds the backend's trust to reach
	// it, and this client holds none.
	GPUs           []noderec.GPUInfo   `json:"gpus"`
	CPU            *noderec.CPUInfo    `json:"cpu"`
	Memory         *noderec.MemoryInfo `json:"memory"`
	TelemetryValid bool                `json:"telemetryValid"`
}

// answered reports whether any service on the host responded to the worker's
// last probe. The worker reports one flag per service, so an engine it starts
// probing arrives as a new field here and joins this list.
func (m manualNode) answered() bool {
	return m.NodeInfoUp || m.OllamaUp || m.LMStudioUp
}
