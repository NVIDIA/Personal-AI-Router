// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func TestDiagnosticMPISelectionProjectsRegisteredTripleWithoutChangingOriginal(t *testing.T) {
	d, run, action := closedRuntimeFixture(t, 3)
	adopted, err := d.adoptRuntime(context.Background(), action)
	if err != nil || adopted.Record == nil {
		t.Fatalf("mocked complete registration: %v", err)
	}
	record, binding := *adopted.Record, run.Binding.diagnosticParticipantBinding
	beforeRecord, _ := json.Marshal(record)
	beforeBinding, _ := json.Marshal(binding)
	for _, ids := range [][]string{{"node-a", "node-b"}, {"node-a", "node-c"}, {"node-b", "node-c"}, {"node-a", "node-b", "node-c"}, nil} {
		selected, view, err := selectDiagnosticMPIRecord(record, binding, ids)
		if err != nil {
			t.Fatal(err)
		}
		want := ids
		if want == nil {
			want = []string{"node-a", "node-b", "node-c"}
		}
		if len(selected.Targets) != len(want) || len(view.Targets) != len(want) || len(view.Pair.Members) != len(want) || selected.Targets[0].NodeID != want[0] {
			t.Fatalf("selected roster/owner changed for %v", want)
		}
		if selected.OperationID != record.OperationID || selected.ReviewID != record.ReviewID || selected.GroupID != record.GroupID || view.Pair.GroupID != closedRosterGroup(view.Pair.Members) {
			t.Fatal("selection rewrote build ownership or lost its inspection identity")
		}
		for i, id := range want {
			if selected.Targets[i].NodeID != id || view.Targets[i].NodeID != id || view.Pair.Members[i].NodeID != id {
				t.Fatal("selection changed canonical member order")
			}
			for _, original := range record.Targets {
				if original.NodeID == id && (!bytes.Equal(original.Registration, selected.Targets[i].Registration) || original.Local != selected.Targets[i].Local || original.PlanDigest != selected.Targets[i].PlanDigest || original.Attempt != selected.Targets[i].Attempt) {
					t.Fatal("selection changed original runtime bytes or ownership")
				}
			}
		}
		// The projection's selected slices/maps must not edit the original roster.
		selected.Targets[0].NodeID = "changed-view"
		view.Targets[0].Address = "changed-view"
		view.Pair.Members[0].NodeID = "changed-view"
		view.Pins[want[0]] = "changed-view"
		view.Generations[want[0]] = "changed-view"
		afterRecord, _ := json.Marshal(record)
		afterBinding, _ := json.Marshal(binding)
		if !bytes.Equal(beforeRecord, afterRecord) || !bytes.Equal(beforeBinding, afterBinding) {
			t.Fatal("projection mutated original registration or participant binding")
		}
	}
	for _, ids := range [][]string{{}, {"node-a"}, {"node-a", "node-a"}, {"node-b", "node-a"}, {"node-a", "node-z"}, {"node-a", "node-b", "node-c", "node-d"}, {"node-a", "node-\n"}} {
		if _, _, err := selectDiagnosticMPIRecord(record, binding, ids); err == nil {
			t.Fatalf("invalid selector admitted: %q", ids)
		}
	}
}

