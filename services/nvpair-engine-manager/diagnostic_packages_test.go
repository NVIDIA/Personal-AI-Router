// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const packageFixtureVersion = "4.1.6-7ubuntu2"
const packageFixtureRecipe = "dgx-spark-ubuntu24.04-arm64-openmpi4-packages-v3"

func TestDiagnosticPackageEmbeddedProgramFitsNativeArgumentLimit(t *testing.T) {
	command := "/usr/bin/python3 -I -c " + onboardingQuote(diagnosticPackageProgram())
	if len(command) >= 128<<10 {
		t.Fatalf("native shell argument exceeds Linux limit: %d bytes", len(command))
	}
	t.Logf("Fixed program shell argument: %d bytes", len(command))
}

func packageFixtureEntries() []map[string]any {
	return []map[string]any{{"name": "libopenmpi-dev", "version": packageFixtureVersion, "architecture": "arm64", "sha256": strings.Repeat("a", 64), "size": float64(10), "installedSize": float64(20), "origin": map[string]any{"site": "ports.ubuntu.com", "archive": "noble", "component": "universe", "label": "Ubuntu", "origin": "Ubuntu", "trusted": true}}}
}

func packageFixtureFamily() []map[string]any {
	family := []map[string]any{}
	for _, name := range []string{"libopenmpi-dev", "openmpi-bin", "libopenmpi3t64", "openmpi-common"} {
		arch := "arm64"
		if name == "openmpi-common" {
			arch = "all"
		}
		family = append(family, map[string]any{"name": name, "status": "install ok installed", "version": packageFixtureVersion, "architecture": arch})
	}
	return family
}

func packageFixtureReview(target diagnosticInspectionTarget, already bool) []byte {
	identity := map[string]any{"nodeId": target.NodeID, "principal": target.Principal, "uid": 1000, "home": "/home/fixture"}
	plan := map[string]any{"recipeId": packageFixtureRecipe, "planDigest": strings.Repeat("b", 64), "rootPackage": map[string]string{"name": "libopenmpi-dev", "version": packageFixtureVersion}, "identity": identity, "packages": packageFixtureEntries(), "archivePolicy": map[string]string{"workerSha256": strings.Repeat("c", 64)}}
	state := "reviewed"
	if already {
		state = "already_installed"
	}
	value := map[string]any{"schemaVersion": 1, "action": "review", "state": state, "effectsApplied": false, "runtimeValidated": false, "identity": identity, "plan": plan, "installedFamily": packageFixtureFamily(), "errors": []any{}}
	if already {
		value["plan"] = nil
	}
	raw, _ := json.Marshal(value)
	return raw
}

func packageFixtureResult(request map[string]json.RawMessage, success bool) []byte {
	var action, id, expected string
	_ = json.Unmarshal(request["action"], &action)
	_ = json.Unmarshal(request["operationId"], &id)
	_ = json.Unmarshal(request["expectedPlanDigest"], &expected)
	if action == "provision" {
		expected = strings.Repeat("b", 64)
	}
	state := "cancelled"
	var receipt any
	if success {
		state = "succeeded"
		receipt = map[string]any{"recipeId": packageFixtureRecipe, "planDigest": expected, "runtimeValidated": false, "packages": packageFixtureEntries(), "installedFamily": packageFixtureFamily(),
			"archiveReceipt": map[string]any{"schemaVersion": 1, "operationId": id, "planDigest": expected, "workerSha256": strings.Repeat("c", 64), "state": "succeeded", "phase": "complete", "verified": true, "downloadExitCode": 0, "installExitCode": 0,
				"archives": []map[string]any{{"name": "libopenmpi-dev", "version": packageFixtureVersion, "architecture": "arm64", "sha256": strings.Repeat("a", 64), "size": 10}}}}
	}
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "action": action, "operationId": id, "planDigest": expected, "state": state, "effectsApplied": true, "cleanupConfirmed": true, "runtimeValidated": false, "receipt": receipt})
	return raw
}

func packageApproval(id string) diagnosticPackageApprove {
	var request diagnosticPackageApprove
	_ = json.Unmarshal([]byte(`{"reviewId":"`+id+`","elevation":[{"nodeId":"node-a","elevationPassword":"separate-admin-fixture","nonInteractive":false},{"nodeId":"node-b","elevationPassword":"separate-admin-fixture","nonInteractive":false}]}`), &request)
	return request
}

