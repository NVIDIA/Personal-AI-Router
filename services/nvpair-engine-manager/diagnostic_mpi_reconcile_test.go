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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func diagnosticMPIReconcileControlFixture(t *testing.T, d *diagnosticService, builds ...string) diagnosticMPIReconcileControl {
	t.Helper()
	self := d.m.mesh.NodeUUID()
	pin, ok := d.m.mesh.PinSHA256(self)
	if !ok {
		t.Fatal("fixture node is not pinned")
	}
	return diagnosticMPIReconcileControl{ControllerNodeID: self, Target: diagnosticMPIRecoveryMember{NodeID: self, Principal: self, ClusterPinSHA256: pin}, BuildOperationIDs: builds}
}

func diagnosticMPIReconcileNativeStub(t *testing.T, d *diagnosticService, node string) {
	t.Helper()
	d.bootstrapTestRun = func(_ context.Context, input io.Reader) ([]byte, error) {
		var request struct {
			Action string          `json:"action"`
			Plan   json.RawMessage `json:"plan"`
		}
		if json.NewDecoder(input).Decode(&request) != nil {
			t.Fatal("native request is malformed")
		}
		var plan diagnosticMPIHeader
		if json.Unmarshal(request.Plan, &plan) != nil {
			t.Fatal("native plan is malformed")
		}
		result := diagnosticMPIResult{SchemaVersion: 1, Action: request.Action, NodeID: node, OperationID: plan.OperationID, PlanDigest: plan.PlanDigest, ProfileDigest: plan.ProfileDigest, State: "cancelled", Output: "private diagnostic output", Stderr: "private diagnostic stderr", CleanupConfirmed: true, UnitOwned: true, UnitProcessesGone: true, UnitMetadataRemoved: true, RankCleanupConfirmed: true, AuthorizationRemoved: true, AgentGone: true, PublicArtifactsRemoved: true}
		return json.Marshal(result)
	}
}

func diagnosticMPIReconcileNativeClean(t *testing.T, d *diagnosticService, node string) {
	t.Helper()
	diagnosticMPIReconcileNativeStub(t, d, node)
	d.reconcileCancel = func(context.Context, diagnosticParticipantRequest) (bool, error) { return true, nil }
}

func diagnosticMPIReconcileRun(t *testing.T, d *diagnosticService, p diagnosticProfile, operationID, buildID string, rank *diagnosticRankRecord, nativeRoot string) diagnosticParticipantRequest {
	t.Helper()
	p.GroupID = "pair-smoke-" + operationID
	p.Bootstrap.OperationID = operationID
	originalBuild := strings.Repeat("c", 32)
	for i := range p.Members {
		p.Members[i].Runtime.BuildOperationID = buildID
		p.Members[i].NCCL.Path = strings.Replace(p.Members[i].NCCL.Path, originalBuild, buildID, 1)
		p.Members[i].Runtime.NCCLLibrary.Path = strings.Replace(p.Members[i].Runtime.NCCLLibrary.Path, originalBuild, buildID, 1)
	}
	r := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: operationID, ProfileDigest: profileDigest(p), ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()}
	body := map[string]any{"schemaVersion": 1, "recipeId": "pair-two-spark-nccl-socket-smoke-v1", "operationId": operationID, "groupId": r.GroupID, "ownerNodeId": p.OwnerNodeID, "profileDigest": r.ProfileDigest, "createdAt": r.ExpiresAt - 60000, "expiresAt": r.ExpiresAt}
	canonical, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	r.BootstrapPlanDigest = hex.EncodeToString(sum[:])
	body["planDigest"] = r.BootstrapPlanDigest
	plan, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateDiagnosticMPIBinding(p, plan, r, false); err != nil {
		t.Fatal(err)
	}
	mpiRecoveryLease(t, d, p, plan, r)
	if rank != nil {
		mpiRecoveryWrite(t, filepath.Join(d.runDir(operationID), "rank.json"), *rank)
	}
	if err := os.MkdirAll(filepath.Join(nativeRoot, operationID), 0700); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDiagnosticMPIReconcileClearsOnlyCompleteInventoryAndIsIdempotent(t *testing.T) {
	d, p, _, _ := mpiRecoveryFixture(t)
	nativeRoot := t.TempDir()
	buildA, buildB := strings.Repeat("c", 32), strings.Repeat("d", 32)
	diagnosticMPIReconcileRun(t, d, p, strings.Repeat("a", 32), buildA, nil, nativeRoot)
	other := p
	other.OwnerNodeID = "node-b"
	diagnosticMPIReconcileRun(t, d, other, strings.Repeat("e", 32), buildB, &diagnosticRankRecord{PID: 2147483647, StartTicks: "1"}, nativeRoot)
	diagnosticMPIReconcileNativeClean(t, d, d.m.mesh.NodeUUID())
	d.recoveryFailed = true
	control := diagnosticMPIReconcileControlFixture(t, d, buildA, buildB)
	for attempt := 0; attempt < 2; attempt++ {
		result, err := d.reconcileRetainedMPIAt(context.Background(), d.m.mesh.NodeUUID(), control, nativeRoot)
		if err != nil || result.State != diagnosticMPIReconcileCompleted || result.RecoveryRequired || len(result.Operations) != 2 || d.recoveryRequired() {
			t.Fatalf("attempt %d: result=%+v hold=%t err=%v", attempt, result, d.recoveryRequired(), err)
		}
		for _, operation := range result.Operations {
			if !operation.CleanupConfirmed || operation.Code != "" || operation.Message != "Owned diagnostic resources are cleaned up." {
				t.Fatalf("unconfirmed receipt: %+v", operation)
			}
		}
	}
	release, err := d.m.exec.admitDiagnosticMutation("engine actions")
	if err != nil {
		t.Fatalf("engine action remained fenced: %v", err)
	}
	release()
	raw, err := json.Marshal(finishDiagnosticMPIReconcile([]diagnosticMPIReconcileOperation{diagnosticMPIReconcileSuccess("node-a", strings.Repeat("a", 32), "node-a")}))
	if err != nil || strings.Contains(string(raw), "private diagnostic") {
		t.Fatalf("private native output escaped: %s %v", raw, err)
	}
}

