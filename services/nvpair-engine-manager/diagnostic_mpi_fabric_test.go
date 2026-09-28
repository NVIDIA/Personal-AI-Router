// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

// Synthetic fabric records and injected proofs only. These tests never read
// native interfaces, contact a participant, or start MPI.

// Managed diagnostic rosters use node identities as principals.
func diagnosticMPINodePrincipals(s *fabricService, r *fabricRunRecord) (*fabricService, *fabricRunRecord) {
	for i := range r.Public.Targets {
		r.Public.Targets[i].Principal = r.Public.Targets[i].NodeID
	}
	for i := range r.Public.CandidateIPs {
		r.Public.CandidateIPs[i].PeerPrincipal = r.Public.CandidateIPs[i].PeerNodeID
	}
	r.Public.QualificationDigest = fabricQualificationDigest(r.Public.OperationID, r.Public.RecipeID, r.Public.Targets, r.Public.CandidateIPs)
	s.qualified[r.Public.OperationID] = r.Public.QualificationDigest
	return s, r
}

func diagnosticMPIDirectFabricFixture(t *testing.T) (*fabricService, *fabricRunRecord) {
	t.Helper()
	return diagnosticMPINodePrincipals(qualifiedDirectFabricFixture(t))
}

func diagnosticMPIRingFabricFixture(t *testing.T) (*fabricService, *fabricRunRecord) {
	t.Helper()
	return diagnosticMPINodePrincipals(qualifiedRingFabricFixture(t))
}

func diagnosticMPIFabricTargets(nodes ...string) []diagnosticManagedTarget {
	targets := make([]diagnosticManagedTarget, 0, len(nodes))
	for _, node := range nodes {
		targets = append(targets, diagnosticManagedTarget{NodeID: node, Principal: node})
	}
	return targets
}

// The local node-a owns s, as the desktop controller owns the fabric it applied.
func diagnosticMPIFabricOwner(t *testing.T, s *fabricService) *diagnosticService {
	t.Helper()
	d := diagnosticTestService(t)
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", "node-a", "node-b", "node-c")
	d.m.mesh = clustertrust.Open(dir)
	d.m.cableLocal = newCableLocalFacts("node-a", 0)
	d.m.exec.fabric = s
	return d
}

func diagnosticMPIFabricCheckFor(t *testing.T, r *fabricRunRecord, owner string) diagnosticMPIFabricCheck {
	t.Helper()
	check := diagnosticMPIFabricCheck{Fabric: diagnosticMPIFabric{OperationID: r.Public.OperationID, QualificationDigest: r.Public.QualificationDigest, RecipeID: r.Public.RecipeID, OwnerNodeID: owner, OwnerPrincipal: owner}}
	for _, target := range r.Public.Targets {
		check.NodeIDs, check.Principals = append(check.NodeIDs, target.NodeID), append(check.Principals, target.Principal)
	}
	members, err := diagnosticMPIFabricMembers(r.Public.RecipeID, check.NodeIDs, check.Principals, r.Public.OperationID, r.Public.QualificationDigest, r.Public.CandidateIPs)
	if err != nil {
		t.Fatal(err)
	}
	check.Members = members
	return check
}

func diagnosticMPIFabricRefusalCode(err error) int {
	var refusal *diagnosticMPIFabricRefusal
	if !errors.As(err, &refusal) {
		return 0
	}
	return refusal.code()
}

func TestDiagnosticMPIFabricBindsTheServingSocketEnds(t *testing.T) {
	_, direct := diagnosticMPIDirectFabricFixture(t)
	nodes := []string{"node-a", "node-b"}
	members, err := diagnosticMPIFabricMembers(fabricRecipe, nodes, nodes, direct.Public.OperationID, direct.Public.QualificationDigest, direct.Public.CandidateIPs)
	lanes, laneErr := vllmGroupDirectSocketFor(nodes, nodes, direct.Public.OperationID, direct.Public.QualificationDigest, direct.Public.CandidateIPs)
	if err != nil || laneErr != nil || len(members) != 2 {
		t.Fatalf("direct binding failed: %v %v", err, laneErr)
	}
	for i, lane := range lanes.Lanes {
		if members[i] != (diagnosticMemberFabric{Interface: lane.InterfaceName, Index: lane.InterfaceIndex, MAC: lane.MAC, Address: lane.LocalAddress, Prefix: 30}) {
			t.Fatalf("member %d left the serving direct-socket lane: %+v", i, members[i])
		}
	}
	for _, iface := range direct.Public.Targets[0].Interfaces {
		if iface.Index < members[0].Index {
			t.Fatal("the coordinator end is not the lowest-index reciprocal lane")
		}
	}
	_, ring := diagnosticMPIRingFabricFixture(t)
	ringNodes := []string{"node-a", "node-b", "node-c"}
	members, err = diagnosticMPIFabricMembers(fabricRingRecipe, ringNodes, ringNodes, ring.Public.OperationID, ring.Public.QualificationDigest, ring.Public.CandidateIPs)
	if err != nil || len(members) != 3 {
		t.Fatalf("ring binding failed: %v", err)
	}
	for i, target := range ring.Public.Targets {
		p0, ok := fabricInterfaceAt(target, "p0")
		if !ok || members[i].Address != target.AdvertisedAddress || members[i].Interface != p0.Name || members[i].Index != p0.Index || members[i].Prefix != 31 {
			t.Fatalf("ring member %d is not bound to its advertised p0 address: %+v", i, members[i])
		}
	}
	for _, mismatch := range []struct {
		recipe string
		nodes  []string
	}{{fabricRingRecipe, nodes}, {fabricRecipe, ringNodes}, {fabricRingRetainedRecipe, ringNodes}} {
		if _, err := diagnosticMPIFabricMembers(mismatch.recipe, mismatch.nodes, mismatch.nodes, ring.Public.OperationID, ring.Public.QualificationDigest, ring.Public.CandidateIPs); err == nil {
			t.Fatalf("%s bound %d members", mismatch.recipe, len(mismatch.nodes))
		}
	}
}

