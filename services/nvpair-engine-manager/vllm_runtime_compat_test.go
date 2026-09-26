// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
)

func TestQwenRuntimeCompatibilityExcludesNodeCustodyButBindsABIAndDependencies(t *testing.T) {
	facts := vllmPythonFacts{
		Python: "3.12.3", Implementation: "cpython",
		SOABI: "cpython-312-aarch64-linux-gnu", Libc: "glibc", Glibc: "2.39", Pip: "24.0",
	}
	report := vllmDependencyReport{Version: "1", PipVersion: "24.0"}
	for _, dependency := range []struct{ name, version, digest string }{
		{"vllm", vllmQwen38Runtime, vllmQwen38WheelSHA256},
		{"torch", "2.13.0+cu130", strings.Repeat("a", 64)},
		{"nvidia-nccl-cu13", "2.29.1", strings.Repeat("b", 64)},
	} {
		var row vllmDependencyReportEntry
		row.Metadata.Name, row.Metadata.Version = dependency.name, dependency.version
		row.Download.Archive.Hashes = map[string]string{"sha256": dependency.digest}
		report.Install = append(report.Install, row)
	}
	a := vllmRuntimeReceipt{
		Schema: vllmLegacyReceiptSchema, Version: vllmQwen38Runtime, Architecture: "arm64",
		WheelSHA256: vllmQwen38WheelSHA256, RecipeID: vllmQwen38RecipeID,
		RecipeSHA256: vllmQwen38RecipeSHA256, NodeID: "node-a",
		BundleReceiptSHA256: strings.Repeat("1", 64), RecipeReceiptSHA256: strings.Repeat("2", 64),
		PythonSHA256: strings.Repeat("3", 64), PipReportSHA256: strings.Repeat("4", 64),
	}
	b := a
	b.NodeID = "node-b"
	b.BundleReceiptSHA256 = strings.Repeat("5", 64)
	b.PythonSHA256 = strings.Repeat("6", 64)
	if vllmRankRuntimeDigest(a) == vllmRankRuntimeDigest(b) {
		t.Fatal("per-node Qwen runtime custody lost its exact digest")
	}
	_, aCompatibility, err := vllmCompatibilityFromLegacyReport(report, facts, a)
	if err != nil {
		t.Fatal(err)
	}
	_, bCompatibility, err := vllmCompatibilityFromLegacyReport(report, facts, b)
	if err != nil || aCompatibility != bCompatibility {
		t.Fatalf("equivalent Qwen members differ: %s %s %v", aCompatibility, bCompatibility, err)
	}
	changedFacts := facts
	changedFacts.SOABI = "cpython-313-aarch64-linux-gnu"
	_, changedABI, err := vllmCompatibilityFromLegacyReport(report, changedFacts, a)
	if err == nil && changedABI == aCompatibility {
		t.Fatal("Python ABI drift retained compatibility")
	}
	report.Install[1].Download.Archive.Hashes["sha256"] = strings.Repeat("c", 64)
	_, changedDependency, err := vllmCompatibilityFromLegacyReport(report, facts, a)
	if err != nil {
		t.Fatal(err)
	}
	if changedDependency == aCompatibility {
		t.Fatal("dependency archive drift retained compatibility")
	}
	report.Install[0].Download.Archive.Hashes["sha256"] = strings.Repeat("d", 64)
	if _, _, err := vllmCompatibilityFromLegacyReport(report, facts, a); err == nil {
		t.Fatal("mismatched vLLM dependency archive was admitted")
	}
}

func TestManagedRuntimeCompatibilityExcludesPathRewrittenConsoleScripts(t *testing.T) {
	facts := vllmPythonFacts{
		Python: "3.12.14", Implementation: "cpython", SOABI: "cpython-312-aarch64-linux-gnu",
		Libc: "glibc", Glibc: "2.39", Pip: "25.0.1",
	}
	receipt := vllmRuntimeReceipt{
		Schema: vllmEnvironmentReceiptSchema, Version: managedVLLMVersion,
		Architecture: "arm64", RecipeID: managedVLLMRecipeID,
		UVSHA256: managedVLLMRunnableRecipes[0].UVSHA256,
	}
	size := int64(10)
	report := vllmManagedDependencyReport{Schema: 1, Packages: []vllmManagedDependencyPackage{
		{Name: "vllm", Version: managedVLLMVersion, Files: []vllmManagedDependencyFile{
			{Path: "vllm/__init__.py", Hash: "sha256=stable-vllm", Size: &size},
			{Path: "../../../bin/vllm", Hash: "sha256=node-a-shebang", Size: &size},
		}},
		{Name: "fastapi", Version: "1.0.0", Files: []vllmManagedDependencyFile{
			{Path: "fastapi/__init__.py", Hash: "sha256=stable-fastapi", Size: &size},
			{Path: "../../../bin/fastapi", Hash: "sha256=node-a-shebang", Size: &size},
		}},
	}}
	_, first, err := vllmCompatibilityFromManagedReport(report, facts, receipt)
	if err != nil {
		t.Fatal(err)
	}
	report.Packages[0].Files[1].Hash = "sha256=node-b-shebang"
	report.Packages[1].Files[1].Hash = "sha256=node-b-shebang"
	_, second, err := vllmCompatibilityFromManagedReport(report, facts, receipt)
	if err != nil || first != second {
		t.Fatalf("path-rewritten console scripts changed compatibility: %s %s %v", first, second, err)
	}
	report.Packages[1].Files[0].Hash = "sha256=changed-wheel-content"
	_, changed, err := vllmCompatibilityFromManagedReport(report, facts, receipt)
	if err != nil || changed == first {
		t.Fatalf("wheel-owned content drift retained compatibility: %s %s %v", first, changed, err)
	}
}

func TestReviewedRuntimeCompatibilityIsRevalidatedAtPrepareAndStart(t *testing.T) {
	recipe := managedVLLMLegacyOwnershipRecipes[0]
	receipt := vllmRuntimeReceipt{
		Schema: vllmLegacyReceiptSchema, Version: recipe.Version, Architecture: recipe.Architecture,
		WheelURL: recipe.WheelURL, WheelSHA256: recipe.WheelSHA256,
		PythonSHA256: strings.Repeat("1", 64), PipReportSHA256: strings.Repeat("2", 64),
	}
	runtimeDigest := vllmRankRuntimeDigest(receipt)
	compatibility := strings.Repeat("a", 64)
	member := vllmGroupMember{
		RuntimeDigest: runtimeDigest, RuntimeCompatibilitySHA256: compatibility,
	}
	e := &Executor{vllmRuntimeLock: func(context.Context, *engineState) (vllmRuntimeLock, error) {
		return vllmRuntimeLock{
			SourceRuntimeDigest: runtimeDigest, CompatibilitySHA256: strings.Repeat("b", 64),
		}, nil
	}}
	if err := e.validateVLLMReviewedRuntime(context.Background(), &engineState{}, receipt, member); err == nil {
		t.Fatal("capture-to-start ABI or dependency drift was admitted")
	}
	e.vllmRuntimeLock = func(context.Context, *engineState) (vllmRuntimeLock, error) {
		return vllmRuntimeLock{
			SourceRuntimeDigest: runtimeDigest, CompatibilitySHA256: compatibility,
		}, nil
	}
	if err := e.validateVLLMReviewedRuntime(context.Background(), &engineState{}, receipt, member); err != nil {
		t.Fatalf("unchanged reviewed runtime was refused: %v", err)
	}
}
