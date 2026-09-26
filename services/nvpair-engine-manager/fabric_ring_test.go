// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
)

func ringTestPort(node, port string) cableprobe.PortRef {
	return cableprobe.PortRef{NodeID: node, SwitchID: "switch-" + node, PortName: port}
}

func ringTestRun(now time.Time) (cableprobe.Run, cableprobe.ReviewRequest) {
	return ringTestRunFor(now, []string{"node-a", "node-b", "node-c"})
}

// The first node owns the receipt; each node's p0 meets the next node's p1.
func ringTestRunFor(now time.Time, nodes []string) (cableprobe.Run, cableprobe.ReviewRequest) {
	targets := make([]cableprobe.Target, 0, 3)
	selection := cableprobe.ReviewRequest{NodeIDs: slices.Clone(nodes), Ports: []cableprobe.PortRef{}}
	for _, node := range nodes {
		target := cableprobe.Target{NodeID: node, Principal: "principal-" + node, Ports: []cableprobe.Port{}}
		for i, port := range []string{"p0", "p1"} {
			ref := ringTestPort(node, port)
			selection.Ports = append(selection.Ports, ref)
			target.Ports = append(target.Ports, cableprobe.Port{SwitchID: ref.SwitchID, PortName: ref.PortName, Interfaces: []cableprobe.Interface{{Name: node + "-primary-" + port, Index: i + 2, MAC: "02:00:00:00:00:0" + string(rune('1'+i))}, {Name: node + "-alias-" + port, Index: i + 12, MAC: "02:00:00:00:00:1" + string(rune('1'+i))}}})
		}
		targets = append(targets, target)
	}
	diagnostics := &cableprobe.RunDiagnostics{Participants: []cableprobe.ParticipantDiagnostic{}}
	for _, node := range nodes {
		diagnostics.Participants = append(diagnostics.Participants, cableprobe.ParticipantDiagnostic{NodeID: node, Phase: "completed", WorkerState: "completed", CleanupConfirmed: true})
	}
	finished := now.Add(-time.Second).UnixMilli()
	return cableprobe.Run{RunID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ReviewID: "review", OwnerNodeID: nodes[0], State: "completed", Result: "reciprocal-observations", CleanupConfirmed: true, StartedAt: finished - 20_000, FinishedAt: finished, Targets: targets, Edges: []cableprobe.Edge{{Left: ringTestPort(nodes[0], "p0"), Right: ringTestPort(nodes[1], "p1")}, {Left: ringTestPort(nodes[0], "p1"), Right: ringTestPort(nodes[2], "p0")}, {Left: ringTestPort(nodes[1], "p0"), Right: ringTestPort(nodes[2], "p1")}}, Topology: &cableprobe.Topology{Layout: "ring", Status: "matched"}, Diagnostics: diagnostics}, selection
}

func TestFabricRingReceiptRequiresFreshExactCleanMatchedRun(t *testing.T) {
	now := time.Now()
	run, selection := ringTestRun(now)
	if !fabricRingReceiptValid(run, selection, now) {
		t.Fatal("exact fresh ring receipt was rejected")
	}
	roles, ok := fabricRingRoles(run)
	if !ok || !slices.Equal(roles, []string{"node-a", "node-b", "node-c"}) {
		t.Fatalf("roles=%v ok=%v", roles, ok)
	}
	cases := map[string]func(*cableprobe.Run, *cableprobe.ReviewRequest){
		"stale": func(r *cableprobe.Run, _ *cableprobe.ReviewRequest) {
			r.FinishedAt = now.Add(-121 * time.Second).UnixMilli()
		},
		"future":  func(r *cableprobe.Run, _ *cableprobe.ReviewRequest) { r.FinishedAt = now.Add(time.Second).UnixMilli() },
		"cleanup": func(r *cableprobe.Run, _ *cableprobe.ReviewRequest) { r.CleanupConfirmed = false },
		"participant cleanup": func(r *cableprobe.Run, _ *cableprobe.ReviewRequest) {
			r.Diagnostics.Participants[1].CleanupConfirmed = false
		},
		"topology": func(r *cableprobe.Run, _ *cableprobe.ReviewRequest) { r.Topology.Status = "missing" },
		"miswired-owner-p0-to-peer-p0": func(r *cableprobe.Run, _ *cableprobe.ReviewRequest) {
			r.Edges[0].Right = ringTestPort("node-b", "p0")
		},
		"selection": func(_ *cableprobe.Run, s *cableprobe.ReviewRequest) { s.Ports[0].PortName = "p9" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate, selected := cloneCableRun(run), cableprobe.ReviewRequest{NodeIDs: slices.Clone(selection.NodeIDs), Ports: slices.Clone(selection.Ports)}
			mutate(&candidate, &selected)
			if fabricRingReceiptValid(candidate, selected, now) && name != "miswired-owner-p0-to-peer-p0" {
				t.Fatal("invalid receipt accepted")
			}
			if name == "miswired-owner-p0-to-peer-p0" {
				if _, ok := fabricRingRoles(candidate); ok {
					t.Fatal("invalid sealed edge pattern accepted")
				}
			}
		})
	}
}

func ringTestTarget(node, principal, switchID string) fabricTarget {
	return fabricTarget{NodeID: node, Principal: principal, Ports: []fabricTargetPort{{SwitchID: switchID, PortName: "p0"}, {SwitchID: switchID, PortName: "p1"}}, Interfaces: []fabricInterface{
		{Name: node + "-p0", Index: 2, MAC: "02:00:00:00:00:01", PhysicalPort: fabricPhysicalPort{Source: "linux-sysfs", SwitchID: switchID, PortName: "p0"}, Addresses: []string{}, Driver: "mlx5_core", RDMADevices: []string{node + "-mlx5-0"}, MTU: 1500},
		{Name: node + "-p1", Index: 3, MAC: "02:00:00:00:00:02", PhysicalPort: fabricPhysicalPort{Source: "linux-sysfs", SwitchID: switchID, PortName: "p1"}, Addresses: []string{}, Driver: "mlx5_core", RDMADevices: []string{node + "-mlx5-1"}, MTU: 1500},
	}}
}

