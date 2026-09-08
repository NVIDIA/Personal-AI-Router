// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command fake-engine is a tiny, pure-Go stand-in for a real inference
// engine, used by the engine-manager tests. It binds the loopback
// address given in OLLAMA_HOST (or FAKE_ADDR) and serves just enough to
// satisfy readiness/health probes and the bundled actions. Being pure
// Go, it compiles and runs on every target OS, so the lifecycle tests
// need no real engine, network, or installer.
//
// It lives under testdata/ so the module's own `go build ./...` and
// `go vet ./...` skip it; the tests build it explicitly in TestMain.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A tiny stateful model registry so list/pull/delete actions are
// observable across calls. Seeded with one model so existing
// list_models assertions still see a non-empty list. loaded tracks which
// of those models are "resident in memory" so the loaded_models endpoints
// (/api/ps, /api/v1/models) have observable, mutable state; a test drives
// it via /testctl/loaded. Both are guarded by modelsMu.
var (
	modelsMu sync.Mutex
	models   = map[string]bool{"llama3.2:1b": true}
	loaded   = map[string]bool{"llama3.2:1b": true}
)

func modelNames() []string {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	out := make([]string, 0, len(models))
	for n := range models {
		out = append(out, n)
	}
	return out
}

func loadedNames() []string {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	out := make([]string, 0, len(loaded))
	for n := range loaded {
		out = append(out, n)
	}
	return out
}

