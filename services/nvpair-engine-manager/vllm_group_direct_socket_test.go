// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func directSocketTestLanes(near, far string) []vllmGroupDirectSocketLane {
	return []vllmGroupDirectSocketLane{
		{NodeID: near, InterfaceName: "enp1s0f0np0", InterfaceIndex: 3, MAC: "02:00:00:0a:00:00", LocalAddress: "172.31.240.1", PeerAddress: "172.31.240.2"},
		{NodeID: far, InterfaceName: "enp1s0f1np1", InterfaceIndex: 4, MAC: "02:00:00:0b:00:01", LocalAddress: "172.31.240.2", PeerAddress: "172.31.240.1"},
	}
}

func directSocketTestPlan() vllmGroupPlan {
	p := vllmGroupTestPlan(2)
	p.DirectSocket = &vllmGroupDirectSocket{Mode: vllmGroupDirectSocketMode, OperationID: strings.Repeat("a", 32),
		QualificationSHA256: strings.Repeat("b", 64), Lanes: directSocketTestLanes("node-a", "node-b")}
	return p
}

// qwenDirectTestPlan is a reviewable two-Spark Qwen3.8 plan bound to the
// qualified direct fabric.
func qwenDirectTestPlan(t *testing.T, r *fabricRunRecord) vllmGroupPlan {
	t.Helper()
	p := vllmGroupTestPlan(2)
	p.Model, p.Runtime, p.Limits = vllmQwen38ModelID, vllmQwen38Runtime, vllmGroupLimitsForModel(vllmQwen38ModelID)
	var err error
	p.Topology, err = qwen38GroupTopology(2, p.Topology.ConfigSHA256)
	if err != nil {
		t.Fatal(err)
	}
	facts := make([]vllmGroupFacts, len(p.Members))
	nodes, principals := make([]string, len(p.Members)), make([]string, len(p.Members))
	for i := range p.Members {
		nodes[i], principals[i] = p.Members[i].NodeID, r.Public.Targets[i].Principal
	}
	if err := bindQwen38FabricFacts(vllmGroupSelection{NodeIDs: nodes}, facts, principals, r.Public.OperationID, r.Public.QualificationDigest, r.Public.CandidateIPs); err != nil {
		t.Fatal(err)
	}
	p.Transport = facts[0].fabricTransport
	for i := range p.Members {
		p.Members[i].Fabric = facts[i].fabric
	}
	if _, err := vllmGroupPlanDigest(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Two qualified links between node-a and node-b, listed out of interface order.
func directFabricTestEndpoints() []fabricCandidateIP {
	endpoint := func(node, peer, principal, address, peerAddress, name string, index int, mac string) fabricCandidateIP {
		return fabricCandidateIP{NodeID: node, PeerNodeID: peer, PeerPrincipal: principal, Address: address, PeerAddress: peerAddress,
			InterfaceName: name, InterfaceIndex: index, MAC: mac, RDMAPort: 1, GIDIndex: 3, GIDType: "RoCE v2"}
	}
	return []fabricCandidateIP{
		endpoint("node-a", "node-b", "principal-b", "172.31.240.5", "172.31.240.6", "enP2p1s0f0np0", 5, "02:00:00:0A:00:04"),
		endpoint("node-b", "node-a", "principal-a", "172.31.240.2", "172.31.240.1", "enp1s0f1np1", 4, "02:00:00:0B:00:01"),
		endpoint("node-a", "node-b", "principal-b", "172.31.240.1", "172.31.240.2", "enp1s0f0np0", 3, "02:00:00:0A:00:00"),
		endpoint("node-b", "node-a", "principal-a", "172.31.240.6", "172.31.240.5", "enP2p1s0f1np1", 6, "02:00:00:0B:00:05"),
	}
}

func TestDirectSocketContractIsDigestedAndLimitedToOrdinaryTwoNodeTP2(t *testing.T) {
	plan := directSocketTestPlan()
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	changed := cloneVLLMGroupPlan(plan)
	changed.DirectSocket.Lanes[0].InterfaceName = "enP2p1s0f0np0"
	if other, err := vllmGroupPlanDigest(changed); err != nil || other == digest || plan.DirectSocket.Lanes[0].InterfaceName != "enp1s0f0np0" {
		t.Fatal("the lane binding is not a deep-copied part of the plan digest")
	}
	for name, spoil := range map[string]func(*vllmGroupPlan){
		"pipeline":       func(p *vllmGroupPlan) { p.Topology.TensorParallel, p.Topology.PipelineParallel = 1, 2 },
		"three nodes":    func(p *vllmGroupPlan) { lanes := p.DirectSocket; *p = vllmGroupTestPlan(3); p.DirectSocket = lanes },
		"qwen profile":   func(p *vllmGroupPlan) { p.Model = vllmQwen38ModelID },
		"not reciprocal": func(p *vllmGroupPlan) { p.DirectSocket.Lanes[1].PeerAddress = "172.31.240.9" },
		"member order": func(p *vllmGroupPlan) {
			p.DirectSocket.Lanes[0], p.DirectSocket.Lanes[1] = p.DirectSocket.Lanes[1], p.DirectSocket.Lanes[0]
		},
		"management address": func(p *vllmGroupPlan) { p.Members[1].Placement = &vllmGroupPlacement{Address: "172.31.240.2"} },
		"upper-case mac":     func(p *vllmGroupPlan) { p.DirectSocket.Lanes[0].MAC = "02:00:00:0A:00:00" },
		"roce mode":          func(p *vllmGroupPlan) { p.DirectSocket.Mode = vllmQwen38Transport },
		"rdma lanes":         func(p *vllmGroupPlan) { p.Members[0].Fabric = &vllmGroupMemberFabric{} },
		"one lane":           func(p *vllmGroupPlan) { p.DirectSocket.Lanes = p.DirectSocket.Lanes[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			spoiled := cloneVLLMGroupPlan(plan)
			spoil(&spoiled)
			if _, err := vllmGroupPlanDigest(spoiled); err == nil {
				t.Fatal("an invalid direct socket plan was digested")
			}
		})
	}
}

func TestDirectSocketRejectsRuntimeWithoutTheBoundedDistributedTimeout(t *testing.T) {
	plan := directSocketTestPlan()
	plan.Runtime = "0.28.0"
	if _, err := vllmGroupPlanDigest(plan); err == nil {
		t.Fatal("a direct-socket plan admitted a runtime that does not receive the 180-second distributed timeout")
	}
}

func TestDirectSocketBindingChoosesOneReciprocalLane(t *testing.T) {
	selection := vllmGroupSelection{NodeIDs: []string{"node-a", "node-b"}}
	principals := []string{"principal-a", "principal-b"}
	operation, qualification := strings.Repeat("a", 32), strings.Repeat("b", 64)
	facts := make([]vllmGroupFacts, 2)
	if err := bindVLLMGroupDirectSocketFacts(selection, facts, principals, operation, qualification, directFabricTestEndpoints()); err != nil {
		t.Fatal(err)
	}
	ds := facts[0].directSocket
	if ds == nil || ds.OperationID != operation || ds.QualificationSHA256 != qualification || len(ds.Lanes) != 2 {
		t.Fatalf("no bound direct socket: %+v", ds)
	}
	near, far := ds.Lanes[0], ds.Lanes[1]
	if near.NodeID != "node-a" || near.InterfaceIndex != 3 || near.MAC != "02:00:00:0a:00:00" || far.NodeID != "node-b" || far.InterfaceName != "enp1s0f1np1" ||
		near.LocalAddress != far.PeerAddress || far.LocalAddress != near.PeerAddress {
		t.Fatalf("the lowest-index reciprocal lane was not chosen: %+v", ds.Lanes)
	}
	for name, spoil := range map[string]func([]fabricCandidateIP){
		"foreign principal": func(e []fabricCandidateIP) { e[2].PeerPrincipal = "principal-x" },
		"no reciprocal":     func(e []fabricCandidateIP) { e[1].Address, e[3].Address = "172.31.240.9", "172.31.240.10" },
		"third member":      func(e []fabricCandidateIP) { e[0].PeerNodeID = "node-c" },
	} {
		endpoints := directFabricTestEndpoints()
		spoil(endpoints)
		if err := bindVLLMGroupDirectSocketFacts(selection, make([]vllmGroupFacts, 2), principals, operation, qualification, endpoints); err == nil {
			t.Fatalf("%s bound a direct socket lane", name)
		}
	}
}

type directFabricTestOwner struct {
	operation, digest string
	endpoints         []fabricCandidateIP
	err               error
}

func (o directFabricTestOwner) qualifiedFabric([]string) (string, string, []fabricCandidateIP, error) {
	return o.operation, o.digest, o.endpoints, o.err
}

func (o directFabricTestOwner) requalifyFabric(context.Context, []string) (string, string, []fabricCandidateIP, error) {
	return o.qualifiedFabric(nil)
}

func TestDirectSocketRevalidationRequiresTheSameQualifiedLane(t *testing.T) {
	plan := directSocketTestPlan()
	owner := directFabricTestOwner{operation: plan.DirectSocket.OperationID, digest: plan.DirectSocket.QualificationSHA256, endpoints: directFabricTestEndpoints()}
	if err := currentVLLMGroupDirectSocket(owner, plan); err != nil {
		t.Fatalf("the exact qualified lane was refused: %v", err)
	}
	for name, change := range map[string]func(*directFabricTestOwner){
		"requalified digest": func(o *directFabricTestOwner) { o.digest = strings.Repeat("c", 64) },
		"other operation":    func(o *directFabricTestOwner) { o.operation = strings.Repeat("d", 32) },
		"lane moved":         func(o *directFabricTestOwner) { o.endpoints[2].InterfaceIndex = 9 },
		"unqualified":        func(o *directFabricTestOwner) { o.err = errors.New("qualified active fabric is unavailable") },
	} {
		changed := owner
		changed.endpoints = directFabricTestEndpoints()
		change(&changed)
		if err := currentVLLMGroupDirectSocket(changed, plan); err == nil {
			t.Fatalf("%s kept a stale direct socket usable", name)
		}
	}
	if err := currentVLLMGroupDirectSocket(struct{}{}, plan); err == nil {
		t.Fatal("a direct socket plan passed without a fabric owner")
	}
}

func TestTwoNodeGroupDefaultsToTP2OnlyOverDirectSocket(t *testing.T) {
	s, facts, pins := groupPlanFactsFixture(2)
	plan, err := assembleVLLMGroupPlan(s, facts, pins)
	if err != nil || plan.Topology.PipelineParallel != 2 || plan.Topology.TensorParallel != 1 || plan.DirectSocket != nil {
		t.Fatalf("genuine fabric absence did not default to PP2: %+v %v", plan.Topology, err)
	}
	s.Parallelism = "tensor"
	if _, err := assembleVLLMGroupPlan(s, facts, pins); !errors.Is(err, errVLLMGroupTensorNeedsDirectFabric) {
		t.Fatalf("explicit TP2 without a direct fabric was admitted: %v", err)
	}
	lanes := &vllmGroupDirectSocket{Mode: vllmGroupDirectSocketMode, OperationID: strings.Repeat("a", 32), QualificationSHA256: strings.Repeat("b", 64),
		Lanes: directSocketTestLanes(s.NodeIDs[0], s.NodeIDs[1])}
	for _, parallelism := range []string{"", "tensor"} {
		s.Parallelism = parallelism
		facts[0].directSocket = lanes
		plan, err := assembleVLLMGroupPlan(s, facts, pins)
		if err != nil || plan.Topology.TensorParallel != 2 || plan.DirectSocket == nil || plan.DirectSocket == lanes || plan.DirectSocket.Lanes[0].InterfaceName != "enp1s0f0np0" {
			t.Fatalf("a bound lane did not produce TP2 over the direct socket (%q): %+v %v", parallelism, plan.DirectSocket, err)
		}
	}
	s.Parallelism = "pipeline"
	if plan, err := assembleVLLMGroupPlan(s, facts, pins); err != nil || plan.Topology.PipelineParallel != 2 || plan.DirectSocket != nil {
		t.Fatalf("explicit PP2 carried the direct socket: %+v %v", plan.DirectSocket, err)
	}
	three, threeFacts, threePins := groupPlanFactsFixture(3)
	threeFacts[0].directSocket = lanes
	if plan, err := assembleVLLMGroupPlan(three, threeFacts, threePins); err != nil || plan.Topology.PipelineParallel != 3 || plan.DirectSocket != nil {
		t.Fatalf("three-node default changed: %+v %v", plan.Topology, err)
	}
}

// qualifiedDirectFabricFixture is one active, freshly qualified direct fabric
// between node-a and node-b whose proofs answer from the retained candidates.
func qualifiedDirectFabricFixture(t *testing.T) (*fabricService, *fabricRunRecord) {
	t.Helper()
	s, r := fabricServiceFixture(t)
	close(r.done)
	r.Public.State, r.Public.RecipeID, r.Public.EffectsApplied = "active", fabricRecipe, true
	r.Attempted = []bool{true, true}
	candidates, err := fabricCandidates(fabricRecipe, r.Public.Targets)
	if err != nil {
		t.Fatal(err)
	}
	for i := range candidates {
		candidates[i].RDMAPort, candidates[i].GIDIndex, candidates[i].GIDType = 1, 3, "RoCE v2"
	}
	r.Public.CandidateIPs = candidates
	r.Public.QualifiedAt = time.Now().UnixMilli()
	r.Public.QualificationDigest = fabricQualificationDigest(r.Public.OperationID, fabricRecipe, r.Public.Targets, candidates)
	s.qualified[r.Public.OperationID] = r.Public.QualificationDigest
	s.control = func(_ context.Context, _ fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		proved := slices.Clone(request.Candidates)
		for i := range proved {
			proved[i].RDMAPort, proved[i].GIDIndex, proved[i].GIDType = 1, 3, "RoCE v2"
		}
		return fabricControlResult{Qualified: true, Candidates: proved}, nil
	}
	if err := s.save(r); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestDirectFabricAbsenceDefaultsWhileStaleOrAmbiguousFailsClosed(t *testing.T) {
	members := []string{"node-a", "node-b"}
	empty := newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	if _, _, _, err := empty.directFabricFor(context.Background(), members); !errors.Is(err, errNoFabric) {
		t.Fatalf("no fabric was not reported as genuine absence: %v", err)
	}
	s, r := qualifiedDirectFabricFixture(t)
	if op, digest, endpoints, err := s.directFabricFor(context.Background(), members); err != nil || op != r.Public.OperationID || digest != r.Public.QualificationDigest || len(endpoints) != 4 {
		t.Fatalf("the exact active direct fabric was not requalified: %v", err)
	}
	if _, _, _, err := s.directFabricFor(context.Background(), []string{"node-x", "node-y"}); !errors.Is(err, errNoFabric) {
		t.Fatalf("an unrelated fabric blocked other members: %v", err)
	}
	for name, spoil := range map[string]func(*fabricService, *fabricRunRecord){
		"requalification fails": func(s *fabricService, _ *fabricRunRecord) {
			s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
				return fabricControlResult{}, errors.New("route changed")
			}
		},
		"recovery required": func(_ *fabricService, r *fabricRunRecord) { r.Public.State = "recovery-required" },
		"ring recipe":       func(_ *fabricService, r *fabricRunRecord) { r.Public.RecipeID = fabricRingRecipe },
		"other peer":        func(_ *fabricService, r *fabricRunRecord) { r.Public.Targets[1].NodeID = "node-c" },
		"second operation": func(s *fabricService, r *fabricRunRecord) {
			other := *r
			other.Public.OperationID = strings.Repeat("e", 32)
			s.runs[other.Public.OperationID] = &other
		},
		"reservation": func(s *fabricService, r *fabricRunRecord) {
			s.reservation = &fabricReservation{OperationID: strings.Repeat("f", 32), Target: r.Public.Targets[0]}
		},
		"incomplete history": func(s *fabricService, _ *fabricRunRecord) { s.recoveryFailed = true },
	} {
		t.Run(name, func(t *testing.T) {
			s, r := qualifiedDirectFabricFixture(t)
			spoil(s, r)
			if _, _, _, err := s.directFabricFor(context.Background(), members); err == nil || errors.Is(err, errNoFabric) {
				t.Fatalf("a stale or ambiguous fabric did not fail closed: %v", err)
			}
		})
	}
	s, r = qualifiedDirectFabricFixture(t)
	r.Public.CleanupConfirmed = true
	if _, _, _, err := s.directFabricFor(context.Background(), members); !errors.Is(err, errNoFabric) {
		t.Fatalf("a cleaned-up operation still counted as present: %v", err)
	}
}

// Status polling during a rollback's admission must neither re-prove the fabric
// against the rollback nor replace the outcome the rollback records.
func TestFabricStatusDoesNotReproveDuringRollbackAdmission(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
		t.Fatal("status re-proved the fabric during rollback admission")
		return fabricControlResult{}, nil
	}
	r.done = make(chan struct{})
	r.Public.Message = "Fabric is active and qualified."
	op, err := s.currentStatus(context.Background(), r.Public.OperationID)
	if err != nil || op.State != "active" || op.Message != r.Public.Message {
		t.Fatalf("status during rollback admission changed the recorded outcome: %+v %v", op, err)
	}
	close(r.done)
}

func TestFabricStatusPreservesQualificationWhileConsumerLeaseHeld(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	lease := directFabricTestLease(1)
	r.ConsumerLease = &lease
	_, inventories := fabricServiceTargets(t)
	facts := map[string]fabricInventory{}
	for i, inventory := range inventories {
		for lane := range inventory.Interfaces {
			inventory.Interfaces[lane].Addresses = []string{r.Public.Targets[i].Interfaces[lane].Address}
		}
		facts[inventory.NodeID] = inventory
	}
	claimed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.inventory = func(ctx context.Context, node string) (fabricInventory, error) {
		once.Do(func() {
			close(claimed)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		return facts[node], ctx.Err()
	}
	digest := r.Public.QualificationDigest
	type statusResult struct {
		op  fabricOperation
		err error
	}
	statusDone := make(chan statusResult, 1)
	go func() {
		op, err := s.currentStatus(context.Background(), r.Public.OperationID)
		statusDone <- statusResult{op: op, err: err}
	}()
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("leased status did not begin its read-only proof")
	}
	if s.qualified[r.Public.OperationID] != digest {
		t.Fatal("leased status withdrew qualification while proof was in progress")
	}
	close(release)
	result := <-statusDone
	if result.err != nil || result.op.State != "active" || s.qualified[r.Public.OperationID] != digest {
		t.Fatalf("leased status did not preserve the reviewed direct qualification: %+v %v", result.op, result.err)
	}
	if !s.consumerLeaseHeld(r.Public.OperationID, lease) {
		t.Fatal("leased status changed the serving-group fabric owner")
	}
}

func TestFabricStatusRequalifiesRetainedLeaseAfterRestart(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	lease := directFabricTestLease(1)
	r.ConsumerLease = &lease
	delete(s.qualified, r.Public.OperationID)
	_, inventories := fabricServiceTargets(t)
	facts := map[string]fabricInventory{}
	for i, inventory := range inventories {
		for lane := range inventory.Interfaces {
			inventory.Interfaces[lane].Addresses = []string{r.Public.Targets[i].Interfaces[lane].Address}
		}
		facts[inventory.NodeID] = inventory
	}
	s.inventory = func(_ context.Context, node string) (fabricInventory, error) {
		return facts[node], nil
	}
	op, err := s.currentStatus(context.Background(), r.Public.OperationID)
	if err != nil || op.State != "active" || s.qualified[r.Public.OperationID] != r.Public.QualificationDigest {
		t.Fatalf("retained lease did not regain process-local qualification: %+v %v", op, err)
	}
}

func TestFabricStatusRevokesStaleQualificationButRetainsConsumerLease(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	lease := directFabricTestLease(1)
	r.ConsumerLease = &lease
	s.inventory = func(context.Context, string) (fabricInventory, error) {
		return fabricInventory{}, errors.New("synthetic leased fabric drift")
	}
	op, err := s.currentStatus(context.Background(), r.Public.OperationID)
	if err != nil || op.State != "recovery-required" || s.qualified[r.Public.OperationID] != "" {
		t.Fatalf("leased drift stayed qualified: %+v %v", op, err)
	}
	if !s.consumerLeaseHeld(r.Public.OperationID, lease) || r.Public.CleanupConfirmed {
		t.Fatal("leased drift lost its cleanup fence")
	}
}

func TestFabricStatusClaimBlocksConcurrentRollbackAdmission(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	_, inventories := fabricServiceTargets(t)
	facts := map[string]fabricInventory{}
	for i, inventory := range inventories {
		for lane := range inventory.Interfaces {
			inventory.Interfaces[lane].Addresses = []string{r.Public.Targets[i].Interfaces[lane].Address}
		}
		facts[inventory.NodeID] = inventory
	}
	claimed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.inventory = func(ctx context.Context, node string) (fabricInventory, error) {
		once.Do(func() {
			close(claimed)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		if err := ctx.Err(); err != nil {
			return fabricInventory{}, err
		}
		inventory, ok := facts[node]
		if !ok {
			return fabricInventory{}, errors.New("unexpected fabric participant")
		}
		return inventory, nil
	}
	type statusResult struct {
		op  fabricOperation
		err error
	}
	statusDone := make(chan statusResult, 1)
	go func() {
		op, err := s.currentStatus(context.Background(), r.Public.OperationID)
		statusDone <- statusResult{op: op, err: err}
	}()
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("status did not claim re-proof before inventory I/O")
	}
	s.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
		return fabricWorkerResult{CleanupConfirmed: true}, nil
	}
	type cancelResult struct {
		op  fabricOperation
		err error
	}
	cancelDone := make(chan cancelResult, 1)
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		op, err := s.cancelContext(cancelCtx, r.Public.OperationID, true)
		cancelDone <- cancelResult{op: op, err: err}
	}()
	select {
	case result := <-cancelDone:
		t.Fatalf("rollback did not wait for the active status proof: %+v %v", result.op, result.err)
	case <-time.After(25 * time.Millisecond):
	}
	s.mu.Lock()
	waiters := r.rollbackWaiters
	s.mu.Unlock()
	if waiters != 1 {
		t.Fatalf("rollback intent was not retained during status proof: %d", waiters)
	}
	if op, err := s.currentStatus(context.Background(), r.Public.OperationID); err != nil || op.State != "active" {
		t.Fatalf("a new status proof did not yield to pending rollback: %+v %v", op, err)
	}
	close(release)
	select {
	case result := <-statusDone:
		if result.err != nil || result.op.State != "active" {
			t.Fatalf("claimed status proof did not finish active: %+v %v", result.op, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claimed status proof did not finish")
	}
	select {
	case result := <-cancelDone:
		if result.err != nil {
			t.Fatalf("rollback did not admit after the status proof settled: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rollback waiter did not admit after status proof")
	}
	s.mu.Lock()
	rollbackDone := r.done
	s.mu.Unlock()
	select {
	case <-rollbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("admitted rollback did not finish")
	}
	if r.Public.State != "cancelled" || !r.Public.CleanupConfirmed {
		t.Fatalf("retry did not complete exact rollback: %+v", r.Public)
	}
}

func TestFabricRollbackWaiterClearsAfterDeadline(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	r.done = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.cancelContext(ctx, r.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("bounded rollback wait did not fail closed: %v", err)
	}
	s.mu.Lock()
	waiters := r.rollbackWaiters
	state := r.Public.State
	digest := s.qualified[r.Public.OperationID]
	s.mu.Unlock()
	close(r.done)
	if waiters != 0 || state != "active" || digest != r.Public.QualificationDigest {
		t.Fatalf("expired rollback intent changed fabric ownership: waiters=%d state=%s digest=%s", waiters, state, digest)
	}
}

func TestFabricConsumerLeaseWaitsForStatusProof(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	_, inventories := fabricServiceTargets(t)
	facts := map[string]fabricInventory{}
	for i, inventory := range inventories {
		for lane := range inventory.Interfaces {
			inventory.Interfaces[lane].Addresses = []string{r.Public.Targets[i].Interfaces[lane].Address}
		}
		facts[inventory.NodeID] = inventory
	}
	claimed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.inventory = func(ctx context.Context, node string) (fabricInventory, error) {
		once.Do(func() {
			close(claimed)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		return facts[node], ctx.Err()
	}
	statusDone := make(chan error, 1)
	go func() {
		_, err := s.currentStatus(context.Background(), r.Public.OperationID)
		statusDone <- err
	}()
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("status did not claim the fabric proof")
	}
	lease := directFabricTestLease(1)
	leaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	leaseDone := make(chan error, 1)
	leaseStarted := make(chan struct{})
	go func() {
		close(leaseStarted)
		leaseDone <- s.acquireConsumerLease(leaseCtx, r.Public.OperationID, r.Public.QualificationDigest, lease, nil)
	}()
	<-leaseStarted
	select {
	case err := <-leaseDone:
		t.Fatalf("lease did not wait for the in-flight status proof: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-statusDone; err != nil {
		t.Fatalf("status proof failed: %v", err)
	}
	if err := <-leaseDone; err != nil || !s.consumerLeaseHeld(r.Public.OperationID, lease) {
		t.Fatalf("lease did not acquire after successful status proof: %v", err)
	}
}

func directFabricTestLease(generation uint64) fabricConsumerLease {
	return fabricConsumerLease{Owner: fabricLeaseOwnerServingGroup, RunID: strings.Repeat("c", 32), Generation: generation, PlanDigest: strings.Repeat("d", 64)}
}

func TestFabricConsumerLeaseBlocksRollbackAndSurvivesRestart(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	id, digest := r.Public.OperationID, r.Public.QualificationDigest
	lease := directFabricTestLease(1)
	if err := s.acquireConsumerLease(context.Background(), id, strings.Repeat("0", 64), lease, nil); err == nil {
		t.Fatal("a lease was granted against a different qualification")
	}
	if err := s.acquireConsumerLease(context.Background(), id, digest, lease, nil); err != nil || !s.consumerLeaseHeld(id, lease) {
		t.Fatalf("the qualified direct fabric was not leased: %v", err)
	}
	if err := s.acquireConsumerLease(context.Background(), id, digest, directFabricTestLease(2), nil); err == nil {
		t.Fatal("a second generation took a held lease")
	}
	if _, err := s.cancel(id, true); err == nil || !strings.Contains(err.Error(), "serving group") {
		t.Fatalf("rollback of a leased fabric was admitted: %v", err)
	}
	restored := newFabricService(s.m)
	if restored.recoveryFailed || !restored.consumerLeaseHeld(id, lease) {
		t.Fatal("the consumer lease was lost across a restart")
	}
	if _, err := restored.cancel(id, true); err == nil || !strings.Contains(err.Error(), "serving group") {
		t.Fatalf("the restored lease did not block rollback: %v", err)
	}
	restored.mu.Lock()
	restored.runs[id].Public.State = "recovery-required"
	restored.mu.Unlock()
	if _, err := restored.recover(context.Background(), id, true); err == nil || !strings.Contains(err.Error(), "serving group") {
		t.Fatalf("recovery of a leased fabric was admitted: %v", err)
	}
	restored.mu.Lock()
	restored.runs[id].Public.State = "active"
	restored.mu.Unlock()
	if err := restored.releaseConsumerLease(id, directFabricTestLease(2)); err != nil || !restored.consumerLeaseHeld(id, lease) {
		t.Fatal("a different generation released the held lease")
	}
	if err := restored.releaseConsumerLease(id, lease); err != nil || restored.consumerLeaseHeld(id, lease) {
		t.Fatalf("the holding generation could not release its lease: %v", err)
	}
	if again := newFabricService(s.m); again.consumerLeaseHeld(id, lease) {
		t.Fatal("the lease release was not retained")
	}
	// A superseded, fully cleaned generation hands the lease to its successor.
	if err := s.releaseConsumerLease(id, lease); err != nil {
		t.Fatal(err)
	}
	if err := s.acquireConsumerLease(context.Background(), id, digest, lease, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.acquireConsumerLease(context.Background(), id, digest, directFabricTestLease(2), &lease); err != nil || !s.consumerLeaseHeld(id, directFabricTestLease(2)) {
		t.Fatalf("a cleaned predecessor's lease was not superseded: %v", err)
	}
}

// Rollback and a starting group race for the same fabric. Exactly one may win:
// a leased fabric refuses rollback, and a rollback revokes the qualification a
// lease requires.
func TestFabricConsumerLeaseSurvivesRecoveryRequiredRestart(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	id, digest := r.Public.OperationID, r.Public.QualificationDigest
	lease := directFabricTestLease(1)
	if err := s.acquireConsumerLease(context.Background(), id, digest, lease, nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	r.Public.State = "recovery-required"
	r.Public.CandidateIPs = nil
	r.Public.QualifiedAt = 0
	r.Public.QualificationDigest = ""
	delete(s.qualified, id)
	err := s.save(r)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("a leased recovery record was not retainable: %v", err)
	}
	restored := newFabricService(s.m)
	if restored.recoveryFailed || !restored.consumerLeaseHeld(id, lease) || restored.runs[id] == nil || restored.runs[id].Public.State != "recovery-required" {
		t.Fatal("a leased recovery record was rejected or lost across restart")
	}
	if err := restored.releaseConsumerLease(id, lease); err != nil || restored.consumerLeaseHeld(id, lease) {
		t.Fatalf("exact group cleanup could not release the recovered lease: %v", err)
	}
}

func TestFabricConsumerLeaseCancelRace(t *testing.T) {
	for range 64 {
		s, r := qualifiedDirectFabricFixture(t)
		s.refreshAccess = func(context.Context, *fabricRunRecord) ([]cableLaunchPlan, error) {
			return nil, errors.New("rollback access is not part of this race")
		}
		id, digest := r.Public.OperationID, r.Public.QualificationDigest
		var ready, done sync.WaitGroup
		var leaseErr, cancelErr error
		ready.Add(2)
		done.Add(2)
		start := make(chan struct{})
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			leaseErr = s.acquireConsumerLease(context.Background(), id, digest, directFabricTestLease(1), nil)
		}()
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, cancelErr = s.cancel(id, true)
		}()
		ready.Wait()
		close(start)
		done.Wait()
		if leaseErr == nil && cancelErr == nil {
			t.Fatal("a rollback began under a granted consumer lease")
		}
		if leaseErr != nil && cancelErr != nil {
			t.Fatalf("neither side won: lease=%v cancel=%v", leaseErr, cancelErr)
		}
		s.mu.Lock()
		rollback := r.done
		s.mu.Unlock()
		<-rollback
	}
}

type fabricLeaseRecorder struct {
	mu              sync.Mutex
	events          *[]string
	refuse          bool
	onAcquire       func(fabricConsumerLease) error
	releaseFailures int
	acquired        []fabricConsumerLease
	released        []fabricConsumerLease
}

func (f *fabricLeaseRecorder) acquireConsumerLease(_ context.Context, _, _ string, lease fabricConsumerLease, _ *fabricConsumerLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.events = append(*f.events, "lease")
	if f.onAcquire != nil {
		if err := f.onAcquire(lease); err != nil {
			return err
		}
	}
	if f.refuse {
		return errors.New("the reviewed direct fabric is no longer active and qualified")
	}
	f.acquired = append(f.acquired, lease)
	return nil
}

func TestServingGroupRetainsRunBeforeAcquiringFabricLease(t *testing.T) {
	rankCalled := false
	g := vllmGroupTestOwner(t, func(context.Context, vllmGroupBinding, string) error {
		rankCalled = true
		return nil
	})
	observed := false
	leaser := &fabricLeaseRecorder{events: &[]string{}}
	leaser.onAcquire = func(lease fabricConsumerLease) error {
		raw, err := os.ReadFile(g.path)
		if err != nil {
			return errors.New("serving-group run was not retained before the fabric lease")
		}
		run, ok := parseRetainedVLLMGroupRun(raw)
		observed = ok && run.RunID == lease.RunID && run.Generation == lease.Generation && run.PlanDigest == lease.PlanDigest
		return errors.New("fixture refuses the lease after observing retained ownership")
	}
	g.fabric = leaser
	review, err := g.reviewPlan(directSocketTestPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.start(context.Background(), review.ReviewID); err == nil || !observed {
		t.Fatalf("fabric lease preceded durable serving-group ownership: observed=%v err=%v", observed, err)
	}
	run := g.status()
	if rankCalled || g.reserved() || run.State != "failed" || !run.CleanupConfirmed {
		t.Fatalf("a refused pre-rank lease did not close cleanly: called=%v held=%v run=%+v", rankCalled, g.reserved(), run)
	}
}

func TestServingGroupReleasesCleanPredecessorLeaseBeforeRetainingSuccessor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rankCalls := 0
		g := vllmGroupTestOwner(t, func(context.Context, vllmGroupBinding, string) error {
			rankCalls++
			return nil
		})
		plan := directSocketTestPlan()
		digest, err := vllmGroupPlanDigest(plan)
		if err != nil {
			t.Fatal(err)
		}
		previous := vllmGroupRun{RunID: strings.Repeat("9", 32), Generation: 1, PlanDigest: digest, Plan: plan, State: "stopped", CleanupConfirmed: true}
		for _, member := range plan.Members {
			previous.Ranks = append(previous.Ranks, vllmGroupRank{NodeID: member.NodeID})
		}
		g.run = previous
		g.held = false
		if err := g.saveLocked(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(g.path)
		if err != nil {
			t.Fatal(err)
		}
		events := []string{}
		leaser := &fabricLeaseRecorder{events: &events, releaseFailures: 1}
		g.fabric = leaser
		review, err := g.reviewPlan(plan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.start(context.Background(), review.ReviewID); err == nil {
			t.Fatal("successor start ignored a failed predecessor lease release")
		}
		after, err := os.ReadFile(g.path)
		if err != nil || !bytes.Equal(before, after) || g.status().RunID != previous.RunID || len(leaser.acquired) != 0 || rankCalls != 0 {
			t.Fatalf("failed predecessor release rewrote ownership: read=%v same=%v acquired=%d calls=%d", err, bytes.Equal(before, after), len(leaser.acquired), rankCalls)
		}
		run, err := g.start(context.Background(), review.ReviewID)
		if err != nil || len(leaser.acquired) != 1 || leaser.acquired[0] != vllmGroupFabricLease(run) || len(leaser.released) != 1 || leaser.released[0] != vllmGroupFabricLease(previous) {
			t.Fatalf("successor did not release N before acquiring N+1: run=%+v err=%v events=%v", run, err, events)
		}
		synctest.Wait()
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
	})
}

func (f *fabricLeaseRecorder) releaseConsumerLease(_ string, lease fabricConsumerLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.events = append(*f.events, "release")
	if f.releaseFailures > 0 {
		f.releaseFailures--
		return errors.New("fixture lease release persistence failed")
	}
	f.released = append(f.released, lease)
	return nil
}

func TestServingGroupLeasesFabricBeforeAnyRankAndReleasesAfterCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		events := []string{}
		g := vllmGroupTestOwner(t, func(_ context.Context, _ vllmGroupBinding, action string) error {
			mu.Lock()
			events = append(events, action)
			mu.Unlock()
			return nil
		})
		leaser := &fabricLeaseRecorder{events: &events}
		g.fabric = leaser
		review, err := g.reviewPlan(directSocketTestPlan())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run, err := g.start(ctx, review.ReviewID)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		mu.Lock()
		first := slices.Index(events, "prepare")
		mu.Unlock()
		if len(leaser.acquired) != 1 || leaser.acquired[0] != vllmGroupFabricLease(run) || first < 1 || events[0] != "lease" {
			t.Fatalf("a rank action preceded the fabric lease: %v", events)
		}
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		if len(leaser.released) != 1 || leaser.released[0] != vllmGroupFabricLease(run) {
			t.Fatalf("clean shutdown kept the fabric lease: %v", events)
		}

		refused := vllmGroupTestOwner(t, func(context.Context, vllmGroupBinding, string) error {
			t.Error("a rank action ran without the fabric lease")
			return nil
		})
		refusedEvents := []string{}
		refused.fabric = &fabricLeaseRecorder{events: &refusedEvents, refuse: true}
		review, err = refused.reviewPlan(directSocketTestPlan())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := refused.start(ctx, review.ReviewID); err == nil || refused.reserved() {
			t.Fatalf("a group started without its fabric lease: %v", err)
		}
	})
}

func TestServingGroupKeepsLeaseUntilEveryRankIsCleaned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := []string{}
		g := vllmGroupTestOwner(t, func(_ context.Context, _ vllmGroupBinding, action string) error {
			if action == "stop" {
				return errors.New("participant cleanup unconfirmed")
			}
			return nil
		})
		leaser := &fabricLeaseRecorder{events: &events}
		g.fabric = leaser
		review, err := g.reviewPlan(directSocketTestPlan())
		if err != nil {
			t.Fatal(err)
		}
		run, err := g.start(context.Background(), review.ReviewID)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := g.stop(context.Background(), run.RunID, run.Generation); err == nil || !g.reserved() {
			t.Fatal("unconfirmed cleanup released the group")
		}
		if len(leaser.released) != 0 {
			t.Fatal("the fabric lease was released before every rank was cleaned")
		}
		g.call = func(context.Context, vllmGroupBinding, string) error { return nil }
		if err := g.reconcile(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		if len(leaser.released) != 1 {
			t.Fatal("confirmed cleanup did not release the fabric lease")
		}
	})
}

