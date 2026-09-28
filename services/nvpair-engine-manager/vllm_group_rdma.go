// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"regexp"
)

const vllmQwen38Transport = "host-buffer-roce"

// DGX Spark also has a historical PCI-domain-2 HCA spelling
// (roceP2p1s0f0); P2 is the PCI domain, not a function number.
var vllmGroupHCA = regexp.MustCompile(`^(mlx5_[0-9]{1,3}|roce(P2)?p[0-9]{1,3}s[0-9]{1,3}f[0-9]{1,3})$`)

type vllmGroupTransport struct {
	Mode                  string `json:"mode"`
	OperationID           string `json:"operationId"`
	QualificationSHA256   string `json:"qualificationSha256"`
	NetGDRLevel           int    `json:"netGdrLevel"`
	NetGDRC2C             int    `json:"netGdrC2c"`
	NetGDRRead            int    `json:"netGdrRead"`
	NetPlugin             string `json:"netPlugin"`
	EnvPlugin             string `json:"envPlugin"`
	GINPlugin             string `json:"ginPlugin"`
	SubnetAwareRouting    *bool  `json:"subnetAwareRouting,omitempty"`
	SubnetPrefixLength    *int   `json:"subnetPrefixLength,omitempty"`
	MergeNICs             *bool  `json:"mergeNICs,omitempty"`
	SocketPayloadFallback bool   `json:"socketPayloadFallback"`
}

type vllmGroupRDMALane struct {
	PeerNodeID     string `json:"peerNodeId"`
	LocalAddress   string `json:"localAddress"`
	PeerAddress    string `json:"peerAddress"`
	InterfaceName  string `json:"interfaceName"`
	InterfaceIndex int    `json:"interfaceIndex"`
	MAC            string `json:"mac"`
	SwitchID       string `json:"switchId"`
	PortName       string `json:"portName"`
	RDMADevice     string `json:"rdmaDevice"`
	GIDPort        int    `json:"gidPort"`
	GIDIndex       int    `json:"gidIndex"`
	GIDType        string `json:"gidType"`
}

type vllmGroupMemberFabric struct {
	Lanes []vllmGroupRDMALane `json:"lanes"`
}

func bindQwen38FabricFacts(selection vllmGroupSelection, facts []vllmGroupFacts, principals []string, operationID, qualification string, endpoints []fabricCandidateIP) error {
	endpoints = fabricLaneEndpoints(endpoints)
	if len(facts) != len(selection.NodeIDs) || len(principals) != len(selection.NodeIDs) || len(endpoints) != 2*len(selection.NodeIDs) || !onboardingID.MatchString(operationID) || !onboardingSHA.MatchString(qualification) {
		return errors.New("qualified fabric endpoint set is incomplete")
	}
	principalByNode, factByNode := map[string]string{}, map[string]int{}
	for index, nodeID := range selection.NodeIDs {
		principalByNode[nodeID], factByNode[nodeID] = principals[index], index
		facts[index].fabric = &vllmGroupMemberFabric{}
	}
	for _, endpoint := range endpoints {
		index, local := factByNode[endpoint.NodeID]
		peerPrincipal, peer := principalByNode[endpoint.PeerNodeID]
		if !local || !peer || endpoint.PeerPrincipal != peerPrincipal {
			return errors.New("qualified fabric endpoint identity differs from current paired membership")
		}
		facts[index].fabric.Lanes = append(facts[index].fabric.Lanes, vllmGroupRDMALane{PeerNodeID: endpoint.PeerNodeID, LocalAddress: endpoint.Address, PeerAddress: endpoint.PeerAddress, InterfaceName: endpoint.InterfaceName, InterfaceIndex: endpoint.InterfaceIndex, MAC: endpoint.MAC, SwitchID: endpoint.SwitchID, PortName: endpoint.PortName, RDMADevice: endpoint.RDMADevice, GIDPort: endpoint.RDMAPort, GIDIndex: endpoint.GIDIndex, GIDType: endpoint.GIDType})
	}
	subnetAwareRouting, prefixLength, mergeNICs := false, 0, true
	facts[0].fabricTransport = &vllmGroupTransport{Mode: vllmQwen38Transport, OperationID: operationID, QualificationSHA256: qualification, NetGDRLevel: 0, NetGDRC2C: 0, NetGDRRead: 0, NetPlugin: "none", EnvPlugin: "none", GINPlugin: "none", SubnetAwareRouting: &subnetAwareRouting, SubnetPrefixLength: &prefixLength, MergeNICs: &mergeNICs, SocketPayloadFallback: false}
	return nil
}

type vllmFabricQualificationOwner interface {
	requalifyFabric(context.Context, []string) (string, string, []fabricCandidateIP, error)
	qualifiedFabric([]string) (string, string, []fabricCandidateIP, error)
}

func requalifyVLLMGroupFabric(ctx context.Context, owner interface{}, nodeIDs []string) (string, string, []fabricCandidateIP, error) {
	qualified, ok := owner.(vllmFabricQualificationOwner)
	if !ok {
		return "", "", nil, errors.New("fresh fabric qualification owner is unavailable")
	}
	return qualified.requalifyFabric(ctx, nodeIDs)
}

