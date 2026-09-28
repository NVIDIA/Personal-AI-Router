// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A native Qwen2 configuration whose heads, KV heads, linear dimensions and
// 64-row padded vocabulary all divide three ways.
const ringSocketTP3Config = `{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","num_attention_heads":24,"num_key_value_heads":3,"num_hidden_layers":24,"hidden_size":1536,"intermediate_size":8448,"vocab_size":32064}`

// ringSocketTestEndpoints are the qualified candidates of a routed ring whose
// roles follow nodes: six lane rows, then one routed proof per member.
func ringSocketTestEndpoints(t *testing.T, nodes ...string) []fabricCandidateIP {
	t.Helper()
	targets := make([]fabricTarget, 0, len(nodes))
	for _, node := range nodes {
		targets = append(targets, ringTestTarget(node, "principal-"+node, "switch-"+node))
	}
	empty := fabricInventory{Interfaces: []fabricObservedInterface{}, Routes: []string{}}
	if err := fabricAllocateRing(targets, []fabricInventory{empty, empty, empty}); err != nil {
		t.Fatal(err)
	}
	candidates, err := fabricCandidates(fabricRingRecipe, targets)
	if err != nil {
		t.Fatal(err)
	}
	return fabricTestProve(candidates, func(int) int { return 3 })
}

func ringSocketTestBinding(t *testing.T, nodes ...string) *vllmGroupRingSocket {
	t.Helper()
	principals := make([]string, len(nodes))
	for i, node := range nodes {
		principals[i] = "principal-" + node
	}
	facts := make([]vllmGroupFacts, len(nodes))
	if err := bindVLLMGroupRingSocketFacts(vllmGroupSelection{NodeIDs: nodes}, facts, principals, strings.Repeat("a", 32), strings.Repeat("b", 64), ringSocketTestEndpoints(t, nodes...)); err != nil {
		t.Fatal(err)
	}
	return facts[0].ringSocket
}

func ringSocketTestPlan(t *testing.T, tensor bool) vllmGroupPlan {
	t.Helper()
	p := vllmGroupTestPlan(3)
	if tensor {
		p.Topology.TensorParallel, p.Topology.PipelineParallel = 3, 1
	}
	p.RingSocket = ringSocketTestBinding(t, "node-a", "node-b", "node-c")
	return p
}

// qualifiedRingFabricFixture is one active, freshly qualified routed ring over
// node-a, node-b and node-c whose proofs answer from the retained candidates.
func qualifiedRingFabricFixture(t *testing.T) (*fabricService, *fabricRunRecord) {
	t.Helper()
	s, r := fabricRingServiceFixture(t)
	close(r.done)
	r.Public.State, r.Public.EffectsApplied = "active", true
	for i := range r.Attempted {
		r.Attempted[i] = true
	}
	candidates, err := fabricCandidates(r.Public.RecipeID, r.Public.Targets)
	if err != nil {
		t.Fatal(err)
	}
	gid := func(int) int { return 3 }
	r.Public.CandidateIPs = fabricTestProve(candidates, gid)
	r.Public.QualifiedAt = time.Now().UnixMilli()
	r.Public.QualificationDigest = fabricQualificationDigest(r.Public.OperationID, r.Public.RecipeID, r.Public.Targets, r.Public.CandidateIPs)
	s.qualified[r.Public.OperationID] = r.Public.QualificationDigest
	s.control = func(_ context.Context, _ fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		return fabricControlResult{Qualified: true, Candidates: fabricTestProve(request.Candidates, gid)}, nil
	}
	if err := s.save(r); err != nil {
		t.Fatal(err)
	}
	return s, r
}