func TestFabricRingUsesOnlySealed31EndpointsAndPublishesSixDirections(t *testing.T) {
	targets := []fabricTarget{ringTestTarget("node-a", "principal-a", "switch-a"), ringTestTarget("node-b", "principal-b", "switch-b"), ringTestTarget("node-c", "principal-c", "switch-c")}
	inventories := []fabricInventory{{NodeID: "node-a", Interfaces: []fabricObservedInterface{}, Routes: []string{"192.168.0.0/24"}}, {NodeID: "node-b", Interfaces: []fabricObservedInterface{}, Routes: []string{}}, {NodeID: "node-c", Interfaces: []fabricObservedInterface{}, Routes: []string{}}}
	if err := fabricAllocateRing(targets, inventories); err != nil {
		t.Fatal(err)
	}
	want := []string{"10.253.0.0/31", "10.253.0.2/31", "10.253.0.4/31", "10.253.0.1/31", "10.253.0.3/31", "10.253.0.5/31"}
	var got []string
	for _, target := range targets {
		for _, iface := range target.Interfaces {
			got = append(got, iface.Address)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("addresses=%v want=%v", got, want)
	}
	candidates, err := fabricRingCandidates(targets)
	if err != nil || len(candidates) != 6 || candidates[0].Address != "10.253.0.0" || candidates[0].PeerAddress != "10.253.0.1" || candidates[5].Address != "10.253.0.5" || candidates[5].PeerAddress != "10.253.0.4" {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	inventories[1].Routes = []string{"10.253.0.0/29"}
	if err := fabricAllocateRing(targets, inventories); err == nil {
		t.Fatal("pool collision was accepted")
	}
}

func TestQualifiedFabricRejectsMereActiveAndBindsDigest(t *testing.T) {
	targets := ringTestAllocated(t)
	service := &fabricService{runs: map[string]*fabricRunRecord{"op": {Public: fabricOperation{OperationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RecipeID: fabricRingRecipe, State: "active", Targets: targets}}}}
	if _, _, _, err := service.qualifiedFabric([]string{"node-a", "node-b", "node-c"}); err == nil {
		t.Fatal("mere active addresses were relabeled as qualified")
	}
	base, err := fabricCandidates(fabricRingRecipe, targets)
	if err != nil {
		t.Fatal(err)
	}
	run := service.runs["op"]
	run.Public.QualifiedAt = time.Now().UnixMilli()
	run.Public.CandidateIPs = fabricLaneEndpoints(fabricTestProve(base, func(i int) int { return i }))
	run.Public.QualificationDigest = fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, run.Public.CandidateIPs)
	service.qualified = map[string]string{run.Public.OperationID: run.Public.QualificationDigest}
	if _, _, _, err := service.qualifiedFabric([]string{"node-a", "node-b", "node-c"}); err == nil {
		t.Fatal("lane proofs alone qualified a routed ring")
	}
	run.Public.CandidateIPs = fabricTestProve(base, func(i int) int { return i })
	run.Public.QualificationDigest = fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, run.Public.CandidateIPs)
	service.qualified = map[string]string{run.Public.OperationID: run.Public.QualificationDigest}
	op, digest, endpoints, err := service.qualifiedFabric([]string{"node-c", "node-a", "node-b"})
	if err != nil || op != run.Public.OperationID || digest != run.Public.QualificationDigest || len(endpoints) != 9 || len(fabricLaneEndpoints(endpoints)) != 6 {
		t.Fatalf("qualification=%s %s %+v err=%v", op, digest, endpoints, err)
	}
	duplicate := cloneFabricOperation(run.Public)
	duplicate.OperationID, duplicate.ReviewID = "dddddddddddddddddddddddddddddddd", "dddddddddddddddddddddddddddddddd"
	duplicate.QualificationDigest = fabricQualificationDigest(duplicate.OperationID, duplicate.RecipeID, duplicate.Targets, duplicate.CandidateIPs)
	service.runs["duplicate"] = &fabricRunRecord{Public: duplicate}
	service.qualified[duplicate.OperationID] = duplicate.QualificationDigest
	if _, _, _, err := service.qualifiedFabric([]string{"node-a", "node-b", "node-c"}); err == nil {
		t.Fatal("ambiguous active fabric was selected nondeterministically")
	}
}

func TestQualifiedFabricPreservesTwoNode30Receipt(t *testing.T) {
	targets, inventories := fabricServiceTargets(t)
	if err := fabricAllocate(targets, inventories); err != nil {
		t.Fatal(err)
	}
	candidates, err := fabricTwoNodeCandidates(targets)
	if err != nil || len(candidates) != 4 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	for i := range candidates {
		candidates[i].RDMAPort, candidates[i].GIDIndex, candidates[i].GIDType = 1, i, "RoCE v2"
	}
	op := fabricOperation{OperationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RecipeID: fabricRecipe, State: "active", Targets: targets, QualifiedAt: 1, CandidateIPs: candidates}
	op.QualificationDigest = fabricQualificationDigest(op.OperationID, op.RecipeID, op.Targets, op.CandidateIPs)
	service := &fabricService{runs: map[string]*fabricRunRecord{"op": {Public: op}}, qualified: map[string]string{op.OperationID: op.QualificationDigest}}
	operationID, digest, endpoints, err := service.qualifiedFabric([]string{"node-b", "node-a"})
	if err != nil || operationID != op.OperationID || digest != op.QualificationDigest || len(endpoints) != 4 {
		t.Fatalf("operation=%s digest=%s endpoints=%+v err=%v", operationID, digest, endpoints, err)
	}
}

