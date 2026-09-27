// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// An ordinary three-node group binds only NCCL Socket to each member's
// advertised address on an active, qualified routed ring; a peer reaches that
// address on the cable the two share or over that cable's host route. The
// master address, VLLM_HOST_IP, Gloo, control and SSH stay on the management
// network, and no RDMA device or address family is granted.
const vllmGroupRingSocketMode = "qualified-ring-socket"

var errVLLMGroupTensorNeedsRingFabric = errors.New("three-node tensor parallel requires an active qualified routed ring between the selected Sparks")

func (m *Manager) vllmGroupRingFabric(ctx context.Context, nodeIDs []string) (string, string, []fabricCandidateIP, error) {
	if m.exec == nil || m.exec.fabric == nil {
		return "", "", nil, errNoFabric
	}
	return m.exec.fabric.ringFabricFor(ctx, nodeIDs)
}

// resolveVLLMGroupRingSocket binds the active routed ring of three ordinary
// members. Genuine absence leaves pipeline stages on the management network;
// a stale, ambiguous or unrouted ring fails closed in every mode. A model that
// cannot split three ways is refused for that reason before the ring is
// consulted.
func resolveVLLMGroupRingSocket(ctx context.Context, s vllmGroupSelection, facts []vllmGroupFacts, principals []string, ringFor func(context.Context, []string) (string, string, []fabricCandidateIP, error)) error {
	if s.Parallelism == "tensor" {
		sum := sha256.Sum256(facts[0].Config)
		tensor := vllmGroupTopology{TensorParallel: 3, PipelineParallel: 1, DataParallel: 1, ConfigSHA256: hex.EncodeToString(sum[:])}
		if _, err := vllmGroupLayerPartition(tensor, 3, facts[0].Config); err != nil {
			return err
		}
	}
	operationID, qualification, endpoints, err := ringFor(ctx, s.NodeIDs)
	switch {
	case err == nil:
		return bindVLLMGroupRingSocketFacts(s, facts, principals, operationID, qualification, endpoints)
	case errors.Is(err, errNoFabric) && s.Parallelism != "tensor":
		return nil
	case errors.Is(err, errNoFabric):
		return errVLLMGroupTensorNeedsRingFabric
	}
	return err
}

// LaneAddresses are the member's two ring cable addresses in ascending order;
// the advertised address is one of them.
type vllmGroupRingSocketMember struct {
	NodeID            string   `json:"nodeId"`
	InterfaceName     string   `json:"interfaceName"`
	InterfaceIndex    int      `json:"interfaceIndex"`
	MAC               string   `json:"mac"`
	AdvertisedAddress string   `json:"advertisedAddress"`
	LaneAddresses     []string `json:"laneAddresses"`
}

// Members are ordered like the plan members.
type vllmGroupRingSocket struct {
	Mode                string                      `json:"mode"`
	OperationID         string                      `json:"operationId"`
	QualificationSHA256 string                      `json:"qualificationSha256"`
	Members             []vllmGroupRingSocketMember `json:"members"`
}

