// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"nvpair-shared/cableprobe"
)

// fabricInventorySnapshot is the bounded, read-only projection the setup UI
// needs before it can name physical ports. It deliberately carries no routes,
// addresses, native paths, commands, credentials, or mutation authority.
type fabricInventorySnapshot struct {
	Nodes []fabricNodeObservation `json:"nodes"`
}

type fabricNodeObservation struct {
	NodeID     string                  `json:"nodeId"`
	Status     string                  `json:"status"`
	ObservedAt int64                   `json:"observedAt,omitempty"`
	Ports      []fabricPortObservation `json:"ports"`
	Reason     string                  `json:"reason,omitempty"`
}

type fabricPortObservation struct {
	SwitchID string `json:"switchId"`
	PortName string `json:"portName"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

func (s *fabricService) topologyInventory(ctx context.Context, nodeIDs []string) (fabricInventorySnapshot, error) {
	if len(nodeIDs) < 1 || len(nodeIDs) > 3 {
		return fabricInventorySnapshot{}, errors.New("select one to three fabric inventory nodes")
	}
	seen := make(map[string]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if !cableIdentifier(nodeID, 128) || seen[nodeID] {
			return fabricInventorySnapshot{}, errors.New("invalid fabric inventory node selection")
		}
		seen[nodeID] = true
	}

	out := fabricInventorySnapshot{Nodes: make([]fabricNodeObservation, 0, len(nodeIDs))}
	for _, nodeID := range nodeIDs {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		facts, err := s.readInventory(readCtx, nodeID)
		cancel()
		if err != nil {
			out.Nodes = append(out.Nodes, fabricNodeObservation{
				NodeID: nodeID,
				Status: "unavailable",
				Ports:  []fabricPortObservation{},
				Reason: "Current paired node-info fabric inventory is unavailable.",
			})
			continue
		}
		out.Nodes = append(out.Nodes, summarizeFabricInventory(facts))
	}
	return out, nil
}

func summarizeFabricInventory(facts fabricInventory) fabricNodeObservation {
	out := fabricNodeObservation{
		NodeID:     facts.NodeID,
		Status:     "observed",
		ObservedAt: facts.ObservedAt,
		Ports:      []fabricPortObservation{},
	}
	refs := map[cableprobe.PortRef]bool{}
	for _, iface := range facts.Interfaces {
		port := iface.PhysicalPort
		if port.SwitchID == "" || port.PortName == "" {
			continue
		}
		refs[cableprobe.PortRef{NodeID: facts.NodeID, SwitchID: port.SwitchID, PortName: port.PortName}] = true
	}
	for ref := range refs {
		_, err := fabricSelectedTarget(facts, ref)
		port := fabricPortObservation{SwitchID: ref.SwitchID, PortName: ref.PortName, Eligible: err == nil}
		if err != nil {
			port.Reason = "Port does not meet the active 200-Gbit ConnectX fabric baseline."
		}
		out.Ports = append(out.Ports, port)
	}
	sort.Slice(out.Ports, func(i, j int) bool {
		if out.Ports[i].PortName != out.Ports[j].PortName {
			return out.Ports[i].PortName < out.Ports[j].PortName
		}
		return out.Ports[i].SwitchID < out.Ports[j].SwitchID
	})
	return out
}
