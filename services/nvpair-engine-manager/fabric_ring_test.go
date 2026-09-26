// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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
	nodes := []string{"node-a", "node-b", "node-c"}
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
	return cableprobe.Run{RunID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ReviewID: "review", OwnerNodeID: "node-a", State: "completed", Result: "reciprocal-observations", CleanupConfirmed: true, StartedAt: finished - 20_000, FinishedAt: finished, Targets: targets, Edges: []cableprobe.Edge{{Left: ringTestPort("node-a", "p0"), Right: ringTestPort("node-b", "p1")}, {Left: ringTestPort("node-a", "p1"), Right: ringTestPort("node-c", "p0")}, {Left: ringTestPort("node-b", "p0"), Right: ringTestPort("node-c", "p1")}}, Topology: &cableprobe.Topology{Layout: "ring", Status: "matched"}, Diagnostics: diagnostics}, selection
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
	targets := []fabricTarget{ringTestTarget("node-a", "principal-a", "switch-a"), ringTestTarget("node-b", "principal-b", "switch-b"), ringTestTarget("node-c", "principal-c", "switch-c")}
	if err := fabricAllocateRing(targets, []fabricInventory{{Interfaces: []fabricObservedInterface{}, Routes: []string{}}, {Interfaces: []fabricObservedInterface{}, Routes: []string{}}, {Interfaces: []fabricObservedInterface{}, Routes: []string{}}}); err != nil {
		t.Fatal(err)
	}
	service := &fabricService{runs: map[string]*fabricRunRecord{"op": {Public: fabricOperation{OperationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RecipeID: fabricRingRecipe, State: "active", Targets: targets}}}}
	if _, _, _, err := service.qualifiedFabric([]string{"node-a", "node-b", "node-c"}); err == nil {
		t.Fatal("mere active addresses were relabeled as qualified")
	}
	base, _ := fabricRingCandidates(targets)
	for i := range base {
		base[i].RDMAPort, base[i].GIDIndex, base[i].GIDType = 1, i, "RoCE v2"
	}
	run := service.runs["op"]
	run.Public.QualifiedAt = time.Now().UnixMilli()
	run.Public.CandidateIPs = base
	run.Public.QualificationDigest = fabricQualificationDigest(run.Public.OperationID, run.Public.RecipeID, run.Public.Targets, base)
	service.qualified = map[string]string{run.Public.OperationID: run.Public.QualificationDigest}
	op, digest, endpoints, err := service.qualifiedFabric([]string{"node-c", "node-a", "node-b"})
	if err != nil || op != run.Public.OperationID || digest != run.Public.QualificationDigest || len(endpoints) != 6 {
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
	if !fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","dev":"node-a-p0","prefsrc":"10.253.0.0","flags":[]}]`), iface, "10.253.0.1") {
		t.Fatal("canonical /31 .0 endpoint route rejected")
	}
	if !fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","from":"10.253.0.0","dev":"node-a-p0","flags":[],"uid":1000,"cache":[]}]`), iface, "10.253.0.1") {
		t.Fatal("source-constrained route get reply rejected")
	}
	if fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","from":"10.253.0.2","dev":"node-a-p0","flags":[]}]`), iface, "10.253.0.1") {
		t.Fatal("route from another source accepted")
	}
	if fabricRouteSelectionQualified([]byte(`[{"dst":"10.253.0.1","dev":"wlP9s9","prefsrc":"10.253.0.0"}]`), iface, "10.253.0.1") {
		t.Fatal("wrong-device route accepted")
	}
}

func fabricRingServiceFixture(t *testing.T) (*fabricService, *fabricRunRecord) {
	t.Helper()
	inventories := []fabricInventory{}
	targets := []fabricTarget{}
	plans := []cableLaunchPlan{}
	for _, node := range []string{"node-a", "node-b", "node-c"} {
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
		qualified := slices.Clone(request.Candidates)
		for i := range qualified {
			qualified[i].RDMAPort, qualified[i].GIDIndex, qualified[i].GIDType = 1, i+2, "RoCE v2"
		}
		return fabricControlResult{Qualified: true, Candidates: qualified}, nil
	}

	service.execute(context.Background(), run)
	want := fabricFailure{NodeID: "node-c", Phase: "qualify", Code: "pinned-identity-unavailable"}
	if run.Public.State != "failed" || !run.Public.CleanupConfirmed || !run.Public.EffectsApplied || run.Public.Failure == nil || *run.Public.Failure != want || len(run.Public.CandidateIPs) != 0 || run.Public.QualificationDigest != "" || !validFabricRecord(*run) {
		t.Fatalf("qualification failure lost typed attribution or cleanup: %+v", run.Public)
	}
}

func TestFabricCurrentStatusPreservesRollbackRefusal(t *testing.T) {
	service, run := fabricRingServiceFixture(t)
	candidates, err := fabricRingCandidates(run.Public.Targets)
	if err != nil {
		t.Fatal(err)
	}
	for i := range candidates {
		candidates[i].RDMAPort, candidates[i].GIDIndex, candidates[i].GIDType = 1, i, "RoCE v2"
	}
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
		qualified := slices.Clone(request.Candidates)
		for i := range qualified {
			qualified[i].RDMAPort, qualified[i].GIDIndex, qualified[i].GIDType = 1, i+2, "RoCE v2"
		}
		return fabricControlResult{Qualified: true, Candidates: qualified}, nil
	}
	service.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if len(run.Public.CandidateIPs) != 0 {
			t.Fatal("CandidateIPs published before every route and identity proof completed")
		}
		return fabricWorkerResult{}, nil
	}
	service.execute(context.Background(), run)
	if run.Public.State != "active" || len(run.Public.CandidateIPs) != 6 || run.Public.QualifiedAt <= 0 || len(run.Public.QualificationDigest) != 64 {
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
		qualified := slices.Clone(request.Candidates)
		for i := range qualified {
			qualified[i].RDMAPort, qualified[i].GIDIndex, qualified[i].GIDType = 1, i+2, "RoCE v2"
		}
		return fabricControlResult{Qualified: true, Candidates: qualified}, nil
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
		qualified := slices.Clone(request.Candidates)
		for i := range qualified {
			qualified[i].RDMAPort, qualified[i].GIDIndex, qualified[i].GIDType = 1, i+2, "RoCE v2"
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
	for _, failure := range []string{"route", "gid", "hca", "pinned"} {
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
			qualified := slices.Clone(request.Candidates)
			for i := range qualified {
				qualified[i].RDMAPort, qualified[i].GIDIndex, qualified[i].GIDType = 1, i, "RoCE v2"
			}
			return fabricControlResult{Qualified: true, Candidates: qualified}, nil
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
