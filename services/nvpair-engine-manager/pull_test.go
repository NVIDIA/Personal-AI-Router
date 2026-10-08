// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPullProgressFromLine(t *testing.T) {
	ev := pullProgressFromLine("ollama", []byte(`{"status":"pulling manifest","total":200,"completed":50}`))
	if ev.Op != "pull" || ev.Engine != "ollama" {
		t.Fatalf("unexpected event meta: %+v", ev)
	}
	if ev.Percent != 25 {
		t.Fatalf("expected 25%%, got %d", ev.Percent)
	}
	if ev.Stage != "pulling manifest" {
		t.Fatalf("expected stage from status, got %q", ev.Stage)
	}

	// No total -> 0% (avoids divide-by-zero), still carries the status.
	ev = pullProgressFromLine("ollama", []byte(`{"status":"verifying"}`))
	if ev.Percent != 0 || ev.Stage != "verifying" {
		t.Fatalf("unexpected zero-total event: %+v", ev)
	}
}

func TestHandlePullRejectsMissingTarget(t *testing.T) {
	s := &controlServer{exec: &Executor{progress: newProgressHub()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlPullPath, strings.NewReader(`{"opId":"x","engine":"ollama"}`))
	s.handlePull(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 when neither model nor params set, got %d", rec.Code)
	}
}

func TestModelFromParams(t *testing.T) {
	cases := []struct {
		params, want string
	}{
		{`{"name":"llama3.2"}`, "llama3.2"},      // Ollama body key
		{`{"model":"owner/repo"}`, "owner/repo"}, // generic placeholder
		{`{"name":"a","model":"b"}`, "a"},        // name wins
		{`{}`, ""},                               // neither present
		{``, ""},                                 // empty params
	}
	for _, c := range cases {
		if got := modelFromParams([]byte(c.params)); got != c.want {
			t.Fatalf("modelFromParams(%q) = %q, want %q", c.params, got, c.want)
		}
	}
}

func TestLlamaCPPModelsEventPercentAggregatesFiles(t *testing.T) {
	var event llamaCPPModelsEvent
	err := json.Unmarshal([]byte(`{
		"model":"owner/repo:Q4_K_M",
		"event":"download_progress",
		"data":{"progress":{
			"model.gguf":{"done":75,"total":100},
			"mmproj.gguf":{"done":25,"total":100}
		}}
	}`), &event)
	if err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if got := event.percent(); got != 50 {
		t.Fatalf("aggregate percent = %d, want 50", got)
	}
}

func newHTTPPullTestExecutor(t *testing.T, server *httptest.Server, action Action) *Executor {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Actions[pullModelAction] = action
	registry := NewRegistry()
	registry.engines[manifest.Engine] = manifest
	executor := NewExecutor(registry, NewReporter(nil), nil, t.TempDir())
	state, err := executor.state(manifest.Engine)
	if err != nil {
		t.Fatalf("resolve engine state: %v", err)
	}
	state.running = true
	state.port = port
	return executor
}

func TestPullModelLlamaCPPSSESubscribesBeforeStarting(t *testing.T) {
	const model = "owner/repo:Q4_K_M"
	subscribed := make(chan struct{})
	started := make(chan struct{})
	var postBeforeSubscribe atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/models/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		close(subscribed)
		_, _ = fmt.Fprint(w, ": ready\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-started:
		case <-r.Context().Done():
			return
		}
		write := func(event map[string]any) {
			data, err := json.Marshal(event)
			if err != nil {
				t.Errorf("encode SSE event: %v", err)
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		write(map[string]any{"model": "other/model", "event": "download_finished", "data": map[string]any{}})
		write(map[string]any{
			"model": model,
			"event": "download_progress",
			"data": map[string]any{"progress": map[string]any{
				"model.gguf":  map[string]int64{"done": 75, "total": 100},
				"mmproj.gguf": map[string]int64{"done": 25, "total": 100},
			}},
		})
		write(map[string]any{"model": model, "event": "download_finished", "data": map[string]any{}})
	})
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-subscribed:
		default:
			postBeforeSubscribe.Store(true)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != model {
			http.Error(w, "invalid model", http.StatusBadRequest)
			return
		}
		close(started)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ex := newHTTPPullTestExecutor(t, server, Action{
		HTTP:             &ActionHTTP{Method: http.MethodPost, Path: "/models"},
		ProgressProtocol: pullProgressProtocolLlamaCPPModelsSSE,
	})
	progress, cancel := ex.progress.subscribe("fake")
	defer cancel()

	result, err := ex.PullModelStream(context.Background(), "fake", model, json.RawMessage(`{"model":"`+model+`"}`))
	if err != nil {
		t.Fatalf("pull model: %v", err)
	}
	var response struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(result, &response); err != nil || !response.Success {
		t.Fatalf("pull result = %s, error %v", result, err)
	}
	if postBeforeSubscribe.Load() {
		t.Fatal("download POST arrived before the SSE subscription was open")
	}
	var events []ProgressEvent
	for len(progress) > 0 {
		events = append(events, <-progress)
	}
	if len(events) != 2 {
		t.Fatalf("progress events = %+v, want downloading and success", events)
	}
	if events[0].Stage != "downloading" || events[0].Percent != 50 {
		t.Fatalf("download event = %+v, want 50%%", events[0])
	}
	if events[1].Stage != "success" || events[1].Percent != 100 {
		t.Fatalf("terminal event = %+v, want success at 100%%", events[1])
	}
}

func TestPullModelOllamaAdvancingProgressRefreshesTimeout(t *testing.T) {
	const (
		idleTimeout     = 400 * time.Millisecond
		progressDelay   = 90 * time.Millisecond
		progressUpdates = 6
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test response does not support flushing")
			return
		}
		for completed := 1; completed <= progressUpdates; completed++ {
			if _, err := fmt.Fprintf(
				w,
				"{\"status\":\"pulling\",\"digest\":\"sha256:model\",\"total\":%d,\"completed\":%d}\n",
				progressUpdates,
				completed,
			); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(progressDelay)
		}
		_, _ = fmt.Fprintln(w, `{"status":"success"}`)
		flusher.Flush()
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ex := newHTTPPullTestExecutor(t, server, Action{
		HTTP: &ActionHTTP{Method: http.MethodPost, Path: "/api/pull"},
	})
	ex.pullProgressTimeout = idleTimeout

	started := time.Now()
	result, err := ex.PullModelStream(
		context.Background(),
		"fake",
		"demo:1b",
		json.RawMessage(`{"name":"demo:1b"}`),
	)
	if err != nil {
		t.Fatalf("pull model: %v", err)
	}
	if elapsed := time.Since(started); elapsed <= idleTimeout {
		t.Fatalf("pull completed in %s, want longer than one %s idle interval", elapsed, idleTimeout)
	}
	if !strings.Contains(string(result), `"status":"success"`) {
		t.Fatalf("pull result = %s, want terminal success", result)
	}
}

func TestPullModelLlamaCPPAdvancingProgressRefreshesTimeout(t *testing.T) {
	const (
		model           = "owner/repo:Q4_K_M"
		idleTimeout     = 400 * time.Millisecond
		progressDelay   = 90 * time.Millisecond
		progressUpdates = 6
	)
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/models/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test response does not support flushing")
			return
		}
		_, _ = fmt.Fprint(w, ": ready\n\n")
		flusher.Flush()
		select {
		case <-started:
		case <-r.Context().Done():
			return
		}
		for completed := 1; completed <= progressUpdates; completed++ {
			if _, err := fmt.Fprintf(
				w,
				"data: {\"model\":%q,\"event\":\"download_progress\",\"data\":{\"progress\":{\"model.gguf\":{\"done\":%d,\"total\":%d}}}}\n\n",
				model,
				completed,
				progressUpdates,
			); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(progressDelay)
		}
		_, _ = fmt.Fprintf(w, "data: {\"model\":%q,\"event\":\"download_finished\",\"data\":{}}\n\n", model)
		flusher.Flush()
	})
	mux.HandleFunc("/models", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ex := newHTTPPullTestExecutor(t, server, Action{
		HTTP:             &ActionHTTP{Method: http.MethodPost, Path: "/models"},
		ProgressProtocol: pullProgressProtocolLlamaCPPModelsSSE,
	})
	ex.pullProgressTimeout = idleTimeout

	startedAt := time.Now()
	result, err := ex.PullModelStream(
		context.Background(),
		"fake",
		model,
		json.RawMessage(`{"model":"`+model+`"}`),
	)
	if err != nil {
		t.Fatalf("pull model: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed <= idleTimeout {
		t.Fatalf("pull completed in %s, want longer than one %s idle interval", elapsed, idleTimeout)
	}
	if !strings.Contains(string(result), `"success":true`) {
		t.Fatalf("pull result = %s, want accepted download response", result)
	}
}

func TestPullModelDuplicateProgressDoesNotRefreshTimeout(t *testing.T) {
	const idleTimeout = 150 * time.Millisecond
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test response does not support flushing")
			return
		}
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			_, err := fmt.Fprintln(w, `{"status":"pulling","digest":"sha256:model","total":100,"completed":1}`)
			if err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ex := newHTTPPullTestExecutor(t, server, Action{
		HTTP: &ActionHTTP{Method: http.MethodPost, Path: "/api/pull"},
	})
	ex.pullProgressTimeout = idleTimeout

	_, err := ex.PullModelStream(
		context.Background(),
		"fake",
		"demo:1b",
		json.RawMessage(`{"name":"demo:1b"}`),
	)
	if err == nil {
		t.Fatal("pull model succeeded despite duplicate-only progress")
	}
	if want := "model download made no progress for 150ms"; !strings.Contains(err.Error(), want) {
		t.Fatalf("pull error = %q, want %q", err, want)
	}
}

func TestPullProgressWatchdogPreservesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, watchdog := newPullProgressWatchdog(parent, time.Hour)
	defer watchdog.stop()

	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("watchdog context did not observe parent cancellation")
	}
	if cause := context.Cause(ctx); cause != context.Canceled {
		t.Fatalf("watchdog cause = %v, want context canceled", cause)
	}
}

// TestActionPullModelStreamsProgress verifies that engine:action with action
// "pull_model" is routed through the streaming pull path, so a local pull
// emits live engine:pull-progress notifications (with computed percentages) and
// still returns the pull's terminal result — matching what remote pulls already
// surface via engine:remote-progress.
func TestActionPullModelStreamsProgress(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["pull_model"] = Action{HTTP: &ActionHTTP{Method: "POST", Path: "/api/pull"}}

	var mu sync.Mutex
	var pulls []map[string]any
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(method string, params any) {
		if method != "engine:pull-progress" {
			return
		}
		mp, _ := params.(map[string]any)
		mu.Lock()
		pulls = append(pulls, mp)
		mu.Unlock()
	}, t.TempDir())

	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}

	var out bytes.Buffer
	mgr := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("7")
	mgr.runAction(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:action",
		Params: json.RawMessage(`{"engine":"fake","action":"pull_model","params":{"name":"demo:1b"}}`)})

	if !strings.Contains(out.String(), "success") {
		t.Fatalf("expected the pull's terminal result in the response, got %s", out.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pulls) == 0 {
		t.Fatal("engine:action{action:pull_model} emitted no engine:pull-progress notifications")
	}
	sawPercent := false
	for _, p := range pulls {
		if p["op"] != "pull" {
			t.Fatalf("expected op=pull on every frame, got %+v", p)
		}
		if pct, ok := p["percent"].(int); ok && pct > 0 {
			sawPercent = true
		}
	}
	if !sawPercent {
		t.Fatalf("expected at least one frame with a computed percent > 0, got %+v", pulls)
	}
}

