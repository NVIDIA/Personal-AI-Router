// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"nvpair-shared/cableprobe"
)

const fabricRingReceiptFreshness = 120 * time.Second

func fabricPortSet(selection cableprobe.ReviewRequest) map[cableprobe.PortRef]bool {
	ports := make(map[cableprobe.PortRef]bool, len(selection.Ports))
	for _, port := range selection.Ports {
		ports[port] = true
	}
	return ports
}

func fabricRunSelection(run cableprobe.Run) cableprobe.ReviewRequest {
	selection := cableprobe.ReviewRequest{NodeIDs: []string{}, Ports: []cableprobe.PortRef{}}
	for _, target := range run.Targets {
		selection.NodeIDs = append(selection.NodeIDs, target.NodeID)
		for _, port := range target.Ports {
			selection.Ports = append(selection.Ports, cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName})
		}
	}
	return selection
}

func fabricSameSelection(left, right cableprobe.ReviewRequest) bool {
	if len(left.NodeIDs) != len(right.NodeIDs) || len(left.Ports) != len(right.Ports) {
		return false
	}
	leftNodes, rightNodes := slices.Clone(left.NodeIDs), slices.Clone(right.NodeIDs)
	sort.Strings(leftNodes)
	sort.Strings(rightNodes)
	if !slices.Equal(leftNodes, rightNodes) {
		return false
	}
	lp, rp := fabricPortSet(left), fabricPortSet(right)
	if len(lp) != len(rp) {
		return false
	}
	for port := range lp {
		if !rp[port] {
			return false
		}
	}
	return true
}

func fabricRingReceiptValid(run cableprobe.Run, selection cableprobe.ReviewRequest, now time.Time) bool {
	if run.State != "completed" || run.Result != "reciprocal-observations" || !run.CleanupConfirmed || run.Topology == nil || run.Topology.Layout != "ring" || run.Topology.Status != "matched" || len(run.Targets) != 3 || len(run.Edges) != 3 || run.FinishedAt < run.StartedAt {
		return false
	}
	age := now.UnixMilli() - run.FinishedAt
	if age < 0 || age > fabricRingReceiptFreshness.Milliseconds() || !fabricSameSelection(fabricRunSelection(run), selection) {
		return false
	}
	if run.Diagnostics == nil || run.Diagnostics.Failure != nil || len(run.Diagnostics.Participants) != 3 {
		return false
	}
	for _, participant := range run.Diagnostics.Participants {
		if participant.Phase != "completed" || participant.Code != "" || participant.WorkerState != "completed" || !participant.CleanupConfirmed || participant.FinalValidationCode != "" {
			return false
		}
	}
	return true
}

func cableEdgeOther(edges []cableprobe.Edge, local cableprobe.PortRef, requiredPeerPort string) (cableprobe.PortRef, bool) {
	for _, edge := range edges {
		if edge.Left == local && edge.Right.PortName == requiredPeerPort {
			return edge.Right, true
		}
		if edge.Right == local && edge.Left.PortName == requiredPeerPort {
			return edge.Left, true
		}
	}
	return cableprobe.PortRef{}, false
}

func cableHasEdge(edges []cableprobe.Edge, left, right cableprobe.PortRef) bool {
	for _, edge := range edges {
		if edge.Left == left && edge.Right == right || edge.Left == right && edge.Right == left {
			return true
		}
	}
	return false
}

