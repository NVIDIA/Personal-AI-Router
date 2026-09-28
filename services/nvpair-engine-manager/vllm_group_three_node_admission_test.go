// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVLLMThreeNodeSystemAdmissionAndCleanRankRestore(t *testing.T) {
	selection, facts, pins := groupPlanFactsFixture(3)
	for rank := range facts {
		facts[rank].ModelPath = t.TempDir()
	}
	plan, err := assembleVLLMGroupPlan(selection, facts, pins)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := vllmGroupPlanDigest(plan)
	binding := vllmGroupBinding{RunID: strings.Repeat("a", 32), Generation: 1, PlanDigest: digest, Plan: plan, Rank: 2}
	member := plan.Members[2]
	placement := vllmRankPlacement{LocalNode: member.NodeID, ModelPath: member.Placement.ModelPath,
		LocalAddress: member.Placement.Address, CoordinatorAddress: plan.Members[0].Placement.Address,
		APIPort: member.Placement.APIPort, MasterPort: member.Placement.MasterPort}
	if err := validateVLLMRankBinding(binding, placement); err != nil {
		t.Fatal("valid third rank rejected", err)
	}
	if _, err := vllmRankArgs("/owned/vllm", binding, placement, *member.Resources); err == nil {
		t.Fatal("legacy two-node direct launcher accepted a PP3 plan")
	}
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := vllmRankReceipt{RunID: binding.RunID, Generation: 1, PlanDigest: digest, Rank: 2, State: "stopped", CleanupConfirmed: true}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), receipt); err != nil {
		t.Fatal(err)
	}
	if rank := f.e.restoreVLLMRankHold(f.st); rank != nil {
		t.Fatal("clean third rank became a permanent recovery hold")
	}
}