func TestQwenFabricLeaseBlocksRollbackSurvivesRestartAndReleasesAfterCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, fabric := qualifiedDirectFabricFixture(t)
		plan := qwenDirectTestPlan(t, fabric)
		g := vllmGroupTestOwner(t, func(_ context.Context, binding vllmGroupBinding, _ string) error {
			if !s.consumerLeaseHeld(fabric.Public.OperationID, vllmGroupFabricLease(vllmGroupRun{RunID: binding.RunID, Generation: binding.Generation, PlanDigest: binding.PlanDigest})) {
				return errors.New("rank action preceded the Qwen fabric lease")
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
			t.Fatal("the Qwen fabric was not leased before rank launch")
		}
		if _, err := s.cancel(fabric.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "serving group") {
			t.Fatalf("rollback of the leased Qwen fabric was admitted: %v", err)
		}
		restored := newFabricService(s.m)
		if restored.recoveryFailed || !restored.consumerLeaseHeld(fabric.Public.OperationID, lease) {
			t.Fatal("the Qwen fabric lease was lost across Engine Manager restart")
		}
		if _, err := restored.cancel(fabric.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "serving group") {
			t.Fatalf("rollback of the restored Qwen fabric lease was admitted: %v", err)
		}
		g.fabric = restored
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		if restored.consumerLeaseHeld(fabric.Public.OperationID, lease) || newFabricService(s.m).consumerLeaseHeld(fabric.Public.OperationID, lease) {
			t.Fatal("confirmed Qwen rank cleanup did not retain the fabric lease release")
		}
	})
}

