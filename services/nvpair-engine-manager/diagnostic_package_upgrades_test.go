// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Go consent/receipt fixture only. The native APT resolver separately proves
// the complete captured dependency closure and real archive metadata.
func packageUpgradeFixtureReview(target diagnosticInspectionTarget, upgrade bool) []byte {
	var value map[string]any
	_ = json.Unmarshal(packageFixtureReview(target, false), &value)
	plan := value["plan"].(map[string]any)
	plan["recipeId"] = diagnosticPackageUpgradeRecipe
	plan["planDigest"] = strings.Repeat("d", 64)
	if target.NodeID == "node-b" {
		plan["planDigest"] = strings.Repeat("e", 64)
	}
	entries := packageFixtureEntries()
	entries[0]["installedVersion"] = nil
	if upgrade {
		entry := packageFixtureEntries()[0]
		entry["name"], entry["installedVersion"], entry["version"] = "gcc-13", "13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1"
		entries = append(entries, entry)
	}
	plan["packages"] = entries
	plan["limits"] = map[string]any{"upgrades": true, "maxDependencyUpgrades": 14, "removals": false, "recommends": false}
	plan["archivePolicy"].(map[string]any)["needrestartMode"] = "l"
	raw, _ := json.Marshal(value)
	return raw
}

func packageUpgradeFixtureSuccess(request map[string]json.RawMessage) []byte {
	var value, plan map[string]any
	_ = json.Unmarshal(packageFixtureResult(request, true), &value)
	_ = json.Unmarshal(request["approvedPlan"], &plan)
	value["planDigest"] = plan["planDigest"]
	receipt := value["receipt"].(map[string]any)
	receipt["recipeId"], receipt["planDigest"], receipt["packages"] = plan["recipeId"], plan["planDigest"], plan["packages"]
	archive := receipt["archiveReceipt"].(map[string]any)
	archive["planDigest"] = plan["planDigest"]
	archives := []map[string]any{}
	for _, item := range plan["packages"].([]any) {
		entry := item.(map[string]any)
		row := map[string]any{}
		for _, field := range []string{"name", "version", "architecture", "sha256", "size"} {
			row[field] = entry[field]
		}
		archives = append(archives, row)
	}
	archive["archives"] = archives
	raw, _ := json.Marshal(value)
	return raw
}