func waitPackages(t *testing.T, d *diagnosticService, id string) diagnosticPackageOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		run := d.packageRuns[id]
		done := run != nil && run.cancel == nil
		var op diagnosticPackageOperation
		if run != nil {
			op = clonePackageOperation(run.Public)
		}
		d.mu.Unlock()
		if done {
			return op
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("package fixture did not settle")
	return diagnosticPackageOperation{}
}

func TestDiagnosticPackagesBindApprovalReceiptsAndKeepSecretsOutOfJournal(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	var installs atomic.Int32
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		var action string
		_ = json.Unmarshal(request["action"], &action)
		if action == "review" {
			return packageFixtureReview(target, false), nil
		}
		installs.Add(1)
		if !bytes.Contains(input, []byte("separate-admin-fixture")) {
			t.Error("separate volatile administrator input was lost")
		}
		return packageFixtureResult(request, true), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil || !review.CanApprove {
		t.Fatalf("review=%+v %v", review, err)
	}
	op, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID))
	if err != nil || duplicate.OperationID != op.OperationID {
		t.Fatal("approval duplicated the operation")
	}
	op = waitPackages(t, d, op.OperationID)
	if op.State != "completed" || !op.CleanupConfirmed || op.RuntimeValidated || installs.Load() != 2 {
		t.Fatalf("terminal=%+v installs=%d", op, installs.Load())
	}
	journal, err := os.ReadFile(d.packageOperationPath(op.OperationID))
	if err != nil || bytes.Contains(journal, []byte("separate-admin-fixture")) || bytes.Contains(journal, []byte("volatile-ssh-fixture")) {
		t.Fatal("journal unavailable or contains access material")
	}
	status, err := d.packageStatus(diagnosticPackageAction{ReviewID: review.ReviewID})
	if err != nil || status.Operation == nil || status.Operation.OperationID != op.OperationID {
		t.Fatal("lost approval reply cannot be reconciled")
	}
}

func TestDiagnosticPackagesAlreadyInstalledIsObservedNoOpWithoutAdminOrGPUChecks(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	var reads atomic.Int32
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if !bytes.Contains(input, []byte(`"action":"review"`)) {
			t.Error("no-op attempted an effect")
		}
		reads.Add(1)
		return packageFixtureReview(target, true), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.approvePackages(context.Background(), diagnosticPackageApprove{ReviewID: review.ReviewID})
	if err != nil {
		t.Fatal(err)
	}
	op = waitPackages(t, d, op.OperationID)
	if op.State != "completed" || !op.CleanupConfirmed || reads.Load() != 4 {
		t.Fatalf("no-op=%+v reads=%d", op, reads.Load())
	}
}

func TestDiagnosticPackagesRejectReplacedTrustedSSHKeyBeforeApprovalAndRecovery(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, _ []byte) ([]byte, error) {
		return packageFixtureReview(target, false), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	binding := d.packageReviews[review.ReviewID]
	target := binding.Targets[0]
	current := d.m.onboarding.targets[target.Candidate.CandidateID]
	current.candidate.HostKeySHA256 = "SHA256:new-OS-trusted-key"
	current.candidate.HostKeyTrusted = true
	if _, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID)); err == nil {
		t.Fatal("changed trusted SSH identity was approved")
	}
	if _, err := d.packageAccess(target, map[string]string{target.Candidate.CandidateID: target.Candidate.HostKeySHA256}); err == nil {
		t.Fatal("recovery admitted replacement trusted host key")
	}
}

