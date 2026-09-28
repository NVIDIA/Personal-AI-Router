// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func pythonLocalPlanFixture(t *testing.T) diagnosticApprovedPackagePlan {
	plan := pythonInstallPlanFixture(t)
	plan.ArchivePolicy.WorkerSHA256 = diagnosticPythonLocalWorkerSHA256
	plan.ArchivePolicy.ArchiveSource, _ = json.Marshal(map[string]any{"kind": "verified-local-archive", "snapshot": "20260901T000000Z", "subdir": ".cache/nvpair/python312-headers-20260901", "dependencies": diagnosticPythonLocalDependencies})
	kib := map[string]uint64{"libexpat1-dev": 712, "libpython3.12-dev": 27505, "python3.12-dev": 502}
	for _, entry := range plan.Packages {
		name := entry["name"].(string)
		artifact := diagnosticPythonSnapshotArtifacts[name]
		entry["sha256"] = artifact.SHA256
		entry["size"] = float64(artifact.Size)
		entry["installedSize"] = float64(kib[name] * 1024)
		entry["origin"] = map[string]any{"kind": "verified_local_archive", "source": "ubuntu-snapshot-20260901T000000Z", "trusted": false}
	}
	return plan
}

func TestPythonLocalArchivePlanRetainsItsActualProvenance(t *testing.T) {
	plan := pythonLocalPlanFixture(t)
	if !validPythonPackagePlan(plan) {
		t.Fatal("valid closed local archive dialect rejected")
	}
	plan.Packages[0]["origin"].(map[string]any)["trusted"] = true
	if validPythonPackagePlan(plan) {
		t.Fatal("local archive impersonated cached APT trust")
	}
	plan = pythonLocalPlanFixture(t)
	plan.Packages[0]["version"] = "3.12.3-1ubuntu0.17"
	if validPythonPackagePlan(plan) {
		t.Fatal("local dialect accepted a runtime/header upgrade")
	}
	plan = pythonLocalPlanFixture(t)
	plan.ArchivePolicy.ArchiveSource = nil
	if validPythonPackagePlan(plan) {
		t.Fatal("local archive lost its mandatory source provenance")
	}
	plan = pythonLocalPlanFixture(t)
	plan.RuntimeBinding = pythonPackagePlanFixture(t).RuntimeBinding
	if validPythonPackagePlan(plan) {
		t.Fatal("first-install dialect accepted an installed runtime repair binding")
	}
}

func TestPythonLocalArchivePayloadAndFixedStageBounds(t *testing.T) {
	if len(diagnosticPythonLocalArchivePayloads) != 3 {
		t.Fatal("closed archive set changed")
	}
	for name, encoded := range diagnosticPythonLocalArchivePayloads {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		artifact := diagnosticPythonSnapshotArtifacts[name]
		sum := sha256.Sum256(data)
		if uint64(len(data)) != artifact.Size || hex.EncodeToString(sum[:]) != artifact.SHA256 {
			t.Fatalf("embedded archive changed: %s", name)
		}
	}
	payload, _ := json.Marshal(map[string]any{"nodeId": "node-c", "principal": "node-c", "archives": diagnosticPythonLocalArchivePayloads})
	if len(payload) > 9<<20 {
		t.Fatal("stage exceeds its closed input bound")
	}
	program, err := diagnosticPythonArchiveStageProgram()
	if err != nil || len(program) > 120<<10 {
		t.Fatalf("fixed stage program exceeds native argument ceiling: %d %v", len(program), err)
	}
}
