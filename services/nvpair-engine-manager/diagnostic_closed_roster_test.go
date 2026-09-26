// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func closedRosterGroup(members []diagnosticSetupMember) string {
	identities := []string{}
	for _, member := range members {
		identities = append(identities, member.NodeID, member.Principal)
	}
	raw, _ := json.Marshal(identities)
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("pair-recipe/nccl-%x", digest[:16])
}

func closedRosterFixture(t *testing.T, count int, self string) (*diagnosticService, diagnosticInspectionRequest, string) {
	t.Helper()
	d := diagnosticTestService(t)
	dir := t.TempDir()
	ids := []string{}
	nodes := []noderec.DirectoryNode{}
	request := diagnosticInspectionRequest{}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("node-%c", 'a'+i)
		ids = append(ids, id)
		request.Members = append(request.Members, diagnosticSetupMember{NodeID: id, Principal: id, GB10Observed: true, ControlAdvertised: true})
		nodes = append(nodes, noderec.DirectoryNode{HostUUID: id, ClusterUUID: id, IP: fmt.Sprintf("192.0.2.%d", i+1), Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: 14323}}})
	}
	clustertrusttest.Join(t, dir, "cluster", self, ids...)
	d.m.mesh = clustertrust.Open(dir)
	d.m.peers.set(nodes)
	request.GroupID = closedRosterGroup(request.Members)
	inventory, err := d.inspectionTargets(request.diagnosticSetupRequest)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range inventory.Targets {
		if row.Local {
			continue
		}
		target := d.m.onboarding.targets[row.Candidate.CandidateID]
		target.candidate.AccessAvailable = true
		target.candidate.AccessID = newOpID()
		target.candidate.HostKeySHA256 = "SHA256:fixture-public-key"
		target.candidate.HostKeyTrusted = true
		target.accessGeneration = newOpID()
		target.expiresAt = time.Now().Add(time.Minute)
		target.access = onboardingAccess{user: "fixture", password: "volatile-roster-fixture"}
	}
	return d, request, dir
}

func closedRuntimeFixture(t *testing.T, count int) (*diagnosticService, *diagnosticRuntimeRun, diagnosticManagedAction) {
	t.Helper()
	d, selection, _ := closedRosterFixture(t, count, "node-a")
	d.m.onboarding.dial = func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
		t.Error("mocked roster transaction attempted SSH")
		return nil, errors.New("unexpected SSH")
	}
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, access onboardingPrivateTarget, input []byte) ([]byte, error) {
		if target.Local && (target.Candidate != nil || access.access != (onboardingAccess{})) {
			t.Error("local participant acquired SSH account state")
		}
		return managedFixtureReply(input, target), nil
	}
	review, err := d.reviewRuntime(context.Background(), selection)
	if err != nil || !review.CanBuild || len(review.Targets) != count {
		t.Fatalf("roster runtime review: %+v %v", review, err)
	}
	op, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: review.ReviewID})
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntime(t, d, op.OperationID)
	if op.State != "completed" || !op.CleanupConfirmed || len(op.Targets) != count {
		t.Fatalf("roster build did not settle: %+v", op)
	}
	return d, d.runtimeRuns[op.OperationID], diagnosticManagedAction{OperationID: op.OperationID, ExpectedRevision: op.Revision}
}

