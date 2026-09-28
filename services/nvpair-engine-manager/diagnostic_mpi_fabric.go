// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/cableprobe"
)

// A fabric review moves only NCCL Socket onto the one active fabric its review
// controller owns. OpenMPI launch, its OOB/BTL subnet, SSH and the rendezvous
// stay on the management network.
type diagnosticMPIFabric struct {
	OperationID         string `json:"operationId"`
	QualificationDigest string `json:"qualificationDigest"`
	RecipeID            string `json:"recipeId"`
	OwnerNodeID         string `json:"ownerNodeId"`
	OwnerPrincipal      string `json:"ownerPrincipal"`
}

// One member's NCCL Socket end: its end of the lowest-index reciprocal lane of
// a direct fabric, or its advertised p0 address on a routed ring.
type diagnosticMemberFabric struct {
	Interface string `json:"interface"`
	Index     int    `json:"index"`
	MAC       string `json:"mac"`
	Address   string `json:"address"`
	Prefix    int    `json:"prefix"`
}

// The fabric owner revalidates a plan from its public fabric binding alone.
// The lists are ordered like the plan members.
type diagnosticMPIFabricCheck struct {
	Fabric     diagnosticMPIFabric      `json:"fabric"`
	NodeIDs    []string                 `json:"nodeIds"`
	Principals []string                 `json:"principals"`
	Members    []diagnosticMemberFabric `json:"members"`
}

type diagnosticMPIReviewFabric struct {
	OperationID         string `json:"operationId"`
	QualificationDigest string `json:"qualificationDigest"`
	RecipeID            string `json:"recipeId"`
}

// Typed review refusals let a client name the remedy without relaying backend
// text. Both codes sit in the implementation-defined server-error range.
const (
	diagnosticMPIFabricAbsentCode   = -32010
	diagnosticMPIFabricUnusableCode = -32011
)

// Requalification and settle waits share this bound; a fabric review adds it
// to the ordinary review budget.
const diagnosticMPIFabricBudget = 30 * time.Second

var errDiagnosticMPINoFabric = errors.New("no active fabric owned by this PAIR node joins exactly the selected Sparks; apply a two-node direct fabric or a three-node routed ring from this node, or review on the management network")

type diagnosticMPIFabricRefusal struct {
	absent bool
	cause  error
}

func (e *diagnosticMPIFabricRefusal) Error() string { return e.cause.Error() }
func (e *diagnosticMPIFabricRefusal) Unwrap() error { return e.cause }

func (e *diagnosticMPIFabricRefusal) code() int {
	if e.absent {
		return diagnosticMPIFabricAbsentCode
	}
	return diagnosticMPIFabricUnusableCode
}

func diagnosticMPIFabricPrefix(recipeID string, ranks int) int {
	switch {
	case recipeID == fabricRecipe && ranks == 2:
		return 30
	case recipeID == fabricRingRecipe && ranks == 3:
		return 31
	}
	return 0
}

func validDiagnosticMPIFabric(f diagnosticMPIFabric, ranks int) bool {
	return onboardingID.MatchString(f.OperationID) && diagnosticDigest.MatchString(f.QualificationDigest) && diagnosticMPIFabricPrefix(f.RecipeID, ranks) != 0 &&
		diagnosticToken.MatchString(f.OwnerNodeID) && diagnosticToken.MatchString(f.OwnerPrincipal)
}

func validDiagnosticMemberFabric(m diagnosticMemberFabric, prefix int) bool {
	mac, macErr := net.ParseMAC(m.MAC)
	address, addressErr := netip.ParseAddr(m.Address)
	return (prefix == 30 || prefix == 31) && m.Prefix == prefix && diagnosticToken.MatchString(m.Interface) && len(m.Interface) <= 15 && !strings.ContainsAny(m.Interface, ":=") && m.Index > 0 &&
		macErr == nil && len(mac) == 6 && mac.String() == m.MAC && addressErr == nil && address.Is4() && address.IsPrivate() && address.String() == m.Address
}