func TestDiagnosticPackagesRejectUnboundSuccessAndMeasuredArchiveMismatch(t *testing.T) {
	request := map[string]json.RawMessage{"action": json.RawMessage(`"provision"`), "operationId": json.RawMessage(`"` + strings.Repeat("d", 32) + `"`)}
	raw := packageFixtureResult(request, true)
	result, err := packageActionResult(raw, "provision", strings.Repeat("d", 32), strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	var review diagnosticPackageResult
	_ = json.Unmarshal(packageFixtureReview(diagnosticInspectionTarget{NodeID: "node-a", Principal: "node-a"}, false), &review)
	plan, err := approvedPackagePlan(review.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if !validPackageReceipt(result, plan, strings.Repeat("d", 32)) {
		t.Fatal("valid measured receipt refused")
	}
	for _, change := range []string{"operation", "plan", "receipt", "archive"} {
		bad := result
		switch change {
		case "operation":
			if _, err := packageActionResult(raw, "provision", strings.Repeat("e", 32), plan.PlanDigest); err == nil {
				t.Fatal("wrong operation accepted")
			}
			continue
		case "plan":
			bad.Receipt = bytes.ReplaceAll(result.Receipt, []byte(plan.PlanDigest), []byte(strings.Repeat("e", 64)))
		case "receipt":
			bad.Receipt = nil
		case "archive":
			bad.Receipt = bytes.ReplaceAll(result.Receipt, []byte(`"verified":true`), []byte(`"verified":false`))
		}
		if validPackageReceipt(bad, plan, strings.Repeat("d", 32)) {
			t.Fatalf("bad %s accepted", change)
		}
	}
}

func TestDiagnosticPackageCancellationUsesOwnedPlanAndRetainsOperation(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	started := make(chan struct{}, 1)
	d.packageTestRun = func(ctx context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		var action string
		_ = json.Unmarshal(request["action"], &action)
		if action == "review" {
			return packageFixtureReview(target, false), nil
		}
		if action == "provision" {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if !bytes.Contains(input, []byte(`"expectedPlanDigest":"`+strings.Repeat("b", 64)+`"`)) {
			t.Error("cleanup lost the approved plan binding")
		}
		return packageFixtureResult(request, false), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fixture did not start")
	}
	if _, err := d.packageOperation(context.Background(), "engine:diagnostic-package-cancel", diagnosticPackageAction{OperationID: op.OperationID}); err != nil {
		t.Fatal(err)
	}
	op = waitPackages(t, d, op.OperationID)
	if op.State != "cancelled" || !op.CleanupConfirmed || len(d.operations) != 0 {
		t.Fatalf("cancel=%+v", op)
	}
}

func TestDiagnosticPackagePartialFailureResumesSameApprovedOperation(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	var failedOnce atomic.Bool
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		var action string
		_ = json.Unmarshal(request["action"], &action)
		if action == "review" {
			return packageFixtureReview(target, false), nil
		}
		if target.NodeID == "node-b" && !failedOnce.Swap(true) {
			raw := packageFixtureResult(request, false)
			return bytes.Replace(raw, []byte(`"state":"cancelled"`), []byte(`"state":"failed"`), 1), nil
		}
		return packageFixtureResult(request, true), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID))
	if err != nil {
		t.Fatal(err)
	}
	op = waitPackages(t, d, op.OperationID)
	if op.State != "failed" || !op.CleanupConfirmed || op.Targets[0].State != "succeeded" {
		t.Fatalf("partial=%+v", op)
	}
	retry, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: op.OperationID, Elevation: packageApproval(review.ReviewID).Elevation})
	if err != nil {
		t.Fatal(err)
	}
	if retry.OperationID != op.OperationID || retry.ReviewID != review.ReviewID {
		t.Fatal("retry replaced approved ownership")
	}
	retry = waitPackages(t, d, retry.OperationID)
	if retry.State != "completed" || !retry.CleanupConfirmed {
		t.Fatalf("resumed=%+v", retry)
	}
}

