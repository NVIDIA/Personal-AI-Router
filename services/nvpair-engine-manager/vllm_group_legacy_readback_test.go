// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Generation 12 is a retained file in the pre-limits Engine Manager format.
// Its plan digest is the SHA-256 of the old struct-order wire.
const historicalVLLMGroupRun = `{"runId":"bd68a6793fe46cbac4ce347001a44add","generation":12,"planDigest":"c749a63620c6febdc0281b5afa0cc0e43679368ff671903df6c92b7870a93786","plan":{"topology":{"tensorParallel":3,"pipelineParallel":1,"dataParallel":1,"configSha256":"8eb740e8bbe4cff95ea7b4588d17a2432deb16e8075bc5828ff7ba9be94d982a"},"coordinator":"811d7911-8843-466c-b128-512fa4838895","model":"HuggingFaceTB/SmolLM2-135M-Instruct@12fd25f77366fa6b3b4b768ec3050bf629380bac","runtime":"0.28.0","members":[{"resources":{"gpu_memory_utilization":0.05,"max_model_len":2048},"placement":{"modelPath":"/home/operator/.config/Nvidia Corporation/Personal AI Router Models/vllm/2ca2f9e9fd30d75e5b9af91992f39028d6b41d995324e1e1ce69783126bd9cba","address":"192.168.0.43","apiPort":8001,"masterPort":29500},"nodeId":"811d7911-8843-466c-b128-512fa4838895","pinSha256":"29a3054e8d0eacf4377f5f4a341a95aa01d9cd971d09dd869ea7d0cedc2f7a2e","gpuUuid":"GPU-0a0a0a0a-0000-4000-8000-00000000000a","modelDigest":"58a61e7b320433a2e2eaf95386fefc88173989ad19644c94b3a84da88abde239","runtimeDigest":"6ca32069938965efdbdf48ad2da0c2049dcee93437e3e79e848bb22fc84886f4","runtimeCompatibilitySha256":"d7ec4aaf55c2a37faa1ddc1648151ed8188518ba74516d757d0af84e16d025fa"},{"resources":{"gpu_memory_utilization":0.05,"max_model_len":2048},"placement":{"modelPath":"/home/operator/.config/Nvidia Corporation/Personal AI Router Models/vllm/2ca2f9e9fd30d75e5b9af91992f39028d6b41d995324e1e1ce69783126bd9cba","address":"192.168.0.109","apiPort":8001,"masterPort":29500},"nodeId":"dd51f463-0e66-4bea-8023-699b82fde606","pinSha256":"aee01771e892660d6d30eb7d4c068eb770760b503b5e5574d77ac90282e2f03e","gpuUuid":"GPU-0b0b0b0b-0000-4000-8000-00000000000b","modelDigest":"58a61e7b320433a2e2eaf95386fefc88173989ad19644c94b3a84da88abde239","runtimeDigest":"0a216bacab94cd3d462fb9f75bd6b18073977b6037575a70b732a08053fc3f98","runtimeCompatibilitySha256":"d7ec4aaf55c2a37faa1ddc1648151ed8188518ba74516d757d0af84e16d025fa"},{"resources":{"gpu_memory_utilization":0.05,"max_model_len":2048},"placement":{"modelPath":"/home/operator/.config/Nvidia Corporation/Personal AI Router Models/vllm/2ca2f9e9fd30d75e5b9af91992f39028d6b41d995324e1e1ce69783126bd9cba","address":"192.168.0.151","apiPort":8001,"masterPort":29500},"nodeId":"d82b03d7-776f-432a-ae44-973090f1b452","pinSha256":"0de0f999798a0c5e1e75c94a93e018b212b92cf6d9f78fde09df3c357f6aa637","gpuUuid":"GPU-0c0c0c0c-0000-4000-8000-00000000000c","modelDigest":"58a61e7b320433a2e2eaf95386fefc88173989ad19644c94b3a84da88abde239","runtimeDigest":"124882856215f7ba40afbced948a9f235790de1057056f95ca63bc526edb5289","runtimeCompatibilitySha256":"d7ec4aaf55c2a37faa1ddc1648151ed8188518ba74516d757d0af84e16d025fa"}]},"state":"stopped","ranks":[{"nodeId":"811d7911-8843-466c-b128-512fa4838895","attempted":true,"started":true,"cleanupConfirmed":true},{"nodeId":"dd51f463-0e66-4bea-8023-699b82fde606","attempted":true,"started":true,"cleanupConfirmed":true},{"nodeId":"d82b03d7-776f-432a-ae44-973090f1b452","attempted":true,"started":true,"cleanupConfirmed":true}],"cleanupConfirmed":true}`

