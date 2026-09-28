// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func groupPlanFactsFixture(n int) (vllmGroupSelection, []vllmGroupFacts, []string) {
	s := vllmGroupSelection{Model: "Qwen/Qwen2.5-0.5B-Instruct@" + strings.Repeat("a", 40)}
	config := []byte(`{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","num_attention_heads":14,"num_key_value_heads":2,"num_hidden_layers":24,"hidden_size":896,"intermediate_size":4864}`)
	var facts []vllmGroupFacts
	var pins []string
	for i := range n {
		node := fmt.Sprintf("node-%d", i)
		s.NodeIDs = append(s.NodeIDs, node)
		memory := 0.05
		length := int32(2048)
		facts = append(facts, vllmGroupFacts{NodeID: node, Model: s.Model, ModelDigest: strings.Repeat("b", 64), RuntimeDigest: strings.Repeat("c", 64), RuntimeVersion: vllmManagedVersion, RuntimeCompatibilitySHA256: strings.Repeat("e", 64), GPUUUID: fmt.Sprintf("GPU-%08x-0000-0000-0000-000000000001", i+1), ModelPath: "/owned/model", APIPort: 8001, Resources: vllmResourceSettings{GPUMemoryUtilization: &memory, MaxModelLen: &length}, Config: append([]byte(nil), config...), Addresses: []vllmGroupAddress{{IP: fmt.Sprintf("192.168.4.%d", i+10), Prefix: "192.168.4.0/24"}}})
		pins = append(pins, strings.Repeat(fmt.Sprint(i+1), 64))
	}
	return s, facts, pins
}

func TestVLLMGroupPlanUsesObservedFactsAndExplicitTopology(t *testing.T) {
	for _, n := range []int{2, 3} {
		s, facts, pins := groupPlanFactsFixture(n)
		plan, err := assembleVLLMGroupPlan(s, facts, pins)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Topology.TensorParallel != 1 || plan.Topology.PipelineParallel != n {
			t.Fatalf("wrong topology: %+v", plan.Topology)
		}
		if plan.Members[1].PinSHA256 != pins[1] || plan.Members[1].ModelDigest != facts[1].ModelDigest || plan.Members[1].Placement.Address != facts[1].Addresses[0].IP || plan.Members[1].Resources == nil {
			t.Fatal("trusted facts were lost")
		}
		digest, _ := vllmGroupPlanDigest(plan)
		cloned := cloneVLLMGroupPlan(plan)
		*cloned.Members[0].Resources.MaxModelLen = 4096
		cloned.Members[0].Placement.Address = "192.168.4.99"
		changed, _ := vllmGroupPlanDigest(cloned)
		if digest == changed || *plan.Members[0].Resources.MaxModelLen != 2048 || plan.Members[0].Placement.Address != "192.168.4.10" {
			t.Fatal("resource/placement binding was omitted or mutable through the review clone")
		}
	}
}