// ringSocketFabricPlan is a reviewable ordinary PP3 plan bound to the ring of
// qualifiedRingFabricFixture.
func ringSocketFabricPlan(t *testing.T, r *fabricRunRecord) vllmGroupPlan {
	t.Helper()
	p := vllmGroupTestPlan(3)
	nodes, principals := make([]string, len(p.Members)), make([]string, len(p.Members))
	for i, member := range p.Members {
		nodes[i] = member.NodeID
		for _, target := range r.Public.Targets {
			if target.NodeID == member.NodeID {
				principals[i] = target.Principal
			}
		}
	}
	facts := make([]vllmGroupFacts, len(nodes))
	if err := bindVLLMGroupRingSocketFacts(vllmGroupSelection{NodeIDs: nodes}, facts, principals, r.Public.OperationID, r.Public.QualificationDigest, r.Public.CandidateIPs); err != nil {
		t.Fatal(err)
	}
	p.RingSocket = facts[0].ringSocket
	if _, err := vllmGroupPlanDigest(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRingSocketContractIsDigestedAndLimitedToOrdinaryThreeNodeGroups(t *testing.T) {
	for _, tensor := range []bool{false, true} {
		if _, err := vllmGroupPlanDigest(ringSocketTestPlan(t, tensor)); err != nil {
			t.Fatalf("an ordinary three-node plan (tensor=%v) refused its ring socket: %v", tensor, err)
		}
	}
	plan := ringSocketTestPlan(t, false)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	changed := cloneVLLMGroupPlan(plan)
	changed.RingSocket.Members[1].InterfaceName = "enP2p1s0f0np0"
	if other, err := vllmGroupPlanDigest(changed); err != nil || other == digest {
		t.Fatalf("the ring binding is not part of the plan digest: %v", err)
	}
	changed.RingSocket.Members[1].LaneAddresses[0] = "10.253.0.9"
	if plan.RingSocket.Members[1].InterfaceName != "node-b-p0" || plan.RingSocket.Members[1].LaneAddresses[0] != "10.253.0.1" {
		t.Fatal("the ring binding is not deep-copied with the plan")
	}
	for name, spoil := range map[string]func(*vllmGroupPlan){
		"two nodes":          func(p *vllmGroupPlan) { ring := p.RingSocket; *p = vllmGroupTestPlan(2); p.RingSocket = ring },
		"qwen profile":       func(p *vllmGroupPlan) { p.Model = vllmQwen38ModelID },
		"retained runtime":   func(p *vllmGroupPlan) { p.Runtime = "0.28.0" },
		"direct socket too":  func(p *vllmGroupPlan) { p.DirectSocket = directSocketTestPlan().DirectSocket },
		"data parallel":      func(p *vllmGroupPlan) { p.Topology.PipelineParallel, p.Topology.DataParallel = 1, 3 },
		"member order":       func(p *vllmGroupPlan) { m := p.RingSocket.Members; m[0], m[1] = m[1], m[0] },
		"management address": func(p *vllmGroupPlan) { p.Members[2].Placement = &vllmGroupPlacement{Address: "10.253.0.5"} },
		"upper-case mac":     func(p *vllmGroupPlan) { p.RingSocket.Members[0].MAC = "02:00:00:0A:00:01" },
		"roce mode":          func(p *vllmGroupPlan) { p.RingSocket.Mode = vllmQwen38Transport },
		"direct mode":        func(p *vllmGroupPlan) { p.RingSocket.Mode = vllmGroupDirectSocketMode },
		"rdma lanes":         func(p *vllmGroupPlan) { p.Members[0].Fabric = &vllmGroupMemberFabric{} },
		"two members":        func(p *vllmGroupPlan) { p.RingSocket.Members = p.RingSocket.Members[:2] },
		"missing operation":  func(p *vllmGroupPlan) { p.RingSocket.OperationID = "" },
		"interface name":     func(p *vllmGroupPlan) { p.RingSocket.Members[0].InterfaceName = "a very long interface" },
		"interface index":    func(p *vllmGroupPlan) { p.RingSocket.Members[0].InterfaceIndex = 0 },
		"advertised off its lanes": func(p *vllmGroupPlan) {
			p.RingSocket.Members[1].AdvertisedAddress = "10.253.0.5"
		},
		"unordered lanes": func(p *vllmGroupPlan) {
			lanes := p.RingSocket.Members[0].LaneAddresses
			lanes[0], lanes[1] = lanes[1], lanes[0]
		},
		"one lane": func(p *vllmGroupPlan) {
			p.RingSocket.Members[0].LaneAddresses = p.RingSocket.Members[0].LaneAddresses[:1]
		},
		"shared lane":    func(p *vllmGroupPlan) { p.RingSocket.Members[2].LaneAddresses = []string{"10.253.0.2", "10.253.0.3"} },
		"not a ring":     func(p *vllmGroupPlan) { p.RingSocket.Members[2].LaneAddresses = []string{"10.253.0.3", "10.253.0.7"} },
		"public address": func(p *vllmGroupPlan) { p.RingSocket.Members[0].LaneAddresses = []string{"10.253.0.0", "198.51.100.2"} },
	} {
		t.Run(name, func(t *testing.T) {
			spoiled := cloneVLLMGroupPlan(plan)
			spoil(&spoiled)
			if _, err := vllmGroupPlanDigest(spoiled); err == nil {
				t.Fatal("an invalid ring socket plan was digested")
			}
		})
	}
}

func TestRingSocketBindingUsesRoutedRingFacts(t *testing.T) {
	ring := ringSocketTestBinding(t, "node-a", "node-b", "node-c")
	want := []vllmGroupRingSocketMember{
		{NodeID: "node-a", InterfaceName: "node-a-p0", InterfaceIndex: 2, MAC: "02:00:00:00:00:01", AdvertisedAddress: "10.253.0.0", LaneAddresses: []string{"10.253.0.0", "10.253.0.2"}},
		{NodeID: "node-b", InterfaceName: "node-b-p0", InterfaceIndex: 2, MAC: "02:00:00:00:00:01", AdvertisedAddress: "10.253.0.4", LaneAddresses: []string{"10.253.0.1", "10.253.0.4"}},
		{NodeID: "node-c", InterfaceName: "node-c-p0", InterfaceIndex: 2, MAC: "02:00:00:00:00:01", AdvertisedAddress: "10.253.0.3", LaneAddresses: []string{"10.253.0.3", "10.253.0.5"}},
	}
	if ring == nil || ring.Mode != vllmGroupRingSocketMode || ring.OperationID != strings.Repeat("a", 32) || ring.QualificationSHA256 != strings.Repeat("b", 64) || !reflect.DeepEqual(ring.Members, want) {
		t.Fatalf("the ring socket did not bind each member's advertised p0 interface and lanes: %+v", ring)
	}
	operation, qualification := strings.Repeat("a", 32), strings.Repeat("b", 64)
	selection := vllmGroupSelection{NodeIDs: []string{"node-c", "node-a", "node-b"}}
	principals := []string{"principal-node-c", "principal-node-a", "principal-node-b"}
	facts := make([]vllmGroupFacts, 3)
	if err := bindVLLMGroupRingSocketFacts(selection, facts, principals, operation, qualification, ringSocketTestEndpoints(t, "node-a", "node-b", "node-c")); err != nil ||
		facts[0].ringSocket.Members[0].NodeID != "node-c" || facts[0].ringSocket.Members[0].AdvertisedAddress != "10.253.0.3" {
		t.Fatalf("ring members did not follow the plan member order: %+v %v", facts[0].ringSocket, err)
	}
	for name, spoil := range map[string]func([]fabricCandidateIP) []fabricCandidateIP{
		"foreign principal": func(e []fabricCandidateIP) []fabricCandidateIP { e[0].PeerPrincipal = "principal-x"; return e },
		"outside member":    func(e []fabricCandidateIP) []fabricCandidateIP { e[1].PeerNodeID = "node-x"; return e },
		"lane proofs only":  func(e []fabricCandidateIP) []fabricCandidateIP { return fabricLaneEndpoints(e) },
		"routed proof on p1": func(e []fabricCandidateIP) []fabricCandidateIP {
			e[6].InterfaceName, e[6].InterfaceIndex = "node-a-p1", 3
			return e
		},
		"advertised unreachable": func(e []fabricCandidateIP) []fabricCandidateIP { e[6].PeerAddress = "10.253.0.5"; return e },
	} {
		endpoints := spoil(ringSocketTestEndpoints(t, "node-a", "node-b", "node-c"))
		principals := []string{"principal-node-a", "principal-node-b", "principal-node-c"}
		if err := bindVLLMGroupRingSocketFacts(vllmGroupSelection{NodeIDs: []string{"node-a", "node-b", "node-c"}}, make([]vllmGroupFacts, 3), principals, operation, qualification, endpoints); err == nil {
			t.Fatalf("%s bound a ring socket", name)
		}
	}
	two := vllmGroupSelection{NodeIDs: []string{"node-a", "node-b"}}
	if err := bindVLLMGroupRingSocketFacts(two, make([]vllmGroupFacts, 2), []string{"principal-node-a", "principal-node-b"}, operation, qualification, ringSocketTestEndpoints(t, "node-a", "node-b", "node-c")); err == nil {
		t.Fatal("a two-node selection bound a ring socket")
	}
}

func TestRingSocketRevalidationRequiresTheSameQualifiedRing(t *testing.T) {
	plan := ringSocketTestPlan(t, false)
	owner := directFabricTestOwner{operation: plan.RingSocket.OperationID, digest: plan.RingSocket.QualificationSHA256, endpoints: ringSocketTestEndpoints(t, "node-a", "node-b", "node-c")}
	if err := currentVLLMGroupRingSocket(owner, plan); err != nil {
		t.Fatalf("the exact qualified ring was refused: %v", err)
	}
	for name, change := range map[string]func(*directFabricTestOwner){
		"requalified digest":         func(o *directFabricTestOwner) { o.digest = strings.Repeat("c", 64) },
		"other operation":            func(o *directFabricTestOwner) { o.operation = strings.Repeat("d", 32) },
		"advertised interface moved": func(o *directFabricTestOwner) { o.endpoints[4].InterfaceIndex = 9 },
		"lane readdressed":           func(o *directFabricTestOwner) { o.endpoints[5].Address = "10.253.0.7" },
		"routes withdrawn":           func(o *directFabricTestOwner) { o.endpoints = fabricLaneEndpoints(o.endpoints) },
		"unqualified":                func(o *directFabricTestOwner) { o.err = errors.New("qualified active fabric is unavailable") },
	} {
		changed := owner
		changed.endpoints = ringSocketTestEndpoints(t, "node-a", "node-b", "node-c")
		change(&changed)
		if err := currentVLLMGroupRingSocket(changed, plan); err == nil {
			t.Fatalf("%s kept a stale ring socket usable", name)
		}
	}
	if err := currentVLLMGroupRingSocket(struct{}{}, plan); err == nil {
		t.Fatal("a ring socket plan passed without a fabric owner")
	}
}

func TestThreeNodeGroupsBindTheRingSocketOnlyOverAnActiveRing(t *testing.T) {
	s, facts, pins := groupPlanFactsFixture(3)
	for _, parallelism := range []string{"", "pipeline"} {
		s.Parallelism = parallelism
		plan, err := assembleVLLMGroupPlan(s, facts, pins)
		if err != nil || plan.Topology.PipelineParallel != 3 || plan.RingSocket != nil {
			t.Fatalf("genuine ring absence did not keep PP3 on the management network (%q): %+v %v", parallelism, plan.Topology, err)
		}
	}
	s.Parallelism = "tensor"
	if _, err := assembleVLLMGroupPlan(s, facts, pins); err == nil || errors.Is(err, errVLLMGroupTensorNeedsRingFabric) || !strings.Contains(err.Error(), "attention") {
		t.Fatalf("a model that cannot split three ways was not refused for its heads without a ring: %v", err)
	}
	ring := ringSocketTestBinding(t, s.NodeIDs...)
	facts[0].ringSocket = ring
	for _, parallelism := range []string{"", "pipeline"} {
		s.Parallelism = parallelism
		plan, err := assembleVLLMGroupPlan(s, facts, pins)
		if err != nil || plan.Topology.PipelineParallel != 3 || plan.RingSocket == nil || plan.RingSocket == ring || plan.RingSocket.Members[2].AdvertisedAddress != "10.253.0.3" {
			t.Fatalf("an active ring did not carry PP3 NCCL Socket (%q): %+v %v", parallelism, plan.RingSocket, err)
		}
	}
	s.Parallelism = "tensor"
	if _, err := assembleVLLMGroupPlan(s, facts, pins); err == nil || !strings.Contains(err.Error(), "attention") {
		t.Fatalf("a model that cannot split three ways was admitted as TP3: %v", err)
	}
	for i := range facts {
		facts[i].Config = []byte(ringSocketTP3Config)
	}
	if plan, err := assembleVLLMGroupPlan(s, facts, pins); err != nil || plan.Topology.TensorParallel != 3 || plan.Topology.PipelineParallel != 1 || plan.RingSocket == nil {
		t.Fatalf("an active ring did not admit TP3 for a model that splits three ways: %+v %v", plan.Topology, err)
	}
	facts[0].ringSocket = nil
	if _, err := assembleVLLMGroupPlan(s, facts, pins); !errors.Is(err, errVLLMGroupTensorNeedsRingFabric) {
		t.Fatalf("a TP3-capable model was admitted as TP3 without a ring: %v", err)
	}
	two, twoFacts, twoPins := groupPlanFactsFixture(2)
	twoFacts[0].ringSocket = ring
	if plan, err := assembleVLLMGroupPlan(two, twoFacts, twoPins); err != nil || plan.Topology.PipelineParallel != 2 || plan.RingSocket != nil {
		t.Fatalf("the two-node default changed: %+v %v", plan, err)
	}
}

func TestThreeNodeRingResolutionRefusesTheModelFirstAndFailsClosed(t *testing.T) {
	s, facts, _ := groupPlanFactsFixture(3)
	principals := []string{"principal-node-0", "principal-node-1", "principal-node-2"}
	errStale := errors.New("a stale or ambiguous fabric operation involves the selected Sparks")
	resolve := func(parallelism, config string, fabricErr error) (*vllmGroupRingSocket, bool, error) {
		selection := s
		selection.Parallelism = parallelism
		bound := slices.Clone(facts)
		for i := range bound {
			bound[i].Config = []byte(config)
		}
		consulted := false
		err := resolveVLLMGroupRingSocket(context.Background(), selection, bound, principals, func(context.Context, []string) (string, string, []fabricCandidateIP, error) {
			consulted = true
			if fabricErr != nil {
				return "", "", nil, fabricErr
			}
			return strings.Repeat("a", 32), strings.Repeat("b", 64), ringSocketTestEndpoints(t, s.NodeIDs...), nil
		})
		return bound[0].ringSocket, consulted, err
	}
	fourteenHeads := string(facts[0].Config)
	for _, fabricErr := range []error{nil, errNoFabric, errStale} {
		if _, consulted, err := resolve("tensor", fourteenHeads, fabricErr); consulted || err == nil || !strings.Contains(err.Error(), "attention") {
			t.Fatalf("TP3 of a model that cannot split three ways was not refused for its heads before the ring (%v): consulted=%v %v", fabricErr, consulted, err)
		}
	}
	if ring, _, err := resolve("pipeline", fourteenHeads, nil); err != nil || ring == nil {
		t.Fatalf("the model gate refused pipeline stages over an active ring: %v", err)
	}
	for _, tc := range []struct {
		parallelism string
		fabric      error
		bound       bool
		want        error
	}{
		{"", nil, true, nil},
		{"pipeline", nil, true, nil},
		{"tensor", nil, true, nil},
		{"", errNoFabric, false, nil},
		{"pipeline", errNoFabric, false, nil},
		{"tensor", errNoFabric, false, errVLLMGroupTensorNeedsRingFabric},
		{"", errStale, false, errStale},
		{"pipeline", errStale, false, errStale},
		{"tensor", errStale, false, errStale},
	} {
		if ring, _, err := resolve(tc.parallelism, ringSocketTP3Config, tc.fabric); !errors.Is(err, tc.want) || (ring != nil) != tc.bound {
			t.Fatalf("%q with ring state %v: bound=%v err=%v", tc.parallelism, tc.fabric, ring != nil, err)
		}
	}
}

func TestRingFabricAbsenceFallsBackWhileStaleOrUnroutedFailsClosed(t *testing.T) {
	members := []string{"node-a", "node-b", "node-c"}
	empty := newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	if _, _, _, err := empty.ringFabricFor(context.Background(), members); !errors.Is(err, errNoFabric) {
		t.Fatalf("no fabric was not reported as genuine absence: %v", err)
	}
	s, r := qualifiedRingFabricFixture(t)
	if op, digest, endpoints, err := s.ringFabricFor(context.Background(), []string{"node-c", "node-a", "node-b"}); err != nil || op != r.Public.OperationID || digest != r.Public.QualificationDigest || len(endpoints) != 9 {
		t.Fatalf("the exact active routed ring was not requalified: %v", err)
	}
	if _, _, _, err := s.ringFabricFor(context.Background(), []string{"node-x", "node-y", "node-z"}); !errors.Is(err, errNoFabric) {
		t.Fatalf("an unrelated ring blocked other members: %v", err)
	}
	for _, invalid := range [][]string{{"node-a", "node-b"}, {"node-a", "node-a", "node-b"}} {
		if _, _, _, err := s.ringFabricFor(context.Background(), invalid); err == nil || errors.Is(err, errNoFabric) {
			t.Fatalf("%v was accepted as a three-member ring", invalid)
		}
	}
	for name, spoil := range map[string]func(*testing.T, *fabricService, *fabricRunRecord){
		"requalification fails": func(_ *testing.T, s *fabricService, _ *fabricRunRecord) {
			s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
				return fabricControlResult{}, errors.New("route changed")
			}
		},
		"recovery required": func(_ *testing.T, _ *fabricService, r *fabricRunRecord) { r.Public.State = "recovery-required" },
		"unrouted ring": func(_ *testing.T, _ *fabricService, r *fabricRunRecord) {
			r.Public.RecipeID, r.Public.Targets = fabricRingRetainedRecipe, ringTestUnrouted(r.Public.Targets)
		},
		"direct recipe": func(_ *testing.T, _ *fabricService, r *fabricRunRecord) { r.Public.RecipeID = fabricRecipe },
		"other member":  func(_ *testing.T, _ *fabricService, r *fabricRunRecord) { r.Public.Targets[2].NodeID = "node-x" },
		"direct fabric within the group": func(t *testing.T, s *fabricService, r *fabricRunRecord) {
			_, direct := qualifiedDirectFabricFixture(t)
			delete(s.runs, r.Public.OperationID)
			s.runs[direct.Public.OperationID] = direct
		},
		"second operation": func(_ *testing.T, s *fabricService, r *fabricRunRecord) {
			other := *r
			other.Public.OperationID = strings.Repeat("e", 32)
			s.runs[other.Public.OperationID] = &other
		},
		"reservation": func(_ *testing.T, s *fabricService, r *fabricRunRecord) {
			s.reservation = &fabricReservation{OperationID: strings.Repeat("f", 32), Target: r.Public.Targets[1]}
		},
		"incomplete history": func(_ *testing.T, s *fabricService, _ *fabricRunRecord) { s.recoveryFailed = true },
	} {
		t.Run(name, func(t *testing.T) {
			s, r := qualifiedRingFabricFixture(t)
			spoil(t, s, r)
			if _, _, _, err := s.ringFabricFor(context.Background(), members); err == nil || errors.Is(err, errNoFabric) {
				t.Fatalf("a stale, ambiguous or unrouted fabric did not fail closed: %v", err)
			}
		})
	}
	s, r = qualifiedRingFabricFixture(t)
	r.Public.RecipeID, r.Public.Targets = fabricRingRetainedRecipe, ringTestUnrouted(r.Public.Targets)
	if _, _, _, err := s.ringFabricFor(context.Background(), members); err == nil || !strings.Contains(err.Error(), "routed") {
		t.Fatalf("an unrouted ring was not named as the refusal: %v", err)
	}
	s, r = qualifiedRingFabricFixture(t)
	r.Public.CleanupConfirmed = true
	if _, _, _, err := s.ringFabricFor(context.Background(), members); !errors.Is(err, errNoFabric) {
		t.Fatalf("a cleaned-up ring still counted as present: %v", err)
	}
}

func TestRingSocketControlRequiresTheQualifiedRingAndItsLease(t *testing.T) {
	s, fabric := qualifiedRingFabricFixture(t)
	plan := ringSocketFabricPlan(t, fabric)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	p := &vllmGroupPeer{m: &Manager{exec: &Executor{fabric: s}}}
	request := vllmGroupPeerRequest{Protocol: vllmGroupPeerProtocol, RunID: strings.Repeat("c", 32), Generation: 1, PlanDigest: digest, Plan: plan, Rank: 0}
	control := func(action string) error {
		r := request
		r.Action = action
		_, err := p.control(context.Background(), r)
		return err
	}
	// Past the fabric gates, this fixture has no paired membership to consult.
	reachedMembership := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "paired serving-group identity")
	}
	for _, action := range []string{"prepare", "start"} {
		if err := control(action); err == nil || !strings.Contains(err.Error(), "lease must be held") {
			t.Fatalf("%s reached a rank without the ring lease: %v", action, err)
		}
	}
	lease := fabricConsumerLease{Owner: fabricLeaseOwnerServingGroup, RunID: request.RunID, Generation: request.Generation, PlanDigest: digest}
	if err := s.acquireConsumerLease(context.Background(), fabric.Public.OperationID, fabric.Public.QualificationDigest, lease, nil); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"prepare", "start", "status"} {
		if err := control(action); !reachedMembership(err) {
			t.Fatalf("%s over the exact leased ring stopped at a fabric gate: %v", action, err)
		}
	}
	s.mu.Lock()
	delete(s.qualified, fabric.Public.OperationID)
	s.mu.Unlock()
	if err := control("status"); err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("a withdrawn ring qualification kept the plan usable: %v", err)
	}
	if err := control("stop"); !reachedMembership(err) {
		t.Fatalf("cleanup was gated on the withdrawn ring: %v", err)
	}
}

