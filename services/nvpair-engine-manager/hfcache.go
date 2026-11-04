// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	settings "nvpair-shared/enginesettings"
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
var hfShardRe = regexp.MustCompile(`-(\d+)-of-(\d+)\.(?:safetensors|bin)$`)

// runHFCacheAction lists the models in act.HFCache.Dir. It needs no running
// engine, so the list is available while the engine is stopped.
func (e *Executor) runHFCacheAction(ctx context.Context, st *engineState, engine string, act Action, params json.RawMessage) (json.RawMessage, error) {
	dir, err := resolvePlaceholders(act.HFCache.Dir, st.pathVars())
	if err != nil {
		return nil, err
	}
	if act.HFCache.Delete {
		return e.deleteHFModel(ctx, st, engine, expandPath(dir), act.HFCache.Tool, params)
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
	// vLLM loads Mistral-format consolidated weights in preference to the
	// Hugging Face-format shards whenever a snapshot has any, with their own
	// index, params.json and the Mistral tokenizer (tekken.json or
	// tokenizer.model.v*). Completeness is judged on what vLLM will load.
	mistralFormat := false
	for name := range files {
		if isConsolidatedWeights(name) {
			mistralFormat = true
		}
	}
	index := "model.safetensors.index.json"
	if mistralFormat {
		index = "consolidated.safetensors.index.json"
		if !hasAny(files, []string{"config.json", "params.json"}) || (!hasMistralTokenizer(files) && !hasAny(files, hfTokenizerFiles)) {
			return 0, false
		}
	} else if _, ok := files["config.json"]; !ok || !hasAny(files, hfTokenizerFiles) {
		return 0, false
	}

	var weights []string
	for name := range files {
		if strings.HasSuffix(name, ".safetensors") && isConsolidatedWeights(name) == mistralFormat {
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
	if len(weights) == 0 || !hfWeightsComplete(path, files, weights, index) {
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
func hfWeightsComplete(snapshot string, files map[string]int64, weights []string, index string) bool {
	if _, ok := files[index]; ok {
		raw, err := os.ReadFile(filepath.Join(snapshot, index))
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
	if total == 0 {
		return false
	}
	// Shard numbers come from file names, so count alone is not enough:
	// -00000-of-00002 and -00001-of-00002 are two files with shard 2 missing.
	for n := 1; n <= total; n++ {
		if !seen[n] {
			return false
		}
	}
	return true
}

// hasMistralTokenizer reports whether a snapshot has the tokenizer vLLM uses
// for a Mistral-format model: tekken.json or a tokenizer.model.v* file.
func hasMistralTokenizer(files map[string]int64) bool {
	for name := range files {
		if name == "tekken.json" || strings.HasPrefix(name, "tokenizer.model.v") {
			return true
		}
	}
	return false
}

func hasAny(files map[string]int64, names []string) bool {
	for _, name := range names {
		if _, ok := files[name]; ok {
			return true
		}
	}
	return false
}

// deleteHFModel removes one model from the hub cache with the engine's own
// Hugging Face tool (`hf cache rm`), so the cache's owner decides which files go:
// it keeps any file in the cache-wide shared store that another model, dataset or
// Space still links to, and takes the store's locks while it does. The model an
// engine process is serving is refused, since the engine would download it
// again on its next start. The model a stopped or ejected engine is set to serve
// is deleted and taken out of its launch arguments, so the engine has no model
// selected and will not start until one is loaded; if the removal fails, that
// selection is put back.
func (e *Executor) deleteHFModel(ctx context.Context, st *engineState, engine, hubDir, toolName string, params json.RawMessage) (json.RawMessage, error) {
	var p struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(params, &p); err != nil || !hfModelIDRe.MatchString(strings.TrimSpace(p.Model)) {
		return nil, fmt.Errorf("delete: params.model must be a Hugging Face model id such as org/name")
	}
	id := strings.TrimSpace(p.Model)
	tool, err := hfToolPath(st, toolName)
	if err != nil {
		return nil, fmt.Errorf("delete %s: %w", id, err)
	}
	served, err := e.servedModel(engine)
	if err != nil {
		return nil, fmt.Errorf("delete: cannot tell which model %s is set to serve, so nothing was deleted: %w", engine, err)
	}
	name := "models--" + strings.ReplaceAll(id, "/", "--")
	repo := filepath.Join(hubDir, name)
	if rel, err := filepath.Rel(hubDir, repo); err != nil || rel != name {
		return nil, fmt.Errorf("delete: refusing a path outside the model cache")
	}
	if _, err := os.Stat(repo); err != nil {
		return nil, fmt.Errorf("delete: %s is not in the model cache", id)
	}
	reselect := func() {}
	if served == id {
		if reselect, err = e.unselectModel(engine, id); err != nil {
			return nil, err
		}
	}
	cmd := exec.CommandContext(ctx, tool, "cache", "rm", "model/"+id, "--cache-dir", hubDir, "--yes")
	cmd.Env = append(os.Environ(), "HF_HUB_DISABLE_TELEMETRY=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		reselect()
		return nil, fmt.Errorf("delete %s: %s failed: %w: %s", id, toolName, err, lastLines(string(out), hfErrorTail))
	}
	if _, err := os.Stat(repo); err == nil {
		reselect()
		return nil, fmt.Errorf("delete %s: %s finished but the model is still in the cache", id, toolName)
	}
	return json.Marshal(map[string]any{"deleted": id})
}

// lastLines returns up to n trailing non-empty lines of out, joined with "; ".
func lastLines(out string, n int) string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// unselectModel takes the model flag out of a stopped engine's saved launch
// arguments, and returns a function that puts it back. A running engine is
// serving the model, so it is refused.
func (e *Executor) unselectModel(engine, id string) (reselect func(), err error) {
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	refused := fmt.Errorf("delete: %s is the model %s is serving; eject it or load another model first", id, engine)
	state, err := e.LaunchSettings(engine)
	if err != nil {
		return nil, err
	}
	flag := modelLaunchFlag(st.manifest)
	if state.Running || flag == "" || !state.Editable {
		return nil, refused
	}
	text, _, err := withoutLaunchFlag(state.LaunchText, flag)
	if err != nil {
		return nil, err
	}
	save := func(launchText string) error {
		_, err := e.ConfigureLaunch(context.Background(), settings.Configure{
			Engine:   engine,
			Settings: settings.Config{ServerPort: state.ServerPort, LaunchText: launchText},
		}, func() error { return nil })
		return err
	}
	if err := save(text); err != nil {
		return nil, fmt.Errorf("delete: could not clear %s from the launch settings of %s: %w", id, engine, err)
	}
	return func() { _ = save(state.LaunchText) }, nil
}

// servedModel returns the model the engine is set to serve, from its effective
// launch arguments, or "" when they name none. It fails when that cannot be
// determined, so a caller that must not touch the served model fails closed.
// The engine's binary is detected first, because the launch cannot be resolved
// before that.
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
	model, _ := launchFlagValue(tokens, "--model")
	return model, nil
}