func TestDiagnosticDependencyUpgradePlanRejectsCandidateAndPolicyDrift(t *testing.T) {
	target := diagnosticInspectionTarget{NodeID: "node-a", Principal: "node-a"}
	for _, kind := range []string{"valid", "new-only", "unknown-new", "wrong-new-version", "wrong-old-version", "wrong-upgrade-version", "wrong-architecture", "duplicate", "missing-old-field", "upgrades-disabled", "removals", "recommends", "excess-limit", "restart-mode", "legacy-upgrade"} {
		t.Run(kind, func(t *testing.T) {
			var review map[string]any
			_ = json.Unmarshal(packageUpgradeFixtureReview(target, kind != "new-only"), &review)
			plan := review["plan"].(map[string]any)
			entries := plan["packages"].([]any)
			first := entries[0].(map[string]any)
			switch kind {
			case "unknown-new":
				first["name"] = "unrelated-package"
			case "wrong-new-version":
				first["version"] = "4.1.6-unreviewed"
			case "wrong-old-version":
				entries[1].(map[string]any)["installedVersion"] = "13.2-unreviewed"
			case "wrong-upgrade-version":
				entries[1].(map[string]any)["version"] = "13.3.0-6ubuntu2~24.04.2"
			case "wrong-architecture":
				entries[1].(map[string]any)["architecture"] = "all"
			case "duplicate":
				plan["packages"] = append(entries, entries[1])
			case "missing-old-field":
				delete(first, "installedVersion")
			case "upgrades-disabled":
				plan["limits"].(map[string]any)["upgrades"] = false
			case "removals":
				plan["limits"].(map[string]any)["removals"] = true
			case "recommends":
				plan["limits"].(map[string]any)["recommends"] = true
			case "excess-limit":
				plan["limits"].(map[string]any)["maxDependencyUpgrades"] = 15
			case "restart-mode":
				plan["archivePolicy"].(map[string]any)["needrestartMode"] = "a"
			case "legacy-upgrade":
				plan["recipeId"] = diagnosticPackageLegacyRecipe
			}
			raw, _ := json.Marshal(plan)
			parsed, err := approvedPackagePlan(raw)
			if kind != "valid" && kind != "new-only" {
				if err == nil {
					t.Fatal("unreviewed dependency plan accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			needs, err := diagnosticPackagePlanUpgrades(parsed)
			if err != nil || needs != (kind == "valid") {
				t.Fatalf("upgrade classification: %v %v", needs, err)
			}
		})
	}
	var legacy diagnosticPackageResult
	_ = json.Unmarshal(packageFixtureReview(target, false), &legacy)
	if _, err := approvedPackagePlan(legacy.Plan); err != nil {
		t.Fatalf("historical v3 refused: %v", err)
	}
}

func TestDiagnosticDependencyUpgradeReviewRequiresExplicitOptIn(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	var effects atomic.Int32
	var flags []bool
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		if string(request["action"]) != `"review"` {
			effects.Add(1)
			return nil, errors.New("unexpected effect")
		}
		flags = append(flags, string(request["dependencyUpgradeReview"]) == "true")
		return packageUpgradeFixtureReview(target, true), nil
	}
	ordinary, err := d.reviewPackages(context.Background(), selection)
	if err != nil || ordinary.CanApprove || ordinary.DependencyUpgradeReview {
		t.Fatalf("ordinary review received upgrade authority: %+v %v", ordinary, err)
	}
	opted, err := d.reviewPackages(context.Background(), selection, true)
	if err != nil || !opted.CanApprove || !opted.DependencyUpgradeReview || !reflect.DeepEqual(flags, []bool{false, false, true, true}) || effects.Load() != 0 {
		t.Fatalf("explicit upgrade review binding failed: %v flags=%v", err, flags)
	}
}

func TestDiagnosticDependencyUpgradeConsentIsExactBeforeEffects(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	var effects atomic.Int32
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if !bytes.Contains(input, []byte(`"action":"review"`)) {
			effects.Add(1)
			return nil, errors.New("unapproved effect")
		}
		return packageUpgradeFixtureReview(target, true), nil
	}
	review, err := d.reviewPackages(context.Background(), selection, true)
	if err != nil || !review.CanApprove {
		t.Fatalf("fixture review: %v", err)
	}
	a := diagnosticDependencyUpgradeApproval{NodeID: "node-a", PlanDigest: strings.Repeat("d", 64)}
	b := diagnosticDependencyUpgradeApproval{NodeID: "node-b", PlanDigest: strings.Repeat("e", 64)}
	for _, approvals := range [][]diagnosticDependencyUpgradeApproval{nil, {a}, {a, b, {NodeID: "node-c", PlanDigest: a.PlanDigest}}, {a, a}, {a, {NodeID: "node-c", PlanDigest: b.PlanDigest}}, {{NodeID: a.NodeID, PlanDigest: strings.Repeat("f", 64)}, b}, {{NodeID: a.NodeID, PlanDigest: "bad"}, b}} {
		request := packageApproval(review.ReviewID)
		request.DependencyUpgradeApprovals = approvals
		if _, err := d.approvePackages(context.Background(), request); err == nil {
			t.Fatal("missing, extra, duplicate or changed consent admitted")
		}
		if effects.Load() != 0 || len(d.packageRuns) != 0 || d.packageActive != "" {
			t.Fatal("consent refusal acquired effect authority")
		}
	}
	if err := validateDiagnosticUpgradeApprovals(d.packageReviews[review.ReviewID], []diagnosticDependencyUpgradeApproval{b, a}); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticDependencyUpgradeApprovalPersistsConsentAndSetsOnlyNeededFlag(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	var effects atomic.Int32
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		if string(request["action"]) == `"review"` {
			return packageUpgradeFixtureReview(target, target.NodeID == "node-a"), nil
		}
		if string(request["action"]) != `"provision"` {
			return nil, errors.New("unexpected action")
		}
		effects.Add(1)
		flag, present := request["dependencyUpgradesApproved"]
		if present != (target.NodeID == "node-a") || present && string(flag) != "true" {
			t.Error("upgrade consent crossed target boundary")
		}
		return packageUpgradeFixtureSuccess(request), nil
	}
	review, err := d.reviewPackages(context.Background(), selection, true)
	if err != nil || !review.CanApprove {
		t.Fatalf("fixture review: %v", err)
	}
	request := packageApproval(review.ReviewID)
	request.DependencyUpgradeApprovals = []diagnosticDependencyUpgradeApproval{{NodeID: "node-a", PlanDigest: strings.Repeat("d", 64)}}
	op, err := d.approvePackages(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op = waitPackages(t, d, op.OperationID)
	if op.State != "completed" || !op.CleanupConfirmed || effects.Load() != 2 {
		t.Fatalf("approved mocked transaction failed: %+v", op)
	}
	journal, err := os.ReadFile(d.packageOperationPath(op.OperationID))
	if err != nil || bytes.Contains(journal, []byte("separate-admin-fixture")) || bytes.Contains(journal, []byte("volatile-ssh-fixture")) {
		t.Fatal("credential material entered the retained journal")
	}
	var saved diagnosticPackageRun
	if json.Unmarshal(journal, &saved) != nil || !reflect.DeepEqual(saved.DependencyUpgradeApprovals, request.DependencyUpgradeApprovals) || !validPackageRun(saved, op.OperationID) {
		t.Fatal("exact public upgrade consent was not retained")
	}
	restarted := newDiagnosticService(d.m)
	if restarted.packageRecoveryFailed || restarted.packageRuns[op.OperationID] == nil || !reflect.DeepEqual(restarted.packageRuns[op.OperationID].DependencyUpgradeApprovals, request.DependencyUpgradeApprovals) {
		t.Fatal("restart lost approved upgrade ownership")
	}
}

func TestDiagnosticDependencyUpgradeInvalidRetainedConsentHoldsLoadAndRetry(t *testing.T) {
	for _, kind := range []string{"missing", "stale", "duplicate", "malformed", "lost-plan-and-consent"} {
		t.Run(kind, func(t *testing.T) {
			d, selection := inspectionFixture(t, "controller")
			d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, _ []byte) ([]byte, error) {
				return packageUpgradeFixtureReview(target, target.NodeID == "node-a"), nil
			}
			review, err := d.reviewPackages(context.Background(), selection, true)
			if err != nil || !review.CanApprove {
				t.Fatalf("fixture review: %v", err)
			}
			approval := diagnosticDependencyUpgradeApproval{NodeID: "node-a", PlanDigest: strings.Repeat("d", 64)}
			run := &diagnosticPackageRun{SchemaVersion: 1, Owner: "pair-mpi-package-controller-v1", Binding: d.packageReviews[review.ReviewID], DependencyUpgradeApprovals: []diagnosticDependencyUpgradeApproval{approval}, Public: diagnosticPackageOperation{OperationID: newOpID(), ReviewID: review.ReviewID, GroupID: review.GroupID, State: "failed", Stage: "failed", Revision: 1, StartedAt: time.Now().UnixMilli(), CleanupConfirmed: true, Targets: append([]diagnosticPackageTarget(nil), review.Targets...)}}
			if !validPackageRun(*run, run.Public.OperationID) {
				t.Fatal("valid retained control fixture rejected")
			}
			switch kind {
			case "missing":
				run.DependencyUpgradeApprovals = nil
			case "stale":
				run.DependencyUpgradeApprovals[0].PlanDigest = strings.Repeat("f", 64)
			case "duplicate":
				run.DependencyUpgradeApprovals = append(run.DependencyUpgradeApprovals, approval)
			case "malformed":
				run.DependencyUpgradeApprovals[0].NodeID = "bad\nnode"
			case "lost-plan-and-consent":
				run.DependencyUpgradeApprovals = nil
				for i := range run.Binding.Review.Targets {
					run.Binding.Review.Targets[i].Review = nil
					run.Public.Targets[i].Review = nil
				}
			}
			d.packageRuns[run.Public.OperationID] = run
			if err := d.savePackageRun(run); err != nil {
				t.Fatal(err)
			}
			var effects atomic.Int32
			d.packageTestRun = func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
				effects.Add(1)
				return nil, errors.New("unexpected replay")
			}
			if _, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: run.Public.OperationID, Elevation: packageApproval(review.ReviewID).Elevation}); err == nil || effects.Load() != 0 {
				t.Fatal("invalid retained consent reached retry effects")
			}
			restarted := newDiagnosticService(d.m)
			if !restarted.packageRecoveryFailed || restarted.packageRuns[run.Public.OperationID] != nil {
				t.Fatal("invalid upgrade journal became usable or empty-success")
			}
		})
	}
}