// fabricRingRoles binds A to the current Pair owner. The fresh cable receipt
// then determines B and C from the exact p0/p1 partner pattern.
func fabricRingRoles(run cableprobe.Run) ([]string, bool) {
	ports := fabricPortSet(fabricRunSelection(run))
	find := func(nodeID, portName string) (cableprobe.PortRef, bool) {
		for port := range ports {
			if port.NodeID == nodeID && port.PortName == portName {
				return port, true
			}
		}
		return cableprobe.PortRef{}, false
	}
	a0, ok0 := find(run.OwnerNodeID, "p0")
	a1, ok1 := find(run.OwnerNodeID, "p1")
	if !ok0 || !ok1 {
		return nil, false
	}
	b1, okB := cableEdgeOther(run.Edges, a0, "p1")
	c0, okC := cableEdgeOther(run.Edges, a1, "p0")
	if !okB || !okC || b1.NodeID == c0.NodeID || b1.NodeID == run.OwnerNodeID || c0.NodeID == run.OwnerNodeID {
		return nil, false
	}
	b0, okB0 := find(b1.NodeID, "p0")
	c1, okC1 := find(c0.NodeID, "p1")
	if !okB0 || !okC1 || !cableHasEdge(run.Edges, b0, c1) {
		return nil, false
	}
	return []string{run.OwnerNodeID, b1.NodeID, c0.NodeID}, true
}

