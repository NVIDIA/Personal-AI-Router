// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// The root rank owner re-checks every lane's HCA name and builds the fixed
// Qwen3.8 command in Python; run its unittest modules against the source file
// this package embeds.
func TestRankOwnerPythonUnitTests(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil && runtime.GOOS == "windows" {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	cmd := exec.Command(python, "-m", "unittest", "-q", "test_vllm_rank_owner_hca", "test_vllm_rank_owner_args")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rank owner unit tests failed: %v\n%s", err, out)
	}
}

func TestQwenRoCEAuxiliaryHCAsPreserveExactFabricBinding(t *testing.T) {
	_, run := qualifiedDirectFabricFixture(t)
	plan := qwenDirectTestPlan(t, run)
	endpoints := append([]fabricCandidateIP(nil), run.Public.CandidateIPs...)
	// P2 is the historical spelling for PCI domain 2, not a function number.
	names := map[string]string{"eth0": "rocep1s0f0", "eth1": "roceP2p1s0f1"}
	for member := range plan.Members {
		for lane := range plan.Members[member].Fabric.Lanes {
			plan.Members[member].Fabric.Lanes[lane].RDMADevice = names[plan.Members[member].Fabric.Lanes[lane].InterfaceName]
		}
	}
	for endpoint := range endpoints {
		endpoints[endpoint].RDMADevice = names[endpoints[endpoint].InterfaceName]
	}
	if err := validateVLLMGroupTransport(plan); err != nil || !vllmFabricEndpointsMatchPlan(plan, endpoints) {
		t.Fatalf("qualified RoCE auxiliary HCAs were refused: %v", err)
	}

	changed := cloneVLLMGroupPlan(plan)
	changed.Members[0].Fabric.Lanes[0].RDMADevice = "rocep2s0f0"
	if !vllmGroupHCA.MatchString(changed.Members[0].Fabric.Lanes[0].RDMADevice) || vllmFabricEndpointsMatchPlan(changed, endpoints) {
		t.Fatal("a different syntactically valid HCA bypassed the qualified endpoint binding")
	}
	for _, name := range []string{"rocep1s0f0/other", "../rocep1s0f0", "rocep1s0f0\n", "rocep1s0f0\x00", "rocep1000s0f0", "roce1s0f0", "mlx5_0/other",
		"roceP0p1s0f0", "roceP1p1s0f0", "roceP3p1s0f0", "roceP22p1s0f0", "rocePp1s0f0", "roceP2s0f0", "roceP2p1s0f0x", "rocep2P1s0f0"} {
		changed := cloneVLLMGroupPlan(plan)
		changed.Members[0].Fabric.Lanes[0].RDMADevice = name
		if err := validateVLLMGroupTransport(changed); err == nil {
			t.Fatalf("untrusted HCA name %q was admitted", name)
		}
	}
}