func TestFabricRouteSelectionAccepts31NetworkEndpointOnlyOnExactDevice(t *testing.T) {
	iface := ringTestTarget("node-a", "principal-a", "switch-a").Interfaces[0]
	iface.Address = "10.253.0.0/31"
	if !fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","dev":"node-a-p0","prefsrc":"10.253.0.0","flags":[]}]`), iface, "10.253.0.1", "") {
		t.Fatal("canonical /31 .0 endpoint route rejected")
	}
	if !fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","from":"10.253.0.0","dev":"node-a-p0","flags":[],"uid":1000,"cache":[]}]`), iface, "10.253.0.1", "") {
		t.Fatal("source-constrained route get reply rejected")
	}
	if fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","from":"10.253.0.2","dev":"node-a-p0","flags":[]}]`), iface, "10.253.0.1", "") {
		t.Fatal("route from another source accepted")
	}
	if fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","dev":"wlP9s9","prefsrc":"10.253.0.0"}]`), iface, "10.253.0.1", "") {
		t.Fatal("wrong-device route accepted")
	}
}

// ringTestInventory is one node's p0 and p1 ports, each with two functions.
func ringTestInventory(node string) fabricInventory {
	facts := fabricServiceInventory(node)
	for i := 0; i < 2; i++ {
		alias := facts.Interfaces[i]
		alias.Name += "-p1"
		alias.Index += 10
		alias.MAC = "02:00:00:00:01:0" + string(rune('1'+i))
		alias.PhysicalPort.PortName = "p1"
		alias.RDMADevices = []string{node + "-mlx5-p1-" + string(rune('0'+i))}
		facts.Interfaces = append(facts.Interfaces, alias)
	}
	return facts
}

func fabricRingServiceFixture(t *testing.T) (*fabricService, *fabricRunRecord) {
	t.Helper()
	inventories := []fabricInventory{}
	targets := []fabricTarget{}
	plans := []cableLaunchPlan{}
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		facts := ringTestInventory(node)
		target, err := fabricSelectedRingTarget(facts, []cableprobe.PortRef{ringTestPort(node, "p0"), ringTestPort(node, "p1")})
		if err != nil {
			t.Fatal(err)
		}
		inventories, targets = append(inventories, facts), append(targets, target)
		plans = append(plans, cableLaunchPlan{NodeID: node, Principal: facts.Principal})
	}
	if err := fabricAllocateRing(targets, inventories); err != nil {
		t.Fatal(err)
	}
	service := newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	run := &fabricRunRecord{Public: fabricOperation{SchemaVersion: 1, OperationID: id, ReviewID: id, OwnerNodeID: "node-a", RecipeID: fabricRingRecipe, CableRunID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", State: "applying", Targets: targets, CreatedAt: time.Now().UnixMilli()}, OwnerPrincipal: "principal-node-a", AdministratorApproved: true, Plans: plans, Attempted: make([]bool, 3), Reserved: make([]bool, 3), cancel: cancel, done: make(chan struct{})}
	service.runs[id] = run
	return service, run
}

func TestFabricRingPartialApplyRollsBackOnlyOwnedAttempts(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	var calls []string
	service.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		calls = append(calls, request.Method+":"+target.NodeID)
		return fabricControlResult{}, nil
	}
	service.worker = func(_ context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		calls = append(calls, request.Method+":"+plan.NodeID)
		if request.Method == "apply" && plan.NodeID == "node-c" {
			return fabricWorkerResult{FailureCode: "network-manager-generation-changed"}, nil
		}
		return fabricWorkerResult{CleanupConfirmed: request.Method == "rollback"}, nil
	}
	service.execute(context.Background(), run)
	want := []string{"reserve:node-a", "reserve:node-b", "reserve:node-c", "inspect:node-a", "inspect:node-b", "inspect:node-c", "apply:node-a", "apply:node-b", "apply:node-c", "rollback:node-c", "release:node-c", "rollback:node-b", "release:node-b", "rollback:node-a", "release:node-a"}
	if !reflect.DeepEqual(calls, want) || run.Public.State != "failed" || !run.Public.CleanupConfirmed || run.Public.Failure == nil || *run.Public.Failure != (fabricFailure{NodeID: "node-c", Phase: "apply", Code: "network-manager-generation-changed"}) || strings.Contains(run.Public.Message, "synthetic") || !validFabricRecord(*run) {
		t.Fatalf("calls=%v state=%s cleanup=%v", calls, run.Public.State, run.Public.CleanupConfirmed)
	}
}

func TestFabricQualificationFailureRetainsExactTargetAndSafeCode(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	service.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		return fabricWorkerResult{CleanupConfirmed: request.Method == "rollback"}, nil
	}
	service.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		if request.Method != "qualify" {
			return fabricControlResult{}, nil
		}
		if target.NodeID == "node-c" {
			return fabricControlResult{FailureCode: "pinned-identity-unavailable"}, nil
		}
		return fabricControlResult{Qualified: true, Candidates: fabricTestProve(request.Candidates, func(i int) int { return i + 2 })}, nil
	}

	service.execute(context.Background(), run)
	want := fabricFailure{NodeID: "node-c", Phase: "qualify", Code: "pinned-identity-unavailable"}
	if run.Public.State != "failed" || !run.Public.CleanupConfirmed || !run.Public.EffectsApplied || run.Public.Failure == nil || *run.Public.Failure != want || len(run.Public.CandidateIPs) != 0 || run.Public.QualificationDigest != "" || !validFabricRecord(*run) {
		t.Fatalf("qualification failure lost typed attribution or cleanup: %+v", run.Public)
	}
}