func TestDirectSocketNativePolicyAndDirectCompiler(t *testing.T) {
	plan := vllmRankSystemPlan{Model: "example/model", DirectSocket: &vllmRankDirectSocket{InterfaceName: "enp1s0f0np0"}}
	pass := func(policy vllmSystemRankPolicy) bool {
		policy.EffectivePrograms.Ingress, policy.EffectivePrograms.Egress = []uint32{1}, []uint32{2}
		policy.IngressAllowed, policy.EgressDenied = true, true
		return systemRankPolicyPassed(plan, vllmSystemRankResult{State: "started", EffectsApplied: true, Supervisor: &vllmRankSystemIdentity{PID: 2}, Policy: &policy})
	}
	if !pass(vllmSystemRankPolicy{Transport: vllmGroupDirectSocketMode, RDMADisabled: true, SocketInterface: "enp1s0f0np0"}) {
		t.Fatal("the exact direct socket policy was refused")
	}
	for name, policy := range map[string]vllmSystemRankPolicy{
		"tcp-only":         {Transport: "tcp-only", RDMADisabled: true},
		"other interface":  {Transport: vllmGroupDirectSocketMode, RDMADisabled: true, SocketInterface: "wlP9s9"},
		"rdma enabled":     {Transport: vllmGroupDirectSocketMode, SocketInterface: "enp1s0f0np0"},
		"rdma device":      {Transport: vllmGroupDirectSocketMode, RDMADisabled: true, SocketInterface: "enp1s0f0np0", HCAs: []string{"rocep1s0f0:1"}},
		"roce transport":   {Transport: vllmQwen38Transport, RDMADisabled: true, SocketInterface: "enp1s0f0np0"},
		"socket fallback":  {Transport: vllmGroupDirectSocketMode, RDMADisabled: true, SocketInterface: "enp1s0f0np0", SocketPayloadFallback: true},
		"missing lane pin": {Transport: vllmGroupDirectSocketMode, RDMADisabled: true},
	} {
		if pass(policy) {
			t.Fatalf("%s satisfied the direct socket policy", name)
		}
	}
	plan.DirectSocket = nil
	if pass(vllmSystemRankPolicy{Transport: "tcp-only", RDMADisabled: true, SocketInterface: "enp1s0f0np0"}) {
		t.Fatal("a management-only rank reported a direct socket interface")
	}

	b, p := managedRankFixture(t, 0)
	args, err := vllmRankArgs("/managed/bin/vllm", b, p, vllmResourceSettings{})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--distributed-timeout-seconds", "--cpu-distributed-timeout-seconds"} {
		if i := slices.Index(args, flag); i < 0 || i+1 >= len(args) || args[i+1] != "180" {
			t.Fatalf("missing %s 180: %v", flag, args)
		}
	}
	b.Plan.DirectSocket = directSocketTestPlan().DirectSocket
	if _, err := vllmRankArgs("/managed/bin/vllm", b, p, vllmResourceSettings{}); err == nil {
		t.Fatal("the uncontained compiler accepted a direct socket plan")
	}
}

