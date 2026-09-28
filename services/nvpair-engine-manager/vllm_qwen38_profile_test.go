// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func qwen38MetadataFixture(t *testing.T) (vllmModel, []byte, []byte, []byte) {
	t.Helper()
	layers := map[string]vllmQwen38QuantLayer{
		"mtp.layers.0.mlp.experts":                                        {Algorithm: "FP8_PB_WO", GroupSize: 128},
		"model.language_model.layers.1.ple.ple_embedding.ngram_embedding": {Algorithm: "FP8"},
	}
	for layer := 0; layer < 48; layer++ {
		layers[fmt.Sprintf("model.language_model.layers.%d.mlp.experts", layer)] = vllmQwen38QuantLayer{Algorithm: "NVFP4", GroupSize: 16}
	}
	config, err := json.Marshal(map[string]any{
		"architectures": []string{"Qwen4ExpForConditionalGeneration"}, "model_type": "qwen4_exp",
		"text_config":         map[string]any{"hidden_size": 2560, "linear_num_key_heads": 16, "num_attention_heads": 24, "num_key_value_heads": 2, "num_experts": 512, "num_hidden_layers": 48, "mtp_num_hidden_layers": 1, "max_position_embeddings": 262144, "ple_layer_ids": []int{2}},
		"quantization_config": map[string]any{"quant_algo": "MIXED_PRECISION", "quant_method": "modelopt", "producer": map[string]string{"name": "modelopt", "version": vllmQwen38ModelOptVersion}, "quantized_layers": layers},
	})
	if err != nil {
		t.Fatal(err)
	}
	quant, err := json.Marshal(map[string]any{
		"producer":     map[string]string{"name": "modelopt", "version": vllmQwen38ModelOptVersion},
		"quantization": map[string]any{"quant_algo": "MIXED_PRECISION", "group_size": 16, "kv_cache_quant_algo": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	readme := []byte("Governing Terms: NVIDIA Open Model License\nADDITIONAL INFORMATION: Qwen Community License 1.0\nServing without MTP requires vLLM commit " + vllmQwen38MinimumCommit)
	model := vllmModel{ID: vllmQwen38ModelID, Source: "huggingface", Revision: vllmQwen38Revision, License: "nvidia-open-model-license"}
	for name, data := range map[string][]byte{"config.json": config, "hf_quant_config.json": quant, "README.md": readme, "model.safetensors.index.json": []byte("{}")} {
		sum := sha256.Sum256(data)
		model.Files = append(model.Files, vllmModelFile{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
		model.Bytes += int64(len(data))
	}
	for shard := 1; shard <= 10; shard++ {
		name := fmt.Sprintf("model-%05d-of-00010.safetensors", shard)
		sum := sha256.Sum256([]byte(name))
		model.Files = append(model.Files, vllmModelFile{Path: name, Size: 12_000_000_000, SHA256: hex.EncodeToString(sum[:])})
	}
	name := "model-fp8-mtp-ple.safetensors"
	sum := sha256.Sum256([]byte(name))
	model.Files = append(model.Files, vllmModelFile{Path: name, Size: vllmQwen38WeightBytes - 120_000_000_000, SHA256: hex.EncodeToString(sum[:])})
	model.Bytes += vllmQwen38WeightBytes
	remaining := int64(vllmQwen38SnapshotBytes) - model.Bytes
	for index := 0; index < 9; index++ {
		name := fmt.Sprintf("fixture-metadata-%02d.json", index)
		size := int64(1)
		if index == 8 {
			size = remaining - 8
		}
		sum := sha256.Sum256([]byte(name))
		model.Files = append(model.Files, vllmModelFile{Path: name, Size: size, SHA256: hex.EncodeToString(sum[:])})
		model.Bytes += size
	}
	return model, config, quant, readme
}

func TestQwen38ProfileValidatesPinnedOfflineMetadata(t *testing.T) {
	model, config, quant, readme := qwen38MetadataFixture(t)
	if err := validateQwen38PinnedMetadata(model, config, quant, readme); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*vllmModel, *[]byte, *[]byte, *[]byte){
		"revision": func(m *vllmModel, _, _, _ *[]byte) { m.Revision = strings.Repeat("0", 40) },
		"license":  func(m *vllmModel, _, _, _ *[]byte) { m.License = "unknown" },
		"architecture": func(_ *vllmModel, c, _, _ *[]byte) {
			*c = []byte(strings.Replace(string(*c), "Qwen4ExpForConditionalGeneration", "Qwen2ForCausalLM", 1))
		},
		"modelopt": func(_ *vllmModel, _, q, _ *[]byte) {
			*q = []byte(strings.Replace(string(*q), vllmQwen38ModelOptVersion, "other", 1))
		},
		"weight bytes": func(m *vllmModel, _, _, _ *[]byte) {
			for index := range m.Files {
				if m.Files[index].Path == "model-fp8-mtp-ple.safetensors" {
					m.Files[index].Size++
					return
				}
			}
		},
		"extra metadata": func(m *vllmModel, _, _, _ *[]byte) {
			sum := sha256.Sum256([]byte("extra.json"))
			m.Files = append(m.Files, vllmModelFile{Path: "extra.json", Size: 1, SHA256: hex.EncodeToString(sum[:])})
			m.Bytes++
		},
		"license evidence": func(_ *vllmModel, _, _, r *[]byte) {
			*r = []byte("incomplete model card")
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, candidateConfig, candidateQuant, candidateReadme := qwen38MetadataFixture(t)
			change(&candidate, &candidateConfig, &candidateQuant, &candidateReadme)
			if validateQwen38PinnedMetadata(candidate, candidateConfig, candidateQuant, candidateReadme) == nil {
				t.Fatal("changed pinned metadata accepted")
			}
		})
	}
}

func TestQwen38ProfileReadsOnlyHashBoundLocalMetadata(t *testing.T) {
	model, config, quant, readme := qwen38MetadataFixture(t)
	dir := t.TempDir()
	artifacts := map[string][]byte{"config.json": config, "hf_quant_config.json": quant, "README.md": readme}
	for name, data := range artifacts {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		for index := range model.Files {
			if model.Files[index].Path == name {
				sum := sha256.Sum256(data)
				model.Files[index].Size = int64(len(data))
				model.Files[index].SHA256 = hex.EncodeToString(sum[:])
			}
		}
	}
	if err := validateQwen38LocalProfile(model, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), append(readme, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	if validateQwen38LocalProfile(model, dir) == nil {
		t.Fatal("changed retained metadata accepted")
	}
}