func (s *cableProductService) freshRingReceipt(selection cableprobe.ReviewRequest) (cableprobe.Run, []string, error) {
	if len(selection.NodeIDs) != 3 || len(selection.Ports) != 6 || !validateCableSelection(selection) {
		return cableprobe.Run{}, nil, errors.New("select exactly three nodes and p0 plus p1 on each node")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var selected cableprobe.Run
	var roles []string
	for _, record := range s.runs {
		run := record.Public
		if !fabricRingReceiptValid(run, selection, now) {
			continue
		}
		candidateRoles, ok := fabricRingRoles(run)
		if !ok {
			continue
		}
		if selected.RunID != "" && run.FinishedAt == selected.FinishedAt {
			return cableprobe.Run{}, nil, errors.New("fresh cable receipt selection is ambiguous")
		}
		if selected.RunID == "" || run.FinishedAt > selected.FinishedAt {
			selected, roles = cloneCableRun(run), candidateRoles
		}
	}
	if selected.RunID == "" {
		return cableprobe.Run{}, nil, errors.New("a fresh completed reciprocal ring cable receipt is required")
	}
	return selected, roles, nil
}

func fabricRingReceiptMatchesInventory(run cableprobe.Run, target fabricTarget, facts fabricInventory) bool {
	var receipt cableprobe.Target
	found := false
	for _, candidate := range run.Targets {
		if candidate.NodeID == target.NodeID {
			receipt, found = candidate, true
			break
		}
	}
	if !found || receipt.Principal != target.Principal || len(receipt.Ports) != 2 {
		return false
	}
	for _, port := range receipt.Ports {
		ref := cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName}
		current, err := fabricSelectedTarget(facts, ref)
		if err != nil || len(port.Interfaces) != len(current.Interfaces) {
			return false
		}
		for _, iface := range current.Interfaces {
			matched := false
			for _, observed := range port.Interfaces {
				matched = matched || observed.Index == iface.Index && observed.Name == iface.Name && strings.EqualFold(observed.MAC, iface.MAC)
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

func fabricInterfaceAt(target fabricTarget, portName string) (fabricInterface, bool) {
	for _, iface := range target.Interfaces {
		if iface.PhysicalPort.PortName == portName {
			return iface, true
		}
	}
	return fabricInterface{}, false
}

type fabricRingSide struct {
	role int
	port string
}

// The sealed ring cabling: each member's p0 meets the next member's p1.
var fabricRingCables = [3][2]fabricRingSide{{{0, "p0"}, {1, "p1"}}, {{0, "p1"}, {2, "p0"}}, {{1, "p0"}, {2, "p1"}}}

// fabricRingRouted returns the targets with each member's advertised p0
// address and the host routes that reach every advertised address over the
// cable two members share. A peer advertised on the shared cable needs none.
func fabricRingRouted(targets []fabricTarget) ([]fabricTarget, error) {
	if len(targets) != 3 {
		return nil, errors.New("three ordered ring targets required")
	}
	routed := make([]fabricTarget, len(targets))
	for role, target := range targets {
		p0, ok := fabricInterfaceAt(target, "p0")
		prefix, err := netip.ParsePrefix(p0.Address)
		if !ok || err != nil || prefix.Bits() != 31 {
			return nil, errors.New("ring member has no reviewed p0 address to advertise")
		}
		routed[role] = target
		routed[role].AdvertisedAddress = prefix.Addr().String()
		routed[role].Interfaces = slices.Clone(target.Interfaces)
		for i := range routed[role].Interfaces {
			routed[role].Interfaces[i].Routes = nil
		}
	}
	for _, cable := range fabricRingCables {
		for _, end := range [][2]fabricRingSide{{cable[0], cable[1]}, {cable[1], cable[0]}} {
			local, peer := end[0], end[1]
			localIface, okLocal := fabricInterfaceAt(targets[local.role], local.port)
			peerIface, okPeer := fabricInterfaceAt(targets[peer.role], peer.port)
			lp, localErr := netip.ParsePrefix(localIface.Address)
			pp, peerErr := netip.ParsePrefix(peerIface.Address)
			if !okLocal || !okPeer || localErr != nil || peerErr != nil || lp.Bits() != 31 || pp.Bits() != 31 || lp.Masked() != pp.Masked() {
				return nil, errors.New("ring endpoint addresses do not share the sealed /31 edge")
			}
			if pp.Addr().String() == routed[peer.role].AdvertisedAddress {
				continue
			}
			for i := range routed[local.role].Interfaces {
				iface := &routed[local.role].Interfaces[i]
				if iface.PhysicalPort.PortName == local.port {
					iface.Routes = append(iface.Routes, fabricRoute{Destination: routed[peer.role].AdvertisedAddress + "/32", Gateway: pp.Addr().String()})
				}
			}
		}
	}
	return routed, nil
}

// A reviewed host route is one /32 private destination outside the p0
// interface's own /31, reached via the other address on that /31.
func validFabricInterfaceRoutes(iface fabricInterface) bool {
	if len(iface.Routes) == 0 {
		return true
	}
	prefix, err := fabricNativePrefix(iface)
	if err != nil || len(iface.Routes) != 1 || prefix.Bits() != 31 || iface.PhysicalPort.PortName != "p0" {
		return false
	}
	route := iface.Routes[0]
	destination, destinationErr := netip.ParsePrefix(route.Destination)
	gateway, gatewayErr := netip.ParseAddr(route.Gateway)
	return destinationErr == nil && gatewayErr == nil && destination.String() == route.Destination && gateway.String() == route.Gateway &&
		destination.Bits() == 32 && destination.Addr().Is4() && destination.Addr().IsPrivate() && !prefix.Contains(destination.Addr()) &&
		prefix.Contains(gateway) && gateway != prefix.Addr()
}

// The recipe-independent shape a native worker accepts: at most one routed
// interface, whose own address is the target's advertised address.
func validFabricTargetRoutes(target fabricTarget) bool {
	advertised := ""
	for _, iface := range target.Interfaces {
		if !validFabricInterfaceRoutes(iface) {
			return false
		}
		if len(iface.Routes) != 0 {
			prefix, err := netip.ParsePrefix(iface.Address)
			if advertised != "" || err != nil {
				return false
			}
			advertised = prefix.Addr().String()
		}
	}
	return target.AdvertisedAddress == advertised
}

// Only the routed ring recipe carries advertised addresses and host routes,
// and exactly the ones its reviewed addresses and cabling imply.
func fabricRecipeTargetsValid(recipeID string, targets []fabricTarget) bool {
	if recipeID != fabricRingRecipe {
		for _, target := range targets {
			if target.AdvertisedAddress != "" {
				return false
			}
			for _, iface := range target.Interfaces {
				if len(iface.Routes) != 0 {
					return false
				}
			}
		}
		return true
	}
	expected, err := fabricRingRouted(targets)
	if err != nil {
		return false
	}
	for role, target := range targets {
		if target.AdvertisedAddress != expected[role].AdvertisedAddress {
			return false
		}
		for i, iface := range target.Interfaces {
			if !slices.Equal(iface.Routes, expected[role].Interfaces[i].Routes) {
				return false
			}
		}
	}
	return true
}

func fabricRingCandidates(targets []fabricTarget) ([]fabricCandidateIP, error) {
	if len(targets) != 3 {
		return nil, errors.New("three ordered ring targets required")
	}
	result := make([]fabricCandidateIP, 0, 6)
	for _, edge := range fabricRingCables {
		left, okLeft := fabricInterfaceAt(targets[edge[0].role], edge[0].port)
		right, okRight := fabricInterfaceAt(targets[edge[1].role], edge[1].port)
		if !okLeft || !okRight || len(left.RDMADevices) != 1 || len(right.RDMADevices) != 1 {
			return nil, errors.New("ring endpoint identity is incomplete")
		}
		lp, leftErr := netip.ParsePrefix(left.Address)
		rp, rightErr := netip.ParsePrefix(right.Address)
		if leftErr != nil || rightErr != nil || lp.Bits() != 31 || rp.Bits() != 31 || lp.Masked() != rp.Masked() {
			return nil, errors.New("ring endpoint addresses do not share the sealed /31 edge")
		}
		appendDirection := func(localTarget, peerTarget fabricTarget, local, peer fabricInterface) {
			localPrefix, _ := netip.ParsePrefix(local.Address)
			peerPrefix, _ := netip.ParsePrefix(peer.Address)
			result = append(result, fabricCandidateIP{NodeID: localTarget.NodeID, PeerNodeID: peerTarget.NodeID, PeerPrincipal: peerTarget.Principal, Address: localPrefix.Addr().String(), PeerAddress: peerPrefix.Addr().String(), InterfaceName: local.Name, InterfaceIndex: local.Index, MAC: local.MAC, SwitchID: local.PhysicalPort.SwitchID, PortName: local.PhysicalPort.PortName, RDMADevice: local.RDMADevices[0]})
		}
		appendDirection(targets[edge[0].role], targets[edge[1].role], left, right)
		appendDirection(targets[edge[1].role], targets[edge[0].role], right, left)
	}
	return result, nil
}

// One routed proof per host route: the member reaches that peer's advertised
// address from its own address on the routed interface.
func fabricRingRoutedCandidates(targets []fabricTarget) ([]fabricCandidateIP, error) {
	result := make([]fabricCandidateIP, 0, len(targets))
	for _, local := range targets {
		for _, iface := range local.Interfaces {
			for _, route := range iface.Routes {
				prefix, err := netip.ParsePrefix(iface.Address)
				destination, destinationErr := netip.ParsePrefix(route.Destination)
				var peer fabricTarget
				peers := 0
				for _, candidate := range targets {
					if destinationErr == nil && candidate.NodeID != local.NodeID && candidate.AdvertisedAddress == destination.Addr().String() {
						peer, peers = candidate, peers+1
					}
				}
				if err != nil || peers != 1 {
					return nil, errors.New("routed ring proof has no single advertised peer")
				}
				result = append(result, fabricCandidateIP{NodeID: local.NodeID, PeerNodeID: peer.NodeID, PeerPrincipal: peer.Principal, Address: prefix.Addr().String(), PeerAddress: destination.Addr().String(), InterfaceName: iface.Name, InterfaceIndex: iface.Index, MAC: iface.MAC, SwitchID: iface.PhysicalPort.SwitchID, PortName: iface.PhysicalPort.PortName, Gateway: route.Gateway})
			}
		}
	}
	return result, nil
}

// Routed proofs reach an advertised address through a peer and never bind
// RDMA, so lane consumers see only the direct cable endpoints.
func fabricLaneEndpoints(endpoints []fabricCandidateIP) []fabricCandidateIP {
	lanes := make([]fabricCandidateIP, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.Gateway == "" {
			lanes = append(lanes, endpoint)
		}
	}
	return lanes
}

func fabricTwoNodeCandidates(targets []fabricTarget) ([]fabricCandidateIP, error) {
	if len(targets) != 2 || len(targets[0].Interfaces) != 2 || len(targets[1].Interfaces) != 2 {
		return nil, errors.New("two complete fabric targets required")
	}
	result := make([]fabricCandidateIP, 0, 4)
	for lane := 0; lane < 2; lane++ {
		left, right := targets[0].Interfaces[lane], targets[1].Interfaces[lane]
		if len(left.RDMADevices) != 1 || len(right.RDMADevices) != 1 {
			return nil, errors.New("two-node endpoint identity is incomplete")
		}
		lp, leftErr := netip.ParsePrefix(left.Address)
		rp, rightErr := netip.ParsePrefix(right.Address)
		if leftErr != nil || rightErr != nil || lp.Bits() != 30 || rp.Bits() != 30 || lp.Masked() != rp.Masked() {
			return nil, errors.New("two-node endpoints do not share the reviewed /30 lane")
		}
		appendDirection := func(localTarget, peerTarget fabricTarget, local, peer fabricInterface) {
			localPrefix, _ := netip.ParsePrefix(local.Address)
			peerPrefix, _ := netip.ParsePrefix(peer.Address)
			result = append(result, fabricCandidateIP{NodeID: localTarget.NodeID, PeerNodeID: peerTarget.NodeID, PeerPrincipal: peerTarget.Principal, Address: localPrefix.Addr().String(), PeerAddress: peerPrefix.Addr().String(), InterfaceName: local.Name, InterfaceIndex: local.Index, MAC: local.MAC, SwitchID: local.PhysicalPort.SwitchID, PortName: local.PhysicalPort.PortName, RDMADevice: local.RDMADevices[0]})
		}
		appendDirection(targets[0], targets[1], left, right)
		appendDirection(targets[1], targets[0], right, left)
	}
	return result, nil
}

func fabricCandidates(recipeID string, targets []fabricTarget) ([]fabricCandidateIP, error) {
	if !fabricRecipeTargetsValid(recipeID, targets) {
		return nil, errors.New("fabric targets do not match their reviewed recipe")
	}
	switch recipeID {
	case fabricRecipe:
		return fabricTwoNodeCandidates(targets)
	case fabricRingRetainedRecipe:
		return fabricRingCandidates(targets)
	case fabricRingRecipe:
		lanes, err := fabricRingCandidates(targets)
		if err != nil {
			return nil, err
		}
		routed, err := fabricRingRoutedCandidates(targets)
		if err != nil {
			return nil, err
		}
		return append(lanes, routed...), nil
	}
	return nil, errors.New("fabric recipe has no qualification contract")
}

func fabricCandidatesQualified(base, qualified []fabricCandidateIP) bool {
	if len(base) != len(qualified) {
		return false
	}
	for i := range base {
		candidate := qualified[i]
		if candidate.Gateway != "" {
			if candidate != base[i] {
				return false
			}
			continue
		}
		if candidate.RDMAPort < 1 || candidate.RDMAPort > 255 || candidate.GIDIndex < 0 || candidate.GIDIndex > 255 || candidate.GIDType != "RoCE v2" {
			return false
		}
		candidate.RDMAPort, candidate.GIDIndex, candidate.GIDType = 0, 0, ""
		if candidate != base[i] {
			return false
		}
	}
	return true
}

func fabricQualificationDigest(operationID, recipeID string, targets []fabricTarget, candidates []fabricCandidateIP) string {
	payload, _ := json.Marshal(struct {
		OperationID string              `json:"operationId"`
		RecipeID    string              `json:"recipeId"`
		Targets     []fabricTarget      `json:"targets"`
		Candidates  []fabricCandidateIP `json:"candidates"`
	}{operationID, recipeID, targets, candidates})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

type fabricProofFailure struct {
	NodeID string
	Code   string
	cause  error
}

func (e *fabricProofFailure) Error() string { return e.Code }
func (e *fabricProofFailure) Unwrap() error { return e.cause }

func (s *fabricService) proveFabric(ctx context.Context, run *fabricRunRecord, method string) ([]fabricCandidateIP, error) {
	if method != "qualify" && method != "requalify" {
		return nil, errors.New("unsupported fabric proof method")
	}
	candidates, err := fabricCandidates(run.Public.RecipeID, run.Public.Targets)
	if err != nil {
		return nil, err
	}
	results := make([][]fabricCandidateIP, len(run.Public.Targets))
	failures := make([]error, len(run.Public.Targets))
	var proofs sync.WaitGroup
	for i, target := range run.Public.Targets {
		local := make([]fabricCandidateIP, 0, len(run.Public.Targets)-1)
		for _, candidate := range candidates {
			if candidate.NodeID == target.NodeID {
				local = append(local, candidate)
			}
		}
		prove := func() {
			result, err := s.control(ctx, target, fabricControlRequest{Method: method, OperationID: run.Public.OperationID, Target: target, Candidates: local})
			if err != nil {
				failures[i] = &fabricProofFailure{NodeID: target.NodeID, Code: fabricQualificationFailureCode(err, "provider-unavailable"), cause: err}
				return
			}
			if result.FailureCode != "" {
				code := result.FailureCode
				if !validFabricQualificationFailureCode(code) {
					code = "result-invalid"
				}
				failures[i] = &fabricProofFailure{NodeID: target.NodeID, Code: code, cause: errors.New("fabric qualification provider reported failure")}
				return
			}
			if !result.Qualified || !fabricCandidatesQualified(local, result.Candidates) {
				failures[i] = &fabricProofFailure{NodeID: target.NodeID, Code: "result-invalid", cause: errors.New("fabric qualification result did not match the request")}
				return
			}
			results[i] = result.Candidates
		}
		if method == "qualify" {
			prove()
		} else {
			proofs.Go(prove)
		}
	}
	proofs.Wait()
	qualified := make([]fabricCandidateIP, 0, len(candidates))
	for i := range results {
		if failures[i] != nil {
			return nil, failures[i]
		}
		qualified = append(qualified, results[i]...)
	}
	ordered := make([]fabricCandidateIP, 0, len(candidates))
	for _, base := range candidates {
		found := false
		for _, candidate := range qualified {
			if candidate.NodeID == base.NodeID && candidate.PeerNodeID == base.PeerNodeID && candidate.InterfaceIndex == base.InterfaceIndex && candidate.Gateway == base.Gateway {
				ordered = append(ordered, candidate)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("qualified fabric endpoint set is incomplete")
		}
	}
	if !fabricCandidatesQualified(candidates, ordered) {
		return nil, errors.New("qualified fabric endpoint set changed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ordered, nil
}

func (s *fabricService) qualifyFabric(ctx context.Context, run *fabricRunRecord) error {
	ordered, err := s.proveFabric(ctx, run, "qualify")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.Public.State != "applying" {
		return errors.New("fabric operation left applying state before qualification commit")
	}
	run.Public.CandidateIPs = ordered
	run.Public.QualifiedAt = time.Now().UnixMilli()
	run.Public.QualificationDigest = fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, ordered)
	if err := s.save(run); err != nil {
		s.recoveryFailed = true
		run.Public.CandidateIPs = nil
		run.Public.QualifiedAt = 0
		run.Public.QualificationDigest = ""
		return errors.New("qualified fabric receipt could not be retained")
	}
	if s.qualified == nil {
		s.qualified = map[string]string{}
	}
	s.qualified[run.Public.OperationID] = run.Public.QualificationDigest
	return nil
}

func (s *fabricService) requalifyActive(ctx context.Context, run *fabricRunRecord) error {
	s.mu.Lock()
	if run == nil || run.Public.State != "active" {
		s.mu.Unlock()
		return errors.New("active fabric operation is unavailable")
	}
	expectedDigest := run.Public.QualificationDigest
	expected := slices.Clone(run.Public.CandidateIPs)
	s.mu.Unlock()
	proved, err := s.proveFabric(ctx, run, "requalify")
	if err != nil || !slices.Equal(proved, expected) {
		s.mu.Lock()
		delete(s.qualified, run.Public.OperationID)
		s.mu.Unlock()
		var failure *fabricProofFailure
		if errors.As(err, &failure) {
			return errors.New("active fabric qualification could not be freshly re-proven on participant " + failure.NodeID + " (" + failure.Code + ")")
		}
		return errors.New("active fabric qualification could not be freshly re-proven")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.Public.State != "active" || run.Public.QualificationDigest != expectedDigest || expectedDigest != fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, run.Public.CandidateIPs) {
		delete(s.qualified, run.Public.OperationID)
		return errors.New("active fabric changed during requalification")
	}
	if s.qualified == nil {
		s.qualified = map[string]string{}
	}
	s.qualified[run.Public.OperationID] = expectedDigest
	return nil
}

func (s *fabricService) qualifiedFabric(nodeIDs []string) (string, string, []fabricCandidateIP, error) {
	want := slices.Clone(nodeIDs)
	sort.Strings(want)
	if len(want) < 2 || len(want) > 3 {
		return "", "", nil, errors.New("qualified fabric requires two or three distinct members")
	}
	for i := range want {
		if !cableIdentifier(want[i], 128) || i > 0 && want[i] == want[i-1] {
			return "", "", nil, errors.New("qualified fabric requires two or three distinct members")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var operationID, digest string
	var endpoints []fabricCandidateIP
	for _, run := range s.runs {
		if run.Public.State != "active" || !fabricQualifiedRecipe(run.Public.RecipeID) || run.Public.QualifiedAt <= 0 || s.qualified[run.Public.OperationID] != run.Public.QualificationDigest {
			continue
		}
		base, err := fabricCandidates(run.Public.RecipeID, run.Public.Targets)
		if err != nil || !fabricCandidatesQualified(base, run.Public.CandidateIPs) || run.Public.QualificationDigest != fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, run.Public.CandidateIPs) {
			continue
		}
		got := make([]string, 0, len(run.Public.Targets))
		for _, target := range run.Public.Targets {
			got = append(got, target.NodeID)
		}
		sort.Strings(got)
		if slices.Equal(want, got) {
			if operationID != "" {
				return "", "", nil, errors.New("multiple qualified active fabrics match the requested members")
			}
			operationID, digest, endpoints = run.Public.OperationID, run.Public.QualificationDigest, slices.Clone(run.Public.CandidateIPs)
		}
	}
	if operationID != "" {
		return operationID, digest, endpoints, nil
	}
	return "", "", nil, errors.New("qualified active fabric is unavailable")
}

func (s *fabricService) requalifyFabric(ctx context.Context, nodeIDs []string) (string, string, []fabricCandidateIP, error) {
	want := slices.Clone(nodeIDs)
	sort.Strings(want)
	s.mu.Lock()
	var selected *fabricRunRecord
	for _, run := range s.runs {
		if run.Public.State != "active" {
			continue
		}
		got := make([]string, 0, len(run.Public.Targets))
		for _, target := range run.Public.Targets {
			got = append(got, target.NodeID)
		}
		sort.Strings(got)
		if slices.Equal(want, got) {
			if selected != nil {
				s.mu.Unlock()
				return "", "", nil, errors.New("multiple active fabrics match the requested members")
			}
			selected = run
		}
	}
	s.mu.Unlock()
	if selected == nil {
		return "", "", nil, errors.New("active fabric is unavailable")
	}
	if err := s.requalifyActive(ctx, selected); err != nil {
		return "", "", nil, err
	}
	return s.qualifiedFabric(nodeIDs)
}