func TestRingFabricLeaseBlocksRollbackSurvivesRestartAndReleasesAfterCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, fabric := qualifiedRingFabricFixture(t)
		plan := ringSocketFabricPlan(t, fabric)
		if operation, qualification, ok := vllmGroupFabricBinding(plan); !ok || operation != fabric.Public.OperationID || qualification != fabric.Public.QualificationDigest {
			t.Fatalf("the ring socket plan does not bind its ring operation: %s %s %v", operation, qualification, ok)
		}
		g := vllmGroupTestOwner(t, func(_ context.Context, binding vllmGroupBinding, _ string) error {
			if !s.consumerLeaseHeld(fabric.Public.OperationID, vllmGroupFabricLease(vllmGroupRun{RunID: binding.RunID, Generation: binding.Generation, PlanDigest: binding.PlanDigest})) {
				return errors.New("rank action preceded the ring fabric lease")
			}
			return nil
		})
		g.fabric = s
		review, err := g.reviewPlan(plan)
		if err != nil {
			t.Fatal(err)
		}
		run, err := g.start(context.Background(), review.ReviewID)
		if err != nil {
			t.Fatal(err)
		}
		lease := vllmGroupFabricLease(run)
		if !s.consumerLeaseHeld(fabric.Public.OperationID, lease) {
			t.Fatal("the ring was not leased before rank launch")
		}
		if _, err := s.cancel(fabric.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "serving group") {
			t.Fatalf("rollback of the leased ring was admitted: %v", err)
		}
		restored := newFabricService(s.m)
		if restored.recoveryFailed || !restored.consumerLeaseHeld(fabric.Public.OperationID, lease) {
			t.Fatal("the ring lease was lost across Engine Manager restart")
		}
		if _, err := restored.cancel(fabric.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "serving group") {
			t.Fatalf("rollback of the restored ring lease was admitted: %v", err)
		}
		g.fabric = restored
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		if restored.consumerLeaseHeld(fabric.Public.OperationID, lease) || newFabricService(s.m).consumerLeaseHeld(fabric.Public.OperationID, lease) {
			t.Fatal("confirmed rank cleanup did not retain the ring lease release")
		}
	})
}