func TestVLLMGroupLegacyRetainedStatusNormalizesWithoutRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serving-group.json")
	before := []byte(historicalVLLMGroupRun)
	fixtureSHA := sha256.Sum256(before)
	if len(before) != 3040 || hex.EncodeToString(fixtureSHA[:]) != "db3b31a1a656cfcc552f861355ddc220abe11b6f7c56faaa3b9fc29ac5596935" {
		t.Fatal("literal historical receipt changed")
	}
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for restart := range 2 {
		g, err := newVLLMServingGroup(&engineState{}, path)
		if err != nil || g.reserved() {
			t.Fatalf("restart %d held exact clean history: held=%v err=%v", restart, g.reserved(), err)
		}
		got := g.status()
		want := vllmGroupLimits{RuntimeSeconds: 600, MemoryMaxBytes: 16 << 30, TasksMax: 512}
		if got.RunID != "bd68a6793fe46cbac4ce347001a44add" || got.Generation != 12 || got.PlanDigest != "c749a63620c6febdc0281b5afa0cc0e43679368ff671903df6c92b7870a93786" || got.State != "stopped" || got.Plan.Limits != want {
			t.Fatalf("restart %d status was not normalized exactly: %+v", restart, got)
		}
		published := groupStatus(g)
		if published.Reserved || published.Run == nil || published.Run.Plan.Limits != want || published.Run.PlanDigest != got.PlanDigest {
			t.Fatalf("restart %d published held or unnormalized status: %+v", restart, published)
		}
		status, err := json.Marshal(got)
		if err != nil || !json.Valid(status) || !containsJSONLimits(status, want) {
			t.Fatalf("restart %d did not publish normalized limits: %s %v", restart, status, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("restart %d rewrote retained history: %v", restart, err)
		}
	}
}

func containsJSONLimits(raw []byte, want vllmGroupLimits) bool {
	var run vllmGroupRun
	return strictDiagnosticJSON(raw, &run) == nil && run.Plan.Limits == want
}

