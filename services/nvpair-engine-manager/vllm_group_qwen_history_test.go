// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func capturedGeneration55Plan() vllmGroupPlan {
	type memberFixture struct {
		node, pin, gpu, runtime, address, modelPath string
		lanes                                       []vllmGroupRDMALane
	}
	lane := func(peer, local, remote, iface, mac, switchID, port, device string, index int) vllmGroupRDMALane {
		return vllmGroupRDMALane{PeerNodeID: peer, LocalAddress: local, PeerAddress: remote, InterfaceName: iface, InterfaceIndex: index, MAC: mac, SwitchID: switchID, PortName: port, RDMADevice: device, GIDPort: 1, GIDIndex: 3, GIDType: "RoCE v2"}
	}
	fixtures := []memberFixture{
		{
			"811d7911-8843-466c-b128-512fa4838895", "29a3054e8d0eacf4377f5f4a341a95aa01d9cd971d09dd869ea7d0cedc2f7a2e", "GPU-0a0a0a0a-0000-4000-8000-00000000000a", "b81594e5e4ee8fdc1e775337217c9709fd7be7c77bde30ffe94fcb1f5c545e0e", "192.168.0.43", "/home/operator/.config/Nvidia Corporation/Personal AI Router Models/vllm/3be3cf17ed2b252f71ac10229920ecfa1627aa8a832ddac6d5412276f3bdcceb",
			[]vllmGroupRDMALane{
				lane("dd51f463-0e66-4bea-8023-699b82fde606", "10.253.0.0", "10.253.0.1", "enp1s0f0np0", "02:00:00:0a:00:00", "00000a0003000002", "p0", "rocep1s0f0", 3),
				lane("d82b03d7-776f-432a-ae44-973090f1b452", "10.253.0.2", "10.253.0.3", "enp1s0f1np1", "02:00:00:0a:00:01", "00000a0003000002", "p1", "rocep1s0f1", 4),
			},
		},
		{
			"dd51f463-0e66-4bea-8023-699b82fde606", "aee01771e892660d6d30eb7d4c068eb770760b503b5e5574d77ac90282e2f03e", "GPU-0b0b0b0b-0000-4000-8000-00000000000b", "3b5e6462f119605b10a443012eb63fdf6ad2746f7ac2217779bb87a4f761e1f9", "192.168.0.109", "/home/operator/.config/Nvidia Corporation/Personal AI Router Models/vllm/3be3cf17ed2b252f71ac10229920ecfa1627aa8a832ddac6d5412276f3bdcceb",
			[]vllmGroupRDMALane{
				lane("811d7911-8843-466c-b128-512fa4838895", "10.253.0.1", "10.253.0.0", "enp1s0f1np1", "02:00:00:0b:00:01", "00000b0003000002", "p1", "rocep1s0f1", 4),
				lane("d82b03d7-776f-432a-ae44-973090f1b452", "10.253.0.4", "10.253.0.5", "enp1s0f0np0", "02:00:00:0b:00:00", "00000b0003000002", "p0", "rocep1s0f0", 3),
			},
		},
		{
			"d82b03d7-776f-432a-ae44-973090f1b452", "0de0f999798a0c5e1e75c94a93e018b212b92cf6d9f78fde09df3c357f6aa637", "GPU-0c0c0c0c-0000-4000-8000-00000000000c", "3511aa29d375a37a0266a17091d5e9c0cbae93dceacea185ea4afef01915f743", "192.168.0.151", "/home/operator/.config/Nvidia Corporation/Personal AI Router Models/vllm/3be3cf17ed2b252f71ac10229920ecfa1627aa8a832ddac6d5412276f3bdcceb",
			[]vllmGroupRDMALane{
				lane("811d7911-8843-466c-b128-512fa4838895", "10.253.0.3", "10.253.0.2", "enp1s0f0np0", "02:00:00:0c:00:00", "00000c0003000002", "p0", "rocep1s0f0", 3),
				lane("dd51f463-0e66-4bea-8023-699b82fde606", "10.253.0.5", "10.253.0.4", "enp1s0f1np1", "02:00:00:0c:00:01", "00000c0003000002", "p1", "rocep1s0f1", 4),
			},
		},
	}
	memory, length := 0.05, int32(2048)
	plan := vllmGroupPlan{
		Topology:  vllmGroupTopology{TensorParallel: 1, PipelineParallel: 1, DataParallel: 3, ExpertParallel: 3, EPLB: true, RedundantExperts: 1, ContextLength: 32768, MaxSequences: 2, KVCacheMemoryBytes: 8 << 30, MTP: vllmTestBool(false), DFlash: vllmTestBool(false), FlashInferAutotune: vllmTestBool(false), ConfigSHA256: "deef67a61f3311faf051b23dc4192f442c7fee4f9cd2f38cbcbe4da55c763a80"},
		Transport: &vllmGroupTransport{Mode: vllmQwen38Transport, OperationID: "2a856bb839a5ef1b0446fb04fbf92425", QualificationSHA256: "099de6f8775872725c7d333f45570cee8f0f6552b21a141cfba225d26cf71f12", NetPlugin: "none", EnvPlugin: "none", GINPlugin: "none"},
		Limits:    vllmGroupLimitsForModel(vllmQwen38ModelID), Coordinator: fixtures[0].node, Model: vllmQwen38ModelID, Runtime: vllmQwen38Runtime,
	}
	for _, fixture := range fixtures {
		resources := vllmResourceSettings{GPUMemoryUtilization: &memory, MaxModelLen: &length}
		plan.Members = append(plan.Members, vllmGroupMember{
			Resources: &resources, Placement: &vllmGroupPlacement{ModelPath: fixture.modelPath, Address: fixture.address, APIPort: 8001, MasterPort: 29500},
			NodeID: fixture.node, PinSHA256: fixture.pin, GPUUUID: fixture.gpu, ModelDigest: "40ec17f214bdc2b09125b84c912c66d212722ef868275568ebccc7d960295802", RuntimeDigest: fixture.runtime, RuntimeCompatibilitySHA256: "0862061480e566c8e9978180f80bac2e712d7c1fcc3d8c7be1fe118402cd2952", Fabric: &vllmGroupMemberFabric{Lanes: fixture.lanes},
		})
	}
	return plan
}

