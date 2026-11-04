// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// modelactions.go runs the two declarative actions over a directory model
// store: scan_dir, which lists it, and launch_model, which chooses the model a
// single-model engine serves. Neither needs the engine running.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
)

// modelParam reads the "model" field of an action's params.
func modelParam(params json.RawMessage) (string, error) {
	var p struct {
		Model string `json:"model"`
	}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &p); err != nil {
			return "", fmt.Errorf("params must be a JSON object with a model: %w", err)
		}
	}
	if p.Model == "" {
		return "", fmt.Errorf("model is required")
	}
	return p.Model, nil
}

// runScanDirAction lists the store as {"models":[{"name":...}]}, the shape a
// result spec of {"array":"models","field":"name"} extracts.
func (e *Executor) runScanDirAction(st *engineState, act Action) (json.RawMessage, error) {
	root, err := resolvePlaceholders(act.ScanDir.Root, st.pathVars())
	if err != nil {
		return nil, err
	}
	type row struct {
		Name string `json:"name"`
	}
	rows := []row{}
	for _, id := range scanModelDirs(expandPath(root), act.ScanDir.Marker, act.ScanDir.depthOrDefault()) {
		rows = append(rows, row{Name: id})
	}
	return json.Marshal(map[string]any{"models": rows})
}

// resolveModelPath resolves an action path templated with {model} and checks
// it names one directory strictly inside the model store.
func (st *engineState) resolveModelPath(template, model string) (string, error) {
	if st.modelsDir == "" {
		return "", fmt.Errorf("%s has no model store", st.manifest.DisplayName)
	}
	vars := st.pathVars()
	vars["model"] = model
	resolved, err := resolvePlaceholders(template, vars)
	if err != nil {
		return "", err
	}
	path := filepath.Clean(expandPath(resolved))
	store := filepath.Clean(st.modelsDir)
	if path == store || !pathWithinRoot(store, path) {
		return "", fmt.Errorf("model %q does not resolve inside the model store", model)
	}
	return path, nil
}

// runLaunchModelAction persists the model a single-model engine serves. The
// model must already be downloaded. Pairing the action with restart_after makes
// a running engine switch to it; a stopped one serves it on its next start.
func (e *Executor) runLaunchModelAction(st *engineState, engine string, act Action, params json.RawMessage) (json.RawMessage, error) {
	model, err := modelParam(params)
	if err != nil {
		return nil, err
	}
	_, depth, _ := st.manifest.modelStoreLayout()
	if !validModelID(model, depth) {
		return nil, fmt.Errorf("%q is not a valid model name", model)
	}
	path, err := st.resolveModelPath(act.LaunchModel.Path, model)
	if err != nil {
		return nil, err
	}
	if !hasMarker(path, act.LaunchModel.Marker) {
		return nil, fmt.Errorf("model %q is not downloaded", model)
	}
	// The override file is shared with the port and launch settings, which
	// write it under opMu; take it too so the writes cannot interleave.
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if err := e.persistLaunchModel(engine, st, model); err != nil {
		return nil, err
	}
	slog.Info("launch model selected", "engine", engine, "model", model)
	return json.Marshal(map[string]string{"model": model})
}

// persistLaunchModel writes runtime.model to the engine's override and applies
// it to the in-memory runtime, so the next start serves model. The caller holds
// st.opMu. The current port is passed through unchanged.
func (e *Executor) persistLaunchModel(engine string, st *engineState, model string) error {
	st.mu.Lock()
	port := st.plat.Runtime.Port
	st.mu.Unlock()
	if err := e.persistRuntimeConfig(engine, port, nil, nil, &model); err != nil {
		return fmt.Errorf("save the launch model: %w", err)
	}
	st.mu.Lock()
	st.plat.Runtime.Model = model
	st.mu.Unlock()
	return nil
}
