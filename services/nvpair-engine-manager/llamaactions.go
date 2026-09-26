// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (e *Executor) actionLlama(ctx context.Context, st *engineState, action string, act Action, params json.RawMessage) (json.RawMessage, error) {
	if action == "cancel_pull" {
		model := modelFromParams(params)
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.pullCancel == nil || (model != "" && model != st.pullModel) {
			return nil, fmt.Errorf("no matching llama model download is active")
		}
		st.pullCancel()
		return json.RawMessage(`{"cancel_requested":true}`), nil
	}
	readOnly := action == "list_models" || action == "loaded_models" || action == "list_downloaded" || action == "get_version"
	if !readOnly {
		st.opMu.Lock()
		defer st.opMu.Unlock()
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		st.mu.Lock()
		if e.shuttingDown.Load() || st.stopPending > 0 {
			st.mu.Unlock()
			cancel()
			return nil, context.Canceled
		}
		st.mutationCancel = cancel
		st.mu.Unlock()
		defer func() { cancel(); st.mu.Lock(); st.mutationCancel = nil; st.mu.Unlock() }()
		if err := validateLlamaOwnedPaths(st.installDir); err != nil {
			return nil, err
		}
	}
	st.mu.Lock()
	running, bin, port, adopted := st.running, st.binPath, st.port, st.adopted
	st.mu.Unlock()
	if !readOnly && adopted {
		return nil, errors.New("this llama listener was started externally; stop it in its own application before managing it with PAIR")
	}
	if !readOnly && bin == "" {
		if _, err := e.Detect("llamacpp"); err != nil {
			return nil, err
		}
		st.mu.Lock()
		bin = st.binPath
		st.mu.Unlock()
	}
	if !readOnly && (!isManagedInstallPath(bin, st.installDir) || !fileExists(bin)) {
		return nil, errors.New("this llama instance is externally owned; install a managed instance to change it")
	}
	if !readOnly {
		presence := e.reconcilePresence(ctx, "llamacpp", st, true, port, false)
		st.mu.Lock()
		running, adopted = st.running, st.adopted
		st.mu.Unlock()
		if adopted || (presence.Occupied && (!presence.Identified || !running)) {
			return nil, errors.New("the llama port has an external or unidentified listener; stop it in its own application before managing it with PAIR")
		}
	}
	if !readOnly && running && !e.probe(ctx, st.plat.Runtime.Ready, port) {
		return nil, errors.New("llama identity/readiness could not be confirmed")
	}
	if act.Builtin != "llama-models" {
		result, err := e.dispatchAction(ctx, st, "llamacpp", action, act, params)
		if err == nil && (action == "load_model" || action == "unload_model") {
			want := "loaded"
			if action == "unload_model" {
				want = "unloaded"
			}
			err = e.waitLlamaModelState(ctx, st, modelFromParams(params), want)
			if err == nil {
				e.pokeLoaded()
			}
		}
		return result, err
	}
	if action == "pull_model" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		st.mu.Lock()
		st.pullCancel, st.pullModel = cancel, modelFromParams(params)
		st.mu.Unlock()
		defer func() { cancel(); st.mu.Lock(); st.pullCancel, st.pullModel = nil, ""; st.mu.Unlock() }()
	}
	if action == "delete_model" && running {
		// Unload only an observed resident model; deleting a cold model needs no
		// runtime mutation and cannot turn another cached model off.
		raw, err := e.dispatchAction(ctx, st, "llamacpp", "loaded_models", st.manifest.Actions["loaded_models"], nil)
		if err != nil {
			return nil, err
		}
		loaded, ok := extractStringsResult(raw, st.manifest.Actions["loaded_models"].Result)
		if !ok {
			return nil, errors.New("llama returned an invalid loaded-model inventory")
		}
		for _, id := range loaded {
			if id == modelFromParams(params) {
				if _, err := e.dispatchAction(ctx, st, "llamacpp", "unload_model", st.manifest.Actions["unload_model"], params); err != nil {
					return nil, err
				}
				if err := e.waitLlamaModelState(ctx, st, id, "unloaded"); err != nil {
					return nil, err
				}
			}
		}
	}
	result, err := e.llamaModelAction(ctx, st, action, params)
	if err != nil {
		return nil, err
	}
	if !readOnly && running {
		refresh := Action{HTTP: &ActionHTTP{Method: "GET", Path: "/models?reload=1"}}
		if _, err := e.dispatchAction(ctx, st, "llamacpp", "refresh_models", refresh, nil); err != nil {
			return nil, fmt.Errorf("model files changed but llama catalogue refresh failed: %w", err)
		}
	}
	if !readOnly {
		e.pokeLoaded()
	}
	return result, nil
}

func (e *Executor) waitLlamaModelState(ctx context.Context, st *engineState, model, want string) error {
	ctx, cancel := context.WithTimeout(ctx, e.actionTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := e.dispatchAction(ctx, st, "llamacpp", "list_models", st.manifest.Actions["list_models"], nil)
		if err != nil {
			return err
		}
		var response struct {
			Data []struct {
				ID     string `json:"id"`
				Status struct {
					Value    string `json:"value"`
					Failed   bool   `json:"failed"`
					ExitCode int    `json:"exit_code"`
				} `json:"status"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Data == nil {
			return errors.New("llama returned an invalid model-state response")
		}
		found := false
		for _, row := range response.Data {
			if row.ID == model {
				found = true
				if row.Status.Value == want {
					return nil
				}
				if row.Status.Failed {
					return fmt.Errorf("llama model failed to load (exit code %d)", row.Status.ExitCode)
				}
				if row.Status.Value == "" {
					return errors.New("llama model status is missing")
				}
			}
		}
		if !found {
			if want == "unloaded" {
				return nil
			}
			return errors.New("requested model is absent from the llama catalogue")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for llama model %s: %w", want, ctx.Err())
		case <-ticker.C:
		}
	}
}