func TestRingSocketNativePolicyAndManagedCompiler(t *testing.T) {
	plan := vllmRankSystemPlan{Model: "example/model", RingSocket: &vllmRankRingSocket{InterfaceName: "node-a-p0"}}
	pass := func(policy vllmSystemRankPolicy) bool {
		policy.EffectivePrograms.Ingress, policy.EffectivePrograms.Egress = []uint32{1}, []uint32{2}
		policy.IngressAllowed, policy.EgressDenied = true, true
		return systemRankPolicyPassed(plan, vllmSystemRankResult{State: "started", EffectsApplied: true, Supervisor: &vllmRankSystemIdentity{PID: 2}, Policy: &policy})
	}
	if !pass(vllmSystemRankPolicy{Transport: vllmGroupRingSocketMode, RDMADisabled: true, SocketInterface: "node-a-p0"}) {
		t.Fatal("the exact ring socket policy was refused")
	}
	for name, policy := range map[string]vllmSystemRankPolicy{
		"tcp-only":              {Transport: "tcp-only", RDMADisabled: true},
		"direct transport":      {Transport: vllmGroupDirectSocketMode, RDMADisabled: true, SocketInterface: "node-a-p0"},
		"other interface":       {Transport: vllmGroupRingSocketMode, RDMADisabled: true, SocketInterface: "wlP9s9"},
		"rdma enabled":          {Transport: vllmGroupRingSocketMode, SocketInterface: "node-a-p0"},
		"rdma device":           {Transport: vllmGroupRingSocketMode, RDMADisabled: true, SocketInterface: "node-a-p0", HCAs: []string{"rocep1s0f0:1"}},
		"roce transport":        {Transport: vllmQwen38Transport, RDMADisabled: true, SocketInterface: "node-a-p0"},
		"socket fallback":       {Transport: vllmGroupRingSocketMode, RDMADisabled: true, SocketInterface: "node-a-p0", SocketPayloadFallback: true},
		"missing interface pin": {Transport: vllmGroupRingSocketMode, RDMADisabled: true},
	} {
		if pass(policy) {
			t.Fatalf("%s satisfied the ring socket policy", name)
		}
	}

	ring := ringSocketTestPlan(t, false)
	digest, err := vllmGroupPlanDigest(ring)
	if err != nil {
		t.Fatal(err)
	}
	b := vllmGroupBinding{RunID: strings.Repeat("c", 32), Generation: 1, PlanDigest: digest, Plan: ring, Rank: 0}
	p := vllmRankPlacement{LocalNode: "node-a", LocalAddress: "10.0.0.1", CoordinatorAddress: "10.0.0.1", ModelPath: filepath.Join(t.TempDir(), "model"), MasterPort: 29601, APIPort: 8181}
	if _, err := vllmRankArgs("/managed/bin/vllm", b, p, vllmResourceSettings{}); err == nil || !strings.Contains(err.Error(), "ring socket") {
		t.Fatalf("the uncontained compiler accepted a ring socket plan: %v", err)
	}
	every := []string{"10.253.0.0", "10.253.0.2", "10.253.0.1", "10.253.0.4", "10.253.0.3", "10.253.0.5"}
	for rank, member := range ring.RingSocket.Members {
		bound := vllmRankRingSocketFor(ring.RingSocket, rank)
		if bound.Mode != vllmGroupRingSocketMode || bound.OperationID != ring.RingSocket.OperationID || bound.QualificationSHA256 != ring.RingSocket.QualificationSHA256 ||
			bound.InterfaceName != member.InterfaceName || bound.InterfaceIndex != member.InterfaceIndex || bound.MAC != member.MAC ||
			bound.AdvertisedAddress != member.AdvertisedAddress || !slices.Equal(bound.RingAddresses, every) {
			t.Fatalf("rank %d ring binding is not its advertised interface plus every ring address: %+v", rank, bound)
		}
	}
}