func TestDiagnosticMPIReviewFabricAcceptsOnlyAnExactFreshlyRequalifiedFabric(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture func(*testing.T) (*fabricService, *fabricRunRecord)
		nodes   []string
	}{{"direct", diagnosticMPIDirectFabricFixture, []string{"node-a", "node-b"}}, {"routed ring", diagnosticMPIRingFabricFixture, []string{"node-a", "node-b", "node-c"}}} {
		t.Run(test.name, func(t *testing.T) {
			s, r := test.fixture(t)
			d := diagnosticMPIFabricOwner(t, s)
			proofs, prove := 0, s.control
			s.control = func(ctx context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
				if request.Method == "requalify" {
					proofs++
				}
				return prove(ctx, target, request)
			}
			fabric, selections, members, err := d.mpiReviewFabric(context.Background(), diagnosticMPIFabricTargets(test.nodes...))
			want := diagnosticMPIFabric{OperationID: r.Public.OperationID, QualificationDigest: r.Public.QualificationDigest, RecipeID: r.Public.RecipeID, OwnerNodeID: "node-a", OwnerPrincipal: "node-a"}
			if err != nil || proofs != len(test.nodes) || fabric != want || len(selections) != len(test.nodes) || !validDiagnosticMPIFabricMembers(fabric, members) {
				t.Fatalf("fabric review binding failed: %+v %v proofs=%d", fabric, err, proofs)
			}
			for i, selection := range selections {
				request := diagnosticMPIFactsRequest{Network: "fabric", Target: diagnosticManagedTarget{NodeID: test.nodes[i], Principal: test.nodes[i]}, Selection: selection, Fabric: members[i]}
				if selection.SwitchID != "switch-"+test.nodes[i] || selection.PortName != "p0" || !validDiagnosticMPIFactsNetwork(request) {
					t.Fatalf("member %d selection lost its physical port identity: %+v", i, selection)
				}
			}
		})
	}
	absent := newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	for name, test := range map[string]struct {
		fixture func(*testing.T) (*fabricService, *fabricRunRecord)
		spoil   func(*diagnosticService, *fabricService, *fabricRunRecord)
		nodes   []string
		code    int
	}{
		"absent":                {diagnosticMPIDirectFabricFixture, func(d *diagnosticService, _ *fabricService, _ *fabricRunRecord) { d.m.exec.fabric = absent }, []string{"node-a", "node-b"}, diagnosticMPIFabricAbsentCode},
		"no fabric owner":       {diagnosticMPIDirectFabricFixture, func(d *diagnosticService, _ *fabricService, _ *fabricRunRecord) { d.m.exec.fabric = nil }, []string{"node-a", "node-b"}, diagnosticMPIFabricAbsentCode},
		"unrelated fabric":      {diagnosticMPIDirectFabricFixture, nil, []string{"node-x", "node-y"}, diagnosticMPIFabricAbsentCode},
		"requalification fails": {diagnosticMPIDirectFabricFixture, func(_ *diagnosticService, s *fabricService, _ *fabricRunRecord) { s.control = diagnosticMPIFailedProof }, []string{"node-a", "node-b"}, diagnosticMPIFabricUnusableCode},
		"recovery required":     {diagnosticMPIDirectFabricFixture, func(_ *diagnosticService, _ *fabricService, r *fabricRunRecord) { r.Public.State = "recovery-required" }, []string{"node-a", "node-b"}, diagnosticMPIFabricUnusableCode},
		"other membership":      {diagnosticMPIDirectFabricFixture, nil, []string{"node-a", "node-c"}, diagnosticMPIFabricUnusableCode},
		"direct for three":      {diagnosticMPIDirectFabricFixture, nil, []string{"node-a", "node-b", "node-c"}, diagnosticMPIFabricUnusableCode},
		"unrouted ring":         {diagnosticMPIRingFabricFixture, func(_ *diagnosticService, _ *fabricService, r *fabricRunRecord) { r.Public.RecipeID = fabricRingRetainedRecipe }, []string{"node-a", "node-b", "node-c"}, diagnosticMPIFabricUnusableCode},
		"two ring members":      {diagnosticMPIRingFabricFixture, nil, []string{"node-a", "node-b"}, diagnosticMPIFabricUnusableCode},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := test.fixture(t)
			d := diagnosticMPIFabricOwner(t, s)
			if test.spoil != nil {
				test.spoil(d, s, r)
			}
			_, _, _, err := d.mpiReviewFabric(context.Background(), diagnosticMPIFabricTargets(test.nodes...))
			if code := diagnosticMPIFabricRefusalCode(err); code != test.code {
				t.Fatalf("refusal code %d, want %d: %v", code, test.code, err)
			}
			if test.code == diagnosticMPIFabricAbsentCode && !strings.Contains(err.Error(), "apply a two-node direct fabric or a three-node routed ring") {
				t.Fatalf("absence did not tell the operator to apply a fabric: %v", err)
			}
		})
	}
}

func diagnosticMPIFailedProof(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
	return fabricControlResult{}, errors.New("route changed")
}