// Both histories use one original three-target build. Only public plans,
// markers and terminal operation records are written to temporary storage.
func mpiSubsetHistoryFixture(t *testing.T, snapshots bool) (*diagnosticService, map[string]*diagnosticMPIReviewBinding, map[string]diagnosticMPIRecoveryControl) {
	t.Helper()
	d := diagnosticTestService(t)
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", "node-a", "node-b", "node-c")
	d.m.mesh = clustertrust.Open(dir)
	record, facts, selection, now := mpiPlanCountFixture(t, 3)
	for i := range record.Targets {
		pin, ok := d.m.mesh.PinSHA256(record.Targets[i].Principal)
		if !ok {
			t.Fatal("fixture pin missing")
		}
		record.Targets[i].ClusterPinSHA256, facts[i].ClusterPinSHA256 = pin, pin
	}
	controllerPin, _ := d.m.mesh.PinSHA256("node-a")
	histories, controls := map[string]*diagnosticMPIReviewBinding{}, map[string]diagnosticMPIRecoveryControl{}
	for i, name := range []string{"AB", "AC"} {
		selected := record
		selected.Targets = []diagnosticManagedTarget{record.Targets[0], record.Targets[i+1]}
		selectedFacts := []diagnosticMPIParticipantFacts{facts[0], facts[i+1]}
		selection.OperationID = strings.Repeat(string(rune('1'+i)), 32)
		p, plan, key, err := compileDiagnosticMPIPlan(selected, selectedFacts, selection, now.Add(time.Duration(i)*time.Second))
		clear(key)
		if err != nil {
			t.Fatal(err)
		}
		var header diagnosticMPIHeader
		if json.Unmarshal(plan, &header) != nil {
			t.Fatal("invalid compiled subset plan")
		}
		bound := &diagnosticMPIReviewBinding{Controller: "node-a", ControllerPin: controllerPin, Profile: p, Plan: plan,
			Public: diagnosticMPIReview{ReviewID: strings.Repeat(string(rune('d'+i)), 32), BuildOperationID: record.OperationID, OperationID: header.OperationID, GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, RecipeID: header.RecipeID, ExpiresAt: header.ExpiresAt}}
		d.mu.Lock()
		err = d.publishMPIReviewLocked(bound, mpiReviewStorage())
		d.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		control := diagnosticMPIRecoveryControl{BuildOperationID: record.OperationID, RegisteredMemberCount: 3}
		for _, member := range p.Members {
			control.Members = append(control.Members, diagnosticMPIRecoveryMember{NodeID: member.NodeID, Principal: member.Principal, ClusterPinSHA256: member.ClusterPinSHA256})
		}
		if snapshots {
			marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
			if err != nil {
				t.Fatal(err)
			}
			marker.State = "consumed"
			if err := d.persistMPIReviewMarker(marker, false, mpiReviewStorage()); err != nil {
				t.Fatal(err)
			}
			r := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: header.OperationID, ProfileDigest: header.ProfileDigest, BootstrapPlanDigest: header.PlanDigest, ExpiresAt: header.ExpiresAt, ExecutionDeadlineAt: header.CreatedAt + 90000}
			mpiRecoverySnapshot(t, d, p, plan, r)
			op := diagnosticOperation{OperationID: header.OperationID, GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, Preset: diagnosticPreset, RecipeID: header.RecipeID, State: "failed", CleanupConfirmed: true, MemberNodeIDs: diagnosticMPIProfileMemberIDs(p), profileDigest: header.ProfileDigest, StartedAt: header.CreatedAt, FinishedAt: header.CreatedAt + 1}
			if err := d.saveOperation(op); err != nil {
				t.Fatal(err)
			}
		}
		histories[name], controls[name] = bound, control
	}
	d.mpiReviews = nil // Lookup must recover from durable public ownership.
	return d, histories, controls
}

func TestDiagnosticMPISelectionLookupReturnsOnlyItsOriginalSubsetHistory(t *testing.T) {
	d, histories, controls := mpiSubsetHistoryFixture(t, true)
	for _, name := range []string{"AB", "AC", "AB"} {
		result, err := d.recoverMPIAsCoordinator(context.Background(), "node-a", controls[name])
		want := histories[name]
		if err != nil || result.RecoveryRequired || result.Reference == nil || result.Operation == nil || result.Reference.OperationID != want.Public.OperationID || result.Operation.OperationID != want.Public.OperationID || !reflect.DeepEqual(result.Reference.MemberNodeIDs, diagnosticMPIProfileMemberIDs(want.Profile)) {
			t.Fatalf("lookup mixed %s with another subset: %+v %v", name, result, err)
		}
	}
	if len(d.cancels) != 0 || len(d.mpiReviews) != 0 {
		t.Fatal("subset lookup acquired new execution authority")
	}
}