// Both ends of a direct lane share its /30; each ring member advertises its
// address on a different /31 cable.
func validDiagnosticMPIFabricMembers(f diagnosticMPIFabric, members []diagnosticMemberFabric) bool {
	prefix := diagnosticMPIFabricPrefix(f.RecipeID, len(members))
	if prefix == 0 {
		return false
	}
	cables, addresses := map[netip.Prefix]bool{}, map[string]bool{}
	for _, m := range members {
		if !validDiagnosticMemberFabric(m, prefix) || addresses[m.Address] {
			return false
		}
		addresses[m.Address] = true
		cables[netip.PrefixFrom(netip.MustParseAddr(m.Address), prefix).Masked()] = true
	}
	if prefix == 30 {
		return len(cables) == 1
	}
	return len(cables) == len(members)
}

// A management request carries no fabric selection. A fabric request names
// one member end whose physical selection and NCCL binding agree.
func validDiagnosticMPIFactsNetwork(request diagnosticMPIFactsRequest) bool {
	s, f := request.Selection, request.Fabric
	switch request.Network {
	case "management":
		return s == (diagnosticMPIInterfaceSelection{}) && f == (diagnosticMemberFabric{})
	case "fabric":
		return s.Network == "fabric" && s.NodeID == request.Target.NodeID && s.Principal == request.Target.Principal && cableIdentifier(s.SwitchID, 128) && cableIdentifier(s.PortName, 128) &&
			s.Interface == (cableprobe.Interface{Name: f.Interface, Index: f.Index, MAC: f.MAC}) && validDiagnosticMemberFabric(f, f.Prefix)
	}
	return false
}

// Each member's end is bound exactly as the serving socket plans bind it.
// Members are ordered like nodeIDs, whose first entry is the coordinator.
func diagnosticMPIFabricMembers(recipeID string, nodeIDs, principals []string, operationID, qualification string, endpoints []fabricCandidateIP) ([]diagnosticMemberFabric, error) {
	members := make([]diagnosticMemberFabric, 0, len(nodeIDs))
	switch prefix := diagnosticMPIFabricPrefix(recipeID, len(nodeIDs)); prefix {
	case 30:
		ds, err := vllmGroupDirectSocketFor(nodeIDs, principals, operationID, qualification, endpoints)
		if err != nil {
			return nil, err
		}
		for _, lane := range ds.Lanes {
			members = append(members, diagnosticMemberFabric{Interface: lane.InterfaceName, Index: lane.InterfaceIndex, MAC: lane.MAC, Address: lane.LocalAddress, Prefix: prefix})
		}
	case 31:
		rs, err := vllmGroupRingSocketFor(nodeIDs, principals, operationID, qualification, endpoints)
		if err != nil {
			return nil, err
		}
		for _, member := range rs.Members {
			members = append(members, diagnosticMemberFabric{Interface: member.InterfaceName, Index: member.InterfaceIndex, MAC: member.MAC, Address: member.AdvertisedAddress, Prefix: prefix})
		}
	default:
		return nil, errors.New("the fabric recipe does not match the selected participant count")
	}
	return members, nil
}

func diagnosticMPIFabricPort(nodeID string, member diagnosticMemberFabric, endpoints []fabricCandidateIP) (fabricCandidateIP, bool) {
	for _, endpoint := range endpoints {
		if endpoint.NodeID == nodeID && endpoint.InterfaceName == member.Interface && endpoint.InterfaceIndex == member.Index && strings.EqualFold(endpoint.MAC, member.MAC) && endpoint.Address == member.Address {
			return endpoint, true
		}
	}
	return fabricCandidateIP{}, false
}

