// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// hfCacheModel is one usable model found in a Hugging Face hub cache. Size is
// the bytes the snapshot occupies on disk.
type hfCacheModel struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// hfCacheResult is the wire shape of an hf_cache action, chosen so the ordinary
// `result: {array: "data", field: "id"}` extractor turns it into a model list.
type hfCacheResult struct {
	Data []hfCacheModel `json:"data"`
}

var hfTokenizerFiles = []string{"tokenizer.json", "tokenizer_config.json", "tokenizer.model", "vocab.json"}

// hfShardRe matches the shard names Hugging Face repos use when they carry no
// index file, such as model-00003-of-00004.safetensors.
var hfShardRe = regexp.MustCompile(`^.+-(\d+)-of-(\d+)\.(?:safetensors|bin)$`)

// runHFCacheAction lists the models in act.HFCache.Dir. It needs no running
// engine, so the list is available while the engine is stopped.
func (e *Executor) runHFCacheAction(st *engineState, engine string, act Action, params json.RawMessage) (json.RawMessage, error) {
	dir, err := resolvePlaceholders(act.HFCache.Dir, st.pathVars())
	if err != nil {
		return nil, err
	}
	if act.HFCache.Delete {
		return e.deleteHFModel(engine, expandPath(dir), params)
	}
	models, err := scanHFCache(expandPath(dir))
	if err != nil {
		return nil, err
	}
	return json.Marshal(hfCacheResult{Data: models})
}

// scanHFCache returns the models in a Hugging Face hub cache directory that
// look servable: a model repository whose latest download is complete and holds
// a config, a tokenizer and weights. Vision-only and GGUF-only repositories
// carry no tokenizer or no safetensors/pytorch weights, and interrupted
// downloads leave shards or .incomplete blobs behind, so all of those are left
// out. This is a heuristic on file layout, not a check that the engine
// supports the architecture.
//
// A missing directory is an empty cache, not an error.
func scanHFCache(dir string) ([]hfCacheModel, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []hfCacheModel{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read model cache: %w", err)
	}
	models := []hfCacheModel{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "models--") || !entry.IsDir() {
			continue
		}
		// Hugging Face stores "org/name" as "org--name" and forbids "--" inside
		// either part, so the mapping reverses cleanly.
		id := strings.ReplaceAll(strings.TrimPrefix(name, "models--"), "--", "/")
		if id == "" {
			continue
		}
		if size, ok := completeHFModel(filepath.Join(dir, name)); ok {
			models = append(models, hfCacheModel{ID: id, Size: size})
		}
	}
	sort.Slice(models, func(i, j int) bool {
		return strings.ToLower(models[i].ID) < strings.ToLower(models[j].ID)
	})
	return models, nil
}

// completeHFModel reports whether any snapshot of the repository at repoPath is
// a complete, servable model, and the size of the first such snapshot.
func completeHFModel(repoPath string) (int64, bool) {
	if blobs, err := os.ReadDir(filepath.Join(repoPath, "blobs")); err == nil {
		for _, blob := range blobs {
			if strings.HasSuffix(blob.Name(), ".incomplete") {
				return 0, false
			}
		}
	}
	snapshots, err := os.ReadDir(filepath.Join(repoPath, "snapshots"))
	if err != nil {
		return 0, false
	}
	for _, snapshot := range snapshots {
		if !snapshot.IsDir() {
			continue
		}
		if size, ok := completeHFSnapshot(filepath.Join(repoPath, "snapshots", snapshot.Name())); ok {
			return size, true
		}
	}
	return 0, false
}

func completeHFSnapshot(path string) (int64, bool) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, false
	}
	// Files are symlinks into blobs/. Stat follows them, so a link whose blob is
	// missing is not a file we have.
	files := map[string]int64{}
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(path, entry.Name()))
		if err != nil || info.IsDir() {
			continue
		}
		files[entry.Name()] = info.Size()
	}
	if _, ok := files["config.json"]; !ok || !hasAny(files, hfTokenizerFiles) {
		return 0, false
	}

	var weights []string
	for name := range files {
		if strings.HasSuffix(name, ".safetensors") {
			weights = append(weights, name)
		}
	}
	if len(weights) == 0 {
		// Repositories that ship both formats are counted once, by safetensors.
		for name := range files {
			if strings.HasPrefix(name, "pytorch_model") && strings.HasSuffix(name, ".bin") {
				weights = append(weights, name)
			}
		}
	}
	if len(weights) == 0 || !hfWeightsComplete(path, files, weights) {
		return 0, false
	}

	var size int64
	for _, bytes := range files {
		size += bytes
	}
	return size, true
}