func historicalRunObject(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(historicalVLLMGroupRun), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func historicalPlan(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	plan, ok := value["plan"].(map[string]any)
	if !ok {
		t.Fatal("historical plan missing")
	}
	return plan
}

func historicalMembers(t *testing.T, value map[string]any) []any {
	t.Helper()
	members, ok := historicalPlan(t, value)["members"].([]any)
	if !ok {
		t.Fatal("historical members missing")
	}
	return members
}

func rebindHistoricalPlanDigest(t *testing.T, value map[string]any) {
	t.Helper()
	planRaw, err := json.Marshal(historicalPlan(t, value))
	if err != nil {
		t.Fatal(err)
	}
	var plan legacyVLLMGroupPlan
	if err := strictDiagnosticJSON(planRaw, &plan); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	value["planDigest"] = hex.EncodeToString(sum[:])
}

func TestVLLMGroupLegacyRetainedRefusalsStayStrict(t *testing.T) {
	tests := map[string]func(*testing.T, map[string]any){
		"active":             func(_ *testing.T, value map[string]any) { value["state"] = "ready" },
		"run cleanup absent": func(_ *testing.T, value map[string]any) { value["cleanupConfirmed"] = false },
		"rank cleanup absent": func(_ *testing.T, value map[string]any) {
			value["ranks"].([]any)[1].(map[string]any)["cleanupConfirmed"] = false
		},
		"explicit zero limits": func(t *testing.T, value map[string]any) {
			historicalPlan(t, value)["limits"] = map[string]any{"runtimeSeconds": 0, "memoryMaxBytes": 0, "tasksMax": 0}
		},
		"explicit current limits": func(t *testing.T, value map[string]any) {
			historicalPlan(t, value)["limits"] = map[string]any{"runtimeSeconds": 600, "memoryMaxBytes": float64(16 << 30), "tasksMax": 512}
		},
		"partial limits": func(t *testing.T, value map[string]any) {
			historicalPlan(t, value)["limits"] = map[string]any{"runtimeSeconds": 600}
		},
		"null limits": func(t *testing.T, value map[string]any) { historicalPlan(t, value)["limits"] = nil },
		"other runtime": func(t *testing.T, value map[string]any) {
			historicalPlan(t, value)["runtime"] = "0.28.1"
			rebindHistoricalPlanDigest(t, value)
		},
		"qwen": func(t *testing.T, value map[string]any) {
			historicalPlan(t, value)["model"] = vllmQwen38ModelID
			rebindHistoricalPlanDigest(t, value)
		},
		"duplicate pin": func(t *testing.T, value map[string]any) {
			members := historicalMembers(t, value)
			members[1].(map[string]any)["pinSha256"] = members[0].(map[string]any)["pinSha256"]
			rebindHistoricalPlanDigest(t, value)
		},
		"case-insensitive duplicate gpu": func(t *testing.T, value map[string]any) {
			members := historicalMembers(t, value)
			members[1].(map[string]any)["gpuUuid"] = "GPU-0A0A0A0A-0000-4000-8000-00000000000A"
			rebindHistoricalPlanDigest(t, value)
		},
		"divergent model digest": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[1].(map[string]any)["modelDigest"] = "f" + historicalMembers(t, value)[1].(map[string]any)["modelDigest"].(string)[1:]
			rebindHistoricalPlanDigest(t, value)
		},
		"divergent runtime compatibility": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[1].(map[string]any)["runtimeCompatibilitySha256"] = "f" + historicalMembers(t, value)[1].(map[string]any)["runtimeCompatibilitySha256"].(string)[1:]
			rebindHistoricalPlanDigest(t, value)
		},
		"missing resources": func(t *testing.T, value map[string]any) {
			delete(historicalMembers(t, value)[0].(map[string]any), "resources")
			rebindHistoricalPlanDigest(t, value)
		},
		"invalid resources": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[0].(map[string]any)["resources"].(map[string]any)["gpu_memory_utilization"] = 2.0
			rebindHistoricalPlanDigest(t, value)
		},
		"resource gpu mismatch": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[0].(map[string]any)["resources"].(map[string]any)["gpu_uuid"] = "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			rebindHistoricalPlanDigest(t, value)
		},
		"missing placement": func(t *testing.T, value map[string]any) {
			delete(historicalMembers(t, value)[0].(map[string]any), "placement")
			rebindHistoricalPlanDigest(t, value)
		},
		"relative model path": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[0].(map[string]any)["placement"].(map[string]any)["modelPath"] = "relative/model"
			rebindHistoricalPlanDigest(t, value)
		},
		"public placement address": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[0].(map[string]any)["placement"].(map[string]any)["address"] = "8.8.8.8"
			rebindHistoricalPlanDigest(t, value)
		},
		"duplicate placement address": func(t *testing.T, value map[string]any) {
			members := historicalMembers(t, value)
			members[1].(map[string]any)["placement"].(map[string]any)["address"] = members[0].(map[string]any)["placement"].(map[string]any)["address"]
			rebindHistoricalPlanDigest(t, value)
		},
		"api port collides with master": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[0].(map[string]any)["placement"].(map[string]any)["apiPort"] = float64(vllmGroupMasterPort)
			rebindHistoricalPlanDigest(t, value)
		},
		"wrong master port": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[0].(map[string]any)["placement"].(map[string]any)["masterPort"] = float64(vllmGroupMasterPort + 1)
			rebindHistoricalPlanDigest(t, value)
		},
		"current topology field": func(t *testing.T, value map[string]any) {
			historicalPlan(t, value)["topology"].(map[string]any)["expertParallel"] = float64(0)
		},
		"current member field": func(t *testing.T, value map[string]any) {
			historicalMembers(t, value)[0].(map[string]any)["fabric"] = nil
		},
		"transport": func(t *testing.T, value map[string]any) { historicalPlan(t, value)["transport"] = nil },
		"digest mismatch": func(t *testing.T, value map[string]any) {
			historicalPlan(t, value)["coordinator"] = "dd51f463-0e66-4bea-8023-699b82fde606"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := historicalRunObject(t)
			mutate(t, value)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "serving-group.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			g, err := newVLLMServingGroup(&engineState{}, path)
			if err == nil || !g.reserved() {
				t.Fatalf("malformed retained owner was accepted: held=%v err=%v", g.reserved(), err)
			}
		})
	}
}

