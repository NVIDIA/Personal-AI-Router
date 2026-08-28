// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// modelResolutionLMSDiskPath resolves a logical LM Studio model id (the
// OpenAI-compatible /v1/models id, an owner/repo pull key, or an on-disk path)
// to concrete files under the models cache via `lms ls --json`.
const modelResolutionLMSDiskPath = "lms-disk-path"

// lmsListEntry is one row from `lms ls --json`.
type lmsListEntry struct {
	ModelKey               string `json:"modelKey"`
	Path                   string `json:"path"`
	IndexedModelIdentifier string `json:"indexedModelIdentifier"`
	DisplayName            string `json:"displayName"`
}

func lmsEntryMatchesModel(entry lmsListEntry, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if entry.ModelKey == model {
		return true
	}
	if entry.IndexedModelIdentifier == model {
		return true
	}
	normModel := normalizeSlashPath(model)
	normPath := normalizeSlashPath(entry.Path)
	if normPath == "" {
		return false
	}
	if normPath == normModel {
		return true
	}
	if strings.HasPrefix(normPath, normModel+"/") {
		return true
	}
	return false
}

func normalizeSlashPath(path string) string {
	return strings.Trim(strings.ReplaceAll(path, "\\", "/"), "/")
}

// lmsDiskPathsToDelete returns absolute filesystem paths for every on-disk file
// that matches the requested model identifier.
func (e *Executor) lmsDiskPathsToDelete(ctx context.Context, cli, modelsDir, model string) ([]string, error) {
	cli = strings.TrimSpace(cli)
	if cli == "" {
		return nil, fmt.Errorf("lms-disk-path: runtime cli is not configured")
	}
	out, err := e.runCommandOutput(ctx, []string{expandPath(cli), "ls", "--json"})
	if err != nil {
		return nil, fmt.Errorf("lms ls --json: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var entries []lmsListEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return nil, fmt.Errorf("lms ls --json: parse: %w", err)
	}
	modelsDir = expandPath(modelsDir)
	seen := make(map[string]bool)
	var paths []string
	for _, entry := range entries {
		if !lmsEntryMatchesModel(entry, model) || strings.TrimSpace(entry.Path) == "" {
			continue
		}
		full := filepath.Join(modelsDir, filepath.FromSlash(entry.Path))
		if seen[full] {
			continue
		}
		seen[full] = true
		paths = append(paths, full)
	}
	return paths, nil
}
