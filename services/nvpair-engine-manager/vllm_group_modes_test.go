// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Synthetic bytes with the independently observed dense 135M model dimensions;
// their computed digest is not presented as the real model's config identity.
const denseLlamaModeConfig = `{"architectures":["LlamaForCausalLM"],"model_type":"llama","num_attention_heads":9,"num_key_value_heads":3,"num_hidden_layers":30,"hidden_size":576,"head_dim":null,"intermediate_size":1536,"vocab_size":49152,"hidden_act":"silu","rope_theta":100000,"rope_scaling":null,"quantization_config":null,"auto_map":null,"torch_dtype":"bfloat16"}`

func TestVLLMGroupModeDefaultsAndExplicitTopology(t *testing.T) {
	for _, tc := range []struct {
		nodes  int
		mode   string
		tp, pp int
	}{{2, "", 1, 2}, {3, "", 1, 3}, {2, "pipeline", 1, 2}, {3, "pipeline", 1, 3}} {
		selection, facts, pins := groupPlanFactsFixture(tc.nodes)
		selection.Parallelism = tc.mode
		plan, err := assembleVLLMGroupPlan(selection, facts, pins)
		if err != nil || plan.Topology.TensorParallel != tc.tp || plan.Topology.PipelineParallel != tc.pp || plan.Topology.DataParallel != 1 {
			t.Fatalf("%d/%q lost explicit/default mode: %+v %v", tc.nodes, tc.mode, plan.Topology, err)
		}
	}
	two, twoFacts, twoPins := groupPlanFactsFixture(2)
	two.Parallelism = "tensor"
	if _, err := assembleVLLMGroupPlan(two, twoFacts, twoPins); !errors.Is(err, errVLLMGroupTensorNeedsDirectFabric) {
		t.Fatal("explicit two-node TP2 was admitted without a direct fabric", err)
	}
	selection, facts, pins := groupPlanFactsFixture(3)
	selection.Parallelism = "tensor"
	if _, err := assembleVLLMGroupPlan(selection, facts, pins); err == nil || !strings.Contains(err.Error(), "attention") {
		t.Fatal("Qwen14-head/2-KV config was incorrectly admitted for TP3", err)
	}
	selection.Parallelism = "replicas"
	if validateVLLMGroupSelection(selection) == nil {
		t.Fatal("independent replicas became a sharded group topology")
	}
}

func TestVLLMGroupLlamaTP3AndPP3BindDifferentPlans(t *testing.T) {
	selection, facts, pins := groupPlanFactsFixture(3)
	selection.Model = "local:" + strings.Repeat("b", 64)
	for i := range facts {
		facts[i].Model = selection.Model
		facts[i].Config = []byte(denseLlamaModeConfig)
	}
	selection.Parallelism = "tensor"
	if _, err := assembleVLLMGroupPlan(selection, facts, pins); !errors.Is(err, errVLLMGroupTensorNeedsRingFabric) {
		t.Fatal("verified-dimension dense Llama fixture was admitted as TP3 without a routed ring", err)
	}
	facts[0].ringSocket = ringSocketTestBinding(t, selection.NodeIDs...)
	tp, err := assembleVLLMGroupPlan(selection, facts, pins)
	if err != nil || tp.Topology.TensorParallel != 3 || tp.Topology.PipelineParallel != 1 || tp.RingSocket == nil {
		t.Fatal("verified-dimension dense Llama fixture did not admit explicit TP3 over the routed ring", err)
	}
	partition, err := vllmGroupLayerPartition(tp.Topology, 3, facts[0].Config)
	if err != nil || partition != "30" {
		t.Fatal("TP3 became pipeline sharding", partition, err)
	}
	selection.Parallelism = "pipeline"
	pp, err := assembleVLLMGroupPlan(selection, facts, pins)
	if err != nil || pp.Topology.TensorParallel != 1 || pp.Topology.PipelineParallel != 3 {
		t.Fatal("PP3 became tensor sharding", err)
	}
	partition, err = vllmGroupLayerPartition(pp.Topology, 3, facts[0].Config)
	if err != nil || partition != "10,10,10" {
		t.Fatal("PP3 layer partition changed", partition, err)
	}
	tpDigest, _ := vllmGroupPlanDigest(tp)
	ppDigest, _ := vllmGroupPlanDigest(pp)
	if tpDigest == ppDigest || tp.Members[1].RuntimeDigest != facts[1].RuntimeDigest || tp.Members[1].PinSHA256 != pins[1] || tp.Members[1].ModelDigest != facts[1].ModelDigest {
		t.Fatal("mode or existing trusted member identity was lost")
	}
	if _, err := vllmGroupLayerPartition(tp.Topology, 3, append(facts[0].Config, ' ')); err == nil {
		t.Fatal("changed config bytes retained a reviewed topology")
	}
}

func TestVLLMGroupLlamaAdapterRejectsUnqualifiedVariants(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"architectures", []string{"OtherForCausalLM"}}, {"model_type", "qwen2"},
		{"quantization_config", map[string]string{"quant_method": "fp8"}},
		{"hidden_act", "gelu"}, {"head_dim", 128}, {"rope_theta", 0},
		{"rope_scaling", map[string]any{"type": "linear", "factor": 2}},
		{"rope_parameters", map[string]string{"rope_type": "llama3"}},
		{"auto_map", map[string]string{"AutoModelForCausalLM": "custom.Model"}},
		{"layer_types", []string{"sliding_attention"}}, {"is_causal", false},
		{"num_attention_heads", 8}, {"num_key_value_heads", 2}, {"hidden_size", 577}, {"intermediate_size", 1537},
		{"vocab_size", 32000}, {"vocab_size", 0},
	} {
		t.Run(tc.key, func(t *testing.T) {
			var config map[string]any
			if err := json.Unmarshal([]byte(denseLlamaModeConfig), &config); err != nil {
				t.Fatal(err)
			}
			config[tc.key] = tc.value
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(data)
			topology := vllmGroupTopology{TensorParallel: 3, PipelineParallel: 1, DataParallel: 1, ConfigSHA256: hex.EncodeToString(hash[:])}
			if _, err := vllmGroupLayerPartition(topology, 3, data); err == nil {
				t.Fatal("unqualified Llama variant was admitted")
			}
		})
	}
}