func TestFabricCurrentStatusPreservesRollbackRefusal(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	candidates, err := fabricCandidates(run.Public.RecipeID, run.Public.Targets)
	if err != nil {
		t.Fatal(err)
	}
	candidates = fabricTestProve(candidates, func(i int) int { return i })
	run.Public.State = "active"
	run.Public.EffectsApplied = true
	run.Public.Message = fabricRollbackAccessUnavailable
	run.Public.QualifiedAt = time.Now().UnixMilli()
	run.Public.CandidateIPs = candidates
	run.Public.QualificationDigest = fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, candidates)
	for i := range run.Attempted {
		run.Attempted[i] = true
	}
	service.inventory = func(context.Context, string) (fabricInventory, error) {
		t.Fatal("rollback refusal was overwritten by active requalification")
		return fabricInventory{}, errors.New("unreachable")
	}
	got, err := service.currentStatus(context.Background(), run.Public.OperationID)
	if err != nil || got.Message != fabricRollbackAccessUnavailable {
		t.Fatalf("rollback refusal was not preserved: %+v %v", got, err)
	}
}

func TestFabricRequalifyFailureNamesParticipantAndCode(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		if target.NodeID == "node-b" {
			return fabricControlResult{FailureCode: "route-unavailable"}, nil
		}
		proved := slices.Clone(request.Candidates)
		for i := range proved {
			proved[i].RDMAPort, proved[i].GIDIndex, proved[i].GIDType = 1, 3, "RoCE v2"
		}
		return fabricControlResult{Qualified: true, Candidates: proved}, nil
	}
	err := s.requalifyActive(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "node-b") || !strings.Contains(err.Error(), "route-unavailable") {
		t.Fatalf("requalification failure hid its participant and code: %v", err)
	}
}

func TestFabricQualificationPrecedesCandidatePublicationAndActiveRelease(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	var calls []string
	service.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		calls = append(calls, request.Method+":"+target.NodeID)
		if request.Method != "qualify" {
			return fabricControlResult{}, nil
		}
		return fabricControlResult{Qualified: true, Candidates: fabricTestProve(request.Candidates, func(i int) int { return i + 2 })}, nil
	}
	service.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if len(run.Public.CandidateIPs) != 0 {
			t.Fatal("CandidateIPs published before every route and identity proof completed")
		}
		return fabricWorkerResult{}, nil
	}
	service.execute(context.Background(), run)
	if run.Public.State != "active" || len(run.Public.CandidateIPs) != 9 || len(fabricLaneEndpoints(run.Public.CandidateIPs)) != 6 || run.Public.QualifiedAt <= 0 || len(run.Public.QualificationDigest) != 64 {
		t.Fatalf("qualified operation=%+v", run.Public)
	}
	wantTail := []string{"qualify:node-a", "qualify:node-b", "qualify:node-c", "release:node-a", "release:node-b", "release:node-c"}
	if !reflect.DeepEqual(calls[len(calls)-len(wantTail):], wantTail) {
		t.Fatalf("qualification/release order=%v", calls)
	}
	if !validFabricRecord(*run) {
		t.Fatal("qualified retained operation failed its exact journal contract")
	}
}

func fabricActiveInventory(target fabricTarget) fabricInventory {
	interfaces := make([]fabricObservedInterface, 0, len(target.Interfaces))
	for _, iface := range target.Interfaces {
		current := iface
		current.Addresses = append(slices.Clone(iface.Addresses), iface.Address)
		interfaces = append(interfaces, fabricObservedInterface{fabricInterface: current})
	}
	return fabricInventory{NodeID: target.NodeID, Principal: target.Principal, Interfaces: interfaces, Routes: []string{}}
}