// Generation 55 ran the three-node layout earlier recipes sealed. Qwen3.8 now
// serves only on two Sparks, so the owner holds that journal for recovery,
// byte for byte, instead of reloading it.
func TestCapturedGeneration55ThreeNodeQwenHistoryIsHeldForRecovery(t *testing.T) {
	plan := capturedGeneration55Plan()
	if _, err := historicalQwenGroupPlanDigest(plan); !errors.Is(err, errQwen38TwoSparks) {
		t.Fatalf("three-node historical digest err = %v, want the two-Spark reason", err)
	}
	ranks := make([]vllmGroupRank, len(plan.Members))
	for i, member := range plan.Members {
		ranks[i] = vllmGroupRank{NodeID: member.NodeID, Attempted: true, Started: true, CleanupConfirmed: true}
	}
	run := vllmGroupRun{RunID: "31bbcb4ebd98ef585019a066ff0c13b5", Generation: 55, PlanDigest: "dc157158b72373d2ac108600a414af84cd0c29ab3eeaa59032c00f60c33688ad", Plan: plan, State: "failed", Ranks: ranks, CleanupConfirmed: true, Failure: "a participant action failed; all owned ranks require cleanup"}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), vllmGroupJournalFile)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	group, err := newVLLMServingGroup(&engineState{}, path)
	if err == nil || !group.reserved() {
		t.Fatalf("three-node Qwen3.8 history reloaded: held=%v err=%v", group.reserved(), err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("held history was rewritten: %v", err)
	}
}
