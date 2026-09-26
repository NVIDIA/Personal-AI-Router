// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"regexp"
	"sort"
	"strings"
)

// An ordinary two-node tensor-parallel group binds only NCCL Socket to one
// reciprocal lane of an active, qualified direct fabric. The master address,
// VLLM_HOST_IP, Gloo, control and SSH stay on the management network, and no
// RDMA device or address family is granted.
const vllmGroupDirectSocketMode = "qualified-direct-socket"

var vllmGroupSocketInterface = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// Ordinary group ranks on the managed runtime bound both the NCCL and the Gloo
// process-group operations, matching the distributed readiness bound.
const vllmGroupDistributedTimeoutSeconds = 180

var errVLLMGroupTensorNeedsDirectFabric = errors.New("two-node tensor parallel requires an active qualified direct fabric between the selected Sparks")

func (m *Manager) vllmGroupDirectFabric(ctx context.Context, nodeIDs []string) (string, string, []fabricCandidateIP, error) {
	if m.exec == nil || m.exec.fabric == nil {
		return "", "", nil, errNoDirectFabric
	}
	return m.exec.fabric.directFabricFor(ctx, nodeIDs)
}

type vllmGroupDirectSocketLane struct {
	NodeID         string `json:"nodeId"`
	InterfaceName  string `json:"interfaceName"`
	InterfaceIndex int    `json:"interfaceIndex"`
	MAC            string `json:"mac"`
	LocalAddress   string `json:"localAddress"`
	PeerAddress    string `json:"peerAddress"`
}

// Lanes are ordered like the plan members; each names that member's end.
type vllmGroupDirectSocket struct {
	Mode                string                      `json:"mode"`
	OperationID         string                      `json:"operationId"`
	QualificationSHA256 string                      `json:"qualificationSha256"`
	Lanes               []vllmGroupDirectSocketLane `json:"lanes"`
}

func validateVLLMGroupDirectSocket(p vllmGroupPlan) error {
	ds := p.DirectSocket
	if ds == nil {
		return nil
	}
	if isQwen38ProfileModel(p.Model) || p.Runtime != vllmManagedVersion || p.Transport != nil || len(p.Members) != 2 || p.Topology.TensorParallel != 2 || p.Topology.PipelineParallel != 1 || p.Topology.DataParallel != 1 {
		return errors.New("the qualified direct socket applies only to an ordinary two-node tensor-parallel group")
	}
	if ds.Mode != vllmGroupDirectSocketMode || !onboardingID.MatchString(ds.OperationID) || !onboardingSHA.MatchString(ds.QualificationSHA256) || len(ds.Lanes) != 2 {
		return errors.New("the qualified direct socket binding is incomplete")
	}
	management := map[string]bool{}
	for _, member := range p.Members {
		if member.Fabric != nil {
			return errors.New("a direct socket group cannot carry RoCE lane bindings")
		}
		if member.Placement != nil {
			management[member.Placement.Address] = true
		}
	}
	for i, lane := range ds.Lanes {
		local, peer := net.ParseIP(lane.LocalAddress), net.ParseIP(lane.PeerAddress)
		_, macErr := net.ParseMAC(lane.MAC)
		if lane.NodeID != p.Members[i].NodeID || !vllmGroupSocketInterface.MatchString(lane.InterfaceName) || lane.InterfaceIndex <= 0 || macErr != nil || len(lane.MAC) != 17 || lane.MAC != strings.ToLower(lane.MAC) ||
			local == nil || peer == nil || local.To4() == nil || peer.To4() == nil || !local.IsPrivate() || !peer.IsPrivate() ||
			local.IsLoopback() || peer.IsLoopback() || local.Equal(peer) || lane.LocalAddress != local.String() || lane.PeerAddress != peer.String() ||
			management[lane.LocalAddress] || management[lane.PeerAddress] {
			return errors.New("direct socket lane identity or address is invalid")
		}
	}
	if ds.Lanes[0].LocalAddress != ds.Lanes[1].PeerAddress || ds.Lanes[1].LocalAddress != ds.Lanes[0].PeerAddress {
		return errors.New("direct socket lanes are not one reciprocal pair")
	}
	return nil
}