// TestActionPullModelCmdMarkerAndResult covers the CLI (LM Studio `lms get`)
// pull path through the same engine:action routing: a Cmd-based pull_model can't
// expose structured line progress, so it emits a single "pulling" marker and
// returns the command's terminal result — the counterpart to the HTTP streaming
// path in TestActionPullModelStreamsProgress.
func TestActionPullModelCmdMarkerAndResult(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["pull_model"] = Action{Cmd: []string{fakeEngineBin, "echo", "pulled:{model}"}}

	var mu sync.Mutex
	var pulls []map[string]any
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(method string, params any) {
		if method != "engine:pull-progress" {
			return
		}
		mp, _ := params.(map[string]any)
		mu.Lock()
		pulls = append(pulls, mp)
		mu.Unlock()
	}, t.TempDir())

	// A Cmd action just runs a binary; the engine need not be started.
	var out bytes.Buffer
	mgr := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("8")
	mgr.runAction(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:action",
		Params: json.RawMessage(`{"engine":"fake","action":"pull_model","params":{"model":"demo:1b"}}`)})

	if !strings.Contains(out.String(), "pulled:demo:1b") {
		t.Fatalf("expected the cmd pull's terminal result in the response, got %s", out.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pulls) != 1 {
		t.Fatalf("expected exactly one pulling marker for a CLI pull, got %d: %+v", len(pulls), pulls)
	}
	if pulls[0]["op"] != "pull" || pulls[0]["stage"] != "pulling" || pulls[0]["message"] != "demo:1b" {
		t.Fatalf("unexpected CLI pull marker: %+v", pulls[0])
	}
	if _, hasPercent := pulls[0]["percent"]; hasPercent {
		t.Fatalf("CLI pull marker must omit indeterminate percent, got %+v", pulls[0])
	}
}