// mpiReviewFabric freshly requalifies the one active fabric this node owns
// that joins exactly the selected targets and binds each member's NCCL Socket
// end. Absence refuses the review; it never falls back to management.
func (d *diagnosticService) mpiReviewFabric(ctx context.Context, targets []diagnosticManagedTarget) (diagnosticMPIFabric, []diagnosticMPIInterfaceSelection, []diagnosticMemberFabric, error) {
	nodeIDs, principals := make([]string, 0, len(targets)), make([]string, 0, len(targets))
	for _, target := range targets {
		nodeIDs, principals = append(nodeIDs, target.NodeID), append(principals, target.Principal)
	}
	recipe := fabricRecipe
	if len(targets) == 3 {
		recipe = fabricRingRecipe
	}
	s := d.m.exec.fabric
	if s == nil || d.m.cableLocal == nil {
		return diagnosticMPIFabric{}, nil, nil, &diagnosticMPIFabricRefusal{absent: true, cause: errDiagnosticMPINoFabric}
	}
	ctx, cancel := context.WithTimeout(ctx, diagnosticMPIFabricBudget)
	defer cancel()
	operationID, qualification, endpoints, err := s.exactFabricFor(ctx, nodeIDs, recipe, "roll it back or recover it, or review on the management network")
	if errors.Is(err, errNoFabric) {
		return diagnosticMPIFabric{}, nil, nil, &diagnosticMPIFabricRefusal{absent: true, cause: errDiagnosticMPINoFabric}
	}
	if err != nil {
		return diagnosticMPIFabric{}, nil, nil, &diagnosticMPIFabricRefusal{cause: err}
	}
	members, err := diagnosticMPIFabricMembers(recipe, nodeIDs, principals, operationID, qualification, endpoints)
	if err != nil {
		return diagnosticMPIFabric{}, nil, nil, &diagnosticMPIFabricRefusal{cause: err}
	}
	selections := make([]diagnosticMPIInterfaceSelection, 0, len(members))
	for i, member := range members {
		port, ok := diagnosticMPIFabricPort(nodeIDs[i], member, endpoints)
		if !ok {
			return diagnosticMPIFabric{}, nil, nil, &diagnosticMPIFabricRefusal{cause: errors.New("the qualified fabric endpoint has no physical port identity")}
		}
		selections = append(selections, diagnosticMPIInterfaceSelection{Network: "fabric", NodeID: nodeIDs[i], Principal: principals[i], SwitchID: port.SwitchID, PortName: port.PortName,
			Interface: cableprobe.Interface{Name: member.Interface, Index: member.Index, MAC: member.MAC}})
	}
	d.m.mesh.Refresh()
	fabric := diagnosticMPIFabric{OperationID: operationID, QualificationDigest: qualification, RecipeID: recipe, OwnerNodeID: d.m.cableLocal.nodeID, OwnerPrincipal: d.m.mesh.NodeUUID()}
	if !validDiagnosticMPIFabric(fabric, len(targets)) || !validDiagnosticMPIFabricMembers(fabric, members) {
		return diagnosticMPIFabric{}, nil, nil, &diagnosticMPIFabricRefusal{cause: errors.New("the qualified fabric does not bind one private NCCL Socket address per member")}
	}
	return fabric, selections, members, nil
}