// A controller review over the fabric sends each participant its NCCL end,
// keeps OpenMPI on management, and revalidates the same fabric at the end.
func TestDiagnosticMPIReviewOverDirectFabricBindsNCCLEndsAndRefusesAbsence(t *testing.T) {
	d, run, dials, _ := mpiReauthorizedFixture(t)
	s, r := diagnosticMPIDirectFabricFixture(t)
	d.m.exec.fabric, d.m.cableLocal = s, newCableLocalFacts("controller", 0)
	posts := 0
	client, _ := d.m.remoteHTTP.Client(run.Binding.Targets[0].Principal)
	client.Transport = mpiReviewMemoryTransport(func(req *http.Request) (*http.Response, error) {
		posts++
		var request diagnosticControlRequest
		if json.NewDecoder(req.Body).Decode(&request) != nil || request.Method != "mpi-review" || request.MPIReview == nil || request.MPIReview.Fabric == nil {
			t.Fatal("fabric review did not reach the coordinator with its binding")
		}
		control := request.MPIReview
		public := diagnosticMPIReviewFabricFor(*control.Fabric)
		review := diagnosticMPIReview{ReviewID: strings.Repeat("d", 32), OperationID: strings.Repeat("e", 32), GroupID: "pair-smoke-" + strings.Repeat("e", 32), BuildOperationID: run.Public.OperationID, OwnerNodeID: control.Record.Targets[0].NodeID, Network: "fabric", Transport: "socket", Fabric: &public, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
		for i, target := range control.Record.Targets {
			facts := control.Requests[i]
			if facts.Network != "fabric" || !validDiagnosticMPIFactsNetwork(facts) || facts.SSHAddress != target.Address || facts.PeerAddress == facts.Fabric.Address {
				t.Fatal("a fabric facts request moved MPI or SSH off management")
			}
			review.Targets = append(review.Targets, diagnosticMPIReviewTarget{NodeID: target.NodeID, SSHAddress: target.Address, Address: facts.Fabric.Address, Interface: facts.Fabric.Interface})
		}
		body, _ := json.Marshal(review)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	request := diagnosticMPIRequest{BuildOperationID: run.Public.OperationID, Network: "fabric", DedicatedTestWindow: true}
	review, err := d.reviewMPI(context.Background(), request)
	if err != nil || posts != 1 || review.Network != "fabric" || review.Fabric == nil || review.Fabric.OperationID != r.Public.OperationID || review.Fabric.QualificationDigest != r.Public.QualificationDigest {
		t.Fatalf("fabric review failed: %+v %v posts=%d", review, err, posts)
	}
	check := diagnosticMPIFabricCheckFor(t, r, "controller")
	for i, target := range review.Targets {
		if target.Address != check.Members[i].Address || target.Interface != check.Members[i].Interface || target.SSHAddress != run.Binding.Targets[i].Address {
			t.Fatalf("review target %d does not name its fabric NCCL end: %+v", i, target)
		}
	}

	d.m.exec.fabric = newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	before := *dials
	if _, err := d.reviewMPI(context.Background(), request); diagnosticMPIFabricRefusalCode(err) != diagnosticMPIFabricAbsentCode || posts != 1 || *dials != before {
		t.Fatalf("absent fabric reached SSH or the coordinator: %v posts=%d", err, posts)
	}
	var out bytes.Buffer
	d.m.codec = NewCodec(&out)
	id := json.RawMessage(`7`)
	params, _ := json.Marshal(request)
	d.m.handleDiagnosticMPI(context.Background(), &Message{ID: &id, Method: "engine:diagnostic-mpi-review", Params: params})
	var reply Message
	if json.Unmarshal(out.Bytes(), &reply) != nil || reply.Error == nil || reply.Error.Code != diagnosticMPIFabricAbsentCode {
		t.Fatalf("absent fabric lacked its typed refusal: %s", out.Bytes())
	}
	s.control = diagnosticMPIFailedProof
	d.m.exec.fabric = s
	out.Reset()
	d.m.handleDiagnosticMPI(context.Background(), &Message{ID: &id, Method: "engine:diagnostic-mpi-review", Params: params})
	if json.Unmarshal(out.Bytes(), &reply) != nil || reply.Error == nil || reply.Error.Code != diagnosticMPIFabricUnusableCode || posts != 1 {
		t.Fatalf("stale fabric lacked its typed refusal: %s", out.Bytes())
	}
}

func mpiFabricPlanFixture(t *testing.T, count int) (diagnosticManagedRecord, []diagnosticMPIParticipantFacts, diagnosticMPISelection, time.Time) {
	t.Helper()
	record, facts, selection, now := mpiPlanCountFixture(t, count)
	selection.Fabric = diagnosticMPIFabric{OperationID: strings.Repeat("e", 32), QualificationDigest: strings.Repeat("f", 64), RecipeID: fabricRecipe, OwnerNodeID: "node-a", OwnerPrincipal: "node-a"}
	addresses, prefix := []string{"10.60.0.1", "10.60.0.2"}, 30
	if count == 3 {
		selection.Fabric.RecipeID, addresses, prefix = fabricRingRecipe, []string{"10.253.0.0", "10.253.0.4", "10.253.0.3"}, 31
	}
	for i := range facts {
		member := diagnosticMemberFabric{Interface: "enp1s0f0np0", Index: 5, MAC: fmt.Sprintf("02:00:00:0a:00:%02x", i+1), Address: addresses[i], Prefix: prefix}
		selection.FabricMembers = append(selection.FabricMembers, member)
		facts[i].Interfaces = append(facts[i].Interfaces, diagnosticMPIInterfaceObservation{Name: member.Interface, Up: true, Addresses: []string{member.Address + "/" + strconv.Itoa(prefix)}})
	}
	return record, facts, selection, now
}

func TestDiagnosticMPIFabricPlanMovesOnlyNCCLSocketAndDigestsTheBinding(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			record, facts, selection, now := mpiFabricPlanFixture(t, count)
			p, raw, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
			defer clear(key)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Fabric  *diagnosticMPIFabric `json:"fabric"`
				Subnet  string               `json:"subnet"`
				Members []struct {
					Interface         string                  `json:"interface"`
					CollectiveAddress string                  `json:"collectiveAddress"`
					Fabric            *diagnosticMemberFabric `json:"fabric"`
				} `json:"members"`
			}
			if json.Unmarshal(raw, &body) != nil || body.Fabric == nil || *body.Fabric != selection.Fabric || p.Fabric != selection.Fabric || body.Subnet != selection.Subnet || p.Bootstrap.Subnet != selection.Subnet {
				t.Fatal("fabric binding is missing from the plan or profile, or moved the MPI subnet")
			}
			for i, member := range p.Members {
				if member.Fabric != selection.FabricMembers[i] || body.Members[i].Fabric == nil || *body.Members[i].Fabric != member.Fabric || member.Interface != facts[i].Interface || body.Members[i].CollectiveAddress != facts[i].CollectiveAddress {
					t.Fatalf("member %d fabric binding or management MPI address changed", i)
				}
				env, err := diagnosticManagedRankEnvironment(p, member, nil)
				if err != nil {
					t.Fatal(err)
				}
				values := map[string]string{}
				for _, entry := range env {
					name, value, _ := strings.Cut(entry, "=")
					values[name] = value
				}
				if values["NCCL_SOCKET_IFNAME"] != "="+member.Fabric.Interface || values["OMPI_MCA_btl_tcp_if_include"] != selection.Subnet || values["OMPI_MCA_oob_tcp_if_include"] != selection.Subnet || values["NCCL_NET"] != "Socket" || values["NCCL_IB_DISABLE"] != "1" {
					t.Fatalf("rank %d environment = %v", i, values)
				}
			}
			var header diagnosticMPIHeader
			if err := json.Unmarshal(raw, &header); err != nil {
				t.Fatal(err)
			}
			request := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: selection.OperationID, ProfileDigest: header.ProfileDigest, ExpiresAt: header.ExpiresAt, BootstrapPlanDigest: header.PlanDigest}
			if _, err := validateDiagnosticMPIBinding(p, raw, request, false); err != nil {
				t.Fatal(err)
			}
			var tampered map[string]any
			if err := json.Unmarshal(raw, &tampered); err != nil {
				t.Fatal(err)
			}
			tampered["fabric"].(map[string]any)["qualificationDigest"] = strings.Repeat("0", 64)
			changed, _ := json.Marshal(tampered)
			if _, err := validateDiagnosticMPIBinding(p, changed, request, false); err == nil {
				t.Fatal("a changed fabric qualification kept the plan digest")
			}
			delete(tampered, "planDigest")
			canonical, err := mpiCanonical(tampered)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(canonical)
			tampered["planDigest"] = hex.EncodeToString(sum[:])
			redigested, _ := mpiCanonical(tampered)
			request.BootstrapPlanDigest = hex.EncodeToString(sum[:])
			if _, err := validateDiagnosticMPIBinding(p, redigested, request, false); err == nil {
				t.Fatal("a re-digested plan left its profile's fabric binding")
			}
			rebound := p
			rebound.Fabric.QualificationDigest = strings.Repeat("0", 64)
			if profileDigest(rebound) == profileDigest(p) {
				t.Fatal("the profile digest ignores the fabric binding")
			}
		})
	}
}

