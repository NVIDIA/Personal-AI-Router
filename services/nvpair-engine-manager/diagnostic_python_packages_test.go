// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func pythonPackagePlanFixture(t *testing.T) diagnosticApprovedPackagePlan {
	t.Helper()
	plan := diagnosticApprovedPackagePlan{SchemaVersion: 1, RecipeID: diagnosticPythonPackageRecipe, PlanDigest: strings.Repeat("f", 64), Identity: &diagnosticPackageIdentity{NodeID: "node-b", Principal: "node-b", UID: 1000, Home: "/home/fixture"}}
	plan.RootPackage.Name, plan.RootPackage.Version = "python3.12-dev", diagnosticPythonPackageVersion
	plan.ArchivePolicy.WorkerSHA256 = strings.Repeat("e", 64)
	plan.Limits.MaxPackages = 3
	plan.RuntimeBinding = &diagnosticPythonRuntimeBinding{SchemaVersion: 1, EnvironmentID: "v0.28.0-fixture", PythonPath: "/home/fixture/.config/Nvidia Corporation/Personal AI Router/engine-bin/vllm/environments/v0.28.0-fixture/bin/python", PythonSHA256: strings.Repeat("a", 64), RuntimeRecordSHA256: strings.Repeat("b", 64), RuntimeReceiptSHA256: strings.Repeat("c", 64), PipReportSHA256: strings.Repeat("d", 64), PyvenvConfigSHA256: strings.Repeat("e", 64), SourceExecutable: "/usr/bin/python3.12", SourceExecutableSHA256: strings.Repeat("a", 64), SourcePackageVersion: diagnosticPythonPackageVersion, PythonVersion: "3.12.3", IncludePath: "/usr/include/python3.12", ConfigHeaderPath: "/usr/include/python3.12/pyconfig.h"}
	for _, name := range []string{"python3.12-dev", "libpython3.12-dev", "libexpat1-dev"} {
		plan.Packages = append(plan.Packages, map[string]any{"name": name, "version": diagnosticPythonPackages[name], "architecture": "arm64", "sha256": strings.Repeat("a", 64), "size": float64(100), "installedSize": float64(200), "origin": map[string]any{"site": "ports.ubuntu.com", "archive": "noble-updates", "component": "main", "label": "Ubuntu", "origin": "Ubuntu", "trusted": true}})
	}
	return plan
}

func TestPythonPackagePlanBindsFixedExistingRuntime(t *testing.T) {
	plan := pythonPackagePlanFixture(t)
	raw, _ := json.Marshal(plan)
	if _, err := approvedPackagePlan(raw); err != nil {
		t.Fatal(err)
	}
	if upgrades, err := diagnosticPackagePlanUpgrades(plan); err != nil || upgrades {
		t.Fatal("runtime repair gained dependency upgrades")
	}
	plan.RuntimeBinding.SourcePackageVersion = "3.12.3-1ubuntu0.17"
	if validPythonPackagePlan(plan) {
		t.Fatal("changed source interpreter version was accepted")
	}
}

func TestPythonSnapshotSourceAndArchiveBytesStayClosed(t *testing.T) {
	for _, firstInstall := range []bool{false, true} {
		for _, change := range []string{"none", "source", "snapshot", "extra", "worker", "hash", "size", "version", "architecture", "missing-url"} {
			t.Run(fmt.Sprintf("first=%t/%s", firstInstall, change), func(t *testing.T) {
				plan := pythonPackagePlanFixture(t)
				if firstInstall {
					plan = pythonInstallPlanFixture(t)
				}
				urls := map[string]string{}
				for name, artifact := range diagnosticPythonSnapshotArtifacts {
					urls[name] = artifact.URL
				}
				source := map[string]any{"kind": "ubuntu-snapshot", "snapshot": "20260901T000000Z", "urls": urls}
				plan.ArchivePolicy.WorkerSHA256 = diagnosticPythonSnapshotWorkerSHA256
				for _, item := range plan.Packages {
					artifact := diagnosticPythonSnapshotArtifacts[item["name"].(string)]
					item["sha256"], item["size"] = artifact.SHA256, float64(artifact.Size)
				}
				switch change {
				case "source":
					urls["python3.12-dev"] = "https://example.invalid/python.deb"
				case "snapshot":
					source["snapshot"] = "20260902T000000Z"
				case "extra":
					source["redirects"] = true
				case "worker":
					plan.ArchivePolicy.WorkerSHA256 = strings.Repeat("b", 64)
				case "hash":
					plan.Packages[0]["sha256"] = strings.Repeat("b", 64)
				case "size":
					plan.Packages[0]["size"] = float64(100)
				case "version":
					plan.Packages[0]["version"] = "3.12.3-1ubuntu0.17"
				case "architecture":
					plan.Packages[0]["architecture"] = "amd64"
				case "missing-url":
					delete(urls, "libexpat1-dev")
				}
				plan.ArchivePolicy.ArchiveSource, _ = json.Marshal(source)
				raw, _ := json.Marshal(plan)
				parsed, err := approvedPackagePlan(raw)
				if change == "none" {
					if err != nil || !bytes.Equal(parsed.ArchivePolicy.ArchiveSource, plan.ArchivePolicy.ArchiveSource) {
						t.Fatalf("producer source was rejected or dropped: %v", err)
					}
				} else if err == nil {
					t.Fatal("changed source/archive accepted")
				}
			})
		}
	}
}

