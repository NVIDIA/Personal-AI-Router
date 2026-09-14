// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxBodyBytes bounds a request we buffer to read its "model" field.
//
// Buffering happens BEFORE admission, so every request waiting on a cold load
// holds its body for the whole wait -- at 64 MiB a handful of queued requests
// was a real amplification point. 8 MiB is far above any realistic chat payload
// (roughly two million tokens of text) while keeping that pool bounded.
const maxBodyBytes = 8 << 20

type modelEntry struct {
	ID     string `json:"id"`
	Object string `json:"object"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleHealth reports every resident model, most recently used first.
//
// It answers before any child exists, with an empty list. That matters more than
// it looks: PAIR's readiness probe hits this, so the engine must read as "up"
// while holding nothing — otherwise starting the engine would require loading a
// model first, and the whole pool would be un-startable from cold.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	models := s.pool.Resident()
	entries := make([]modelEntry, len(models))
	for i, m := range models {
		entries[i] = modelEntry{ID: m, Object: "model"}
	}
	// `model` is the most recently used one, kept for parity with
	// mlx_lm.server's own /health so a client written against that still works.
	var mru any
	if len(models) > 0 {
		mru = models[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "model": mru, "models": entries, "max_models": s.pool.max,
	})
}

// handleUnload releases a resident model's memory on request.
//
// mlx-lm has no unload of its own; a pool child is one model for its lifetime,
// so ending it is the unload. Body is {"model": "<id>"}. Unloading something not
// resident succeeds: the caller wanted the memory back and it is already back.
func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model is required"})
		return
	}
	if err := s.pool.Unload(req.Model); err != nil {
		// In use, not invalid: the caller can retry once the stream finishes.
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unloaded": req.Model})
}

// handleModels lists the downloadable catalogue.
//
// Scanned from the Hugging Face cache here rather than forwarded to a child,
// because with no child running there is nobody to forward to — and "what can I
// load" is exactly the question asked when nothing is loaded.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	entries := make([]modelEntry, 0)
	seen := map[string]bool{}
	candidates := scanHFCache(s.hfCache)
	for _, dir := range s.modelDirs {
		candidates = append(candidates, scanModelsDir(dir)...)
	}
	candidates = append(candidates, readRegistered(s.registeredFile)...)
	for _, id := range append(candidates, s.extra...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entries = append(entries, modelEntry{ID: id, Object: "model"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": entries})
}

// scanModelsDir lists model directories held OUTSIDE the Hugging Face cache.
//
// A model you built yourself -- a local quantization, say -- has no repo id and
// never enters the cache, so a catalogue that only scans the cache cannot show
// it. mlx_lm.server has the same blind spot and papers over it by advertising
// its single --model path; a pool has no single model, so it scans instead.
//
// Entries are advertised by absolute path, which is exactly what a request must
// name to load them.
func scanModelsDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if !isModelDir(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// isModelDir reports whether a directory holds something mlx_lm can serve.
//
// No weight-index requirement, unlike the cache scan: a single-shard local
// build legitimately has only model.safetensors. Shared with the registered-path
// list so a path added through the UI is judged by exactly the same test as one
// found by scanning -- a path that passes here is one mlx_lm.server can load.
func isModelDir(p string) bool {
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return false
	}
	if !hasAll(p, "config.json") {
		return false
	}
	return hasAll(p, "tokenizer_config.json") || hasAll(p, "tokenizer.json")
}

// readRegistered returns the model paths a user added by hand, one per line.
//
// Read on every catalogue request rather than at startup so a path registered
// through the UI shows up without restarting the engine. Entries that no longer
// pass isModelDir are skipped rather than reported: a model directory the user
// has since deleted or moved should quietly stop being offered, not break the
// catalogue for every other model.
func readRegistered(path string) []string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || strings.HasPrefix(p, "#") || !isModelDir(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// scanHFCache returns the repo ids of cached models that look like a servable
// MLX model, mirroring the test mlx_lm.server applies: a repo is a candidate
// when its snapshot carries a config, a weight index and a tokenizer config.
func scanHFCache(root string) []string {
	hub, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range hub {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, "models--") {
			continue
		}
		snaps, err := os.ReadDir(filepath.Join(root, name, "snapshots"))
		if err != nil {
			continue
		}
		for _, snap := range snaps {
			dir := filepath.Join(root, name, "snapshots", snap.Name())
			// The same three files mlx_lm.server tests for. The weight INDEX
			// specifically is what separates a servable LLM from the embedding
			// and BERT models that share a cache and also carry a config and a
			// tokenizer config -- without it the catalogue offers models that
			// cannot be loaded.
			if !hasAll(dir, "config.json", "model.safetensors.index.json", "tokenizer_config.json") {
				continue
			}
			// repo id: models--org--name -> org/name
			out = append(out, strings.ReplaceAll(strings.TrimPrefix(name, "models--"), "--", "/"))
			break
		}
	}
	return out
}

func hasAll(dir string, files ...string) bool {
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

// handleInference routes a completion to the child holding its model.
func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || strings.TrimSpace(probe.Model) == "" {
		http.Error(w, `request must name a "model"`, http.StatusBadRequest)
		return
	}

	// The release pins the child against eviction for the whole exchange,
	// including a streamed response: without it a concurrent request for another
	// model could kill the process mid-generation, and at max-models=1 that is
	// the ordinary case rather than a corner one.
	port, release, err := s.pool.Acquire(r.Context(), probe.Model)
	if err != nil {
		slog.Warn("could not serve model", "model", probe.Model, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	defer release()

	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Streaming responses must not be buffered, or a token stream arrives as one
	// blob when the generation ends.
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Warn("upstream model failed", "model", probe.Model, "port", port, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	proxy.ServeHTTP(w, r)
}

// Server is the single listener PAIR sees as "the MLX engine".
type Server struct {
	pool    *Pool
	hfCache string
	// extra are models that live outside the Hugging Face cache -- a directory
	// of weights, typically. mlx_lm.server advertises its own --model path the
	// same way; a pool has no single --model, so they are declared instead.
	// Loading one never needed this: any id in a request is passed straight
	// through. This is only so the catalogue can show them.
	extra []string
	// modelDirs are directories of models kept outside the cache, scanned on
	// each request so a model built while the engine is running appears without
	// a restart.
	modelDirs []string
	// registeredFile lists model paths added by hand through the UI, one per
	// line. There is no MLX catalogue to browse -- the hub is empty for this
	// engine -- so a model that is neither in the cache nor under a scanned
	// directory can only be named. Re-read per request, like modelDirs.
	registeredFile string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/unload", s.handleUnload)
	mux.HandleFunc("/v1/chat/completions", s.handleInference)
	mux.HandleFunc("/v1/completions", s.handleInference)
	return mux
}

func (s *Server) Listen(addr string) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// No write timeout: a large model's first token can be minutes away, and
		// a streamed completion is open for as long as it generates.
		ReadHeaderTimeout: 15 * time.Second,
	}
}