// currentMPIFabric runs on the fabric owner. The plan's fabric must still be
// this node's same active, qualified operation, binding every member to the
// same NCCL Socket end the review chose.
func (d *diagnosticService) currentMPIFabric(ctx context.Context, check diagnosticMPIFabricCheck) error {
	f, count := check.Fabric, len(check.NodeIDs)
	if len(check.Principals) != count || len(check.Members) != count || !validDiagnosticMPIFabric(f, count) || !validDiagnosticMPIFabricMembers(f, check.Members) {
		return errors.New("invalid reviewed fabric binding")
	}
	for i, nodeID := range check.NodeIDs {
		if !diagnosticToken.MatchString(nodeID) || !diagnosticToken.MatchString(check.Principals[i]) || i > 0 && check.NodeIDs[i-1] >= nodeID {
			return errors.New("invalid reviewed fabric members")
		}
	}
	s := d.m.exec.fabric
	d.m.mesh.Refresh()
	if s == nil || d.m.cableLocal == nil || f.OwnerNodeID != d.m.cableLocal.nodeID || f.OwnerPrincipal != d.m.mesh.NodeUUID() {
		return errors.New("the reviewed fabric belongs to another owner")
	}
	ctx, cancel := context.WithTimeout(ctx, diagnosticMPIFabricBudget)
	defer cancel()
	operationID, digest, endpoints, err := s.settledQualifiedFabric(ctx, check.NodeIDs)
	if err != nil || operationID != f.OperationID || digest != f.QualificationDigest {
		return errors.New("the reviewed fabric changed or was rolled back; review the NCCL test again")
	}
	members, err := diagnosticMPIFabricMembers(f.RecipeID, check.NodeIDs, check.Principals, operationID, digest, endpoints)
	if err != nil || !slices.Equal(members, check.Members) {
		return errors.New("the reviewed fabric no longer binds the same NCCL Socket endpoints; review the NCCL test again")
	}
	return nil
}

// mpiFabricCurrent asks the fabric owner whether a fabric plan's fabric is
// unchanged. A management plan has no fabric to confirm.
func (d *diagnosticService) mpiFabricCurrent(ctx context.Context, p diagnosticProfile) error {
	if p.Fabric == (diagnosticMPIFabric{}) {
		return nil
	}
	check := diagnosticMPIFabricCheck{Fabric: p.Fabric}
	for _, member := range p.Members {
		check.NodeIDs = append(check.NodeIDs, member.NodeID)
		check.Principals = append(check.Principals, member.Principal)
		check.Members = append(check.Members, member.Fabric)
	}
	ctx, cancel := context.WithTimeout(ctx, diagnosticMPIFabricBudget)
	defer cancel()
	if d.m.cableLocal != nil && p.Fabric.OwnerNodeID == d.m.cableLocal.nodeID {
		return d.currentMPIFabric(ctx, check)
	}
	d.m.mesh.Refresh()
	peer, ok := d.m.peers.lookup(p.Fabric.OwnerNodeID)
	if !ok || peer.clusterUUID != p.Fabric.OwnerPrincipal || !d.m.mesh.Clustered() || !d.m.mesh.HasPin(peer.clusterUUID) {
		return errors.New("the reviewed fabric owner is not a current paired participant")
	}
	client, err := d.m.remoteClient(ctx, peer)
	if err != nil {
		return err
	}
	raw, err := client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: "mpi-fabric", MPIFabric: &check})
	if err != nil {
		return err
	}
	var current diagnosticMPIFabric
	if strictDiagnosticJSON(raw, &current) != nil || current != p.Fabric {
		return errors.New("the reviewed fabric owner confirmed a different fabric")
	}
	return nil
}

// The review controller must own the fabric it binds, so the coordinator can
// ask that exact node to revalidate it before launch.
func (d *diagnosticService) mpiFabricOwnedBy(f diagnosticMPIFabric, caller string) bool {
	if f.OwnerPrincipal != caller {
		return false
	}
	if d.m.cableLocal != nil && f.OwnerNodeID == d.m.cableLocal.nodeID {
		return caller == d.m.mesh.NodeUUID()
	}
	peer, ok := d.m.peers.lookup(f.OwnerNodeID)
	return ok && peer.clusterUUID == caller
}