func TestDiagnosticMPIReconcileMalformedOrUnconfirmedInventoryKeepsHold(t *testing.T) {
	for _, scenario := range []string{"malformed-lease", "unconfirmed-rank", "unregistered-build", "foreign-native-entry"} {
		t.Run(scenario, func(t *testing.T) {
			d, p, _, _ := mpiRecoveryFixture(t)
			nativeRoot := t.TempDir()
			build := strings.Repeat("c", 32)
			id := strings.Repeat("a", 32)
			var rank *diagnosticRankRecord
			if scenario == "unconfirmed-rank" {
				rank = &diagnosticRankRecord{PID: 0, StartTicks: ""}
			}
			diagnosticMPIReconcileRun(t, d, p, id, build, rank, nativeRoot)
			if scenario == "malformed-lease" {
				if err := os.WriteFile(filepath.Join(d.runDir(id), "lease.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "foreign-native-entry" {
				if err := os.WriteFile(filepath.Join(nativeRoot, "foreign"), []byte("not owned"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			diagnosticMPIReconcileNativeClean(t, d, d.m.mesh.NodeUUID())
			if scenario == "unconfirmed-rank" {
				d.reconcileCancel = func(context.Context, diagnosticParticipantRequest) (bool, error) {
					return false, context.DeadlineExceeded
				}
			}
			d.recoveryFailed = true
			allowed := build
			if scenario == "unregistered-build" {
				allowed = strings.Repeat("f", 32)
			}
			ctx := context.Background()
			if scenario == "unconfirmed-rank" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			control := diagnosticMPIReconcileControlFixture(t, d, allowed)
			// A pinned remote coordinator must be limited to its registered builds.
			caller := d.m.mesh.NodeUUID()
			if scenario == "unregistered-build" {
				caller = "node-b"
				control.ControllerNodeID = caller
			}
			result, err := d.reconcileRetainedMPIAt(ctx, caller, control, nativeRoot)
			if err != nil && scenario != "unregistered-build" {
				t.Fatal(err)
			}
			if scenario == "unregistered-build" && err != nil {
				// The fixture's controller is not the local self. Exercise the same
				// build filter directly with a valid local envelope.
				control.ControllerNodeID, caller = d.m.mesh.NodeUUID(), d.m.mesh.NodeUUID()
				result, err = d.reconcileRetainedMPIAt(ctx, caller, control, nativeRoot)
			}
			if err != nil || result.State != diagnosticMPIReconcileRequired || !result.RecoveryRequired || !d.recoveryRequired() {
				t.Fatalf("scenario=%s result=%+v hold=%t err=%v", scenario, result, d.recoveryRequired(), err)
			}
			if release, err := d.m.exec.admitDiagnosticMutation("engine actions"); err == nil {
				release()
				t.Fatal("engine action escaped incomplete cleanup")
			}
		})
	}
}

func TestDiagnosticMPIReconcileResultIsStrictAndRedacted(t *testing.T) {
	good := finishDiagnosticMPIReconcile([]diagnosticMPIReconcileOperation{diagnosticMPIReconcileFailure("node-a", strings.Repeat("a", 32), "node-b", "cleanup-unconfirmed")})
	if !validDiagnosticMPIReconcileResult(good, "node-a") {
		t.Fatal("valid fixed receipt rejected")
	}
	raw, _ := json.Marshal(good)
	for _, forbidden := range []string{"password", "stderr", "private", "profile", "planDigest", "pid", "path"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(forbidden)) {
			t.Fatalf("receipt exposed %q: %s", forbidden, raw)
		}
	}
	good.Operations[0].Code = "raw-native-error"
	if validDiagnosticMPIReconcileResult(good, "node-a") {
		t.Fatal("unbounded failure code accepted")
	}
	good = finishDiagnosticMPIReconcile([]diagnosticMPIReconcileOperation{diagnosticMPIReconcileFailure("node-a", strings.Repeat("a", 32), "node-b", "cleanup-unconfirmed")})
	good.Operations[0].Message = "arbitrary printable failure"
	if validDiagnosticMPIReconcileResult(good, "node-a") {
		t.Fatal("unbound public failure message accepted")
	}
}

func TestDiagnosticMPIReconcileCoordinatorKeepsTimeoutIdentity(t *testing.T) {
	nodes := []string{"node-a", "node-b", "node-c"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := coordinateDiagnosticMPIReconcile(ctx, nodes, func(ctx context.Context, node string) diagnosticMPIReconcileResult {
		switch node {
		case "node-a":
			return finishDiagnosticMPIReconcile([]diagnosticMPIReconcileOperation{diagnosticMPIReconcileSuccess(node, strings.Repeat("a", 32), node)})
		case "node-c":
			return finishDiagnosticMPIReconcile([]diagnosticMPIReconcileOperation{diagnosticMPIReconcileFailure(node, strings.Repeat("c", 32), node, "cleanup-unconfirmed")})
		default:
			<-ctx.Done()
			return diagnosticMPIReconcileResult{}
		}
	})
	if !result.RecoveryRequired || result.State != diagnosticMPIReconcileRequired || len(result.Operations) != 3 {
		t.Fatalf("aggregate=%+v", result)
	}
	seen := map[string]diagnosticMPIReconcileOperation{}
	for _, operation := range result.Operations {
		seen[operation.NodeID] = operation
	}
	if !seen["node-a"].CleanupConfirmed || seen["node-b"].Code != "participant-unavailable" || seen["node-c"].OperationID != strings.Repeat("c", 32) || seen["node-c"].Code != "cleanup-unconfirmed" {
		t.Fatalf("node attribution changed: %+v", seen)
	}
}

func diagnosticMPISecondManagedRun(t *testing.T, d *diagnosticService, source *diagnosticRuntimeRun, count int) (*diagnosticRuntimeRun, diagnosticManagedAction) {
	t.Helper()
	raw, err := json.Marshal(source)
	var run diagnosticRuntimeRun
	if err != nil || json.Unmarshal(raw, &run) != nil {
		t.Fatal("fixture runtime clone failed")
	}
	run.Public.OperationID = strings.Repeat("9", 32)
	run.Public.ReviewID = strings.Repeat("8", 32)
	run.Public.Adopted = false
	run.Public.Stage = "artifacts-built"
	run.Public.Revision = 7
	run.Public.Targets = run.Public.Targets[:count]
	run.Binding.Targets = run.Binding.Targets[:count]
	run.Binding.Review.Targets = run.Binding.Review.Targets[:count]
	run.Binding.Pair.Members = run.Binding.Pair.Members[:count]
	run.Binding.Pair.GroupID = closedRosterGroup(run.Binding.Pair.Members)
	run.Public.GroupID = run.Binding.Pair.GroupID
	run.Binding.Review.OperationID = run.Public.OperationID
	run.Binding.Review.ReviewID = run.Public.ReviewID
	run.Binding.Review.GroupID = run.Public.GroupID
	run.Binding.Pins = map[string]string{}
	for i, target := range run.Binding.Targets {
		run.Binding.Pins[target.Principal] = source.Binding.Pins[target.Principal]
		reviewInput, _ := json.Marshal(map[string]string{"action": "review", "operationId": run.Public.OperationID})
		statusInput, _ := json.Marshal(map[string]string{"action": "status", "operationId": run.Public.OperationID})
		run.Binding.Review.Targets[i].Review = managedFixtureReply(reviewInput, target)
		run.Public.Targets[i].Receipt = managedFixtureReply(statusInput, target)
		run.Public.Targets[i].State = "built"
		run.Public.Targets[i].Attempt = 1
		run.Public.Targets[i].CleanupConfirmed = true
	}
	d.runtimeRuns[run.Public.OperationID] = &run
	if err := d.saveRuntime(&run); err != nil {
		t.Fatal(err)
	}
	return &run, diagnosticManagedAction{OperationID: run.Public.OperationID, ExpectedRevision: run.Public.Revision}
}

func TestDiagnosticMPIReconcileRegistryKeepsExactAdoptedRosterAcrossWorkerUpdate(t *testing.T) {
	const retainedWorker = "04905d1b06199b6274040349fae36f2db65e7a4ee3adcb0d7d39b6a03aeef288"
	d, threeNode, firstRequest := closedRuntimeFixture(t, 3)
	if _, err := d.adoptRuntime(context.Background(), firstRequest); err != nil {
		t.Fatal(err)
	}
	twoNode, secondRequest := diagnosticMPISecondManagedRun(t, d, threeNode, 2)
	if _, err := d.adoptRuntime(context.Background(), secondRequest); err != nil {
		t.Fatal(err)
	}
	expectedRegistry := map[string][]byte{}
	for _, run := range []*diagnosticRuntimeRun{threeNode, twoNode} {
		raw, err := os.ReadFile(d.managedPath(run.Public.OperationID))
		var disk diagnosticManagedDisk
		if err != nil || json.Unmarshal(raw, &disk) != nil {
			t.Fatal("fixture managed registry is unavailable")
		}
		for i := range run.Binding.Review.Targets {
			setRuntimeReviewWorker(t, &run.Binding.Review.Targets[i], retainedWorker)
		}
		disk.Binding = run.Binding
		if err := writeJSONAtomic(d.runtimePath(run.Public.OperationID), run); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONAtomic(d.managedPath(run.Public.OperationID), disk); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.managedPath(run.Public.OperationID), 0600); err != nil {
			t.Fatal(err)
		}
		expectedRegistry[run.Public.OperationID], err = os.ReadFile(d.managedPath(run.Public.OperationID))
		if err != nil {
			t.Fatal(err)
		}
	}
	restarted := newDiagnosticService(d.m)
	t.Cleanup(restarted.shutdown)
	inventory := restarted.managedRuntimeInventory()
	counts := map[int]bool{}
	for _, record := range inventory.Records {
		restored := restarted.runtimeRuns[record.OperationID]
		if restored == nil || !restored.Public.Adopted || runtimeRetrySourceStatus(restored) != "changed" {
			t.Fatalf("historical adopted operation did not survive restart: %+v", restored)
		}
		counts[len(record.Targets)] = true
	}
	if restarted.runtimeRecoveryFailed || inventory.RecoveryRequired || len(inventory.Records) != 2 || !counts[2] || !counts[3] {
		t.Fatalf("historical two/three-node rosters did not survive restart: %+v", inventory)
	}
	if err := restarted.diagnosticMPIReconcileRegistry(inventory.Records); err != nil {
		t.Fatalf("cleanup registry rejected the exact historical roster: %v", err)
	}
	for id, expected := range expectedRegistry {
		actual, err := os.ReadFile(d.managedPath(id))
		if err != nil || !bytes.Equal(expected, actual) {
			t.Fatal("historical registry validation rewrote retained authority")
		}
	}
}

func TestDiagnosticManagedFreshAdoptionRejectsHistoricalWorkerBeforeEffects(t *testing.T) {
	const retainedWorker = "04905d1b06199b6274040349fae36f2db65e7a4ee3adcb0d7d39b6a03aeef288"
	d, run, request := managedFixture(t)
	for i := range run.Binding.Review.Targets {
		setRuntimeReviewWorker(t, &run.Binding.Review.Targets[i], retainedWorker)
	}
	calls := 0
	d.runtimeTestRun = func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
		calls++
		return nil, errors.New("stale adoption reached a participant")
	}
	if result, err := d.adoptRuntime(context.Background(), request); err == nil || result.Record != nil || calls != 0 {
		t.Fatalf("historical worker reached fresh adoption: result=%+v calls=%d err=%v", result, calls, err)
	}
	if _, err := os.Lstat(d.managedPath(request.OperationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("historical worker published fresh managed authority")
	}
}