func TestVLLMGroupFactsWaitsForTransientLifecycleReaderAndDeadline(t *testing.T) {
	model := "example/model@" + strings.Repeat("a", 40)
	t.Run("release", func(t *testing.T) {
		f := vllmResourceFixture(t)
		f.st.opMu.Lock()
		result := make(chan error, 1)
		go func() {
			_, err := f.e.inspectVLLMGroupFacts(context.Background(), model, 3)
			result <- err
		}()
		select {
		case <-result:
			t.Fatal("group facts did not wait for the transient lifecycle reader")
		case <-time.After(40 * time.Millisecond):
		}
		f.st.opMu.Unlock()
		select {
		case err := <-result:
			if err == nil || strings.Contains(err.Error(), "owner is busy") {
				t.Fatalf("group facts did not advance after lifecycle release: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("group facts did not resume after lifecycle release")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		f := vllmResourceFixture(t)
		f.st.opMu.Lock()
		defer f.st.opMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		_, err := f.e.inspectVLLMGroupFacts(ctx, model, 3)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "owner is busy") {
			t.Fatalf("group facts did not fail closed at its real deadline: %v", err)
		}
	})
}

func TestVLLMGroupPlanRejectsUnavailableOrIncompatibleMemberFacts(t *testing.T) {
	for name, change := range map[string]func([]vllmGroupFacts){
		"different model":                 func(f []vllmGroupFacts) { f[1].Model = "other/model" },
		"different runtime compatibility": func(f []vllmGroupFacts) { f[1].RuntimeCompatibilitySHA256 = strings.Repeat("d", 64) },
		"different config":                func(f []vllmGroupFacts) { f[1].Config = append(f[1].Config, ' ') },
		"missing path":                    func(f []vllmGroupFacts) { f[1].ModelPath = "" },
		"different subnet": func(f []vllmGroupFacts) {
			f[1].Addresses = []vllmGroupAddress{{IP: "192.168.5.10", Prefix: "192.168.5.0/24"}}
		},
		"ambiguous address": func(f []vllmGroupFacts) {
			f[1].Addresses = append(f[1].Addresses, vllmGroupAddress{IP: "192.168.4.90", Prefix: "192.168.4.0/24"})
		},
		"missing GPU": func(f []vllmGroupFacts) { f[1].GPUUUID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s, f, p := groupPlanFactsFixture(2)
			change(f)
			if _, err := assembleVLLMGroupPlan(s, f, p); err == nil {
				t.Fatal("incompatible facts accepted")
			}
		})
	}
}

func TestVLLMGroupCompatibilityKeepsEachExactRuntimeBound(t *testing.T) {
	selection, facts, pins := groupPlanFactsFixture(2)
	facts[1].RuntimeDigest = strings.Repeat("f", 64)
	plan, err := assembleVLLMGroupPlan(selection, facts, pins)
	if err != nil || plan.Members[1].RuntimeDigest != facts[1].RuntimeDigest {
		t.Fatalf("compatible runtimes lost their distinct byte identities: %v", err)
	}
	before, _ := vllmGroupPlanDigest(plan)
	plan.Members[1].RuntimeDigest = strings.Repeat("a", 64)
	after, err := vllmGroupPlanDigest(plan)
	if err != nil || before == after {
		t.Fatal("per-node byte identity is absent from the reviewed plan digest")
	}
	plan.Members[1].RuntimeCompatibilitySHA256 = ""
	if _, err := vllmGroupPlanDigest(plan); err == nil {
		t.Fatal("unqualified legacy runtime compatibility was accepted")
	}
}

func TestVLLMGroupPlanSelectionNeverAcceptsCallerHashes(t *testing.T) {
	var request struct {
		Selection vllmGroupSelection `json:"selection"`
	}
	raw := json.RawMessage(`{"selection":{"nodeIds":["a","b"],"model":"m/n","runtimeDigest":"invented"}}`)
	if decodeActionParams(raw, &request) == nil {
		t.Fatal("selection accepted a caller-supplied runtime digest")
	}
}

func TestVLLMGroupPlanKeepsDistinctLocalPathsBound(t *testing.T) {
	for _, count := range []int{2, 3} {
		selection, facts, pins := groupPlanFactsFixture(count)
		for rank := range facts {
			facts[rank].ModelPath = fmt.Sprintf("/owned/node-%d/models/snapshot", rank)
		}
		plan, err := assembleVLLMGroupPlan(selection, facts, pins)
		if err != nil {
			t.Fatalf("%d distinct participant paths: %v", count, err)
		}
		for rank, member := range plan.Members {
			if member.Placement.ModelPath != facts[rank].ModelPath {
				t.Fatalf("rank %d lost its own verified local model path", rank)
			}
		}
		digest, err := vllmGroupPlanDigest(plan)
		if err != nil {
			t.Fatal(err)
		}
		request := vllmGroupPeerRequest{Protocol: vllmGroupPeerProtocol, Action: "prepare",
			RunID: strings.Repeat("a", 32), Generation: 1, PlanDigest: digest, Plan: cloneVLLMGroupPlan(plan), Rank: 1}
		if err := validateVLLMGroupPeerRequest(request); err != nil {
			t.Fatal(err)
		}
		request.Plan.Members[1].Placement.ModelPath = "/changed/after-review"
		if err := validateVLLMGroupPeerRequest(request); err == nil {
			t.Fatal("a changed participant-local model path reused the earlier reviewed digest")
		}
		if plan.Members[1].Placement.ModelPath != facts[1].ModelPath {
			t.Fatal("a caller mutated the retained reviewed placement")
		}
	}
}