// diagnosticMPIFabricObserved checks a participant's fresh node-info facts: the
// selected fabric interface keeps its reviewed identity and port, carries
// exactly its reviewed address, and is not the management interface.
func diagnosticMPIFabricObserved(info diagnosticMPIConnections, request diagnosticMPIFactsRequest, management string, now time.Time) error {
	observed, err := projectDiagnosticMPIAddress(info, request.Selection, now)
	if err != nil {
		return err
	}
	_, subnet, err := net.ParseCIDR(observed.Subnet)
	if err != nil {
		return errors.New("selected fabric interface subnet is invalid")
	}
	ones, _ := subnet.Mask.Size()
	if observed.Address != request.Fabric.Address || ones != request.Fabric.Prefix || request.Fabric.Interface == management {
		return errors.New("selected fabric interface no longer carries its reviewed NCCL Socket address")
	}
	return nil
}

// mpiFabricChoice binds compilation to the participant's fresh observation of
// its fabric interface, as mpiInterfaceChoice does for the management subnet.
func mpiFabricChoice(f diagnosticMPIParticipantFacts, m diagnosticMemberFabric) error {
	for _, iface := range f.Interfaces {
		if iface.Name != m.Interface {
			continue
		}
		if !iface.Up || len(iface.Addresses) != 1 {
			return errors.New("fresh observations do not show the reviewed fabric address alone on its interface")
		}
		ip, network, err := net.ParseCIDR(iface.Addresses[0])
		if err != nil {
			return errors.New("fabric interface address observation is malformed")
		}
		ones, _ := network.Mask.Size()
		if ip.String() != m.Address || ones != m.Prefix {
			return errors.New("fresh observations do not show the reviewed fabric address alone on its interface")
		}
		return nil
	}
	return errors.New("fresh observations lack the reviewed fabric interface")
}

// The plan carries exactly its profile's fabric binding, member by member.
func diagnosticMPIPlanFabricBound(p diagnosticProfile, raw json.RawMessage) bool {
	var plan struct {
		Fabric  *diagnosticMPIFabric `json:"fabric"`
		Members []struct {
			NodeID string                  `json:"nodeId"`
			Fabric *diagnosticMemberFabric `json:"fabric"`
		} `json:"members"`
	}
	if json.Unmarshal(raw, &plan) != nil {
		return false
	}
	if plan.Fabric == nil {
		for _, member := range plan.Members {
			if member.Fabric != nil {
				return false
			}
		}
		return p.Fabric == (diagnosticMPIFabric{})
	}
	if *plan.Fabric != p.Fabric || len(plan.Members) != len(p.Members) {
		return false
	}
	for i, member := range plan.Members {
		if member.NodeID != p.Members[i].NodeID || member.Fabric == nil || *member.Fabric != p.Members[i].Fabric {
			return false
		}
	}
	return true
}

// verifyDiagnosticMemberFabric is the rank's native check: the reviewed NCCL
// interface keeps its index and MAC, is up, and carries exactly its reviewed
// IPv4 address, so NCCL cannot bind another address on it.
func verifyDiagnosticMemberFabric(f diagnosticMemberFabric, byName func(string) (*net.Interface, error), addresses func(*net.Interface) ([]net.Addr, error)) error {
	iface, err := byName(f.Interface)
	if err != nil || iface == nil || iface.Index != f.Index || iface.HardwareAddr.String() != f.MAC || iface.Flags&net.FlagUp == 0 {
		return errors.New("reviewed fabric interface identity or availability changed")
	}
	values, err := addresses(iface)
	if err != nil || len(values) > 32 {
		return errors.New("reviewed fabric interface addresses are unavailable")
	}
	want, found := f.Address+"/"+strconv.Itoa(f.Prefix), 0
	for _, value := range values {
		ip, network, parseErr := net.ParseCIDR(value.String())
		if parseErr != nil {
			return errors.New("reviewed fabric interface address is malformed")
		}
		if ip.To4() == nil {
			continue
		}
		ones, _ := network.Mask.Size()
		if ip.String()+"/"+strconv.Itoa(ones) != want {
			return errors.New("reviewed fabric interface carries another IPv4 address")
		}
		found++
	}
	if found != 1 {
		return errors.New("reviewed fabric interface lost its reviewed address")
	}
	return nil
}
