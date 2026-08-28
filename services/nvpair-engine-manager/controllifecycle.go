// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// controllifecycle.go holds the non-streaming ec endpoints: remote start and
// stop. They are plain request/response operations returning the resulting
// EngineStatus; start may run for the engine's bounded readiness allowance.
// They map onto the same lifecycle the local engine:start / engine:stop use
// (including adopted-orphan reclaim and foreign-listener decline, exactly as
// local engine:stop).

import (
	"encoding/json"
	"io"
	"net/http"
)

const (
	controlStartPath = "/v1/engines/start"
	controlStopPath  = "/v1/engines/stop"
)

// startRequest is the POST /v1/engines/start body. A remote (ec) start pins the
// engine to loopback just like a local start: inference engines are never
// exposed on the LAN directly — a cluster peer reaches this node's engine only
// through the promoted proxy's pin-gated mTLS ingress, which forwards to the
// loopback backend. Binding the engine to 0.0.0.0 would re-open the very
// unauthenticated LAN surface this design removes.
type startRequest struct {
	Engine string `json:"engine"`
	Port   int    `json:"port,omitempty"`
}

// stopRequest is the POST /v1/engines/stop body.
type stopRequest struct {
	Engine string `json:"engine"`
}

func (s *controlServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req startRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Engine == "" {
		http.Error(w, `"engine" is required`, http.StatusBadRequest)
		return
	}
	if req.Port < 0 || req.Port > 65535 {
		http.Error(w, "port must be between 0 and 65535", http.StatusBadRequest)
		return
	}
	if err := s.exec.StartWith(r.Context(), req.Engine, startOpts{Port: req.Port, Bind: "127.0.0.1"}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeStatus(w, req.Engine)
}

func (s *controlServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req stopRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Engine == "" {
		http.Error(w, `"engine" is required`, http.StatusBadRequest)
		return
	}
	if err := s.exec.Stop(req.Engine); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeStatus(w, req.Engine)
}

// writeStatus responds with the engine's current EngineStatus.
func (s *controlServer) writeStatus(w http.ResponseWriter, engine string) {
	st, err := s.exec.Status(engine)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