func validateVLLMGroupRingSocket(p vllmGroupPlan) error {
	rs := p.RingSocket
	if rs == nil {
		return nil
	}
	tensor := p.Topology.TensorParallel == 3 && p.Topology.PipelineParallel == 1
	pipeline := p.Topology.TensorParallel == 1 && p.Topology.PipelineParallel == 3
	if isQwen38ProfileModel(p.Model) || p.Runtime != vllmManagedVersion || p.Transport != nil || p.DirectSocket != nil || len(p.Members) != 3 || !tensor && !pipeline || p.Topology.DataParallel != 1 {
		return errors.New("the qualified ring socket applies only to an ordinary three-node tensor- or pipeline-parallel group on the managed runtime")
	}
	if rs.Mode != vllmGroupRingSocketMode || !onboardingID.MatchString(rs.OperationID) || !onboardingSHA.MatchString(rs.QualificationSHA256) || len(rs.Members) != 3 {
		return errors.New("the qualified ring socket binding is incomplete")
	}
	management := map[netip.Addr]bool{}
	for _, member := range p.Members {
		if member.Fabric != nil {
			return errors.New("a ring socket group cannot carry RoCE lane bindings")
		}
		if member.Placement != nil {
			if address, err := netip.ParseAddr(member.Placement.Address); err == nil {
				management[address] = true
			}
		}
	}
	owners := map[netip.Addr]int{}
	for i, member := range rs.Members {
		_, macErr := net.ParseMAC(member.MAC)
		advertised, advertisedErr := vllmGroupRingAddress(member.AdvertisedAddress)
		if member.NodeID != p.Members[i].NodeID || !vllmGroupSocketInterface.MatchString(member.InterfaceName) || member.InterfaceIndex <= 0 ||
			macErr != nil || len(member.MAC) != 17 || member.MAC != strings.ToLower(member.MAC) || advertisedErr != nil || len(member.LaneAddresses) != 2 {
			return errors.New("ring socket member identity or address is invalid")
		}
		first, firstErr := vllmGroupRingAddress(member.LaneAddresses[0])
		second, secondErr := vllmGroupRingAddress(member.LaneAddresses[1])
		if firstErr != nil || secondErr != nil || !first.Less(second) || advertised != first && advertised != second {
			return errors.New("a ring socket member must advertise one of its two ordered lane addresses")
		}
		for _, lane := range []netip.Addr{first, second} {
			if _, taken := owners[lane]; taken || management[lane] {
				return errors.New("ring socket lane addresses must be distinct and off the management network")
			}
			owners[lane] = i
		}
	}
	// Six cable ends that pair into /31 edges between distinct members can only
	// form the three-edge ring.
	for lane, owner := range owners {
		end := lane.As4()
		end[3] ^= 1
		if peer, ok := owners[netip.AddrFrom4(end)]; !ok || peer == owner {
			return errors.New("ring socket lane addresses do not form the three-member ring")
		}
	}
	return nil
}

func vllmGroupRingAddress(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() || !address.IsPrivate() || address.String() != value {
		return netip.Addr{}, errors.New("ring socket address is not one canonical private IPv4 address")
	}
	return address, nil
}

func cloneVLLMGroupRingSocket(rs *vllmGroupRingSocket) *vllmGroupRingSocket {
	if rs == nil {
		return nil
	}
	value := *rs
	value.Members = append([]vllmGroupRingSocketMember(nil), rs.Members...)
	for i := range value.Members {
		value.Members[i].LaneAddresses = append([]string(nil), rs.Members[i].LaneAddresses...)
	}
	return &value
}

// A member's routed proof names its advertised address and the interface that
// carries it; its two lane endpoints are its ring cable addresses.
func vllmGroupRingSocketMemberFor(nodeID string, endpoints []fabricCandidateIP) (vllmGroupRingSocketMember, bool) {
	var routed []fabricCandidateIP
	var lanes []netip.Addr
	for _, endpoint := range endpoints {
		if endpoint.NodeID != nodeID {
			continue
		}
		if endpoint.Gateway != "" {
			routed = append(routed, endpoint)
			continue
		}
		address, err := netip.ParseAddr(endpoint.Address)
		if err != nil {
			return vllmGroupRingSocketMember{}, false
		}
		lanes = append(lanes, address)
	}
	if len(routed) != 1 || len(lanes) != 2 {
		return vllmGroupRingSocketMember{}, false
	}
	slices.SortFunc(lanes, netip.Addr.Compare)
	carrier := routed[0]
	return vllmGroupRingSocketMember{NodeID: nodeID, InterfaceName: carrier.InterfaceName, InterfaceIndex: carrier.InterfaceIndex,
		MAC: strings.ToLower(carrier.MAC), AdvertisedAddress: carrier.Address, LaneAddresses: []string{lanes[0].String(), lanes[1].String()}}, true
}

