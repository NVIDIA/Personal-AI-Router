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
	"slices"
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
var hfShardRe = regexp.MustCompile(`-(\d+)-of-(\d+)\.(?:safetensors|bin)$`)

// runHFCacheAction lists the models in act.HFCache.Dir. It needs no running
// engine, so the list is available while the engine is stopped.
func (e *Executor) runHFCacheAction(ctx context.Context, st *engineState, engine string, act Action, params json.RawMessage) (json.RawMessage, error) {
	dir, err := resolvePlaceholders(act.HFCache.Dir, hfActionVars(st))
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
		if disk, _, ok := completeHFSnapshot(filepath.Join(repoPath, "snapshots", snapshot.Name())); ok {
			return disk, true
		}
	}
	return 0, false
}

// completeHFSnapshot reports whether the snapshot at path is a complete,
// servable model, the bytes it occupies on disk, and the bytes of the weights
// vLLM will load from it. The two differ when a repository ships the same
// weights in another format as well: the cache list shows what the snapshot
// takes on disk, and launch sizing uses what is loaded.
func completeHFSnapshot(path string) (disk, weightBytes int64, ok bool) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, 0, false
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
			return 0, 0, false
		}
	} else if _, ok := files["config.json"]; !ok || !hasAny(files, hfTokenizerFiles) {
		return 0, 0, false
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
		return 0, 0, false
	}

	for _, bytes := range files {
		disk += bytes
	}
	for _, name := range weights {
		weightBytes += files[name]
	}
	return disk, weightBytes, true
}

// hfWeightsComplete verifies the weight files are all present. An index file
// lists every shard; without one, only shard names like
// model-00003-of-00004.safetensors say how many there should be, and several
// unnumbered weight files cannot be told apart from an interrupted download.
// This is the same rule vLLM applies: it loads the index's shards when there is
// an index, and otherwise every weight file it finds.
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
	// One unnumbered weight file is the whole model. A file named as one shard
	// of several is not, however many files there are.
	if len(weights) == 1 && !hfShardRe.MatchString(weights[0]) {
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
// Space still links to, and takes the store's locks while it does.
//
// The model a running engine is serving is refused. That is asked of the engine
// itself, because an adopted engine may serve a model other than the one its
// saved arguments name, and the delete is refused when the engine cannot say. A
// running engine's selected model is refused too, since its next start would
// download it again. The model a stopped engine is set to serve is deleted and
// taken out of its launch arguments, so the engine has no model selected and
// will not start until one is loaded; if the removal fails, that selection is
// put back. The engine's operation lock is held from the first check to the
// rollback, so a settings save or start cannot land in between and be undone.
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
	name := "models--" + strings.ReplaceAll(id, "/", "--")
	repo := filepath.Join(hubDir, name)
	if rel, err := filepath.Rel(hubDir, repo); err != nil || rel != name {
		return nil, fmt.Errorf("delete: refusing a path outside the model cache")
	}
	if _, err := os.Stat(repo); err != nil {
		return nil, fmt.Errorf("delete: %s is not in the model cache", id)
	}
	err = e.editLaunch(engine, func(edit *launchEdit) error {
		refused := fmt.Errorf("delete: %s is the model %s is serving; eject it or load another model first", id, engine)
		if edit.before.Running {
			serving, err := e.servingModels(ctx, st, engine)
			if err != nil {
				return fmt.Errorf("delete: cannot tell which model %s is serving, so nothing was deleted: %w", engine, err)
			}
			if slices.Contains(serving, id) {
				return refused
			}
		}
		selected, err := e.selectedModelLocked(edit)
		if err != nil {
			return fmt.Errorf("delete: cannot tell which model %s is set to serve, so nothing was deleted: %w", engine, err)
		}
		unselected := false
		if selected == id {
			flag := modelLaunchFlag(st.manifest)
			if edit.before.Running || flag == "" || !edit.before.Editable {
				return refused
			}
			text, _, err := withoutLaunchFlag(edit.before.LaunchText, flag)
			if err != nil {
				return err
			}
			if err := edit.save(ctx, text, false); err != nil {
				return fmt.Errorf("delete: could not clear %s from the launch settings of %s: %w", id, engine, err)
			}
			unselected = true
		}
		failed := func(err error) error {
			if !unselected {
				return err
			}
			if restoreErr := edit.restore(ctx); restoreErr != nil {
				return fmt.Errorf("%w; putting %s back in the launch settings of %s also failed: %v", err, id, engine, restoreErr)
			}
			return err
		}
		cmd := exec.CommandContext(ctx, tool, "cache", "rm", "model/"+id, "--cache-dir", hubDir, "--yes")
		cmd.Env = os.Environ()
		for key, value := range engineHFEnv(st).toolEnv() {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return failed(fmt.Errorf("delete %s: %s failed: %w: %s", id, toolName, err, lastLines(string(out), hfErrorTail)))
		}
		if _, err := os.Stat(repo); err == nil {
			return failed(fmt.Errorf("delete %s: %s finished but the model is still in the cache", id, toolName))
		}
		return nil
	})
	if err != nil {
		return nil, err
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

// selectedModelLocked returns the model the engine's launch arguments select,
// or "" when they name none. It fails when that cannot be determined, so a
// caller that must not touch the selected model fails closed. The engine's
// binary is detected first when the launch cannot be resolved without it.
func (e *Executor) selectedModelLocked(edit *launchEdit) (string, error) {
	state := edit.before
	if state.LaunchText == "" {
		if _, err := e.Detect(edit.engine); err != nil {
			return "", err
		}
		state = e.launchStateLocked(edit.engine, edit.st)
		edit.before = state
	}
	tokens, err := parseLaunchText(state.LaunchText)
	if err != nil {
		return "", err
	}
	flag := modelLaunchFlag(edit.st.manifest)
	if flag == "" {
		return "", nil
	}
	model, _ := launchFlagValue(tokens, flag)
	return model, nil
}
