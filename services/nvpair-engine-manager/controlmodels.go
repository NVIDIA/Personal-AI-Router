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
	controlLoadPath   = "/v1/models/load"
	controlUnloadPath = "/v1/models/unload"
	controlDeletePath = "/v1/models/delete"
)

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
