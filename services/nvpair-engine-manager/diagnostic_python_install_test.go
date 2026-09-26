// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func pythonInstallPlanFixture(t *testing.T) diagnosticApprovedPackagePlan {
	t.Helper()
	plan := pythonPackagePlanFixture(t)
	plan.RuntimeBinding = &diagnosticPythonRuntimeBinding{SchemaVersion: 2,
		InstallRoot:      "/home/fixture/.config/Nvidia Corporation/Personal AI Router/engine-bin/vllm",
		SourceExecutable: "/usr/bin/python3.12", SourceExecutableSHA256: strings.Repeat("a", 64),
		SourcePackageVersion: diagnosticPythonPackageVersion, PythonVersion: "3.12.3",
		IncludePath: "/usr/include/python3.12", ConfigHeaderPath: "/usr/include/python3.12/pyconfig.h"}
	return plan
}

func TestPythonFirstInstallSourceCannotMasqueradeAsRuntime(t *testing.T) {
	plan := pythonInstallPlanFixture(t)
	raw, _ := json.Marshal(plan)
	if _, err := approvedPackagePlan(raw); err != nil {
		t.Fatal(err)
	}
	if !validPythonPreparationBinding(plan.RuntimeBinding, plan.Identity, true) || validPythonPreparationBinding(plan.RuntimeBinding, plan.Identity, false) {
		t.Fatal("first-install source was not disjoint from installed-runtime repair")
	}
	var wire struct {
		RuntimeBinding map[string]json.RawMessage `json:"runtimeBinding"`
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.RuntimeBinding) != 8 {
		t.Fatal("source binding contains runtime fields")
	}
	plan.RuntimeBinding.RuntimeRecordSHA256 = strings.Repeat("b", 64)
	if validPythonPackagePlan(plan) {
		t.Fatal("source binding accepted a placeholder runtime receipt")
	}
}

func TestPythonFirstInstallReceiptRequiresUnchangedSourceAndNormalAccount(t *testing.T) {
	plan := pythonInstallPlanFixture(t)
	family := []diagnosticInstalledMPIPackage{}
	for name, version := range diagnosticPythonPackages {
		family = append(family, diagnosticInstalledMPIPackage{Name: name, Version: version, Architecture: "arm64", Status: "install ok installed"})
	}
	facts := &diagnosticPythonPrerequisites{CheckedAsUID: 1000, SourceBindingUnchanged: true, Compiler: true, PythonHeaders: true, ConfigHeader: true}
	receipt := map[string]any{"recipeId": plan.RecipeID, "planDigest": plan.PlanDigest, "runtimeValidated": false,
		"packages": plan.Packages, "runtimeBinding": plan.RuntimeBinding, "runtimePrerequisites": facts, "installedFamily": family}
	raw, _ := json.Marshal(receipt)
	if !validPackageReceipt(diagnosticPackageResult{Receipt: raw}, plan, strings.Repeat("c", 32)) {
		t.Fatal("valid source postcheck rejected")
	}
	changed := *plan.RuntimeBinding
	changed.SourceExecutableSHA256 = strings.Repeat("d", 64)
	receipt["runtimeBinding"] = &changed
	raw, _ = json.Marshal(receipt)
	if validPackageReceipt(diagnosticPackageResult{Receipt: raw}, plan, strings.Repeat("c", 32)) {
		t.Fatal("changed source passed postcheck")
	}
	receipt["runtimeBinding"] = plan.RuntimeBinding
	facts.CheckedAsUID = 0
	raw, _ = json.Marshal(receipt)
	if validPackageReceipt(diagnosticPackageResult{Receipt: raw}, plan, strings.Repeat("c", 32)) {
		t.Fatal("root postcheck passed")
	}
}

func TestPythonFirstInstallRecoveryRetainsModeAndGlobalHold(t *testing.T) {
	review := diagnosticPackageReview{FirstInstall: true, Purpose: diagnosticPythonPackagePurpose,
		ReviewID: strings.Repeat("b", 32), GroupID: diagnosticPythonPackagePrefix + "node-b", Targets: []diagnosticPackageTarget{{NodeID: "node-b"}}}
	id := strings.Repeat("a", 32)
	run := &diagnosticPackageRun{Binding: diagnosticPackageBinding{Review: review}, Public: diagnosticPackageOperation{
		FirstInstall: true, Purpose: review.Purpose, OperationID: id, ReviewID: review.ReviewID, GroupID: review.GroupID,
		State: "interrupted", CleanupConfirmed: false}}
	d := &diagnosticService{packageRuns: map[string]*diagnosticPackageRun{id: run}}
	global, err := d.packageStatus(diagnosticPackageAction{})
	if err != nil || global.Operation == nil || !global.Operation.FirstInstall {
		t.Fatal("shared-owner hold lost first-install mode")
	}
	request := diagnosticPackageAction{Purpose: review.Purpose, RuntimeNodeID: "node-b", OperationID: id}
	if _, err := d.packageStatus(request); err == nil {
		t.Fatal("repair status crossed into first-install operation")
	}
	if _, err := d.packageOperation(context.Background(), "engine:diagnostic-package-cancel", request); err == nil {
		t.Fatal("repair cancellation crossed modes")
	}
	if _, err := d.approvePackages(context.Background(), diagnosticPackageApprove{Purpose: review.Purpose, RuntimeNodeID: "node-b", ReviewID: review.ReviewID}); err == nil {
		t.Fatal("lost approval crossed modes")
	}
	request.FirstInstall = true
	if _, err := d.packageStatus(request); err != nil {
		t.Fatal(err)
	}
	if validatePackageActionScope(diagnosticPackageAction{FirstInstall: true, OperationID: id}) == nil {
		t.Fatal("first-install flag escaped Python purpose")
	}
}

func TestPythonFirstInstallTransportUsesRetainedMode(t *testing.T) {
	d := &diagnosticService{packageTestRun: func(_ context.Context, _ diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		return append([]byte(nil), input...), nil
	}}
	for _, firstInstall := range []bool{false, true} {
		binding := diagnosticPackageBinding{Review: diagnosticPackageReview{Purpose: diagnosticPythonPackagePurpose, FirstInstall: firstInstall}}
		raw, err := d.packageNativeBound(context.Background(), diagnosticInspectionTarget{}, onboardingPrivateTarget{}, binding, map[string]any{"action": "status"})
		var request map[string]any
		if err != nil || json.Unmarshal(raw, &request) != nil || request["runtimePreparation"] != true {
			t.Fatal("fixed native purpose lost")
		}
		if firstInstall && request["firstInstall"] != true || !firstInstall && request["firstInstall"] != nil {
			t.Fatal("native operation guessed preparation mode")
		}
	}
}
