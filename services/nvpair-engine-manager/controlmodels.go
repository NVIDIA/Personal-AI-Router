// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// controlmodels.go holds fast ec endpoints for remote model load, unload, and delete.
// They mirror the local engine:action mapping in modelops.go and return the
// action result as JSON.

import (
	"encoding/json"
	"io"
	"net/http"
)

const (
	controlLoadPath       = "/v1/models/load"
	controlUnloadPath     = "/v1/models/unload"
	controlDeletePath     = "/v1/models/delete"
	controlCancelPullPath = "/v1/models/cancel-pull"
)

type cancelPullRequest struct {
	Engine      string `json:"engine"`
	Model       string `json:"model"`
	OperationID string `json:"operationId"`
}

// handleCancelPull cancels an in-flight pull. vLLM pulls are bound to an exact
// immutable model and operation ID, so a vLLM cancel must name both; other
// engines cancel by engine and model.
func (s *controlServer) handleCancelPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req cancelPullRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxControlBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Engine == "" {
		http.Error(w, `"engine" is required`, http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		http.Error(w, `"model" is required`, http.StatusBadRequest)
		return
	}
	params, _ := json.Marshal(map[string]string{"model": req.Model})
	if req.Engine == "vllm" {
		if _, _, modelErr := parseExactVLLMHFModel(req.Model); modelErr != nil || !vllmPullOperationToken.MatchString(req.OperationID) {
			http.Error(w, "cancel requires vllm, the exact model and operationId", http.StatusBadRequest)
			return
		}
		params, _ = json.Marshal(vllmPullParams{Model: req.Model, OperationID: req.OperationID})
	}
	result, err := s.exec.Action(r.Context(), req.Engine, "cancel_pull", params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result)
}

func (s *controlServer) handleLoad(w http.ResponseWriter, r *http.Request) {
	s.handleModelAction(w, r, "load")
}

func (s *controlServer) handleUnload(w http.ResponseWriter, r *http.Request) {
	s.handleModelAction(w, r, "unload")
}

func (s *controlServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.handleModelAction(w, r, "delete")
}

func (s *controlServer) handleModelAction(w http.ResponseWriter, r *http.Request, op string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req modelActionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Engine == "" {
		http.Error(w, `"engine" is required`, http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		http.Error(w, `"model" is required`, http.StatusBadRequest)
		return
	}
	var (
		res json.RawMessage
		err error
	)
	switch op {
	case "load":
		res, err = s.exec.ModelLoad(r.Context(), req.Engine, req.Model)
	case "unload":
		res, err = s.exec.ModelUnload(r.Context(), req.Engine, req.Model)
	case "delete":
		res, err = s.exec.ModelDelete(r.Context(), req.Engine, req.Model)
	default:
		http.Error(w, "unknown model op", http.StatusInternalServerError)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if len(res) == 0 {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	_, _ = w.Write(res)
}
