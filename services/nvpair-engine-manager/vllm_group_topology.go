// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Required in the reviewed plan. Missing/old topology never silently becomes a
// new layout; the user must obtain a fresh review carrying these exact fields.
type vllmGroupTopology struct {
	TensorParallel     int    `json:"tensorParallel"`
	PipelineParallel   int    `json:"pipelineParallel"`
	DataParallel       int    `json:"dataParallel"`
	ExpertParallel     int    `json:"expertParallel,omitempty"`
	EPLB               bool   `json:"eplb,omitempty"`
	RedundantExperts   int    `json:"redundantExperts,omitempty"`
	ContextLength      int    `json:"contextLength,omitempty"`
	MaxSequences       int    `json:"maxSequences,omitempty"`
	KVCacheMemoryBytes int64  `json:"kvCacheMemoryBytes,omitempty"`
	MTP                *bool  `json:"mtp,omitempty"`
	DFlash             *bool  `json:"dflash,omitempty"`
	FlashInferAutotune *bool  `json:"flashinferAutotune,omitempty"`
	ConfigSHA256       string `json:"configSha256"`
}

func validateVLLMGroupTopology(topology vllmGroupTopology, nodes int) error {
	if !onboardingSHA.MatchString(topology.ConfigSHA256) || topology.DataParallel != 1 || topology.ExpertParallel != 0 || topology.EPLB || topology.RedundantExperts != 0 || topology.ContextLength != 0 || topology.MaxSequences != 0 || topology.KVCacheMemoryBytes != 0 || topology.MTP != nil || topology.DFlash != nil || topology.FlashInferAutotune != nil {
		return errors.New("exact model config digest and one data-parallel group are required")
	}
	if (nodes == 2 || nodes == 3) && (topology.TensorParallel == nodes && topology.PipelineParallel == 1 || topology.TensorParallel == 1 && topology.PipelineParallel == nodes) {
		return nil
	}
	return errors.New("review two or three nodes with either tensor or pipeline parallelism across all selected ranks")
}

func validateQwen38GroupTopology(topology vllmGroupTopology, nodes int) error {
	layout, err := qwen38Layout(nodes)
	if err != nil {
		return err
	}
	if !onboardingSHA.MatchString(topology.ConfigSHA256) || topology.TensorParallel != layout.TensorParallel || topology.PipelineParallel != layout.PipelineParallel || topology.DataParallel != layout.DataParallel || topology.ExpertParallel != layout.ExpertParallel || topology.EPLB != layout.EPLB || topology.RedundantExperts != layout.RedundantExperts || topology.ContextLength != qwen38Profile().ContextLength || topology.MaxSequences != qwen38Profile().MaxSequences || topology.KVCacheMemoryBytes != 8<<30 || topology.MTP == nil || *topology.MTP || topology.DFlash == nil || *topology.DFlash || topology.FlashInferAutotune == nil || *topology.FlashInferAutotune {
		return errors.New("Qwen3.8 requires the exact fixed two-node TP2+EP2 profile with max sequences 2, MTP off and DFlash off")
	}
	return nil
}

func qwen38GroupTopology(nodes int, configSHA256 string) (vllmGroupTopology, error) {
	layout, err := qwen38Layout(nodes)
	if err != nil {
		return vllmGroupTopology{}, err
	}
	off := false
	return vllmGroupTopology{TensorParallel: layout.TensorParallel, PipelineParallel: layout.PipelineParallel, DataParallel: layout.DataParallel, ExpertParallel: layout.ExpertParallel, EPLB: layout.EPLB, RedundantExperts: layout.RedundantExperts, ContextLength: qwen38Profile().ContextLength, MaxSequences: qwen38Profile().MaxSequences, KVCacheMemoryBytes: 8 << 30, MTP: &off, DFlash: &off, FlashInferAutotune: &off, ConfigSHA256: configSHA256}, nil
}