func TestFabricRestartRequiresFreshRouteGIDHCAAndPinnedPeerProof(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	service.control = func(_ context.Context, _ fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		return fabricControlResult{Qualified: true, Candidates: fabricTestProve(request.Candidates, func(i int) int { return i + 2 })}, nil
	}
	service.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
		return fabricWorkerResult{}, nil
	}
	service.execute(context.Background(), run)
	restored := newFabricService(service.m)
	if _, _, _, err := restored.qualifiedFabric([]string{"node-a", "node-b", "node-c"}); err == nil {
		t.Fatal("restart reused persisted CandidateIPs without a fresh proof")
	}
	inventories := map[string]fabricInventory{}
	for _, target := range run.Public.Targets {
		inventories[target.NodeID] = fabricActiveInventory(target)
	}
	restored.inventory = func(_ context.Context, nodeID string) (fabricInventory, error) { return inventories[nodeID], nil }
	mode := "valid"
	restored.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		if request.Method != "requalify" {
			return fabricControlResult{}, errors.New("unexpected control method")
		}
		if mode == "route" && target.NodeID == "node-a" || mode == "pinned" && target.NodeID == "node-c" {
			return fabricControlResult{}, errors.New("synthetic fresh proof failure")
		}
		qualified := fabricTestProve(request.Candidates, func(i int) int { return i + 2 })
		if mode == "gateway" && target.NodeID == "node-c" {
			qualified[len(qualified)-1].Gateway = "10.253.0.3"
		}
		if mode == "gid" && target.NodeID == "node-b" {
			qualified[0].GIDIndex++
		}
		if mode == "hca" && target.NodeID == "node-b" {
			qualified[0].RDMADevice += "-drift"
		}
		return fabricControlResult{Qualified: true, Candidates: qualified}, nil
	}
	if _, err := restored.currentStatus(context.Background(), run.Public.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := restored.qualifiedFabric([]string{"node-a", "node-b", "node-c"}); err != nil {
		t.Fatalf("fresh restart proof was not consumable: %v", err)
	}
	for _, failure := range []string{"route", "gateway", "gid", "hca", "pinned"} {
		mode = failure
		if _, err := restored.currentStatus(context.Background(), run.Public.OperationID); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := restored.qualifiedFabric([]string{"node-a", "node-b", "node-c"}); err == nil {
			t.Fatalf("%s drift left persisted CandidateIPs consumable", failure)
		}
		mode = "valid"
		if _, err := restored.currentStatus(context.Background(), run.Public.OperationID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFabricQualificationFailureWithdrawsCandidatesAndRollsBackAllTargets(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	var rolledBack []string
	service.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		if request.Method == "qualify" && target.NodeID == "node-b" {
			return fabricControlResult{}, errors.New("synthetic pinned identity mismatch")
		}
		if request.Method == "qualify" {
			return fabricControlResult{Qualified: true, Candidates: fabricTestProve(request.Candidates, func(i int) int { return i })}, nil
		}
		return fabricControlResult{}, nil
	}
	service.worker = func(_ context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if request.Method == "rollback" {
			rolledBack = append(rolledBack, plan.NodeID)
		}
		return fabricWorkerResult{CleanupConfirmed: request.Method == "rollback"}, nil
	}
	service.execute(context.Background(), run)
	if !reflect.DeepEqual(rolledBack, []string{"node-c", "node-b", "node-a"}) || run.Public.State != "failed" || !run.Public.CleanupConfirmed || run.Public.QualifiedAt != 0 || run.Public.QualificationDigest != "" || len(run.Public.CandidateIPs) != 0 {
		t.Fatalf("rollback=%v operation=%+v", rolledBack, run.Public)
	}
}

// fabricTestProve is what a participant's successful proof reports: a RoCE
// binding for each lane and every routed proof exactly as requested.
func fabricTestProve(candidates []fabricCandidateIP, gidIndex func(int) int) []fabricCandidateIP {
	proved := slices.Clone(candidates)
	for i := range proved {
		if proved[i].Gateway == "" {
			proved[i].RDMAPort, proved[i].GIDIndex, proved[i].GIDType = 1, gidIndex(i), "RoCE v2"
		}
	}
	return proved
}

func ringTestAllocated(t *testing.T) []fabricTarget {
	t.Helper()
	targets := []fabricTarget{ringTestTarget("node-a", "principal-a", "switch-a"), ringTestTarget("node-b", "principal-b", "switch-b"), ringTestTarget("node-c", "principal-c", "switch-c")}
	empty := fabricInventory{Interfaces: []fabricObservedInterface{}, Routes: []string{}}
	if err := fabricAllocateRing(targets, []fabricInventory{empty, empty, empty}); err != nil {
		t.Fatal(err)
	}
	return targets
}

func ringTestClone(targets []fabricTarget) []fabricTarget {
	out := slices.Clone(targets)
	for i := range out {
		out[i].Interfaces = slices.Clone(out[i].Interfaces)
		for j := range out[i].Interfaces {
			out[i].Interfaces[j].Routes = slices.Clone(out[i].Interfaces[j].Routes)
		}
	}
	return out
}

func ringTestUnrouted(targets []fabricTarget) []fabricTarget {
	out := ringTestClone(targets)
	for i := range out {
		out[i].AdvertisedAddress = ""
		for j := range out[i].Interfaces {
			out[i].Interfaces[j].Routes = nil
		}
	}
	return out
}

func TestFabricRingRoutedAllocationReachesEveryAdvertisedAddressOverTheSharedCable(t *testing.T) {
	targets := ringTestAllocated(t)
	wantAdvertised := []string{"10.253.0.0", "10.253.0.4", "10.253.0.3"}
	wantRoutes := [][]fabricRoute{
		{{Destination: "10.253.0.4/32", Gateway: "10.253.0.1"}},
		{{Destination: "10.253.0.3/32", Gateway: "10.253.0.5"}},
		{{Destination: "10.253.0.0/32", Gateway: "10.253.0.2"}},
	}
	for role, target := range targets {
		p0, _ := fabricInterfaceAt(target, "p0")
		p1, _ := fabricInterfaceAt(target, "p1")
		if target.AdvertisedAddress != wantAdvertised[role] || !reflect.DeepEqual(p0.Routes, wantRoutes[role]) || len(p1.Routes) != 0 {
			t.Fatalf("role %d advertised=%s p0=%+v p1=%+v", role, target.AdvertisedAddress, p0.Routes, p1.Routes)
		}
	}
	if !fabricRecipeTargetsValid(fabricRingRecipe, targets) || fabricRecipeTargetsValid(fabricRingRetainedRecipe, targets) || fabricRecipeTargetsValid(fabricRecipe, targets) {
		t.Fatal("routed targets were not bound to exactly the routed recipe")
	}
	candidates, err := fabricCandidates(fabricRingRecipe, targets)
	if err != nil || len(candidates) != 9 || len(fabricLaneEndpoints(candidates)) != 6 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	wantRouted := []fabricCandidateIP{
		{NodeID: "node-a", PeerNodeID: "node-b", PeerPrincipal: "principal-b", Address: "10.253.0.0", PeerAddress: "10.253.0.4", InterfaceName: "node-a-p0", InterfaceIndex: 2, MAC: "02:00:00:00:00:01", SwitchID: "switch-a", PortName: "p0", Gateway: "10.253.0.1"},
		{NodeID: "node-b", PeerNodeID: "node-c", PeerPrincipal: "principal-c", Address: "10.253.0.4", PeerAddress: "10.253.0.3", InterfaceName: "node-b-p0", InterfaceIndex: 2, MAC: "02:00:00:00:00:01", SwitchID: "switch-b", PortName: "p0", Gateway: "10.253.0.5"},
		{NodeID: "node-c", PeerNodeID: "node-a", PeerPrincipal: "principal-a", Address: "10.253.0.3", PeerAddress: "10.253.0.0", InterfaceName: "node-c-p0", InterfaceIndex: 2, MAC: "02:00:00:00:00:01", SwitchID: "switch-c", PortName: "p0", Gateway: "10.253.0.2"},
	}
	if !reflect.DeepEqual(candidates[6:], wantRouted) {
		t.Fatalf("routed proofs=%+v", candidates[6:])
	}
	for _, from := range targets {
		for _, to := range targets {
			if from.NodeID == to.NodeID {
				continue
			}
			var lane, reach *fabricCandidateIP
			for i := range candidates {
				candidate := &candidates[i]
				if candidate.NodeID != from.NodeID || candidate.PeerNodeID != to.NodeID {
					continue
				}
				if candidate.Gateway == "" {
					lane = candidate
				}
				if candidate.PeerAddress == to.AdvertisedAddress {
					reach = candidate
				}
			}
			// Replies stay on attached links only if the request leaves on the
			// same cable, from the same source, through that cable's peer.
			if lane == nil || reach == nil || reach.InterfaceIndex != lane.InterfaceIndex || reach.Address != lane.Address || reach.Gateway != "" && reach.Gateway != lane.PeerAddress {
				t.Fatalf("%s cannot reach %s's advertised address over their shared cable", from.NodeID, to.NodeID)
			}
		}
	}
}

func TestFabricRecipeTargetsRejectEveryRouteMismatch(t *testing.T) {
	routed := ringTestAllocated(t)
	for name, mutate := range map[string]func([]fabricTarget){
		"missing route": func(targets []fabricTarget) { targets[0].Interfaces[0].Routes = nil },
		"extra route": func(targets []fabricTarget) {
			targets[0].Interfaces[0].Routes = append(targets[0].Interfaces[0].Routes, fabricRoute{Destination: "10.253.0.5/32", Gateway: "10.253.0.1"})
		},
		"wrong gateway":     func(targets []fabricTarget) { targets[1].Interfaces[0].Routes[0].Gateway = "10.253.0.4" },
		"wrong destination": func(targets []fabricTarget) { targets[1].Interfaces[0].Routes[0].Destination = "10.253.0.2/32" },
		"route on p1": func(targets []fabricTarget) {
			targets[2].Interfaces[0].Routes, targets[2].Interfaces[1].Routes = nil, targets[2].Interfaces[0].Routes
		},
		"extra p1 route": func(targets []fabricTarget) {
			targets[2].Interfaces[1].Routes = []fabricRoute{{Destination: "10.253.0.4/32", Gateway: "10.253.0.4"}}
		},
		"advertised p1":      func(targets []fabricTarget) { targets[0].AdvertisedAddress = "10.253.0.2" },
		"advertised missing": func(targets []fabricTarget) { targets[1].AdvertisedAddress = "" },
	} {
		t.Run(name, func(t *testing.T) {
			targets := ringTestClone(routed)
			mutate(targets)
			if fabricRecipeTargetsValid(fabricRingRecipe, targets) {
				t.Fatal("routed recipe accepted a route mismatch")
			}
			if _, err := fabricCandidates(fabricRingRecipe, targets); err == nil {
				t.Fatal("route mismatch produced qualification candidates")
			}
		})
	}
	unrouted := ringTestUnrouted(routed)
	if !fabricRecipeTargetsValid(fabricRingRetainedRecipe, unrouted) || fabricRecipeTargetsValid(fabricRingRecipe, unrouted) {
		t.Fatal("unrouted ring targets were not bound to exactly the retained recipe")
	}
	retainedWithRoute := ringTestClone(unrouted)
	retainedWithRoute[0].Interfaces[0].Routes = routed[0].Interfaces[0].Routes
	retainedWithRoute[0].AdvertisedAddress = routed[0].AdvertisedAddress
	if fabricRecipeTargetsValid(fabricRingRetainedRecipe, retainedWithRoute) {
		t.Fatal("retained ring recipe accepted a host route")
	}
	direct, inventories := fabricServiceTargets(t)
	if err := fabricAllocate(direct, inventories); err != nil {
		t.Fatal(err)
	}
	if !fabricRecipeTargetsValid(fabricRecipe, direct) {
		t.Fatal("direct targets rejected")
	}
	direct[0].Interfaces[0].Routes = []fabricRoute{{Destination: "10.253.0.4/32", Gateway: "172.31.240.2"}}
	if fabricRecipeTargetsValid(fabricRecipe, direct) || validFabricTargetRoutes(direct[0]) {
		t.Fatal("direct recipe accepted a host route")
	}
}

func TestFabricRoutedRecordValidationRejectsRouteDrift(t *testing.T) {
	_, run := fabricRingServiceFixture(t)
	if run.Public.RecipeID != fabricRingRecipe || !validFabricRecord(*run) {
		t.Fatal("exact routed ring record rejected")
	}
	for name, mutate := range map[string]func(*fabricRunRecord){
		"gateway":          func(r *fabricRunRecord) { r.Public.Targets[0].Interfaces[0].Routes[0].Gateway = "10.253.0.5" },
		"retained recipe":  func(r *fabricRunRecord) { r.Public.RecipeID = fabricRingRetainedRecipe },
		"advertised":       func(r *fabricRunRecord) { r.Public.Targets[2].AdvertisedAddress = "10.253.0.5" },
		"routes withdrawn": func(r *fabricRunRecord) { r.Public.Targets = ringTestUnrouted(r.Public.Targets) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *run
			changed.Public.Targets = ringTestClone(run.Public.Targets)
			mutate(&changed)
			if validFabricRecord(changed) {
				t.Fatal("route drift passed the retained record contract")
			}
		})
	}
}

func TestFabricRoutedQualificationRequiresEveryExactRoutedProof(t *testing.T) {
	targets := ringTestAllocated(t)
	base, err := fabricCandidates(fabricRingRecipe, targets)
	if err != nil {
		t.Fatal(err)
	}
	proved := fabricTestProve(base, func(i int) int { return i })
	if !fabricCandidatesQualified(base, proved) {
		t.Fatal("complete routed proof rejected")
	}
	if fabricCandidatesQualified(base, fabricLaneEndpoints(proved)) {
		t.Fatal("lane proofs alone qualified a routed ring")
	}
	for name, mutate := range map[string]func(*fabricCandidateIP){
		"wrong gateway":   func(c *fabricCandidateIP) { c.Gateway = "10.253.0.2" },
		"wrong interface": func(c *fabricCandidateIP) { c.InterfaceName, c.InterfaceIndex, c.PortName = "node-a-p1", 3, "p1" },
		"wrong source":    func(c *fabricCandidateIP) { c.Address = "10.253.0.2" },
		"RDMA claim":      func(c *fabricCandidateIP) { c.RDMADevice, c.RDMAPort, c.GIDType = "node-a-mlx5-0", 1, "RoCE v2" },
		"unrouted":        func(c *fabricCandidateIP) { c.Gateway = "" },
	} {
		changed := slices.Clone(proved)
		mutate(&changed[6])
		if fabricCandidatesQualified(base, changed) {
			t.Fatalf("%s routed proof accepted", name)
		}
	}
	retained, err := fabricCandidates(fabricRingRetainedRecipe, ringTestUnrouted(targets))
	if err != nil || len(retained) != 6 || !fabricCandidatesQualified(retained, fabricTestProve(retained, func(i int) int { return i })) {
		t.Fatalf("retained ring qualification changed: %+v %v", retained, err)
	}
	if fabricCandidatesQualified(retained, proved) {
		t.Fatal("routed proofs joined a retained ring")
	}
}

func TestFabricLocalRoutedProofBindsOnlyTheReviewedP0Route(t *testing.T) {
	targets := ringTestAllocated(t)
	base, err := fabricCandidates(fabricRingRecipe, targets)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := fabricLocalEndpoint(targets[0], base[0]); !ok {
		t.Fatal("lane candidate no longer binds its owned interface")
	}
	routed := base[6]
	iface, route, ok := fabricLocalEndpoint(targets[0], routed)
	if !ok || iface.PhysicalPort.PortName != "p0" || route != (fabricRoute{Destination: "10.253.0.4/32", Gateway: "10.253.0.1"}) {
		t.Fatalf("routed candidate binding=%+v %+v %v", iface, route, ok)
	}
	for name, mutate := range map[string]func(*fabricCandidateIP){
		"wrong gateway":     func(c *fabricCandidateIP) { c.Gateway = "10.253.0.5" },
		"wrong destination": func(c *fabricCandidateIP) { c.PeerAddress = "10.253.0.3" },
		"wrong interface": func(c *fabricCandidateIP) {
			c.InterfaceName, c.InterfaceIndex, c.MAC, c.PortName = "node-a-p1", 3, "02:00:00:00:00:02", "p1"
		},
		"wrong source": func(c *fabricCandidateIP) { c.Address = "10.253.0.1" },
		"RDMA claim":   func(c *fabricCandidateIP) { c.RDMADevice = "node-a-mlx5-0" },
	} {
		candidate := routed
		mutate(&candidate)
		if _, _, ok := fabricLocalEndpoint(targets[0], candidate); ok {
			t.Fatalf("%s routed candidate bound a local endpoint", name)
		}
	}
	if !validFabricTargetRoutes(ringTestUnrouted(targets)[0]) || !validFabricTargetRoutes(targets[0]) {
		t.Fatal("well-formed routed or unrouted target rejected")
	}
	moved := ringTestClone(targets)[0]
	moved.Interfaces[0].Routes, moved.Interfaces[1].Routes = nil, moved.Interfaces[0].Routes
	moved.AdvertisedAddress = "10.253.0.2"
	if validFabricTargetRoutes(moved) {
		t.Fatal("native worker shape admitted a p1 host route")
	}
}

func TestFabricRoutedSelectionRequiresExactGatewayDeviceAndSource(t *testing.T) {
	iface := ringTestAllocated(t)[0].Interfaces[0]
	accepted := `[{"dst":"10.253.0.4","gateway":"10.253.0.1","from":"10.253.0.0","dev":"node-a-p0","flags":[],"uid":1000,"cache":[]}]`
	if !fabricRouteSelectionQualified([]byte(accepted), iface, "10.253.0.4", "10.253.0.1") {
		t.Fatal("exact routed selection rejected")
	}
	if fabricRouteSelectionQualified([]byte(accepted), iface, "10.253.0.4", "") {
		t.Fatal("a gatewayed reply satisfied an on-link lane proof")
	}
	for name, data := range map[string]string{
		"on-link fallback": `[{"dst":"10.253.0.4","from":"10.253.0.0","dev":"node-a-p0"}]`,
		"wrong gateway":    `[{"dst":"10.253.0.4","gateway":"10.253.0.5","from":"10.253.0.0","dev":"node-a-p0"}]`,
		"other interface":  `[{"dst":"10.253.0.4","gateway":"10.253.0.1","from":"10.253.0.0","dev":"node-a-p1"}]`,
		"management route": `[{"dst":"10.253.0.4","gateway":"192.0.2.1","from":"10.253.0.0","dev":"wlP9s9"}]`,
		"wrong source":     `[{"dst":"10.253.0.4","gateway":"10.253.0.1","from":"10.253.0.2","dev":"node-a-p0"}]`,
		"unreachable":      `[{"type":"unreachable","dst":"10.253.0.4","gateway":"10.253.0.1","from":"10.253.0.0","dev":"node-a-p0"}]`,
	} {
		if fabricRouteSelectionQualified([]byte(data), iface, "10.253.0.4", "10.253.0.1") {
			t.Fatalf("%s routed selection accepted", name)
		}
	}
}

func TestFabricQualificationDigestBindsRoutesAndAdvertisedAddresses(t *testing.T) {
	const operationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	targets := ringTestAllocated(t)
	base, err := fabricCandidates(fabricRingRecipe, targets)
	if err != nil {
		t.Fatal(err)
	}
	candidates := fabricTestProve(base, func(i int) int { return i })
	digest := fabricQualificationDigest(operationID, fabricRingRecipe, targets, candidates)
	for name, mutate := range map[string]func([]fabricTarget){
		"gateway":     func(changed []fabricTarget) { changed[0].Interfaces[0].Routes[0].Gateway = "10.253.0.5" },
		"destination": func(changed []fabricTarget) { changed[0].Interfaces[0].Routes[0].Destination = "10.253.0.3/32" },
		"withdrawn":   func(changed []fabricTarget) { changed[1].Interfaces[0].Routes = nil },
		"advertised":  func(changed []fabricTarget) { changed[2].AdvertisedAddress = "10.253.0.5" },
	} {
		changed := ringTestClone(targets)
		mutate(changed)
		if fabricQualificationDigest(operationID, fabricRingRecipe, changed, candidates) == digest {
			t.Fatalf("%s change left the qualification digest unchanged", name)
		}
	}
	rerouted := slices.Clone(candidates)
	rerouted[8].Gateway = "10.253.0.3"
	if fabricQualificationDigest(operationID, fabricRingRecipe, targets, rerouted) == digest || fabricQualificationDigest(operationID, fabricRingRetainedRecipe, targets, candidates) == digest {
		t.Fatal("routed proof or recipe change left the qualification digest unchanged")
	}
}

func TestFabricRetainedRingRemainsRequalifiableAndRollsBackWithoutRoutes(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	run.Public.RecipeID = fabricRingRetainedRecipe
	run.Public.Targets = ringTestUnrouted(run.Public.Targets)
	candidates, err := fabricCandidates(run.Public.RecipeID, run.Public.Targets)
	if err != nil || len(candidates) != 6 {
		t.Fatalf("retained candidates=%+v err=%v", candidates, err)
	}
	run.Public.State, run.Public.EffectsApplied = "active", true
	for i := range run.Attempted {
		run.Attempted[i] = true
	}
	gid := func(int) int { return 3 }
	run.Public.CandidateIPs = fabricTestProve(candidates, gid)
	run.Public.QualifiedAt = time.Now().UnixMilli()
	run.Public.QualificationDigest = fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, run.Public.CandidateIPs)
	if !validFabricRecord(*run) {
		t.Fatal("qualified retained ring record rejected")
	}
	if err := service.save(run); err != nil {
		t.Fatal(err)
	}
	restored := newFabricService(service.m)
	retained := restored.runs[run.Public.OperationID]
	if restored.recoveryFailed || retained == nil || retained.Public.RecipeID != fabricRingRetainedRecipe || retained.Public.State != "active" {
		t.Fatalf("retained ring record was not reloaded: %+v", retained)
	}
	inventories := map[string]fabricInventory{}
	for _, target := range retained.Public.Targets {
		inventories[target.NodeID] = fabricActiveInventory(target)
	}
	restored.inventory = func(_ context.Context, nodeID string) (fabricInventory, error) { return inventories[nodeID], nil }
	restored.control = func(_ context.Context, _ fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		if request.Method != "requalify" {
			return fabricControlResult{}, nil
		}
		for _, candidate := range request.Candidates {
			if candidate.Gateway != "" {
				t.Error("retained ring was asked for a routed proof")
			}
		}
		return fabricControlResult{Qualified: true, Candidates: fabricTestProve(request.Candidates, gid)}, nil
	}
	nodes := []string{"node-a", "node-b", "node-c"}
	if _, err := restored.currentStatus(context.Background(), run.Public.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, _, endpoints, err := restored.qualifiedFabric(nodes); err != nil || len(endpoints) != 6 {
		t.Fatalf("retained ring was not requalified for status: %+v %v", endpoints, err)
	}
	restored.refreshAccess = func(context.Context, *fabricRunRecord) ([]cableLaunchPlan, error) { return retained.Plans, nil }
	var rolledBack []fabricTarget
	restored.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if request.Method == "rollback" {
			rolledBack = append(rolledBack, request.Target)
		}
		return fabricWorkerResult{CleanupConfirmed: request.Method == "rollback"}, nil
	}
	if _, err := restored.cancel(run.Public.OperationID, true); err != nil {
		t.Fatal(err)
	}
	restored.mu.Lock()
	done := retained.done
	restored.mu.Unlock()
	<-done
	if retained.Public.State != "cancelled" || !retained.Public.CleanupConfirmed || len(rolledBack) != 3 || !validFabricRecord(*retained) {
		t.Fatalf("retained ring rollback was not exact: %+v", retained.Public)
	}
	for _, target := range rolledBack {
		role := slices.IndexFunc(retained.Public.Targets, func(candidate fabricTarget) bool { return candidate.NodeID == target.NodeID })
		if role < 0 || !reflect.DeepEqual(target, retained.Public.Targets[role]) || target.AdvertisedAddress != "" || len(target.Interfaces[0].Routes)+len(target.Interfaces[1].Routes) != 0 {
			t.Fatalf("retained rollback target changed: %+v", target)
		}
	}
}

func TestFabricRingReviewOffersOnlyTheRoutedRecipe(t *testing.T) {
	m, _, _ := cableTestManager(t, func(*http.Request) (*http.Response, error) {
		return cableTestResponse(t, cableTestInfo(t, time.Now(), 1)), nil
	})
	run, selection := ringTestRunFor(time.Now(), []string{"host-owner", "node-b", "node-c"})
	m.cables.runs[run.RunID] = &cableProductRun{Public: run}
	s := m.exec.fabric
	s.inventory = func(_ context.Context, node string) (fabricInventory, error) { return ringTestInventory(node), nil }
	review, err := s.review(context.Background(), cableProductReviewRequest{ReviewRequest: selection})
	if err != nil {
		t.Fatal(err)
	}
	if review.RecipeID != fabricRingRecipe || review.CableRunID != run.RunID || len(review.Targets) != 3 || !fabricRecipeTargetsValid(fabricRingRecipe, review.Targets) {
		t.Fatalf("ring review did not bind the routed recipe: %+v", review)
	}
	if review.Permission == nil || !strings.Contains(strings.Join(review.Permission.Effects, " "), "/32 host route") {
		t.Fatal("ring review effects omit the reviewed host routes")
	}
}