func TestPythonPackageScopeDoesNotRelaxMPIRoster(t *testing.T) {
	review := diagnosticPackageReview{Purpose: diagnosticPythonPackagePurpose, GroupID: diagnosticPythonPackagePrefix + "node-b", Targets: []diagnosticPackageTarget{{NodeID: "node-b"}}}
	binding := diagnosticPackageBinding{Review: review, RuntimeNodeID: "node-b", diagnosticParticipantBinding: diagnosticParticipantBinding{Pair: diagnosticSetupRequest{GroupID: review.GroupID, Members: []diagnosticSetupMember{{NodeID: "node-b", Principal: "node-b"}}}, Controller: "node-a", ControllerPin: strings.Repeat("a", 64), Pins: map[string]string{"node-b": strings.Repeat("b", 64)}, Targets: []diagnosticInspectionTarget{{NodeID: "node-b", Principal: "node-b", Address: "192.0.2.2", Candidate: &onboardingCandidate{CandidateID: strings.Repeat("a", 32), Address: "192.0.2.2", Port: 22, HostKeySHA256: "SHA256:fixture"}}}}}
	if !validPackageParticipantBinding(binding) {
		t.Fatal("fixed singleton runtime scope was rejected")
	}
	if validDiagnosticParticipantBinding(binding.diagnosticParticipantBinding) {
		t.Fatal("MPI participant validation was widened")
	}
	if packageScopeMatches("", "", false, review) || packageScopeMatches(diagnosticPythonPackagePurpose, "node-a", false, review) {
		t.Fatal("cross-purpose or stale-node request was accepted")
	}
	if !packageScopeMatches(diagnosticPythonPackagePurpose, "node-b", false, review) {
		t.Fatal("retained runtime scope was rejected")
	}
}

func TestPythonPackageStatusKeepsGlobalOwnerHoldVisible(t *testing.T) {
	review := diagnosticPackageReview{Purpose: diagnosticPythonPackagePurpose, ReviewID: strings.Repeat("b", 32), GroupID: diagnosticPythonPackagePrefix + "node-b", Targets: []diagnosticPackageTarget{{NodeID: "node-b"}}}
	id := strings.Repeat("a", 32)
	run := &diagnosticPackageRun{Binding: diagnosticPackageBinding{Review: review}, Public: diagnosticPackageOperation{Purpose: review.Purpose, OperationID: id, ReviewID: review.ReviewID, GroupID: review.GroupID, State: "interrupted", CleanupConfirmed: false}}
	d := &diagnosticService{packageRuns: map[string]*diagnosticPackageRun{id: run}}
	status, err := d.packageStatus(diagnosticPackageAction{})
	if err != nil || status.Operation == nil || status.Operation.OperationID != id {
		t.Fatal("global package-owner recovery disappeared")
	}
	if _, err := d.packageStatus(diagnosticPackageAction{OperationID: id}); err == nil {
		t.Fatal("scoped runtime lookup omitted its purpose")
	}
	if _, err := d.packageStatus(diagnosticPackageAction{OperationID: id, Purpose: diagnosticPythonPackagePurpose, RuntimeNodeID: "node-b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.approvePackages(context.Background(), diagnosticPackageApprove{ReviewID: review.ReviewID}); err == nil {
		t.Fatal("approval crossed retained purposes")
	}
}

func TestPythonPreparationInputHasNoPackageSelector(t *testing.T) {
	var request diagnosticPythonPreparationRequest
	if onboardingDecode(json.RawMessage(`{"nodeId":"node-b","packages":["python3.12-dev"]}`), &request) == nil {
		t.Fatal("caller package selector entered the fixed recipe")
	}
	if onboardingDecode(json.RawMessage(`{"nodeId":"node-b"}`), &request) != nil {
		t.Fatal("fixed node selector rejected")
	}
}

func TestPythonPackageReceiptRequiresNormalUserPostcheck(t *testing.T) {
	plan := pythonPackagePlanFixture(t)
	family := []diagnosticInstalledMPIPackage{}
	for name, version := range diagnosticPythonPackages {
		family = append(family, diagnosticInstalledMPIPackage{Name: name, Version: version, Architecture: "arm64", Status: "install ok installed"})
	}
	facts := &diagnosticPythonPrerequisites{CheckedAsUID: 1000, SourceBindingUnchanged: true, Compiler: true, PythonHeaders: true, ConfigHeader: true}
	receipt := map[string]any{"recipeId": plan.RecipeID, "planDigest": plan.PlanDigest, "runtimeValidated": false, "packages": plan.Packages, "runtimeBinding": plan.RuntimeBinding, "runtimePrerequisites": facts, "installedFamily": family}
	raw, _ := json.Marshal(receipt)
	if !validPackageReceipt(diagnosticPackageResult{Receipt: raw}, plan, strings.Repeat("a", 32)) {
		t.Fatal("valid fixed repair receipt rejected")
	}
	facts.CheckedAsUID = 0
	raw, _ = json.Marshal(receipt)
	if validPackageReceipt(diagnosticPackageResult{Receipt: raw}, plan, strings.Repeat("a", 32)) {
		t.Fatal("root or unmatched-user postcheck was accepted")
	}
}

func TestPackageProgramCompressionRetainsPinnedModules(t *testing.T) {
	program := diagnosticPackageProgram()
	start, end := strings.Index(program, `"`), strings.LastIndex(program, `"`)
	if start < 0 || end <= start {
		t.Fatal("fixed program encoding missing")
	}
	encoded, err := base64.StdEncoding.DecodeString(program[start+1 : end])
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zlib.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	_ = reader.Close()
	if err != nil || !bytes.HasSuffix(decoded, []byte(diagnosticPackagesPython)) || !bytes.Contains(decoded, []byte("diagnostic_tools_remote")) || len("/usr/bin/python3 -I -c "+onboardingQuote(program)) >= 128<<10 {
		t.Fatal("fixed modules or native argument bound changed")
	}
}