// hfWeightsComplete verifies the weight files are all present. An index file
// lists every shard; without one, only shard names like
// model-00003-of-00004.safetensors say how many there should be, and several
// unnumbered weight files cannot be told apart from an interrupted download.
func hfWeightsComplete(snapshot string, files map[string]int64, weights []string) bool {
	if _, ok := files["model.safetensors.index.json"]; ok {
		raw, err := os.ReadFile(filepath.Join(snapshot, "model.safetensors.index.json"))
		if err != nil {
			return false
		}
		var index struct {
			WeightMap map[string]string `json:"weight_map"`
		}
		if json.Unmarshal(raw, &index) != nil || len(index.WeightMap) == 0 {
			return false
		}
		for _, shard := range index.WeightMap {
			if _, ok := files[shard]; !ok {
				return false
			}
		}
		return true
	}
	if len(weights) == 1 {
		return true
	}
	seen := map[int]bool{}
	total := 0
	for _, name := range weights {
		match := hfShardRe.FindStringSubmatch(name)
		if match == nil {
			return false
		}
		n, _ := strconv.Atoi(match[1])
		count, _ := strconv.Atoi(match[2])
		if total != 0 && count != total {
			return false
		}
		total = count
		seen[n] = true
	}
	if total == 0 || len(seen) != total {
		return false
	}
	for n := 1; n <= total; n++ {
		if !seen[n] {
			return false
		}
	}
	return true
}

func hasAny(files map[string]int64, names []string) bool {
	for _, name := range names {
		if _, ok := files[name]; ok {
			return true
		}
	}
	return false
}

// deleteHFModel removes one model's repository folder from the hub cache, and
// its download locks. The model the engine is serving, or will serve at its next
// start, is refused: deleting it would break the running engine, or make the next
// start download the model again without being asked.
func (e *Executor) deleteHFModel(engine, hubDir string, params json.RawMessage) (json.RawMessage, error) {
	var p struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(params, &p); err != nil || !hfModelIDRe.MatchString(strings.TrimSpace(p.Model)) {
		return nil, fmt.Errorf("delete: params.model must be a Hugging Face model id such as org/name")
	}
	id := strings.TrimSpace(p.Model)
	served, err := e.servedModel(engine)
	if err != nil {
		return nil, fmt.Errorf("delete: cannot tell which model %s is set to serve, so nothing was deleted: %w", engine, err)
	}
	if served == id {
		return nil, fmt.Errorf("delete: %s is the model %s is set to serve; switch to another model first", id, engine)
	}
	name := "models--" + strings.ReplaceAll(id, "/", "--")
	repo := filepath.Join(hubDir, name)
	if rel, err := filepath.Rel(hubDir, repo); err != nil || rel != name {
		return nil, fmt.Errorf("delete: refusing a path outside the model cache")
	}
	if _, err := os.Stat(repo); err != nil {
		return nil, fmt.Errorf("delete: %s is not in the model cache", id)
	}
	freed := hfBlobBytes(repo)
	if err := os.RemoveAll(repo); err != nil {
		return nil, fmt.Errorf("delete %s: %w", id, err)
	}
	_ = os.RemoveAll(filepath.Join(hubDir, ".locks", name))
	return json.Marshal(map[string]any{"deleted": id, "freed": freed})
}

// servedModel returns the model the engine is set to serve, from its effective
// launch arguments. It fails when that cannot be determined, so a caller that
// must not touch the served model fails closed. The engine's binary is detected
// first, because the launch cannot be resolved before that.
func (e *Executor) servedModel(engine string) (string, error) {
	state, err := e.LaunchSettings(engine)
	if err != nil {
		return "", err
	}
	if state.LaunchText == "" {
		if _, err := e.Detect(engine); err != nil {
			return "", err
		}
		if state, err = e.LaunchSettings(engine); err != nil {
			return "", err
		}
	}
	tokens, err := parseLaunchText(state.LaunchText)
	if err != nil {
		return "", err
	}
	model, ok := launchFlagValue(tokens, "--model")
	if !ok || model == "" {
		return "", fmt.Errorf("the launch arguments name no --model")
	}
	return model, nil
}
