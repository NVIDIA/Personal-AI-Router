// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// controlstream.go holds the long-running ec endpoints that stream live
// progress: remote install and remote model pull. Both return a chunked
// application/x-ndjson body — zero or more {"type":"progress",...} frames as the
// operation advances, terminated by exactly one {"type":"result",...} or
// {"type":"error",...} frame. The initiating node relays each progress frame up
// to its UI as engine:remote-progress and settles the RPC on the terminal frame.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// controlInstallPath streams a remote engine install.
const controlInstallPath = "/v1/engines/install"

// controlPullPath streams a remote model pull.
const controlPullPath = "/v1/models/pull"

// maxControlBody caps a request body on the ec surface. Requests are small
// JSON control messages; the large payloads are downloads the target fetches
// itself, never carried over this wire.
const maxControlBody = 1 << 20 // 1 MiB

// streamFrame is one NDJSON frame on the ec streaming endpoints. Type is
// "progress", "result", or "error". Percent is omitted when indeterminate (0).
type streamFrame struct {
	Type    string          `json:"type"`
	OpID    string          `json:"opId,omitempty"`
	Engine  string          `json:"engine,omitempty"`
	Op      string          `json:"op,omitempty"`
	Stage   string          `json:"stage,omitempty"`
	Percent int             `json:"percent,omitempty"`
	Message string          `json:"message,omitempty"`
	Status  *EngineStatus   `json:"status,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// installRequest is the POST /v1/engines/install body.
type installRequest struct {
	OpID   string `json:"opId"`
	Engine string `json:"engine"`
	Start  bool   `json:"start"`
}

// pullRequest is the POST /v1/models/pull body. Params, when set, is passed
// through to the manifest pull_model action verbatim (same as engine:action);
// otherwise Model is used to build a default body.
type pullRequest struct {
	OpID   string          `json:"opId"`
	Engine string          `json:"engine"`
	Model  string          `json:"model"`
	Params json.RawMessage `json:"params,omitempty"`
}

// handleInstall runs an install (optionally starting the engine after) and
// streams its progress, ending with the resulting EngineStatus.
func (s *controlServer) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req installRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Engine == "" {
		http.Error(w, `"engine" is required`, http.StatusBadRequest)
		return
	}
	s.streamOp(w, r, req.OpID, req.Engine, "install", func(ctx context.Context) (streamFrame, error) {
		if err := s.exec.Install(ctx, req.Engine); err != nil {
			return streamFrame{}, err
		}
		if req.Start {
			if err := s.exec.StartWith(ctx, req.Engine, startOpts{}); err != nil {
				return streamFrame{}, err
			}
		}
		st, err := s.exec.Status(req.Engine)
		if err != nil {
			return streamFrame{}, err
		}
		return streamFrame{Type: "result", OpID: req.OpID, Engine: req.Engine, Op: "install", Status: &st}, nil
	})
}

// handlePull runs a model pull and streams its download progress, ending with
// the pull_model action's final result.
func (s *controlServer) handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req pullRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Engine == "" {
		http.Error(w, `"engine" is required`, http.StatusBadRequest)
		return
	}
	if req.Model == "" && len(req.Params) == 0 {
		http.Error(w, `"model" or "params" is required`, http.StatusBadRequest)
		return
	}
	s.streamOp(w, r, req.OpID, req.Engine, "pull", func(ctx context.Context) (streamFrame, error) {
		res, err := s.exec.PullModelStream(ctx, req.Engine, req.Model, req.Params)
		if err != nil {
			model := req.Model
			if model == "" {
				model = modelFromParams(req.Params)
			}
			msg := s.exec.reportPullFailed(req.Engine, model, err)
			return streamFrame{}, fmt.Errorf("%s", msg)
		}
		return streamFrame{Type: "result", OpID: req.OpID, Engine: req.Engine, Op: "pull", Result: res}, nil
	})
}

// streamOp is the shared engine of the streaming ec endpoints. It subscribes to
// the engine's progress before launching run (so no early frame is missed),
// forwards each progress event as an NDJSON frame, and writes run's terminal
// frame (or an error frame) last. run executes on the request context, so a
// disconnected initiator cancels the operation.
func (s *controlServer) streamOp(w http.ResponseWriter, r *http.Request, opID, engine, op string, run func(ctx context.Context) (streamFrame, error)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, cancelSub := s.exec.progress.subscribe(engine)
	defer cancelSub()

	enc := json.NewEncoder(w)
	writeProgress := func(ev ProgressEvent) {
		f := streamFrame{
			Type: "progress", OpID: opID, Engine: ev.Engine, Op: ev.Op,
			Stage: ev.Stage, Message: ev.Message,
		}
		if wirePercentIncluded(ev.Percent) {
			f.Percent = ev.Percent
		}
		_ = enc.Encode(f)
		flusher.Flush()
	}

	type opResult struct {
		frame streamFrame
		err   error
	}
	resCh := make(chan opResult, 1)
	go func() {
		f, err := run(r.Context())
		resCh <- opResult{frame: f, err: err}
	}()

	for {
		select {
		case ev := <-events:
			writeProgress(ev)
		case res := <-resCh:
			// Drain any progress buffered before the terminal frame.
			for drained := false; !drained; {
				select {
				case ev := <-events:
					writeProgress(ev)
				default:
					drained = true
				}
			}
			frame := res.frame
			if res.err != nil {
				frame = streamFrame{Type: "error", OpID: opID, Engine: engine, Op: op, Message: res.err.Error()}
			}
			_ = enc.Encode(frame)
			flusher.Flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}