func TestDiagnosticPackageRestartRetainsOwnershipWithoutAccessAndRejectsMalformedJournal(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, _ []byte) ([]byte, error) {
		return packageFixtureReview(target, false), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	id := newOpID()
	run := &diagnosticPackageRun{SchemaVersion: 1, Owner: "pair-mpi-package-controller-v1", Binding: d.packageReviews[review.ReviewID], Public: diagnosticPackageOperation{OperationID: id, ReviewID: review.ReviewID, GroupID: review.GroupID, State: "running", Stage: "installing", Revision: 1, StartedAt: time.Now().UnixMilli(), Targets: append([]diagnosticPackageTarget(nil), review.Targets...)}}
	run.Public.Targets[0].State = "installing"
	run.Public.Targets[0].CleanupConfirmed = false
	if err := d.savePackageRun(run); err != nil {
		t.Fatal(err)
	}
	d.m.onboarding = newOnboardingService(d.m)
	restarted := newDiagnosticService(d.m)
	status, err := restarted.packageStatus(diagnosticPackageAction{GroupID: review.GroupID})
	if err != nil || status.Operation == nil || status.Operation.OperationID != id || status.Operation.State != "interrupted" || status.Operation.CleanupConfirmed {
		t.Fatalf("restart=%+v %v", status, err)
	}
	for _, target := range d.m.onboarding.targets {
		if target.candidate.AccessAvailable || target.access.password != "" || target.access.elevationPassword != "" {
			t.Fatal("restart restored credential material")
		}
	}
	bad := filepath.Join(d.m.exec.baseDir, "diagnostic-package-operations", newOpID()+".json")
	if err := os.WriteFile(bad, []byte(`{"operation":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := newDiagnosticService(d.m)
	status, _ = corrupt.packageStatus(diagnosticPackageAction{})
	if !status.RecoveryRequired {
		t.Fatal("malformed ownership was treated as no prior effects")
	}
}

func retainedPackageFixture(t *testing.T) (*diagnosticService, *diagnosticPackageRun) {
	t.Helper()
	d, selection := inspectionFixture(t, "controller")
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, _ []byte) ([]byte, error) {
		return packageFixtureReview(target, false), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	run := &diagnosticPackageRun{SchemaVersion: 1, Owner: "pair-mpi-package-controller-v1", Binding: d.packageReviews[review.ReviewID], Public: diagnosticPackageOperation{OperationID: newOpID(), ReviewID: review.ReviewID, GroupID: review.GroupID, State: "failed", Stage: "cleanup-unconfirmed", Revision: 1, StartedAt: time.Now().UnixMilli(), Targets: append([]diagnosticPackageTarget(nil), review.Targets...)}}
	run.Public.Targets[0].State = "cleanup_unconfirmed"
	run.Public.Targets[0].CleanupConfirmed = false
	d.packageRuns[run.Public.OperationID] = run
	d.packageActive = run.Public.OperationID
	if err := d.savePackageRun(run); err != nil {
		t.Fatal(err)
	}
	return d, run
}

func TestDiagnosticPackagesRejectPairVersionMismatchAndNoOpDrift(t *testing.T) {
	for _, mode := range []string{"pair-mismatch", "no-op-drift"} {
		t.Run(mode, func(t *testing.T) {
			d, selection := inspectionFixture(t, "controller")
			var calls atomic.Int32
			d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				if !bytes.Contains(input, []byte(`"action":"review"`)) {
					t.Error("version drift attempted installation")
				}
				raw := packageFixtureReview(target, true)
				if mode == "pair-mismatch" && target.NodeID == "node-b" || mode == "no-op-drift" && calls.Add(1) > 2 {
					raw = bytes.ReplaceAll(raw, []byte(packageFixtureVersion), []byte("4.1.6-7ubuntu3"))
				}
				return raw, nil
			}
			review, err := d.reviewPackages(context.Background(), selection)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "pair-mismatch" {
				if review.CanApprove {
					t.Fatal("different MPI families were approvable")
				}
				return
			}
			op, err := d.approvePackages(context.Background(), diagnosticPackageApprove{ReviewID: review.ReviewID})
			if err != nil {
				t.Fatal(err)
			}
			if done := waitPackages(t, d, op.OperationID); done.State != "failed" || !done.CleanupConfirmed {
				t.Fatalf("no-op drift accepted: %+v", done)
			}
		})
	}
}

func TestDiagnosticPackagesRecoveryChecksPinsBeforeEffectsAndKeepsFailedRetry(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		t.Run(action, func(t *testing.T) {
			d, run := retainedPackageFixture(t)
			var effects atomic.Int32
			d.packageTestRun = func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
				effects.Add(1)
				return nil, nil
			}
			run.Binding.ControllerPin = strings.Repeat("f", 64)
			request := diagnosticPackageAction{OperationID: run.Public.OperationID, Elevation: packageApproval(run.Public.ReviewID).Elevation}
			var err error
			if action == "retry" {
				_, err = d.retryPackages(context.Background(), request)
			} else {
				_, err = d.packageOperation(context.Background(), "engine:diagnostic-package-cancel", request)
			}
			if err == nil || effects.Load() != 0 {
				t.Fatal("recovery touched a changed trust identity")
			}
			if run.Public.State != "failed" || run.cancel != nil || run.reconciling {
				t.Fatal("failed access/trust validation changed the retained intent")
			}
		})
	}
}

func TestDiagnosticPackageCancelDuringRetryNeverStartsAndSurvivesRestart(t *testing.T) {
	d, run := retainedPackageFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var provisions atomic.Int32
	d.packageTestRun = func(ctx context.Context, _ diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if bytes.Contains(input, []byte(`"action":"reconcile"`)) {
			close(entered)
			<-ctx.Done()
			<-release
			return nil, ctx.Err()
		}
		provisions.Add(1)
		return nil, nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: run.Public.OperationID, Elevation: packageApproval(run.Public.ReviewID).Elevation})
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retry did not reach reconciliation")
	}
	if _, err := d.packageOperation(context.Background(), "engine:diagnostic-package-cancel", diagnosticPackageAction{OperationID: run.Public.OperationID}); err != nil {
		t.Fatal(err)
	}
	// Rehydrate the durable checkpoint before the old worker can finish.
	d.m.onboarding = newOnboardingService(d.m)
	restarted := newDiagnosticService(d.m)
	status, _ := restarted.packageStatus(diagnosticPackageAction{OperationID: run.Public.OperationID})
	if status.Operation == nil || status.Operation.State != "cancelled" || status.Operation.CleanupConfirmed {
		t.Fatal("accepted cancellation was lost on restart")
	}
	close(release)
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled retry succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled retry did not return")
	}
	if provisions.Load() != 0 || run.Public.State != "cancelled" || run.cancel != nil {
		t.Fatal("cancelled reconciliation launched new work")
	}
}

func TestDiagnosticPackagesCompletedHistoryDoesNotDisableRecovery(t *testing.T) {
	d, run := retainedPackageFixture(t)
	run.Public.State = "completed"
	run.Public.CleanupConfirmed = true
	for i := range run.Public.Targets {
		run.Public.Targets[i].CleanupConfirmed = true
	}
	for i := 0; i < 65; i++ {
		if i > 0 {
			run.Public.OperationID = newOpID()
		}
		if err := d.savePackageRun(run); err != nil {
			t.Fatal(err)
		}
	}
	restarted := newDiagnosticService(d.m)
	status, err := restarted.packageStatus(diagnosticPackageAction{})
	if err != nil || status.RecoveryRequired || len(restarted.packageRuns) != 65 {
		t.Fatalf("completed history disabled recovery: %d %+v %v", len(restarted.packageRuns), status, err)
	}
}

func TestDiagnosticPackagesCloseLostUnstartedApprovalBeforeNewReview(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	var effects atomic.Int32
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if !bytes.Contains(input, []byte(`"action":"review"`)) {
			effects.Add(1)
		}
		return packageFixtureReview(target, true), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	status, err := d.packageStatus(diagnosticPackageAction{ReviewID: review.ReviewID})
	if err != nil || status.Operation != nil || status.ReviewClosed {
		t.Fatal("ordinary lookup closed an approval")
	}
	status, err = d.packageStatus(diagnosticPackageAction{ReviewID: review.ReviewID, CloseUnstartedReview: true})
	if err != nil || status.Operation != nil || !status.ReviewClosed {
		t.Fatal("lost unstarted approval could not be conclusively closed")
	}
	if _, err := d.approvePackages(context.Background(), diagnosticPackageApprove{ReviewID: review.ReviewID}); err == nil {
		t.Fatal("closed approval subsequently started")
	}
	fresh, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.approvePackages(context.Background(), diagnosticPackageApprove{ReviewID: fresh.ReviewID})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitPackages(t, d, op.OperationID)
	status, err = d.packageStatus(diagnosticPackageAction{ReviewID: fresh.ReviewID, CloseUnstartedReview: true})
	if err != nil || status.Operation == nil || status.Operation.OperationID != op.OperationID || status.ReviewClosed || effects.Load() != 0 {
		t.Fatal("accepted operation was misclassified as never started")
	}
}

func TestDiagnosticPackageReconciledAbsenceClosesOperationInsteadOfLateRetry(t *testing.T) {
	d, run := retainedPackageFixture(t)
	var provisions atomic.Int32
	d.packageTestRun = func(_ context.Context, _ diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		if !bytes.Contains(input, []byte(`"action":"reconcile"`)) {
			provisions.Add(1)
		}
		return packageFixtureResult(request, false), nil
	}
	_, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: run.Public.OperationID, Elevation: packageApproval(run.Public.ReviewID).Elevation})
	if err == nil || provisions.Load() != 0 || run.Public.State != "cancelled" || !run.Public.CleanupConfirmed || d.packageActive != "" {
		t.Fatal("closed absent attempt was relaunched or could not release ownership")
	}
	if _, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: run.Public.OperationID}); err == nil {
		t.Fatal("terminal absence fence could be retried")
	}
}