func TestDiagnosticMPIFabricPlanRefusesOverlapPartialAndUnobservedBindings(t *testing.T) {
	for name, change := range map[string]func(*diagnosticMPISelection, []diagnosticMPIParticipantFacts){
		"management interface": func(s *diagnosticMPISelection, f []diagnosticMPIParticipantFacts) { s.FabricMembers[0].Interface = f[0].Interface },
		"partial":              func(s *diagnosticMPISelection, _ []diagnosticMPIParticipantFacts) { s.FabricMembers = s.FabricMembers[:1] },
		"unbound members":      func(s *diagnosticMPISelection, _ []diagnosticMPIParticipantFacts) { s.Fabric = diagnosticMPIFabric{} },
		"ring recipe":          func(s *diagnosticMPISelection, _ []diagnosticMPIParticipantFacts) { s.Fabric.RecipeID = fabricRingRecipe },
		"unrouted ring recipe": func(s *diagnosticMPISelection, _ []diagnosticMPIParticipantFacts) { s.Fabric.RecipeID = fabricRingRetainedRecipe },
		"split lane":           func(s *diagnosticMPISelection, _ []diagnosticMPIParticipantFacts) { s.FabricMembers[1].Address = "10.60.0.6" },
		"public address":       func(s *diagnosticMPISelection, _ []diagnosticMPIParticipantFacts) { s.FabricMembers[1].Address = "198.51.100.2" },
		"missing owner":        func(s *diagnosticMPISelection, _ []diagnosticMPIParticipantFacts) { s.Fabric.OwnerPrincipal = "" },
		"unobserved":           func(_ *diagnosticMPISelection, f []diagnosticMPIParticipantFacts) { f[1].Interfaces = f[1].Interfaces[:1] },
		"second address": func(_ *diagnosticMPISelection, f []diagnosticMPIParticipantFacts) {
			f[1].Interfaces[1].Addresses = append(f[1].Interfaces[1].Addresses, "10.61.0.2/30")
		},
		"down": func(_ *diagnosticMPISelection, f []diagnosticMPIParticipantFacts) { f[1].Interfaces[1].Up = false },
	} {
		t.Run(name, func(t *testing.T) {
			record, facts, selection, now := mpiFabricPlanFixture(t, 2)
			change(&selection, facts)
			_, raw, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
			if err == nil || len(raw) != 0 || len(key) != 0 {
				clear(key)
				t.Fatal("an invalid fabric binding produced a usable plan or identity")
			}
		})
	}
}

