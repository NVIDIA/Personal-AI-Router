// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"nvpair-shared/engines"
)

const (
	controlCheckUpdatesPath = "/v1/engines/check-updates"
	controlUpdatePath       = "/v1/engines/update"
)

type engineUpdateRequest struct {
	Engine string `json:"engine"`
	OpID   string `json:"opId,omitempty"`
}

func readUpdateRequest(w http.ResponseWriter, r *http.Request) (req engineUpdateRequest, ok bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return req, false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return req, false
	}
	if _, ok := engines.ByName(req.Engine); !ok {
		http.Error(w, fmt.Sprintf("unknown engine %q", req.Engine), http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func (s *controlServer) handleCheckUpdates(w http.ResponseWriter, r *http.Request) {
	req, ok := readUpdateRequest(w, r)
	if !ok {
		return
	}
	update, err := s.exec.CheckEngineUpdate(r.Context(), req.Engine)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(engineUpdateResult{UpdateAvailable: update})
}

func (s *controlServer) handleUpdate(w http.ResponseWriter, r *http.Request) {
	req, ok := readUpdateRequest(w, r)
	if !ok {
		return
	}
	s.streamOp(w, r, req.OpID, req.Engine, "update", nil, func(ctx context.Context) (streamFrame, error) {
		if err := s.exec.UpdateEngine(ctx, req.Engine); err != nil {
			return streamFrame{}, err
		}
		st, err := s.exec.Status(req.Engine)
		return streamFrame{Type: "result", OpID: req.OpID, Engine: req.Engine, Op: "update", Status: &st}, err
	})
}
