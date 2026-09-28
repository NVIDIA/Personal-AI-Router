// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	vllmQwen38Repository             = "nvidia/Qwen3.8-Flash-Next-NVFP4"
	vllmQwen38Revision               = "fc694b54fb0174e0913e6adf86691ef85a4ead47"
	vllmQwen38ModelID                = vllmQwen38Repository + "@" + vllmQwen38Revision
	vllmQwen38MinimumCommit          = "d4d703caf908786416585ceb1f369e2e0363358b"
	vllmQwen38ModelOptVersion        = "0.46.0.dev281+g73d778422"
	vllmQwen38WeightBytes     int64  = 132680249378
	vllmQwen38ShardIndexBytes int64  = 31_275_518
	vllmQwen38SnapshotBytes   uint64 = 132734505212
	vllmQwen38SnapshotFiles          = 24
)

type vllmQwen38Layout struct {
	Label                                                                                   string
	Nodes, TensorParallel, PipelineParallel, DataParallel, ExpertParallel, RedundantExperts int
	EPLB                                                                                    bool
}

type vllmQwen38Profile struct {
	ModelID, Architecture, Quantization, MinimumCommit string
	ContextLength, MinSequences, MaxSequences          int
	MTP, DFlash                                        bool
	Layouts                                            []vllmQwen38Layout
}

func qwen38Profile() vllmQwen38Profile {
	return vllmQwen38Profile{
		ModelID: vllmQwen38ModelID, Architecture: "Qwen4ExpForConditionalGeneration",
		Quantization: "ModelOpt NVFP4", MinimumCommit: vllmQwen38MinimumCommit,
		ContextLength: 32768, MinSequences: 1, MaxSequences: 2,
		Layouts: []vllmQwen38Layout{
			{Label: "TP2+EP2", Nodes: 2, TensorParallel: 2, PipelineParallel: 1, DataParallel: 1, ExpertParallel: 2},
		},
	}
}

var errQwen38TwoSparks = errors.New("Qwen3.8 runs on exactly two Sparks: its two key-value heads cannot split three ways, a full copy of its shared weights on each of three ranks does not fit in Spark memory, and this runtime cannot run it as pipeline stages")

// qwen38RetiredLayout is the three-node layout recipes v1 and v2 sealed. It
// copies the shared weights to every rank and cannot fit a Spark; it is
// recognized only to validate those recipes' ownership receipts.
func qwen38RetiredLayout(nodes int) (vllmQwen38Layout, bool) {
	retired := vllmQwen38Layout{Label: "TP1xDP3+EP3+EPLB", Nodes: 3, TensorParallel: 1, PipelineParallel: 1, DataParallel: 3, ExpertParallel: 3, EPLB: true, RedundantExperts: 1}
	return retired, nodes == retired.Nodes
}

func isQwen38ProfileModel(model string) bool { return model == vllmQwen38ModelID }

func validateQwen38SnapshotPlan(model string, plan vllmSnapshotPlan) error {
	if !isQwen38ProfileModel(model) {
		return nil
	}
	if plan.Files != vllmQwen38SnapshotFiles || plan.Bytes != vllmQwen38SnapshotBytes {
		return errors.New("Qwen3.8 profile requires the pinned 24-file, 132734505212-byte snapshot")
	}
	return nil
}

func qwen38Layout(nodes int) (vllmQwen38Layout, error) {
	for _, layout := range qwen38Profile().Layouts {
		if layout.Nodes == nodes {
			return layout, nil
		}
	}
	return vllmQwen38Layout{}, errQwen38TwoSparks
}

type vllmQwen38QuantLayer struct {
	Algorithm string `json:"quant_algo"`
	GroupSize int    `json:"group_size"`
}