func TestDiagnosticMPISelectionLegacyUnstartedMarkerCannotInferTripleBuildSubset(t *testing.T) {
	d, histories, controls := mpiSubsetHistoryFixture(t, false)
	marker, err := d.readMPIReviewMarker(histories["AB"].Public.ReviewID, mpiReviewStorage())
	if err != nil {
		t.Fatal(err)
	}
	marker.MemberSetDigest = ""
	if err := d.persistMPIReviewMarker(marker, false, mpiReviewStorage()); err != nil {
		t.Fatal(err)
	}
	if result, err := d.recoverMPIAsCoordinator(context.Background(), "node-a", controls["AB"]); err == nil {
		t.Fatalf("legacy unstarted marker guessed a subset of three: %+v", result)
	}
}

func TestDiagnosticMPISelectionCorruptOtherSubsetSnapshotFailsBeforeFiltering(t *testing.T) {
	d, histories, controls := mpiSubsetHistoryFixture(t, true)
	if err := os.WriteFile(d.coordinatorBootstrapPath(histories["AC"].Public.OperationID), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := d.recoverMPIAsCoordinator(context.Background(), "node-a", controls["AB"]); err == nil {
		t.Fatalf("AB lookup hid corrupt AC ownership: %+v", result)
	}
}

func TestDiagnosticMPISelectionLocalControlRefusesForeignOwnerWithoutForwarding(t *testing.T) {
	d, histories, _ := mpiSubsetHistoryFixture(t, true)
	// A forwarding regression must fail locally, before any client or network
	// can be constructed. Correct local control never consults this directory.
	d.m.peers = nil
	bound := histories["AB"]
	body := diagnosticControlRequest{Method: "engine:diagnostic-status", Request: diagnosticRequest{GroupID: bound.Public.GroupID, OperationID: bound.Public.OperationID}}
	raw, err := d.callOriginalMPIControl(context.Background(), "node-a", body)
	var operation diagnosticOperation
	if err != nil || json.Unmarshal(raw, &operation) != nil || operation.OperationID != bound.Public.OperationID || operation.OwnerNodeID != "node-a" {
		t.Fatalf("normal local status lost its original operation: %s %v", raw, err)
	}
	raw, err = d.callOriginalMPIControl(context.Background(), "node-a", diagnosticControlRequest{Method: "mpi-close-review", MPIReviewID: bound.Public.ReviewID})
	var closed diagnosticMPIReviewStatus
	if err != nil || json.Unmarshal(raw, &closed) != nil || closed.ReviewClosed || closed.Operation == nil || closed.Operation.OperationID != bound.Public.OperationID {
		t.Fatalf("normal local closure lost the consumed original operation: %s %v", raw, err)
	}
	foreign := diagnosticTestProfile()
	foreign.OwnerNodeID = "node-b"
	for i := range foreign.Members {
		foreign.Members[i].Principal = foreign.Members[i].NodeID
	}
	d.profiles = func() ([]diagnosticProfile, error) { return []diagnosticProfile{foreign}, nil }
	for _, method := range []string{"engine:diagnostic-status", "engine:diagnostic-cancel"} {
		body := diagnosticControlRequest{Method: method, Request: diagnosticRequest{GroupID: foreign.GroupID, OperationID: strings.Repeat("f", 32)}}
		if raw, err := d.callOriginalMPIControl(context.Background(), "node-a", body); err == nil || !strings.Contains(err.Error(), "another native owner") || len(raw) != 0 {
			t.Fatalf("wrong local owner was forwarded or admitted: %s %v", raw, err)
		}
	}
	if len(d.cancels) != 0 {
		t.Fatal("wrong-owner control acquired cancellation authority")
	}
}