// TestActionPullModelFailureEmitsTerminalError proves a failed LOCAL pull emits
// a terminal engine:pull-progress error frame (in addition to the JSON-RPC
// error), so a UI that already stopped waiting on the synchronous call still
// converges off "pulling" instead of appearing stuck.
func TestActionPullModelFailureEmitsTerminalError(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["pull_model"] = Action{HTTP: &ActionHTTP{Method: "POST", Path: "/api/pull"}}

	var mu sync.Mutex
	var pulls []map[string]any
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	var reportBuf bytes.Buffer
	reporter := NewReporter(NewCodec(&reportBuf))
	ex := NewExecutor(reg, reporter, func(method string, params any) {
		if method != "engine:pull-progress" {
			return
		}
		mp, _ := params.(map[string]any)
		mu.Lock()
		pulls = append(pulls, mp)
		mu.Unlock()
	}, t.TempDir())

	// The engine is never started, so the HTTP pull path fails with "not running".
	var out bytes.Buffer
	mgr := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("9")
	mgr.runAction(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:action",
		Params: json.RawMessage(`{"engine":"fake","action":"pull_model","params":{"name":"demo:1b"}}`)})

	if !strings.Contains(out.String(), "Fake Engine experienced an error while downloading a model") {
		t.Fatalf("expected a formatted JSON-RPC error for the failed pull, got %s", out.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pulls) != 1 {
		t.Fatalf("expected exactly one terminal error frame, got %d: %+v", len(pulls), pulls)
	}
	if pulls[0]["op"] != "pull" || pulls[0]["stage"] != "error" {
		t.Fatalf("expected a terminal error frame, got %+v", pulls[0])
	}
	if pct, _ := pulls[0]["percent"].(int); pct != -1 {
		t.Fatalf("expected percent -1 on the error frame, got %+v", pulls[0])
	}
	msg, _ := pulls[0]["message"].(string)
	if !strings.Contains(msg, "Fake Engine experienced an error while downloading a model") {
		t.Fatalf("expected formatted error on the progress frame, got %+v", pulls[0])
	}
	if !strings.Contains(msg, `engine "fake" is not running`) {
		t.Fatalf("expected engine detail in progress message, got %q", msg)
	}

	snap := reporter.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected one errors:report entry, got %+v", snap)
	}
	if snap[0].Operation != "pull" || snap[0].ModelName != "demo:1b" || snap[0].Action != "retry" {
		t.Fatalf("unexpected errors:report entry: %+v", snap[0])
	}
	if !strings.Contains(reportBuf.String(), `"errors:report"`) {
		t.Fatalf("expected errors:report on the wire, got %s", reportBuf.String())
	}
}