func cloneVLLMGroupDirectSocket(ds *vllmGroupDirectSocket) *vllmGroupDirectSocket {
	if ds == nil {
		return nil
	}
	value := *ds
	value.Lanes = append([]vllmGroupDirectSocketLane(nil), ds.Lanes...)
	return &value
}

func vllmGroupDirectSocketLaneFrom(endpoint fabricCandidateIP) vllmGroupDirectSocketLane {
	return vllmGroupDirectSocketLane{NodeID: endpoint.NodeID, InterfaceName: endpoint.InterfaceName, InterfaceIndex: endpoint.InterfaceIndex,
		MAC: strings.ToLower(endpoint.MAC), LocalAddress: endpoint.Address, PeerAddress: endpoint.PeerAddress}
}

// The lane is chosen deterministically: the coordinator endpoint with the
// lowest interface index, paired with the peer endpoint facing it.
func bindVLLMGroupDirectSocketFacts(selection vllmGroupSelection, facts []vllmGroupFacts, principals []string, operationID, qualification string, endpoints []fabricCandidateIP) error {
	if len(selection.NodeIDs) != 2 || len(facts) != 2 || len(principals) != 2 || len(endpoints) != 4 || !onboardingID.MatchString(operationID) || !onboardingSHA.MatchString(qualification) {
		return errors.New("qualified direct fabric endpoint set is incomplete")
	}
	coordinator, peer := selection.NodeIDs[0], selection.NodeIDs[1]
	var local []fabricCandidateIP
	for _, endpoint := range endpoints {
		switch {
		case endpoint.NodeID == coordinator && endpoint.PeerNodeID == peer && endpoint.PeerPrincipal == principals[1]:
			local = append(local, endpoint)
		case endpoint.NodeID == peer && endpoint.PeerNodeID == coordinator && endpoint.PeerPrincipal == principals[0]:
		default:
			return errors.New("qualified direct fabric endpoint identity differs from current paired membership")
		}
	}
	sort.Slice(local, func(i, j int) bool { return local[i].InterfaceIndex < local[j].InterfaceIndex })
	for _, near := range local {
		for _, far := range endpoints {
			if far.NodeID == peer && far.Address == near.PeerAddress && far.PeerAddress == near.Address {
				facts[0].directSocket = &vllmGroupDirectSocket{Mode: vllmGroupDirectSocketMode, OperationID: operationID, QualificationSHA256: qualification,
					Lanes: []vllmGroupDirectSocketLane{vllmGroupDirectSocketLaneFrom(near), vllmGroupDirectSocketLaneFrom(far)}}
				return nil
			}
		}
	}
	return errors.New("qualified direct fabric has no reciprocal lane between the selected members")
}

// A direct socket plan is usable only while its exact reviewed fabric remains
// active and freshly qualified with the same reciprocal lane.
func currentVLLMGroupDirectSocket(owner interface{}, plan vllmGroupPlan) error {
	ds := plan.DirectSocket
	if ds == nil {
		return nil
	}
	if err := validateVLLMGroupDirectSocket(plan); err != nil {
		return err
	}
	qualified, ok := owner.(vllmFabricQualificationOwner)
	if !ok {
		return errors.New("reviewed direct fabric owner is unavailable")
	}
	operation, digest, endpoints, err := qualified.qualifiedFabric([]string{plan.Members[0].NodeID, plan.Members[1].NodeID})
	if err != nil || operation != ds.OperationID || digest != ds.QualificationSHA256 || !vllmGroupDirectSocketLanesQualified(ds, endpoints) {
		return errors.New("reviewed direct fabric qualification is no longer active")
	}
	return nil
}

func vllmGroupDirectSocketLanesQualified(ds *vllmGroupDirectSocket, endpoints []fabricCandidateIP) bool {
	for _, lane := range ds.Lanes {
		found := false
		for _, endpoint := range endpoints {
			found = found || endpoint.NodeID == lane.NodeID && endpoint.Address == lane.LocalAddress && endpoint.PeerAddress == lane.PeerAddress &&
				endpoint.InterfaceName == lane.InterfaceName && endpoint.InterfaceIndex == lane.InterfaceIndex && strings.EqualFold(endpoint.MAC, lane.MAC)
		}
		if !found {
			return false
		}
	}
	return true
}

func vllmGroupFabricLease(run vllmGroupRun) fabricConsumerLease {
	return fabricConsumerLease{Owner: fabricLeaseOwnerServingGroup, RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest}
}