func TestDiagnosticClosedRosterSetupPinsEveryMemberAndKeepsLegacyIdentity(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d, request, dir := closedRosterFixture(t, count, "node-a")
			review, err := d.setupReview(request.diagnosticSetupRequest)
			if err != nil || len(review.Members) != count || review.Executable || review.EffectsApplied {
				t.Fatalf("roster review: %+v %v", review, err)
			}
			if count == 3 && !strings.HasPrefix(review.Prerequisites[0].Detail, "All three participants") {
				t.Fatal("three-member review described only a pair")
			}
			if count == 2 {
				body, _ := json.Marshal([]string{"node-a", "node-a", "node-b", "node-b"})
				digest := sha256.Sum256(body)
				if request.GroupID != fmt.Sprintf("pair-recipe/nccl-%x", digest[:16]) {
					t.Fatal("legacy group identity changed")
				}
			}
			clustertrusttest.RemovePeerPin(t, dir, request.Members[count-1].Principal)
			if _, err := d.setupReview(request.diagnosticSetupRequest); err == nil {
				t.Fatal("last participant lost its pin without closing admission")
			}
		})
	}
	for _, count := range []int{0, 1, 4} {
		request := diagnosticSetupRequest{}
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("node-%c", 'a'+i)
			request.Members = append(request.Members, diagnosticSetupMember{NodeID: id, Principal: id})
		}
		request.GroupID = closedRosterGroup(request.Members)
		if validateDiagnosticSetupRoster(request) == nil {
			t.Fatalf("unsupported count %d admitted", count)
		}
	}
}

func TestDiagnosticClosedRosterInspectionVisitsThreeDistinctPeers(t *testing.T) {
	d, request, _ := closedRosterFixture(t, 3, "controller")
	seen := map[string]string{}
	d.m.onboarding.dial = func(_ context.Context, _ onboardingCandidate, _ onboardingAccess) (*onboardingSSH, error) {
		return &onboardingSSH{testRun: func(_ context.Context, _ string, input io.Reader) ([]byte, error) {
			var body map[string]string
			if json.NewDecoder(input).Decode(&body) != nil {
				t.Fatal("invalid inspection input")
			}
			seen[body["nodeId"]] = body["peerAddress"]
			return inspectionReply(body["nodeId"]), nil
		}}, nil
	}
	result, err := d.inspectParticipants(context.Background(), request)
	if err != nil || len(result.Targets) != 3 || len(seen) != 3 || seen["node-a"] != "192.0.2.2" || seen["node-b"] != "192.0.2.3" || seen["node-c"] != "192.0.2.1" {
		t.Fatalf("inspection lost a participant or indexed the pair: %+v %+v %v", result, seen, err)
	}
}

func TestDiagnosticClosedRosterPackageTransactionIncludesLocalAndAllRemoteMembers(t *testing.T) {
	d, request, _ := closedRosterFixture(t, 3, "node-a")
	var mu sync.Mutex
	provisioned := map[string]int{}
	d.m.onboarding.dial = func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
		t.Error("mocked package transaction attempted SSH")
		return nil, errors.New("unexpected SSH")
	}
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, access onboardingPrivateTarget, input []byte) ([]byte, error) {
		if target.Local && (target.NodeID != "node-a" || target.Candidate != nil || access.access != (onboardingAccess{})) {
			t.Error("local package target changed its owner or acquired credentials")
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(input, &body)
		if string(body["action"]) == `"review"` {
			return packageFixtureReview(target, false), nil
		}
		mu.Lock()
		provisioned[target.NodeID]++
		mu.Unlock()
		return packageFixtureResult(body, true), nil
	}
	review, err := d.reviewPackages(context.Background(), request)
	if err != nil || !review.CanApprove || len(review.Targets) != 3 {
		t.Fatalf("package review: %+v %v", review, err)
	}
	approval := diagnosticPackageApprove{ReviewID: review.ReviewID}
	for _, target := range review.Targets {
		approval.Elevation = append(approval.Elevation, diagnosticPackageElevation{NodeID: target.NodeID, NonInteractive: true})
	}
	op, err := d.approvePackages(context.Background(), approval)
	if err != nil {
		t.Fatal(err)
	}
	op = waitPackages(t, d, op.OperationID)
	mu.Lock()
	defer mu.Unlock()
	if op.State != "completed" || !op.CleanupConfirmed || len(op.Targets) != 3 || len(provisioned) != 3 {
		t.Fatalf("package roster did not complete: %+v %+v", op, provisioned)
	}
	for _, count := range provisioned {
		if count != 1 {
			t.Fatal("package approval repeated a participant")
		}
	}
	raw, err := os.ReadFile(d.packageOperationPath(op.OperationID))
	var retained diagnosticPackageRun
	if err != nil || json.Unmarshal(raw, &retained) != nil || !validPackageRun(retained, op.OperationID) || bytes.Contains(raw, []byte("volatile-roster-fixture")) {
		t.Fatal("three-member package record lost its full original binding")
	}
	retained.Public.Targets = retained.Public.Targets[:2]
	if validPackageRun(retained, op.OperationID) {
		t.Fatal("truncated public package roster admitted")
	}
}