func diagnosticMPIFabricProfile(t *testing.T) diagnosticProfile {
	t.Helper()
	p := bootstrapProfileFixture(t)
	p.Fabric = diagnosticMPIFabric{OperationID: strings.Repeat("e", 32), QualificationDigest: strings.Repeat("f", 64), RecipeID: fabricRecipe, OwnerNodeID: "node-a", OwnerPrincipal: "node-a"}
	for i := range p.Members {
		p.Members[i].Fabric = diagnosticMemberFabric{Interface: "enp1s0f0np0", Index: 5, MAC: fmt.Sprintf("02:00:00:0a:00:%02x", i+1), Address: []string{"10.60.0.1", "10.60.0.2"}[i], Prefix: 30}
	}
	return p
}

func TestDiagnosticMPIFabricProfileKeepsMPIOnManagementAndValueEquality(t *testing.T) {
	management, err := json.Marshal(bootstrapProfileFixture(t))
	if err != nil || bytes.Contains(management, []byte(`"fabric"`)) {
		t.Fatal("the management profile gained fabric bytes")
	}
	p := diagnosticMPIFabricProfile(t)
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	var restored diagnosticProfile
	if err := json.Unmarshal(raw, &restored); err != nil || restored.Members[1] != p.Members[1] || restored.Fabric != p.Fabric {
		t.Fatal("fabric binding did not survive as comparable values")
	}
	env, err := diagnosticManagedRankEnvironment(p, p.Members[1], nil)
	if err != nil || !strings.Contains(strings.Join(env, "\n"), "NCCL_SOCKET_IFNAME==enp1s0f0np0\n") || !strings.Contains(strings.Join(env, "\n"), "OMPI_MCA_oob_tcp_if_include=10.50.0.0/24") || !strings.Contains(strings.Join(env, "\n"), "OMPI_MCA_btl_tcp_if_include=10.50.0.0/24") {
		t.Fatalf("rank mirror moved more than NCCL Socket: %v %v", env, err)
	}
	for i, mutate := range []func(*diagnosticProfile){
		func(p *diagnosticProfile) { p.Members[1].Fabric = diagnosticMemberFabric{} },
		func(p *diagnosticProfile) { p.Fabric = diagnosticMPIFabric{} },
		func(p *diagnosticProfile) { p.Members[0].Fabric.Interface = p.Members[0].Interface },
		func(p *diagnosticProfile) { p.Members[0].Fabric.Address, p.Members[1].Fabric.Address = "10.50.0.1", "10.50.0.2" },
		func(p *diagnosticProfile) { p.Fabric.RecipeID = fabricRingRecipe },
		func(p *diagnosticProfile) { p.Members[1].Fabric.Prefix = 31 },
	} {
		changed := diagnosticMPIFabricProfile(t)
		mutate(&changed)
		if changed.validate() == nil {
			t.Fatalf("fabric mutation %d was accepted", i)
		}
	}
	legacy := diagnosticTestProfile()
	legacy.Fabric = p.Fabric
	if legacy.validate() == nil {
		t.Fatal("an operator profile accepted a fabric binding")
	}
}

func TestDiagnosticRankFabricCheckRequiresTheReviewedSoleAddress(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:0a:00:01")
	member := diagnosticMemberFabric{Interface: "enp1s0f0np0", Index: 5, MAC: "02:00:00:0a:00:01", Address: "10.60.0.1", Prefix: 30}
	addresses := func(values ...string) func(*net.Interface) ([]net.Addr, error) {
		return func(*net.Interface) ([]net.Addr, error) {
			result := []net.Addr{}
			for _, value := range values {
				ip, network, err := net.ParseCIDR(value)
				if err != nil {
					t.Fatal(err)
				}
				result = append(result, &net.IPNet{IP: ip, Mask: network.Mask})
			}
			return result, nil
		}
	}
	lookup := func(iface net.Interface) func(string) (*net.Interface, error) {
		return func(name string) (*net.Interface, error) {
			if name != iface.Name {
				return nil, errors.New("interface absent")
			}
			return &iface, nil
		}
	}
	current := net.Interface{Name: member.Interface, Index: 5, HardwareAddr: mac, Flags: net.FlagUp}
	if err := verifyDiagnosticMemberFabric(member, lookup(current), addresses("10.60.0.1/30", "fe80::1/64")); err != nil {
		t.Fatal(err)
	}
	other, _ := net.ParseMAC("02:00:00:0a:00:09")
	for name, test := range map[string]struct {
		iface     net.Interface
		addresses func(*net.Interface) ([]net.Addr, error)
	}{
		"index":          {net.Interface{Name: member.Interface, Index: 6, HardwareAddr: mac, Flags: net.FlagUp}, addresses("10.60.0.1/30")},
		"mac":            {net.Interface{Name: member.Interface, Index: 5, HardwareAddr: other, Flags: net.FlagUp}, addresses("10.60.0.1/30")},
		"down":           {net.Interface{Name: member.Interface, Index: 5, HardwareAddr: mac}, addresses("10.60.0.1/30")},
		"renamed":        {net.Interface{Name: "enp1s0f1np1", Index: 5, HardwareAddr: mac, Flags: net.FlagUp}, addresses("10.60.0.1/30")},
		"second address": {current, addresses("10.60.0.1/30", "10.61.0.1/30")},
		"prefix":         {current, addresses("10.60.0.1/31")},
		"lost address":   {current, addresses("fe80::1/64")},
		"unreadable":     {current, func(*net.Interface) ([]net.Addr, error) { return nil, errors.New("netlink unavailable") }},
	} {
		if err := verifyDiagnosticMemberFabric(member, lookup(test.iface), test.addresses); err == nil {
			t.Fatalf("%s: a changed fabric interface admitted the rank", name)
		}
	}
}