// Uses config.json bytes already read under the verified local model owner.
// It neither imports model code nor fetches weights/configuration from a URL.
func vllmGroupLayerPartition(topology vllmGroupTopology, nodes int, data []byte) (string, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return "", errors.New("bounded verified model configuration is required")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != topology.ConfigSHA256 {
		return "", errors.New("local model configuration changed after group review")
	}
	if topology.MTP != nil || topology.DFlash != nil || topology.FlashInferAutotune != nil || topology.ExpertParallel != 0 || topology.EPLB || topology.RedundantExperts != 0 || topology.ContextLength != 0 || topology.MaxSequences != 0 || topology.KVCacheMemoryBytes != 0 {
		if err := validateQwen38GroupTopology(topology, nodes); err != nil {
			return "", err
		}
		var config vllmQwen38Config
		if json.Unmarshal(data, &config) != nil {
			return "", errors.New("Qwen3.8 group requires the exact pinned model configuration")
		}
		if err := validateQwen38ConfigMetadata(config); err != nil {
			return "", err
		}
		return strconv.Itoa(config.Text.Layers), nil
	}
	if err := validateVLLMGroupTopology(topology, nodes); err != nil {
		return "", err
	}
	var config struct {
		Architectures    []string        `json:"architectures"`
		ModelType        string          `json:"model_type"`
		AttentionHeads   int             `json:"num_attention_heads"`
		KVHeads          int             `json:"num_key_value_heads"`
		Layers           int             `json:"num_hidden_layers"`
		HiddenSize       int             `json:"hidden_size"`
		IntermediateSize int             `json:"intermediate_size"`
		VocabSize        int             `json:"vocab_size"`
		Quantization     json.RawMessage `json:"quantization_config"`
		HeadDim          *int            `json:"head_dim"`
		HiddenAct        string          `json:"hidden_act"`
		RopeTheta        float64         `json:"rope_theta"`
		RopeScaling      json.RawMessage `json:"rope_scaling"`
		RopeParameters   json.RawMessage `json:"rope_parameters"`
		AutoMap          json.RawMessage `json:"auto_map"`
		LayerTypes       json.RawMessage `json:"layer_types"`
		IsCausal         *bool           `json:"is_causal"`
	}
	if json.Unmarshal(data, &config) != nil || len(config.Architectures) != 1 {
		return "", errors.New("one supported native causal language model architecture is required")
	}
	llama := config.Architectures[0] == "LlamaForCausalLM" && config.ModelType == "llama"
	qwen := config.Architectures[0] == "Qwen2ForCausalLM" && config.ModelType == "qwen2"
	if !llama && !qwen {
		return "", errors.New("this bounded adapter supports native Qwen2ForCausalLM or dense LlamaForCausalLM")
	}
	if len(config.Quantization) != 0 && string(config.Quantization) != "null" {
		return "", errors.New("quantized model topology needs separate kernel and group-size qualification")
	}
	tp, pp := topology.TensorParallel, topology.PipelineParallel
	if config.AttentionHeads <= 0 || config.KVHeads <= 0 || config.HiddenSize <= 0 || config.IntermediateSize <= 0 || config.Layers < pp || config.Layers > 65536 {
		return "", errors.New("positive complete model dimensions and at least one layer per pipeline stage are required")
	}
	if llama {
		// v0.28 llama.py uses dense SiLU MLPs, partitioned Q/KV heads and a
		// standard derived head dimension. Other RoPE/draft/custom-code variants
		// need their own qualification; this is not an architecture wildcard.
		if config.HiddenAct != "silu" || config.RopeTheta <= 0 || config.IsCausal != nil && !*config.IsCausal ||
			config.HeadDim != nil && *config.HeadDim != config.HiddenSize/config.AttentionHeads {
			return "", errors.New("bounded Llama topology requires dense causal SiLU and the standard head dimension/RoPE")
		}
		for _, variant := range []json.RawMessage{config.RopeScaling, config.RopeParameters, config.AutoMap, config.LayerTypes} {
			if len(variant) != 0 && string(variant) != "null" {
				return "", errors.New("custom-code, scaled-RoPE and per-layer Llama variants require separate qualification")
			}
		}
	}
	// Mirror pinned qwen2.py/llama.py attention partition/replication requirements, and
	// require whole linear shards. Do not infer TP3 from a three-member group.
	if config.AttentionHeads%tp != 0 || config.HiddenSize%config.AttentionHeads != 0 || config.HiddenSize%tp != 0 || config.IntermediateSize%tp != 0 || config.AttentionHeads%config.KVHeads != 0 || config.KVHeads >= tp && config.KVHeads%tp != 0 || config.KVHeads < tp && tp%config.KVHeads != 0 {
		return "", errors.New("model attention, KV heads or linear dimensions do not divide the reviewed tensor topology")
	}
	// Both pinned native implementations use the default 64-row vocabulary
	// padding before TP division. TP3 needs this additional config fact; 64-row
	// padding already divides the existing TP1/TP2 layouts. Widen before rounding.
	if tp == 3 && (config.VocabSize <= 0 || ((uint64(config.VocabSize)+63)/64*64)%uint64(tp) != 0) {
		return "", errors.New("positive model vocabulary padded to 64 rows must divide the reviewed TP3 topology")
	}
	// Pin v0.28's default uneven partition algorithm explicitly, preventing an
	// inherited VLLM_PP_LAYER_PARTITION from changing the reviewed stage layout.
	layers := make([]int, pp)
	for i := range layers {
		layers[i] = config.Layers / pp
	}
	for i := 2; i < config.Layers%pp+2; i++ {
		layers[pp-i]++
	}
	parts := make([]string, pp)
	for i, count := range layers {
		parts[i] = strconv.Itoa(count)
	}
	return strings.Join(parts, ","), nil
}