func TestDiagnosticClosedRosterBuildAdoptionAndRestartKeepOneCompleteRecord(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d, run, request := closedRuntimeFixture(t, count)
			if runtimeRetrySourceStatus(run) != "current" {
				t.Fatal("complete original source roster was not current")
			}
			result, err := d.adoptRuntime(context.Background(), request)
			if err != nil || result.Record == nil || len(result.Record.Targets) != count || !result.Operation.Adopted || result.Record.RunAvailable || result.Record.RuntimeValidated {
				t.Fatalf("adoption=%+v %v", result, err)
			}
			before, err := os.ReadFile(d.managedPath(request.OperationID))
			if err != nil {
				t.Fatal(err)
			}
			d.m.onboarding = newOnboardingService(d.m)
			restarted := newDiagnosticService(d.m)
			t.Cleanup(restarted.shutdown)
			inventory := restarted.managedRuntimeInventory()
			if inventory.RecoveryRequired || len(inventory.Records) != 1 || len(inventory.Records[0].Targets) != count || inventory.Records[0].OperationID != request.OperationID {
				t.Fatalf("restarted inventory=%+v", inventory)
			}
			restored := restarted.runtimeRuns[request.OperationID]
			if restored == nil || len(restored.Binding.Targets) != count || len(restored.Binding.Generations) != 0 {
				t.Fatal("restart lost roster or restored volatile account generations")
			}
			for _, target := range d.m.onboarding.targets {
				if target.candidate.AccessAvailable || target.access != (onboardingAccess{}) {
					t.Fatal("restart restored account access")
				}
			}
			after, _ := os.ReadFile(d.managedPath(request.OperationID))
			if !bytes.Equal(before, after) {
				t.Fatal("record lookup rewrote original registered bytes")
			}
		})
	}
}

func TestDiagnosticClosedRosterThirdMemberDriftPreventsPublication(t *testing.T) {
	for _, kind := range []string{"generation", "account", "receipt", "pin"} {
		t.Run(kind, func(t *testing.T) {
			d, run, request := closedRuntimeFixture(t, 3)
			third := run.Binding.Targets[2]
			d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				result := managedFixtureReply(input, target)
				if target.NodeID != third.NodeID {
					return result, nil
				}
				access := d.m.onboarding.targets[third.Candidate.CandidateID]
				switch kind {
				case "generation":
					access.accessGeneration = newOpID()
				case "account":
					access.access.user = "different-user"
				case "receipt":
					result = []byte(strings.Replace(string(result), `"nodeId":"node-c"`, `"nodeId":"other-node"`, 1))
				case "pin":
					run.Binding.Pins[third.Principal] = strings.Repeat("f", 64)
				}
				return result, nil
			}
			if result, err := d.adoptRuntime(context.Background(), request); err == nil || result.Record != nil || result.Operation.Adopted {
				t.Fatalf("third-member %s drift published adoption: %+v %v", kind, result, err)
			}
			if _, err := os.Stat(d.managedPath(request.OperationID)); !os.IsNotExist(err) {
				t.Fatal("failed all-member verification wrote a registry record")
			}
		})
	}
}