func TestVLLMGroupLegacyFailedHistoryCanBeRead(t *testing.T) {
	value := historicalRunObject(t)
	value["state"] = "failed"
	value["failure"] = "historical participant failed"
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := parseRetainedVLLMGroupRun(raw)
	if !ok || g.State != "failed" || g.Failure != "historical participant failed" || g.Plan.Limits != (vllmGroupLimits{RuntimeSeconds: 600, MemoryMaxBytes: 16 << 30, TasksMax: 512}) {
		t.Fatalf("clean failed history did not normalize: ok=%v run=%+v", ok, g)
	}
}

func TestVLLMGroupLegacyHeldCleanupRemainsReloadable(t *testing.T) {
	value := historicalRunObject(t)
	value["state"], value["cleanupConfirmed"], value["failure"] = "cleanup-required", false, "original held failure"
	for _, raw := range value["ranks"].([]any) {
		raw.(map[string]any)["cleanupConfirmed"] = false
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "serving-group.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := newVLLMServingGroup(&engineState{}, path)
	if err != nil || !g.reserved() || !g.run.legacyDigest {
		t.Fatalf("legacy hold=%v run=%+v err=%v", g.reserved(), g.run, err)
	}
	g.call = func(context.Context, vllmGroupBinding, string) error { return nil }
	g.cleanup(g.status(), nil)
	reloaded, err := newVLLMServingGroup(&engineState{}, path)
	if err != nil || reloaded.reserved() || !reloaded.run.CleanupConfirmed || reloaded.run.State != "failed" || reloaded.run.Failure != "original held failure" {
		t.Fatalf("reloaded legacy cleanup=%+v held=%v err=%v", reloaded.run, reloaded.reserved(), err)
	}
}

func TestVLLMGroupLegacyHeldPeerProtocolAdmitsOnlyHistoricalCleanup(t *testing.T) {
	value := historicalRunObject(t)
	value["state"], value["cleanupConfirmed"], value["failure"] = "cleanup-required", false, "original held failure"
	for _, raw := range value["ranks"].([]any) {
		raw.(map[string]any)["cleanupConfirmed"] = false
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := parseRetainedVLLMGroupRun(data)
	if !ok || !run.legacyDigest {
		t.Fatal("historical held generation did not retain its original digest")
	}
	request := vllmGroupRequest(vllmGroupBinding{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest, Plan: run.Plan, Rank: 0}, "reconcile")
	for _, action := range []string{"status", "stop", "reconcile"} {
		request.Action = action
		if err = validateVLLMGroupPeerRequest(request); err != nil {
			t.Fatalf("historical cleanup action %s rejected: %v", action, err)
		}
	}
	for _, action := range []string{"prepare", "start", "ready"} {
		request.Action = action
		if err = validateVLLMGroupPeerRequest(request); err == nil {
			t.Fatalf("historical cleanup-only generation admitted %s", action)
		}
	}
	request.Action = "reconcile"
	request.Plan.Members[0].Placement.APIPort++
	if err = validateVLLMGroupPeerRequest(request); err == nil {
		t.Fatal("changed historical cleanup plan retained authority")
	}
}

func TestVLLMGroupRestoredHistoricalRankMatchesExactCleanupBinding(t *testing.T) {
	value := historicalRunObject(t)
	value["state"], value["cleanupConfirmed"], value["failure"] = "cleanup-required", false, "original held failure"
	for _, raw := range value["ranks"].([]any) {
		raw.(map[string]any)["cleanupConfirmed"] = false
	}
	data, _ := json.Marshal(value)
	run, ok := parseRetainedVLLMGroupRun(data)
	if !ok || !run.legacyDigest {
		t.Fatal("historical cleanup fixture unavailable")
	}
	binding := vllmGroupBinding{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest, Plan: run.Plan, Rank: 1}
	rank := &vllmManagedRank{
		binding: vllmGroupBinding{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest, Rank: 1},
		receipt: vllmRankReceipt{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest, Rank: 1, State: "cleanup-required"},
	}
	if !rank.matches(binding) {
		t.Fatal("restored exact historical rank could not bind cleanup")
	}
	binding.Generation++
	if rank.matches(binding) {
		t.Fatal("changed historical generation matched restored rank")
	}
}