type vllmQwen38Config struct {
	Architectures []string `json:"architectures"`
	ModelType     string   `json:"model_type"`
	Text          struct {
		HiddenSize            int   `json:"hidden_size"`
		LinearKeyHeads        int   `json:"linear_num_key_heads"`
		AttentionHeads        int   `json:"num_attention_heads"`
		KeyValueHeads         int   `json:"num_key_value_heads"`
		Experts               int   `json:"num_experts"`
		Layers                int   `json:"num_hidden_layers"`
		MTPHiddenLayers       int   `json:"mtp_num_hidden_layers"`
		MaxPositionEmbeddings int   `json:"max_position_embeddings"`
		PLELayerIDs           []int `json:"ple_layer_ids"`
	} `json:"text_config"`
	Quantization struct {
		Algorithm string `json:"quant_algo"`
		Method    string `json:"quant_method"`
		Producer  struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"producer"`
		Layers map[string]vllmQwen38QuantLayer `json:"quantized_layers"`
	} `json:"quantization_config"`
}

func validateQwen38ConfigMetadata(config vllmQwen38Config) error {
	if len(config.Architectures) != 1 || config.Architectures[0] != "Qwen4ExpForConditionalGeneration" || config.ModelType != "qwen4_exp" {
		return errors.New("Qwen3.8 profile requires Qwen4ExpForConditionalGeneration metadata")
	}
	if config.Text.HiddenSize != 2560 || config.Text.LinearKeyHeads != 16 || config.Text.AttentionHeads != 24 ||
		config.Text.KeyValueHeads != 2 || config.Text.Experts != 512 || config.Text.Layers != 48 ||
		config.Text.MTPHiddenLayers != 1 || config.Text.MaxPositionEmbeddings != 262144 ||
		len(config.Text.PLELayerIDs) != 1 || config.Text.PLELayerIDs[0] != 2 {
		return errors.New("Qwen3.8 architecture dimensions differ from the pinned profile")
	}
	if config.Quantization.Method != "modelopt" || config.Quantization.Algorithm != "MIXED_PRECISION" ||
		config.Quantization.Producer.Name != "modelopt" || config.Quantization.Producer.Version != vllmQwen38ModelOptVersion {
		return errors.New("Qwen3.8 profile requires the pinned ModelOpt mixed-precision metadata")
	}
	for layer := 0; layer < 48; layer++ {
		entry := config.Quantization.Layers[fmt.Sprintf("model.language_model.layers.%d.mlp.experts", layer)]
		if entry.Algorithm != "NVFP4" || entry.GroupSize != 16 {
			return errors.New("Qwen3.8 profile requires NVFP4 group-16 metadata for every routed expert layer")
		}
	}
	mtp := config.Quantization.Layers["mtp.layers.0.mlp.experts"]
	ple := config.Quantization.Layers["model.language_model.layers.1.ple.ple_embedding.ngram_embedding"]
	if mtp.Algorithm != "FP8_PB_WO" || mtp.GroupSize != 128 || ple.Algorithm != "FP8" {
		return errors.New("Qwen3.8 profile requires the pinned FP8 MTP and PLE metadata even while MTP speculative decoding remains disabled")
	}
	return nil
}

func validateQwen38PinnedMetadata(model vllmModel, configData, quantData, readme []byte) error {
	if model.ID != vllmQwen38ModelID || model.Source != "huggingface" || model.Revision != vllmQwen38Revision ||
		model.License != "nvidia-open-model-license" || len(model.Files) != vllmQwen38SnapshotFiles ||
		model.Bytes != int64(vllmQwen38SnapshotBytes) {
		return errors.New("Qwen3.8 profile requires the exact pinned Hugging Face snapshot and license")
	}
	files := make(map[string]vllmModelFile, len(model.Files))
	var artifactBytes int64
	for _, file := range model.Files {
		if _, exists := files[file.Path]; exists || file.Size <= 0 || !onboardingSHA.MatchString(file.SHA256) {
			return errors.New("Qwen3.8 profile requires one valid record per retained artifact")
		}
		files[file.Path] = file
		artifactBytes += file.Size
	}
	if artifactBytes != model.Bytes {
		return errors.New("Qwen3.8 profile artifact inventory byte count changed")
	}
	for _, name := range []string{"config.json", "hf_quant_config.json", "README.md", "model.safetensors.index.json"} {
		if _, ok := files[name]; !ok {
			return fmt.Errorf("Qwen3.8 profile is missing pinned artifact %s", name)
		}
	}
	wantWeights := map[string]bool{"model-fp8-mtp-ple.safetensors": true}
	for shard := 1; shard <= 10; shard++ {
		wantWeights[fmt.Sprintf("model-%05d-of-00010.safetensors", shard)] = true
	}
	var weightBytes int64
	weightCount := 0
	for name, file := range files {
		if filepath.Ext(name) != ".safetensors" {
			continue
		}
		if !wantWeights[name] {
			return fmt.Errorf("Qwen3.8 profile has unexpected weight artifact %s", name)
		}
		delete(wantWeights, name)
		weightCount++
		weightBytes += file.Size
	}
	if len(wantWeights) != 0 || weightCount != 11 || weightBytes != vllmQwen38WeightBytes {
		return errors.New("Qwen3.8 profile requires the pinned 11-file weight set")
	}
	var config vllmQwen38Config
	if json.Unmarshal(configData, &config) != nil {
		return errors.New("Qwen3.8 profile requires Qwen4ExpForConditionalGeneration metadata")
	}
	if err := validateQwen38ConfigMetadata(config); err != nil {
		return err
	}
	var quant struct {
		Producer struct {
			Name, Version string
		} `json:"producer"`
		Quantization struct {
			Algorithm string          `json:"quant_algo"`
			GroupSize int             `json:"group_size"`
			KVCache   json.RawMessage `json:"kv_cache_quant_algo"`
		} `json:"quantization"`
	}
	if json.Unmarshal(quantData, &quant) != nil || quant.Producer.Name != "modelopt" ||
		quant.Producer.Version != vllmQwen38ModelOptVersion || quant.Quantization.Algorithm != "MIXED_PRECISION" ||
		quant.Quantization.GroupSize != 16 || string(quant.Quantization.KVCache) != "null" {
		return errors.New("Qwen3.8 hf_quant_config.json differs from the pinned ModelOpt recipe")
	}
	for _, phrase := range [][]byte{[]byte("NVIDIA Open Model License"), []byte("Qwen Community License 1.0"), []byte(vllmQwen38MinimumCommit)} {
		if !bytes.Contains(readme, phrase) {
			return errors.New("Qwen3.8 README lacks pinned license or minimum-runtime evidence")
		}
	}
	return nil
}

func readQwen38ProfileArtifact(model vllmModel, dir, name string, limit int64) ([]byte, error) {
	var expected *vllmModelFile
	for index := range model.Files {
		if model.Files[index].Path == name {
			if expected != nil {
				return nil, errors.New("duplicate Qwen3.8 profile artifact")
			}
			expected = &model.Files[index]
		}
	}
	if expected == nil || expected.Size <= 0 || expected.Size > limit || !onboardingSHA.MatchString(expected.SHA256) {
		return nil, fmt.Errorf("invalid Qwen3.8 profile artifact %s", name)
	}
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := validateVLLMOwnedPath(dir, path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) != expected.Size || int64(len(data)) > limit {
		return nil, fmt.Errorf("Qwen3.8 profile artifact %s changed", name)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected.SHA256 {
		return nil, fmt.Errorf("Qwen3.8 profile artifact %s changed", name)
	}
	return data, nil
}

func validateQwen38LocalProfile(model vllmModel, dir string) error {
	config, err := readQwen38ProfileArtifact(model, dir, "config.json", 1<<20)
	if err != nil {
		return err
	}
	quant, err := readQwen38ProfileArtifact(model, dir, "hf_quant_config.json", 1<<20)
	if err != nil {
		return err
	}
	readme, err := readQwen38ProfileArtifact(model, dir, "README.md", 1<<20)
	if err != nil {
		return err
	}
	return validateQwen38PinnedMetadata(model, config, quant, readme)
}
