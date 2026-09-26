// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

type vllmSelectModelRequest struct {
	Model string `json:"model"`
}

type vllmSelectModelResult struct {
	Engine   string       `json:"engine"`
	Model    string       `json:"model"`
	Selected bool         `json:"selected"`
	Status   EngineStatus `json:"status"`
}

func selectedVLLMModel(st *engineState) (string, error) {
	selected := filepath.Join(st.modelDir, "selected")
	info, err := os.Lstat(selected)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("managed vLLM selection is not an owned symbolic link")
	}
	target, err := os.Readlink(selected)
	if err != nil || filepath.IsAbs(target) || filepath.Base(target) != target || target == "." || target == ".." {
		return "", errors.New("managed vLLM selection target is invalid")
	}
	dir := filepath.Join(st.modelDir, target)
	if err := validateVLLMOwnedPath(st.modelDir, dir); err != nil {
		return "", err
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := readVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), 8<<20, &record); err != nil {
		return "", errors.New("selected vLLM model receipt is unavailable")
	}
	want, err := vllmModelPath(st, record.ID)
	if err != nil || filepath.Clean(want) != filepath.Clean(dir) {
		return "", errors.New("selected vLLM model receipt does not own its target")
	}
	return record.ID, nil
}

func (e *Executor) SelectVLLMModel(ctx context.Context, model string) (vllmSelectModelResult, error) {
	var result vllmSelectModelResult
	if !remoteModelID(model) {
		return result, errors.New("an exact retained owner/model@revision id is required")
	}
	if err := e.rejectVLLMGroupMutation("vllm", "select a model for"); err != nil {
		return result, err
	}
	st, err := e.state("vllm")
	if err != nil {
		return result, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if err := e.rejectVLLMGroupMutation("vllm", "select a model for"); err != nil {
		return result, err
	}
	if err := rejectVLLMGroupOwnerMutation(st, "model selection"); err != nil {
		return result, err
	}
	st.mu.Lock()
	busy := st.running || st.proc != nil || st.adopted || st.stopPending != 0
	st.mu.Unlock()
	if busy || e.shuttingDown.Load() {
		return result, errors.New("stop vLLM before selecting a model")
	}
	_, dir, err := verifyVLLMModel(ctx, st, model)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(st.modelDir, 0o700); err != nil {
		return result, err
	}
	target := filepath.Base(dir)
	selected := filepath.Join(st.modelDir, "selected")
	tmp := filepath.Join(st.modelDir, ".selected-"+newOpID()+".tmp")
	if err := os.Symlink(target, tmp); err != nil {
		return result, fmt.Errorf("create managed vLLM selection: %w", err)
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, selected); err != nil {
		return result, fmt.Errorf("commit managed vLLM selection: %w", err)
	}
	if runtime.GOOS != "windows" {
		dirHandle, err := os.Open(st.modelDir)
		if err != nil {
			return result, fmt.Errorf("open managed model directory for sync: %w", err)
		}
		syncErr := dirHandle.Sync()
		closeErr := dirHandle.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return result, fmt.Errorf("sync managed model selection: %w", err)
		}
	}
	if got, err := selectedVLLMModel(st); err != nil || got != model {
		return result, errors.New("managed vLLM selection did not retain its exact model binding")
	}
	e.emitState("vllm")
	status := e.snapshot("vllm", st)
	return vllmSelectModelResult{Engine: "vllm", Model: model, Selected: true, Status: status}, nil
}