// The root-executed owner is Python; drive its pure containment functions with
// the embedded bytes when an interpreter is available.
func TestRankOwnerDirectSocketContainment(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil && runtime.GOOS == "windows" {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	driver := `import json, sys
ns = {"__name__": "pair_fixed_rank_worker"}
exec(compile(sys.stdin.read(), "<owner>", "exec"), ns)
ns["pwd"] = type("P", (), {"getpwuid": staticmethod(lambda uid: type("E", (), {"pw_gid": uid})())})
base = {"owner": ns["OWNER"], "runId": "a"*32, "generation": 1, "rank": 0, "planDigest": "b"*64, "nodeId": "node-a", "model": "example/model@" + "a"*40,
    "modelDigest": "c"*64, "runtimeDigest": "d"*64, "configSha256": "e"*64, "uid": 1000, "manager": {"pid": 1234, "startTicks": "9", "uid": 1000},
    "runtimeDir": "/opt/rt", "modelPath": "/opt/model", "resources": {"gpu_memory_utilization": 0.05, "max_model_len": 2048},
    "gpuUuid": "GPU-0a0a0a0a-0000-4000-8000-00000000000a", "peers": ["192.168.0.43", "192.168.0.109"], "localAddress": "192.168.0.43",
    "coordinatorAddress": "192.168.0.43", "apiPort": 8001, "masterPort": 29500, "topology": {"tensorParallel": 2, "pipelineParallel": 1, "dataParallel": 1},
    "devices": ["/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-modeset", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"], "limits": ns["LIMITS"]}
lane = {"mode": "qualified-direct-socket", "operationId": "f"*32, "qualificationSha256": "1"*64, "interfaceName": "enp1s0f0np0", "interfaceIndex": 3,
    "mac": "02:00:00:0a:00:00", "localAddress": "172.31.240.1", "peerAddress": "172.31.240.2", "peerNodeId": "node-b"}
def verdict(change):
    plan = json.loads(json.dumps(dict(base, directSocket=lane)))
    change(plan)
    try:
        ns["check_plan"](plan)
        return "ok"
    except ns["Unavailable"] as exc:
        return str(exc)
direct = dict(base, directSocket=lane)
env = {"VLLM_HOST_IP": "192.168.0.43", "NCCL_NET": "Socket", "NCCL_IB_DISABLE": "1", "NCCL_SOCKET_IFNAME": "=wlP9s9", "GLOO_SOCKET_IFNAME": "wlP9s9"}
bound = ns["bind_direct_socket"](env, lane, "wlP9s9", True)
refused = []
for observed, management in ((False, "wlP9s9"), (True, "enp1s0f0np0")):
    try:
        ns["bind_direct_socket"](env, lane, management, observed)
    except ns["Unavailable"] as exc:
        refused.append(str(exc))
print(json.dumps({
    "direct": verdict(lambda p: None),
    "pipeline": verdict(lambda p: p.update(topology={"tensorParallel": 1, "pipelineParallel": 2, "dataParallel": 1})),
    "uverbs": verdict(lambda p: p.update(devices=p["devices"] + ["/dev/infiniband/uverbs0", "/dev/infiniband/uverbs1"])),
    "managementLane": verdict(lambda p: p["directSocket"].update(peerAddress="192.168.0.109")),
    "rdmaKey": verdict(lambda p: p["directSocket"].update(rdmaDevice="rocep1s0f0")),
    "properties": ns["unit_properties"](direct, "/run/credential.json"),
    "bound": bound, "refused": refused,
    "arguments": ns["model_arguments"](direct, "/rt/venv/bin/vllm", "0.29.0"),
    "legacyArguments": ns["model_arguments"](direct, "/rt/bin/vllm", "0.28.0"),
    "status": ns["transport_name"](direct)}))
`
	cmd := exec.Command(python, "-I", "-c", driver)
	cmd.Stdin = strings.NewReader(vllmRankSystemWorkerSource)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("owner driver failed: %v\n%s", err, out)
	}
	var got struct {
		Direct, Pipeline, Uverbs, ManagementLane, RDMAKey string
		Properties                                        []string
		Bound                                             map[string]string
		Refused                                           []string
		Arguments, LegacyArguments                        []string
		Status                                            string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("owner driver output: %v\n%s", err, out)
	}
	if got.Direct != "ok" || got.Pipeline != "qualified_direct_socket_required" || got.Uverbs != "fixed_device_policy_required" ||
		got.ManagementLane != "qualified_direct_socket_required" || got.RDMAKey != "qualified_direct_socket_required" {
		t.Fatalf("direct socket plan admission changed: %+v", got)
	}
	for _, property := range got.Properties {
		if strings.Contains(property, "AF_IB") || strings.Contains(property, "infiniband") {
			t.Fatalf("a direct socket unit gained RDMA access: %s", property)
		}
	}
	if !slices.Contains(got.Properties, "IPAddressAllow=127.0.0.1/32 172.31.240.1/32 172.31.240.2/32 192.168.0.109/32 192.168.0.43/32") {
		t.Fatalf("the unit address allowlist is not exactly management plus the lane: %v", got.Properties)
	}
	if got.Bound["NCCL_SOCKET_IFNAME"] != "=enp1s0f0np0" || got.Bound["GLOO_SOCKET_IFNAME"] != "wlP9s9" || got.Bound["VLLM_HOST_IP"] != "192.168.0.43" ||
		got.Bound["NCCL_NET"] != "Socket" || got.Bound["NCCL_IB_DISABLE"] != "1" || len(got.Refused) != 2 {
		t.Fatalf("NCCL was not the only lane-bound traffic: %+v", got)
	}
	if !slices.Contains(got.Arguments, "--distributed-timeout-seconds") || !slices.Contains(got.Arguments, "--cpu-distributed-timeout-seconds") ||
		slices.Contains(got.LegacyArguments, "--distributed-timeout-seconds") || !slices.Contains(got.Arguments, "192.168.0.43") || got.Status != vllmGroupDirectSocketMode {
		t.Fatalf("launch arguments changed: %+v", got)
	}
}