func TestDiagnosticClosedRosterRetryKeepsBuiltMembersAndCancelCoversAllOriginalRanks(t *testing.T) {
	d, run, _ := closedRuntimeFixture(t, 3)
	run.Public.State, run.Public.Stage = "failed", "build-failed"
	run.Public.Targets[2].State = "failed"
	if err := d.saveRuntime(run); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	retries, cancellations := map[string]int{}, map[string]int{}
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		var action string
		_ = json.Unmarshal(request["action"], &action)
		mu.Lock()
		defer mu.Unlock()
		if action == "retry" {
			retries[target.NodeID]++
			return runtimeFixtureReply(input, target, "built", 2), nil
		}
		if action == "cancel" {
			cancellations[target.NodeID]++
			return runtimeFixtureReply(input, target, "cancelled", 1), nil
		}
		if target.NodeID == "node-c" && retries[target.NodeID] == 0 {
			return runtimeFixtureReply(input, target, "failed", 1), nil
		}
		return managedFixtureReply(input, target), nil
	}
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", diagnosticRuntimeAction{OperationID: run.Public.OperationID, ExpectedRevision: run.Public.Revision}); err != nil {
		t.Fatal(err)
	}
	op := waitRuntime(t, d, run.Public.OperationID)
	mu.Lock()
	if op.State != "completed" || len(retries) != 1 || retries["node-c"] != 1 || op.Targets[0].Attempt != 1 || op.Targets[1].Attempt != 1 || op.Targets[2].Attempt != 2 {
		t.Fatalf("retry changed completed participants: %+v %+v", op, retries)
	}
	mu.Unlock()
	run.Public.State, run.Public.CleanupConfirmed = "failed", false
	for i := range run.Public.Targets {
		run.Public.Targets[i].State = "unknown"
		run.Public.Targets[i].CleanupConfirmed = false
	}
	if err := d.saveRuntime(run); err != nil {
		t.Fatal(err)
	}
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID}); err != nil {
		t.Fatal(err)
	}
	op = waitRuntime(t, d, run.Public.OperationID)
	mu.Lock()
	defer mu.Unlock()
	if !op.CleanupConfirmed || len(cancellations) != 3 || op.OperationID != run.Public.OperationID {
		t.Fatalf("cancellation lost original roster: %+v %+v", op, cancellations)
	}
}

func TestDiagnosticClosedRosterMalformedShapesCannotIndexOrRebindTargets(t *testing.T) {
	d, run, _ := closedRuntimeFixture(t, 3)
	original, _ := json.Marshal(run)
	for _, kind := range []string{"public-count", "review-count", "member-count", "pins-count", "duplicate-principal", "reordered", "public-identity", "review-identity"} {
		t.Run(kind, func(t *testing.T) {
			var bad diagnosticRuntimeRun
			_ = json.Unmarshal(original, &bad)
			switch kind {
			case "public-count":
				bad.Public.Targets = bad.Public.Targets[:2]
			case "review-count":
				bad.Binding.Review.Targets = bad.Binding.Review.Targets[:2]
			case "member-count":
				bad.Binding.Pair.Members = bad.Binding.Pair.Members[:2]
			case "pins-count":
				delete(bad.Binding.Pins, "node-c")
			case "duplicate-principal":
				bad.Binding.Pair.Members[2].Principal = "node-b"
			case "reordered":
				bad.Binding.Pair.Members[1], bad.Binding.Pair.Members[2] = bad.Binding.Pair.Members[2], bad.Binding.Pair.Members[1]
			case "public-identity":
				bad.Public.Targets[2].Principal = "foreign"
			case "review-identity":
				bad.Binding.Review.Targets[2].Address = "192.0.2.99"
			}
			if managedRunShape(&bad) == nil {
				t.Fatal("malformed full roster reached managed adoption")
			}
			if err := d.saveRuntime(&bad); err != nil {
				t.Fatal(err)
			}
			restarted := newDiagnosticService(d.m)
			t.Cleanup(restarted.shutdown)
			if !restarted.runtimeRecoveryFailed || restarted.runtimeRuns[run.Public.OperationID] != nil {
				t.Fatal("malformed durable roster became a recovered build")
			}
		})
	}
	for _, index := range []int{-1, 3} {
		if _, _, err := runtimeTargetPlan(run, index); err == nil {
			t.Fatal("out-of-roster plan index admitted")
		}
	}
}