func TestDiagnosticMPIFactsObserveTheFabricEndBesideManagement(t *testing.T) {
	now := time.UnixMilli(1800000000000)
	fixture := `{"hostUuid":"spark-b","connections":{"source":"host-os","status":"observed","observedAt":1799999999900,"interfaces":[{"name":"control0","index":7,"mac":"02:00:00:00:00:07","physical":true,"up":true,"addresses":["192.0.2.10/24"]},{"name":"fabric0","index":3,"mac":"02:00:00:0a:00:01","physical":true,"up":true,"addresses":["10.253.0.4/31"],"physicalPort":{"source":"linux-sysfs","switchId":"0011223344556677","portName":"p0"}}]}}`
	parse := func(text string) diagnosticMPIConnections {
		var info diagnosticMPIConnections
		if err := json.Unmarshal([]byte(text), &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	target := diagnosticManagedTarget{NodeID: "spark-b", Principal: "spark-b"}
	request := diagnosticMPIFactsRequest{Network: "fabric", Target: target, Fabric: diagnosticMemberFabric{Interface: "fabric0", Index: 3, MAC: "02:00:00:0a:00:01", Address: "10.253.0.4", Prefix: 31},
		Selection: diagnosticMPIInterfaceSelection{Network: "fabric", NodeID: "spark-b", Principal: "spark-b", SwitchID: "0011223344556677", PortName: "p0", Interface: cableprobe.Interface{Name: "fabric0", Index: 3, MAC: "02:00:00:0a:00:01"}}}
	if !validDiagnosticMPIFactsNetwork(request) {
		t.Fatal("a consistent fabric facts request was refused")
	}
	if err := diagnosticMPIFabricObserved(parse(fixture), request, "control0", now); err != nil {
		t.Fatal(err)
	}
	management := diagnosticMPIInterfaceSelection{Network: "management", NodeID: "spark-b", Principal: "spark-b", Interface: cableprobe.Interface{Name: "control0", Index: 7, MAC: "02:00:00:00:00:07"}}
	if _, err := projectDiagnosticMPIAddress(parse(fixture), management, now); err != nil {
		t.Fatal(err)
	}
	if _, err := projectDiagnosticMPIAddress(parse(strings.Replace(fixture, `"192.0.2.10/24"`, `"192.0.2.10/31"`, 1)), management, now); err == nil {
		t.Fatal("the management baseline accepted a point-to-point /31")
	}
	if err := diagnosticMPIFabricObserved(parse(fixture), request, "fabric0", now); err == nil {
		t.Fatal("the NCCL fabric end was accepted as the management interface")
	}
	for _, change := range []struct{ from, to string }{
		{`"10.253.0.4/31"`, `"10.253.0.6/31"`},
		{`"10.253.0.4/31"`, `"10.253.0.5/30"`},
		{`"10.253.0.4/31"`, `"10.253.0.4/31","10.254.0.4/31"`},
		{`"index":3`, `"index":4`},
		{`"portName":"p0"`, `"portName":"p1"`},
		{`"up":true,"addresses":["10.253.0.4/31"]`, `"up":false,"addresses":["10.253.0.4/31"]`},
	} {
		if err := diagnosticMPIFabricObserved(parse(strings.Replace(fixture, change.from, change.to, 1)), request, "control0", now); err == nil {
			t.Fatalf("changed fabric facts admitted: %s", change.to)
		}
	}
	for name, mutate := range map[string]func(*diagnosticMPIFactsRequest){
		"management with selection": func(r *diagnosticMPIFactsRequest) { r.Network = "management" },
		"other interface":           func(r *diagnosticMPIFactsRequest) { r.Selection.Interface.Index = 4 },
		"other participant":         func(r *diagnosticMPIFactsRequest) { r.Selection.NodeID = "spark-c" },
		"missing port":              func(r *diagnosticMPIFactsRequest) { r.Selection.PortName = "" },
		"unknown network":           func(r *diagnosticMPIFactsRequest) { r.Network = "rdma" },
	} {
		changed := request
		mutate(&changed)
		if validDiagnosticMPIFactsNetwork(changed) {
			t.Fatalf("%s facts request was accepted", name)
		}
	}
	if !validDiagnosticMPIFactsNetwork(diagnosticMPIFactsRequest{Network: "management", Target: target}) {
		t.Fatal("the management facts request changed shape")
	}
}

func TestDiagnosticMPIFabricRevalidationRefusesAChangedOrRolledBackFabric(t *testing.T) {
	s, r := diagnosticMPIDirectFabricFixture(t)
	d := diagnosticMPIFabricOwner(t, s)
	check := diagnosticMPIFabricCheckFor(t, r, "node-a")
	if err := d.currentMPIFabric(context.Background(), check); err != nil {
		t.Fatal(err)
	}
	p := diagnosticMPIFabricProfile(t)
	p.Fabric = check.Fabric
	for i := range p.Members {
		p.Members[i].Fabric = check.Members[i]
	}
	if err := d.mpiFabricCurrent(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	d.m.exec.fabric = nil
	if err := d.mpiFabricCurrent(context.Background(), bootstrapProfileFixture(t)); err != nil {
		t.Fatalf("a management plan consulted a fabric: %v", err)
	}
	for name, spoil := range map[string]func(*fabricService, *fabricRunRecord, *diagnosticMPIFabricCheck){
		"rolled back":     func(_ *fabricService, r *fabricRunRecord, _ *diagnosticMPIFabricCheck) { r.Public.State = "cancelled" },
		"rolling back":    func(_ *fabricService, r *fabricRunRecord, _ *diagnosticMPIFabricCheck) { r.Public.State = "rolling-back" },
		"proof withdrawn": func(s *fabricService, r *fabricRunRecord, _ *diagnosticMPIFabricCheck) { delete(s.qualified, r.Public.OperationID) },
		"reapplied": func(s *fabricService, r *fabricRunRecord, _ *diagnosticMPIFabricCheck) {
			delete(s.runs, r.Public.OperationID)
			next := *r
			next.Public.OperationID = strings.Repeat("b", 32)
			next.Public.QualificationDigest = fabricQualificationDigest(next.Public.OperationID, next.Public.RecipeID, next.Public.Targets, next.Public.CandidateIPs)
			s.runs[next.Public.OperationID], s.qualified[next.Public.OperationID] = &next, next.Public.QualificationDigest
		},
		"other owner":  func(_ *fabricService, _ *fabricRunRecord, c *diagnosticMPIFabricCheck) { c.Fabric.OwnerNodeID = "node-b" },
		"other lane":   func(_ *fabricService, _ *fabricRunRecord, c *diagnosticMPIFabricCheck) { c.Members[1].MAC = "02:00:00:0a:00:09" },
		"other member": func(_ *fabricService, _ *fabricRunRecord, c *diagnosticMPIFabricCheck) { c.NodeIDs[1] = "node-c" },
	} {
		t.Run(name, func(t *testing.T) {
			s, r := diagnosticMPIDirectFabricFixture(t)
			d := diagnosticMPIFabricOwner(t, s)
			check := diagnosticMPIFabricCheckFor(t, r, "node-a")
			spoil(s, r, &check)
			if err := d.currentMPIFabric(context.Background(), check); err == nil {
				t.Fatal("a changed fabric passed revalidation")
			}
		})
	}
	t.Run("status proof settles", func(t *testing.T) {
		s, r := diagnosticMPIDirectFabricFixture(t)
		d := diagnosticMPIFabricOwner(t, s)
		check := diagnosticMPIFabricCheckFor(t, r, "node-a")
		proof := make(chan struct{})
		s.mu.Lock()
		digest := s.qualified[r.Public.OperationID]
		delete(s.qualified, r.Public.OperationID)
		r.done = proof
		s.mu.Unlock()
		go func() {
			time.Sleep(50 * time.Millisecond)
			s.mu.Lock()
			s.qualified[r.Public.OperationID] = digest
			s.mu.Unlock()
			close(proof)
		}()
		if err := d.currentMPIFabric(context.Background(), check); err != nil {
			t.Fatalf("a status re-proof in flight refused an unchanged fabric: %v", err)
		}
		s.mu.Lock()
		delete(s.qualified, r.Public.OperationID)
		r.done = make(chan struct{})
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := d.currentMPIFabric(ctx, check); err == nil {
			t.Fatal("an unsettled proof passed revalidation")
		}
	})
}

func TestDiagnosticMPIFabricCoordinatorAsksTheRemoteFabricOwner(t *testing.T) {
	d := diagnosticTestService(t)
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", "node-a", "controller", "node-b")
	d.m.mesh = clustertrust.Open(dir)
	d.m.cableLocal = newCableLocalFacts("node-a", 0)
	d.m.peers.set([]noderec.DirectoryNode{{HostUUID: "controller", ClusterUUID: "controller", IP: "192.0.2.9", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: 14323}}}})
	pool := clustertrust.NewPeerClientPool(d.m.mesh, 0)
	d.m.remoteHTTP, d.m.readyHTTP = pool, pool
	client, ok := pool.Client("controller")
	if !ok {
		t.Fatal("fixture fabric owner is not pinned")
	}
	p := diagnosticMPIFabricProfile(t)
	p.Fabric.OwnerNodeID, p.Fabric.OwnerPrincipal = "controller", "controller"
	status, answer := http.StatusOK, p.Fabric
	requests := 0
	client.Transport = mpiReviewMemoryTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		var request diagnosticControlRequest
		if req.URL.Path != diagnosticControlPath || json.NewDecoder(req.Body).Decode(&request) != nil || request.Method != "mpi-fabric" || request.MPIFabric == nil || request.MPIReview != nil || request.Participant != nil || request.Bootstrap != nil {
			t.Fatal("the coordinator sent more than the fixed fabric revalidation")
		}
		check := request.MPIFabric
		if check.Fabric != p.Fabric || strings.Join(check.NodeIDs, ",") != "node-a,node-b" || strings.Join(check.Principals, ",") != "node-a,node-b" || len(check.Members) != 2 || check.Members[0] != p.Members[0].Fabric || check.Members[1] != p.Members[1].Fabric {
			t.Fatalf("fabric revalidation did not carry the exact plan binding: %+v", check)
		}
		body, _ := json.Marshal(answer)
		if status != http.StatusOK {
			body = []byte("the reviewed fabric changed or was rolled back")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	if err := d.mpiFabricCurrent(context.Background(), p); err != nil || requests != 1 {
		t.Fatalf("the fabric owner's confirmation was refused: %v requests=%d", err, requests)
	}
	answer.QualificationDigest = strings.Repeat("0", 64)
	if err := d.mpiFabricCurrent(context.Background(), p); err == nil {
		t.Fatal("a different confirmed fabric was accepted")
	}
	status = http.StatusConflict
	if err := d.mpiFabricCurrent(context.Background(), p); err == nil {
		t.Fatal("a refused revalidation was accepted")
	}
	p.Fabric.OwnerPrincipal = "node-b"
	if err := d.mpiFabricCurrent(context.Background(), p); err == nil || requests != 3 {
		t.Fatalf("an owner identity mismatch reached the network: %v requests=%d", err, requests)
	}
}

// mpiFabricReviewFixture publishes a direct-fabric review whose fabric the
// local coordinator also owns, so revalidation stays in process.
func mpiFabricReviewFixture(t *testing.T) (*diagnosticService, *diagnosticMPIReviewBinding, *fabricService, *fabricRunRecord) {
	t.Helper()
	d := diagnosticTestService(t)
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", "node-a", "node-b")
	d.m.mesh = clustertrust.Open(dir)
	s, r := diagnosticMPIDirectFabricFixture(t)
	d.m.exec.fabric, d.m.cableLocal = s, newCableLocalFacts("node-a", 0)
	check := diagnosticMPIFabricCheckFor(t, r, "node-a")
	p := bootstrapProfileFixture(t)
	p.Fabric = check.Fabric
	for i := range p.Members {
		p.Members[i].Fabric = check.Members[i]
	}
	request := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: p.Bootstrap.OperationID, ProfileDigest: profileDigest(p), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	body := map[string]any{"schemaVersion": 1, "recipeId": diagnosticMPIQuickRecipe, "operationId": request.OperationID, "groupId": request.GroupID, "ownerNodeId": p.OwnerNodeID, "profileDigest": request.ProfileDigest, "createdAt": request.ExpiresAt - 60000, "expiresAt": request.ExpiresAt}
	canonical, _ := json.Marshal(body)
	sum := sha256.Sum256(canonical)
	body["planDigest"] = hex.EncodeToString(sum[:])
	plan, _ := json.Marshal(body)
	pin, ok := d.m.mesh.PinSHA256("node-b")
	if !ok {
		t.Fatal("fixture controller pin missing")
	}
	bound := &diagnosticMPIReviewBinding{Controller: "node-b", ControllerPin: pin, Profile: p, Plan: plan, PrivateKey: []byte("volatile-mpi-review-key-fixture"),
		Public: diagnosticMPIReview{ReviewID: strings.Repeat("d", 32), BuildOperationID: strings.Repeat("c", 32), OperationID: request.OperationID, GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, RecipeID: diagnosticMPIQuickRecipe, ExpiresAt: request.ExpiresAt}}
	d.mu.Lock()
	err := d.publishMPIReviewLocked(bound, mpiReviewStorage())
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return d, bound, s, r
}

func TestDiagnosticMPIApproveRevalidatesTheFabricBeforeConsumingTheReview(t *testing.T) {
	for name, spoil := range map[string]func(*fabricService, *fabricRunRecord){
		"rolled back":     func(_ *fabricService, r *fabricRunRecord) { r.Public.State = "cancelled" },
		"proof withdrawn": func(s *fabricService, r *fabricRunRecord) { delete(s.qualified, r.Public.OperationID) },
		"requalified differently": func(s *fabricService, r *fabricRunRecord) {
			r.Public.CandidateIPs[0].GIDIndex++
			r.Public.QualificationDigest = fabricQualificationDigest(r.Public.OperationID, r.Public.RecipeID, r.Public.Targets, r.Public.CandidateIPs)
			s.qualified[r.Public.OperationID] = r.Public.QualificationDigest
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, bound, s, r := mpiFabricReviewFixture(t)
			spoil(s, r)
			if _, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), mpiReviewNoStart(t)); err == nil {
				t.Fatal("approval consumed a review whose fabric changed")
			}
			marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
			if err != nil || marker.State != "unconsumed" || len(bound.PrivateKey) == 0 {
				t.Fatalf("a fabric refusal consumed the review: %+v %v", marker, err)
			}
			result, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
			if err != nil || !result.ReviewClosed {
				t.Fatalf("a fabric refusal left the review unclosable: %+v %v", result, err)
			}
		})
	}
	d, bound, _, _ := mpiFabricReviewFixture(t)
	started := false
	op, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), func(p diagnosticProfile, _ json.RawMessage, key []byte) (diagnosticOperation, error) {
		defer clear(key)
		started = p.Fabric == bound.Profile.Fabric
		op := mpiReviewTestOperation(bound)
		op.RecipeID = diagnosticMPIQuickRecipe
		return op, nil
	})
	if err != nil || !started || op.OperationID != bound.Public.OperationID {
		t.Fatalf("an unchanged fabric did not reach its one start: %+v %v", op, err)
	}
}