func bindVLLMGroupRingSocketFacts(selection vllmGroupSelection, facts []vllmGroupFacts, principals []string, operationID, qualification string, endpoints []fabricCandidateIP) error {
	if len(selection.NodeIDs) != 3 || len(facts) != 3 || len(principals) != 3 || len(endpoints) != 9 || !onboardingID.MatchString(operationID) || !onboardingSHA.MatchString(qualification) {
		return errors.New("qualified ring fabric endpoint set is incomplete")
	}
	principalByNode := map[string]string{}
	for rank, nodeID := range selection.NodeIDs {
		principalByNode[nodeID] = principals[rank]
	}
	for _, endpoint := range endpoints {
		_, local := principalByNode[endpoint.NodeID]
		peer, known := principalByNode[endpoint.PeerNodeID]
		if !local || !known || endpoint.NodeID == endpoint.PeerNodeID || endpoint.PeerPrincipal != peer {
			return errors.New("qualified ring fabric endpoint identity differs from current paired membership")
		}
	}
	ring := &vllmGroupRingSocket{Mode: vllmGroupRingSocketMode, OperationID: operationID, QualificationSHA256: qualification}
	for _, nodeID := range selection.NodeIDs {
		member, ok := vllmGroupRingSocketMemberFor(nodeID, endpoints)
		if !ok {
			return errors.New("qualified ring fabric does not advertise one routed address per member")
		}
		ring.Members = append(ring.Members, member)
	}
	if !vllmGroupRingSocketQualified(ring, endpoints) {
		return errors.New("qualified ring fabric does not reach every member's advertised address")
	}
	facts[0].ringSocket = ring
	return nil
}

// The qualified endpoints must still carry each member's advertised address on
// its bound interface, keep its lane addresses, and reach every peer's
// advertised address: on the cable the two share or over its host route.
func vllmGroupRingSocketQualified(rs *vllmGroupRingSocket, endpoints []fabricCandidateIP) bool {
	if len(endpoints) != 9 || len(rs.Members) != 3 {
		return false
	}
	for _, member := range rs.Members {
		var lanes []string
		routed := 0
		for _, endpoint := range endpoints {
			if endpoint.NodeID != member.NodeID {
				continue
			}
			carrier := endpoint.Address == member.AdvertisedAddress
			if carrier && (endpoint.InterfaceName != member.InterfaceName || endpoint.InterfaceIndex != member.InterfaceIndex || !strings.EqualFold(endpoint.MAC, member.MAC)) {
				return false
			}
			switch {
			case endpoint.Gateway == "":
				lanes = append(lanes, endpoint.Address)
			case carrier:
				routed++
			default:
				return false
			}
		}
		if routed != 1 || len(lanes) != 2 || lanes[0] == lanes[1] || !slices.Contains(member.LaneAddresses, lanes[0]) || !slices.Contains(member.LaneAddresses, lanes[1]) {
			return false
		}
		for _, peer := range rs.Members {
			reached := peer.NodeID == member.NodeID
			for _, endpoint := range endpoints {
				reached = reached || endpoint.NodeID == member.NodeID && endpoint.PeerNodeID == peer.NodeID && endpoint.PeerAddress == peer.AdvertisedAddress
			}
			if !reached {
				return false
			}
		}
	}
	return true
}

// A ring socket plan is usable only while its exact reviewed ring remains
// active and freshly qualified with the same advertised interfaces.
func currentVLLMGroupRingSocket(owner interface{}, plan vllmGroupPlan) error {
	rs := plan.RingSocket
	if rs == nil {
		return nil
	}
	if err := validateVLLMGroupRingSocket(plan); err != nil {
		return err
	}
	qualified, ok := owner.(vllmFabricQualificationOwner)
	if !ok {
		return errors.New("reviewed ring fabric owner is unavailable")
	}
	operation, digest, endpoints, err := qualified.qualifiedFabric([]string{plan.Members[0].NodeID, plan.Members[1].NodeID, plan.Members[2].NodeID})
	if err != nil || operation != rs.OperationID || digest != rs.QualificationSHA256 || !vllmGroupRingSocketQualified(rs, endpoints) {
		return errors.New("reviewed ring fabric qualification is no longer active")
	}
	return nil
}

// A peer reaches this rank's advertised address from whichever of its own lane
// addresses faces this rank, so the rank admits every ring address.
func vllmRankRingSocketFor(rs *vllmGroupRingSocket, rank int) *vllmRankRingSocket {
	member := rs.Members[rank]
	ring := make([]string, 0, 2*len(rs.Members))
	for _, peer := range rs.Members {
		ring = append(ring, peer.LaneAddresses...)
	}
	return &vllmRankRingSocket{Mode: rs.Mode, OperationID: rs.OperationID, QualificationSHA256: rs.QualificationSHA256,
		InterfaceName: member.InterfaceName, InterfaceIndex: member.InterfaceIndex, MAC: member.MAC,
		AdvertisedAddress: member.AdvertisedAddress, RingAddresses: ring}
}