func currentVLLMGroupTransport(owner interface{}, plan vllmGroupPlan) error {
	if !isQwen38ProfileModel(plan.Model) {
		return nil
	}
	if err := validateVLLMGroupTransport(plan); err != nil {
		return err
	}
	nodes := make([]string, 0, len(plan.Members))
	for _, member := range plan.Members {
		nodes = append(nodes, member.NodeID)
	}
	qualified, ok := owner.(vllmFabricQualificationOwner)
	if !ok {
		return errors.New("reviewed Qwen3.8 fabric qualification owner is unavailable")
	}
	operation, digest, endpoints, err := qualified.qualifiedFabric(nodes)
	if err != nil || operation != plan.Transport.OperationID || digest != plan.Transport.QualificationSHA256 || !vllmFabricEndpointsMatchPlan(plan, endpoints) {
		return errors.New("reviewed Qwen3.8 fabric qualification is no longer active")
	}
	return nil
}

func vllmFabricEndpointsMatchPlan(plan vllmGroupPlan, endpoints []fabricCandidateIP) bool {
	endpoints = fabricLaneEndpoints(endpoints)
	if len(endpoints) != len(plan.Members)*2 {
		return false
	}
	for _, member := range plan.Members {
		if member.Fabric == nil {
			return false
		}
		for _, lane := range member.Fabric.Lanes {
			found := false
			for _, endpoint := range endpoints {
				found = found || endpoint.NodeID == member.NodeID && endpoint.PeerNodeID == lane.PeerNodeID && endpoint.Address == lane.LocalAddress && endpoint.PeerAddress == lane.PeerAddress && endpoint.InterfaceName == lane.InterfaceName && endpoint.InterfaceIndex == lane.InterfaceIndex && endpoint.MAC == lane.MAC && endpoint.SwitchID == lane.SwitchID && endpoint.PortName == lane.PortName && endpoint.RDMADevice == lane.RDMADevice && endpoint.RDMAPort == lane.GIDPort && endpoint.GIDIndex == lane.GIDIndex && endpoint.GIDType == lane.GIDType
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func validateVLLMGroupTransport(plan vllmGroupPlan) error {
	if plan.Transport == nil || plan.Transport.Mode != vllmQwen38Transport || !onboardingID.MatchString(plan.Transport.OperationID) || !onboardingSHA.MatchString(plan.Transport.QualificationSHA256) || plan.Transport.NetGDRLevel != 0 || plan.Transport.NetGDRC2C != 0 || plan.Transport.NetGDRRead != 0 || plan.Transport.NetPlugin != "none" || plan.Transport.EnvPlugin != "none" || plan.Transport.GINPlugin != "none" || plan.Transport.SubnetAwareRouting == nil || *plan.Transport.SubnetAwareRouting || plan.Transport.SubnetPrefixLength == nil || *plan.Transport.SubnetPrefixLength != 0 || plan.Transport.MergeNICs == nil || !*plan.Transport.MergeNICs || plan.Transport.SocketPayloadFallback {
		return errors.New("Qwen3.8 requires one current qualified host-buffer RoCE operation with no Socket payload fallback")
	}
	type edge struct{ local, peer, localAddress, peerAddress string }
	edges := make(map[edge]bool, len(plan.Members)*2)
	const lanePrefixLength = 30
	for _, member := range plan.Members {
		if member.Fabric == nil || len(member.Fabric.Lanes) != 2 {
			return errors.New("each Qwen3.8 member requires two exact qualified RoCE lanes")
		}
		interfaces, devices := map[int]bool{}, map[string]bool{}
		gidIndex := -1
		for _, lane := range member.Fabric.Lanes {
			local, remote := net.ParseIP(lane.LocalAddress), net.ParseIP(lane.PeerAddress)
			_, macErr := net.ParseMAC(lane.MAC)
			sameLanePrefix := local != nil && remote != nil && local.Mask(net.CIDRMask(lanePrefixLength, 32)).Equal(remote.Mask(net.CIDRMask(lanePrefixLength, 32)))
			if !cableIdentifier(lane.PeerNodeID, 128) || lane.PeerNodeID == member.NodeID || local == nil || remote == nil || local.To4() == nil || remote.To4() == nil || !local.IsPrivate() || !remote.IsPrivate() || local.IsLoopback() || remote.IsLoopback() || local.Equal(remote) || !sameLanePrefix || !cableIdentifier(lane.InterfaceName, 64) || lane.InterfaceIndex <= 0 || macErr != nil || !cableIdentifier(lane.SwitchID, 128) || !cableIdentifier(lane.PortName, 128) || !vllmGroupHCA.MatchString(lane.RDMADevice) || lane.GIDPort <= 0 || lane.GIDPort > 255 || lane.GIDIndex < 0 || lane.GIDIndex > 255 || lane.GIDType != "RoCE v2" {
				return errors.New("Qwen3.8 RoCE lane identity, address, HCA or GID binding is invalid")
			}
			if interfaces[lane.InterfaceIndex] || devices[lane.RDMADevice] {
				return errors.New("Qwen3.8 member RoCE lanes must use distinct interfaces and HCAs")
			}
			if gidIndex >= 0 && gidIndex != lane.GIDIndex {
				return errors.New("Qwen3.8 member RoCE lanes require one exact common GID index")
			}
			gidIndex = lane.GIDIndex
			interfaces[lane.InterfaceIndex], devices[lane.RDMADevice] = true, true
			edges[edge{member.NodeID, lane.PeerNodeID, lane.LocalAddress, lane.PeerAddress}] = true
		}
	}
	for value := range edges {
		if !edges[edge{value.peer, value.local, value.peerAddress, value.localAddress}] {
			return errors.New("Qwen3.8 RoCE plan lacks a reciprocal qualified lane")
		}
	}
	if len(edges) != len(plan.Members)*2 {
		return errors.New("Qwen3.8 RoCE plan has duplicate or incomplete directed lanes")
	}
	return nil
}