// The root-executed owner is Python; drive its pure containment functions with
// the embedded bytes and the rank binding Go compiles.
func TestRankOwnerRingSocketContainment(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil && runtime.GOOS == "windows" {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	ring := ringSocketTestPlan(t, false).RingSocket
	binding, err := json.Marshal(vllmRankRingSocketFor(ring, 0))
	if err != nil {
		t.Fatal(err)
	}
	driver := `import json, os, sys
ns = {"__name__": "pair_fixed_rank_worker"}
exec(compile(sys.stdin.read(), "<owner>", "exec"), ns)
ns["pwd"] = type("P", (), {"getpwuid": staticmethod(lambda uid: type("E", (), {"pw_gid": uid})())})
binding = json.loads(os.environ["PAIR_TEST_RING_SOCKET"])
base = {"owner": ns["OWNER"], "runId": "a"*32, "generation": 1, "rank": 0, "planDigest": "b"*64, "nodeId": "node-a", "model": "example/model@" + "a"*40,
    "modelDigest": "c"*64, "runtimeDigest": "d"*64, "configSha256": "e"*64, "uid": 1000, "manager": {"pid": 1234, "startTicks": "9", "uid": 1000},
    "runtimeDir": "/opt/rt", "modelPath": "/opt/model", "resources": {"gpu_memory_utilization": 0.05, "max_model_len": 2048},
    "gpuUuid": "GPU-0a0a0a0a-0000-4000-8000-00000000000a", "peers": ["192.168.0.43", "192.168.0.109", "192.168.0.110"], "localAddress": "192.168.0.43",
    "coordinatorAddress": "192.168.0.43", "apiPort": 8001, "masterPort": 29500, "topology": {"tensorParallel": 1, "pipelineParallel": 3, "dataParallel": 1},
    "devices": ["/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-modeset", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"], "limits": ns["LIMITS"], "ringSocket": binding}
lane = {"mode": "qualified-direct-socket", "operationId": "f"*32, "qualificationSha256": "1"*64, "interfaceName": "enp1s0f0np0", "interfaceIndex": 3,
    "mac": "02:00:00:0a:00:00", "localAddress": "172.31.240.1", "peerAddress": "172.31.240.2", "peerNodeId": "node-b"}
def verdict(change):
    plan = json.loads(json.dumps(base))
    change(plan)
    try:
        ns["check_plan"](plan)
        return "ok"
    except ns["Unavailable"] as exc:
        return str(exc)
def two_peers(plan):
    plan.update(peers=plan["peers"][:2], topology={"tensorParallel": 2, "pipelineParallel": 1, "dataParallel": 1})
env = {"VLLM_HOST_IP": "192.168.0.43", "NCCL_NET": "Socket", "NCCL_IB_DISABLE": "1", "NCCL_SOCKET_IFNAME": "=wlP9s9", "GLOO_SOCKET_IFNAME": "wlP9s9"}
bound = ns["bind_ring_socket"](env, binding, "wlP9s9", True)
refused = []
for observed, management in ((False, "wlP9s9"), (True, binding["interfaceName"])):
    try:
        ns["bind_ring_socket"](env, binding, management, observed)
    except ns["Unavailable"] as exc:
        refused.append(str(exc))
print(json.dumps({
    "pipeline": verdict(lambda p: None),
    "tensor": verdict(lambda p: p.update(topology={"tensorParallel": 3, "pipelineParallel": 1, "dataParallel": 1})),
    "twoPeers": verdict(two_peers),
    "withDirect": verdict(lambda p: p.update(directSocket=lane)),
    "managementRing": verdict(lambda p: p["ringSocket"]["ringAddresses"].__setitem__(5, "192.168.0.110")),
    "advertisedOff": verdict(lambda p: p["ringSocket"].update(advertisedAddress="10.253.0.9")),
    "fiveAddresses": verdict(lambda p: p["ringSocket"]["ringAddresses"].pop()),
    "notARing": verdict(lambda p: p["ringSocket"]["ringAddresses"].__setitem__(5, "10.253.0.9")),
    "malformed": verdict(lambda p: p["ringSocket"]["ringAddresses"].__setitem__(1, "ring-lane")),
    "rdmaKey": verdict(lambda p: p["ringSocket"].update(rdmaDevice="rocep1s0f0")),
    "uverbs": verdict(lambda p: p.update(devices=p["devices"] + ["/dev/infiniband/uverbs0", "/dev/infiniband/uverbs1"])),
    "properties": ns["unit_properties"](base, "/run/credential.json"),
    "bound": bound, "refused": refused,
    "arguments": ns["model_arguments"](base, "/rt/venv/bin/vllm", "0.29.0"),
    "status": ns["transport_name"](base)}))
`
	cmd := exec.Command(python, "-I", "-c", driver)
	cmd.Env = append(os.Environ(), "PAIR_TEST_RING_SOCKET="+string(binding))
	cmd.Stdin = strings.NewReader(vllmRankSystemWorkerSource)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("owner driver failed: %v\n%s", err, out)
	}
	var got struct {
		Pipeline, Tensor, TwoPeers, WithDirect, ManagementRing, AdvertisedOff, FiveAddresses, NotARing, Malformed, RDMAKey, Uverbs string
		Properties                                                                                                                 []string
		Bound                                                                                                                      map[string]string
		Refused                                                                                                                    []string
		Arguments                                                                                                                  []string
		Status                                                                                                                     string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("owner driver output: %v\n%s", err, out)
	}
	if got.Pipeline != "ok" || got.Tensor != "ok" || got.WithDirect != "qualified_direct_socket_required" || got.Uverbs != "fixed_device_policy_required" {
		t.Fatalf("ring socket plan admission changed: %+v", got)
	}
	for name, verdict := range map[string]string{"two peers": got.TwoPeers, "management ring address": got.ManagementRing, "advertised off the ring": got.AdvertisedOff,
		"five ring addresses": got.FiveAddresses, "addresses outside three cable pairs": got.NotARing, "malformed ring address": got.Malformed, "RDMA key": got.RDMAKey} {
		if verdict != "qualified_ring_socket_required" {
			t.Fatalf("%s was not refused as a ring socket binding: %s", name, verdict)
		}
	}
	allowed := append([]string{"127.0.0.1", "192.168.0.43", "192.168.0.109", "192.168.0.110"}, vllmRankRingSocketFor(ring, 0).RingAddresses...)
	slices.Sort(allowed)
	for i := range allowed {
		allowed[i] += "/32"
	}
	if !slices.Contains(got.Properties, "IPAddressAllow="+strings.Join(allowed, " ")) || !slices.Contains(got.Properties, "RestrictAddressFamilies=AF_UNIX AF_INET AF_NETLINK") {
		t.Fatalf("the unit allowlist is not exactly management plus every ring address, without RDMA families: %v", got.Properties)
	}
	for _, property := range got.Properties {
		if strings.Contains(property, "AF_IB") || strings.Contains(property, "infiniband") {
			t.Fatalf("a ring socket unit gained RDMA access: %s", property)
		}
	}
	if got.Bound["NCCL_SOCKET_IFNAME"] != "=node-a-p0" || got.Bound["GLOO_SOCKET_IFNAME"] != "wlP9s9" || got.Bound["VLLM_HOST_IP"] != "192.168.0.43" ||
		got.Bound["NCCL_NET"] != "Socket" || got.Bound["NCCL_IB_DISABLE"] != "1" ||
		!slices.Equal(got.Refused, []string{"qualified_ring_socket_interface_changed", "qualified_ring_socket_interface_changed"}) {
		t.Fatalf("NCCL was not the only ring-bound traffic: %+v", got)
	}
	stages := slices.Index(got.Arguments, "--pipeline-parallel-size")
	if stages < 0 || stages+1 >= len(got.Arguments) || got.Arguments[stages+1] != "3" || !slices.Contains(got.Arguments, "--distributed-timeout-seconds") ||
		!slices.Contains(got.Arguments, "--cpu-distributed-timeout-seconds") || !slices.Contains(got.Arguments, "192.168.0.43") || got.Status != vllmGroupRingSocketMode {
		t.Fatalf("launch arguments or transport status changed: %+v", got)
	}
}