func decodeBody(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

func bstr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func main() {
	// Subcommands used by command-mode + cmd-action tests. They run and
	// exit (no server), standing in for a daemon's control CLI.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "touch": // write a marker file so a test can assert the command ran
			if len(os.Args) > 2 {
				_ = os.WriteFile(os.Args[2], []byte("ok"), 0o644)
			}
			return
		case "echo": // print args to stdout so a cmd-action can capture output
			fmt.Println(strings.Join(os.Args[2:], " "))
			return
		case "resolvesim": // stand in for `lms get`: succeed only for a Hugging
			// Face URL (and only when it doesn't name a "nope" repo), else emit
			// LM Studio's resolve-failure on stderr and exit non-zero. Lets a
			// cmd-action test exercise the lms-get Hub→Hugging-Face fallback.
			arg := ""
			if len(os.Args) > 2 {
				arg = os.Args[2]
			}
			if strings.HasPrefix(arg, "https://huggingface.co/") && !strings.Contains(arg, "nope") {
				fmt.Println("downloaded " + arg)
				return
			}
			fmt.Fprintf(os.Stderr, "Error: Failed to resolve artifact %q: The artifact does not exist or you do not have permission to read it\n", strings.ToLower(arg))
			os.Exit(1)
		case "noserve": // run but never bind — used to test readiness timeout
			time.Sleep(time.Hour)
			return
		case "failmark": // append a byte then exit non-zero — for uninstall-retry tests
			if len(os.Args) > 2 {
				if f, err := os.OpenFile(os.Args[2], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
					_, _ = f.WriteString("x")
					_ = f.Close()
				}
			}
			os.Exit(1)
		case "downloadsim": // stand in for a flaky `lms get`: emit LM Studio's
			// transient "Download failed: Timed-out" on the first <fails>
			// invocations (counted in a file), then succeed — so a test can
			// prove the runner resumes a transient download in place.
			// args: downloadsim <counterfile> <fails> <model...>
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "downloadsim: need <counterfile> <fails>")
				os.Exit(2)
			}
			counterFile := os.Args[2]
			n := 1
			if b, err := os.ReadFile(counterFile); err == nil {
				if prev, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					n = prev + 1
				}
			}
			_ = os.WriteFile(counterFile, []byte(strconv.Itoa(n)), 0o644)
			if fails, _ := strconv.Atoi(os.Args[3]); n <= fails {
				fmt.Fprintln(os.Stderr, "Error: Download failed: Timed-out. Please try to resume. - You can try to resume the download within LM Studio.")
				os.Exit(1)
			}
			fmt.Println("downloaded")
			return
		}
	}

	addr := os.Getenv("FAKE_ADDR")
	if addr == "" {
		addr = strings.TrimPrefix(os.Getenv("OLLAMA_HOST"), "http://")
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if path := os.Getenv("FAKE_PID_FILE"); path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			log.Fatalf("write FAKE_PID_FILE %q: %v", path, err)
		}
	}
	// vLLM mode: a vLLM process serves exactly one model, named on its command
	// line, and keeps it resident. FAKE_ENGINE_MODEL reseeds the registry with
	// just that model so the OpenAI list/loaded endpoints report vLLM's shape.
	if m := os.Getenv("FAKE_ENGINE_MODEL"); m != "" {
		modelsMu.Lock()
		models = map[string]bool{m: true}
		loaded = map[string]bool{m: true}
		modelsMu.Unlock()
	}
	if raw := os.Getenv("FAKE_START_DELAY"); raw != "" {
		delay, err := time.ParseDuration(raw)
		if err != nil {
			log.Fatalf("invalid FAKE_START_DELAY %q: %v", raw, err)
		}
		time.Sleep(delay)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Ollama-shape model API (stateful).
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		ms := make([]map[string]string, 0)
		for _, n := range modelNames() {
			ms = append(ms, map[string]string{"name": n, "model": n})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
	})
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		if n := bstr(decodeBody(r), "name"); n != "" {
			modelsMu.Lock()
			models[n] = true
			modelsMu.Unlock()
		}
		// Stream Ollama-shape NDJSON status frames (a manifest line, two
		// download frames carrying total/completed so the streaming pull path
		// can compute a percentage, then a terminal success) so the
		// engine:pull-progress path has real frames to relay. A buffered
		// engine:action reader tolerates this too (it just concatenates).
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{
			`{"status":"pulling manifest"}`,
			`{"status":"pulling","total":100,"completed":40}`,
			`{"status":"pulling","total":100,"completed":100}`,
			`{"status":"success"}`,
		} {
			_, _ = w.Write([]byte(line + "\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/api/delete", func(w http.ResponseWriter, r *http.Request) {
		if n := bstr(decodeBody(r), "name"); n != "" {
			modelsMu.Lock()
			delete(models, n)
			modelsMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		b := decodeBody(r)
		model := bstr(b, "model")
		w.Header().Set("Content-Type", "application/json")
		if keep, ok := b["keep_alive"]; ok {
			if n, ok := keep.(float64); ok && n == 0 && model != "" {
				modelsMu.Lock()
				delete(loaded, model)
				modelsMu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{
					"model":       model,
					"response":    "",
					"done":        true,
					"done_reason": "unload",
				})
				return
			}
		}
		if model != "" && bstr(b, "prompt") == "" {
			modelsMu.Lock()
			loaded[model] = true
			modelsMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":       model,
				"response":    "",
				"done":        true,
				"done_reason": "load",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "response": "ack:" + bstr(b, "prompt"), "done": true})
	})
	// Ollama-shape loaded-models API: /api/ps lists only resident models (every
	// row is loaded), mirroring the real endpoint's {models:[{name,size_vram,
	// expires_at}]} shape.
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		ms := make([]map[string]any, 0)
		for _, n := range loadedNames() {
			ms = append(ms, map[string]any{"name": n, "model": n, "size_vram": 1712345088, "expires_at": "2026-01-01T00:00:00Z"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
	})
	// LM Studio-shape (OpenAI-compatible).
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		data := make([]map[string]string, 0)
		for _, n := range modelNames() {
			data = append(data, map[string]string{"id": n, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	// LM Studio native REST v1 models API: /api/v1/models lists every model with
	// a loaded_instances array, so the loaded_models nonempty-array row filter
	// has something to match on.
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		modelsMu.Lock()
		data := make([]map[string]any, 0, len(models))
		for n := range models {
			instances := []any{}
			if loaded[n] {
				instances = []any{map[string]any{"id": n, "config": map[string]any{}}}
			}
			data = append(data, map[string]any{"key": n, "type": "llm", "loaded_instances": instances})
		}
		modelsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": data})
	})
	// Test-control: replace the loaded set wholesale from {"names":[...]} so a
	// test can simulate a load, an unload, or a TTL eviction without a real
	// engine. Not part of any engine's real API.
	mux.HandleFunc("/testctl/loaded", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Names []string `json:"names"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		modelsMu.Lock()
		loaded = make(map[string]bool, len(body.Names))
		for _, n := range body.Names {
			loaded[n] = true
		}
		modelsMu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b := decodeBody(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": bstr(b, "model"), "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	})
	// vLLM-shape readiness/health: a bare 200 on /health, and a /version that
	// answers JSON with a version field. /version is what distinguishes vLLM
	// from any other OpenAI-compatible server on a probe (LM Studio has no
	// such route), so nvpair-manual-nodes uses it as its disambiguator.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "0.0.0-fake"})
	})
	mux.HandleFunc("/api/error", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		os.Exit(1) // simulate an unexpected engine crash
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("fake-engine listen %s: %v", addr, err)
	}
	fmt.Fprintf(os.Stderr, "fake-engine listening on %s\n", ln.Addr())
	if err := http.Serve(ln, mux); err != nil {
		log.Fatalf("fake-engine serve: %v", err)
	}
}