func TestDiagnosticMPIGoFabricPlanBindsInNativePortWithoutEffects(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			record, facts, selection, now := mpiFabricPlanFixture(t, count)
			testDiagnosticMPIGoPlanBindsInNativePort(t, record, facts, selection, now)
		})
	}
}

func TestDiagnosticMPIGoFabricPlanMatchesPinnedPythonAdapter(t *testing.T) {
	source, _, err := canonicalDiagnosticMPIPythonSources(diagnosticMPIAdapterPython, diagnosticMPINativePython)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("Python is required for this pure adapter schema check")
	}
	for _, count := range []int{2, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			record, facts, selection, now := mpiFabricPlanFixture(t, count)
			_, plan, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
			defer clear(key)
			if err != nil {
				t.Fatal(err)
			}
			sockets := []string{}
			for _, member := range selection.FabricMembers {
				sockets = append(sockets, member.Interface)
			}
			input, _ := json.Marshal(map[string]any{"source": source, "plan": json.RawMessage(plan), "now": now.UnixMilli(), "subnet": selection.Subnet, "sockets": sockets})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			code := "import sys,json\nx=json.load(sys.stdin)\ns={'__name__':'mpi_socket_adapter'}\nexec(x['source'],s)\np=s['validate_plan'](x['plan'],x['now'])\nspec=s['compile_spec'](p)\nassert spec['fixedMCA']['btl_tcp_if_include']==spec['fixedMCA']['oob_tcp_if_include']==x['subnet']\nfor m,socket in zip(p['members'],x['sockets']):\n assert spec['rankEnvironments'][m['nodeId']]['NCCL_SOCKET_IFNAME']=='='+socket\n assert '-host '+m['collectiveAddress']+' ' in spec['coordinator']['mpiApp']\nprint('fabric plan keeps MPI on management')\n"
			cmd := exec.CommandContext(ctx, python, "-I", "-c", code)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "PYTHONDONTWRITEBYTECODE=1"}
			cmd.Stdin = bytes.NewReader(input)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("pinned adapter refused the Go fabric plan: %v; %s", err, output)
			}
		})
	}
}
