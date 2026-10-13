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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullProgressFromLine(t *testing.T) {
	ev := pullProgressFromLine("ollama", []byte(`{"status":"pulling manifest","total":200,"completed":50}`))
	require.Equal(t, "pull", ev.Op, "unexpected event meta (%v)", ev)
	require.Equal(t, "ollama", ev.Engine, "unexpected event meta (%v)", ev)
	require.Equal(t, 25, ev.Percent, "expected 25")
	require.Equal(t, "pulling manifest", ev.Stage, "expected stage from status")

	// No total -> 0% (avoids divide-by-zero), still carries the status.
	ev = pullProgressFromLine("ollama", []byte(`{"status":"verifying"}`))
	require.Equal(t, 0, ev.Percent, "unexpected zero-total event (%v)", ev)
	require.Equal(t, "verifying", ev.Stage, "unexpected zero-total event (%v)", ev)
}

func TestHandlePullRejectsMissingTarget(t *testing.T) {
	s := &controlServer{exec: &Executor{progress: newProgressHub()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlPullPath, strings.NewReader(`{"opId":"x","engine":"ollama"}`))
	s.handlePull(rec, req)
	require.Equal(t, 400, rec.Code, "expected 400 when neither model nor params set")
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
		require.Equal(t, c.want, modelFromParams([]byte(c.params)), "modelFromParams")
	}
}

func TestLlamaCPPModelsEventPercentAggregatesFiles(t *testing.T) {
	var event llamaCPPModelsEvent
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"owner/repo:Q4_K_M",
		"event":"download_progress",
		"data":{"progress":{
			"model.gguf":{"done":75,"total":100},
			"mmproj.gguf":{"done":25,"total":100}
		}}
	}`), &event), "decode event")
	require.Equal(t, 50, event.percent(), "aggregate percent")
}

func newHTTPPullTestExecutor(t *testing.T, server *httptest.Server, action Action) *Executor {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err, "split test server address")
	port, err := strconv.Atoi(portText)
	require.NoError(t, err, "parse test server port")
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Actions[pullModelAction] = action
	registry := NewRegistry()
	registry.engines[manifest.Engine] = manifest
	executor := NewExecutor(registry, NewReporter(nil), nil, t.TempDir())
	state, err := executor.state(manifest.Engine)
	require.NoError(t, err, "resolve engine state")
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
			if !assert.NoError(t, err, "encode SSE event") {
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
	require.NoError(t, err, "pull model")
	var response struct {
		Success bool `json:"success"`
	}
	require.NoError(t, json.Unmarshal(result, &response), "decode pull result")
	require.True(t, response.Success, "pull result must succeed")
	require.False(t, postBeforeSubscribe.Load(), "download POST arrived before the SSE subscription was open")
	var events []ProgressEvent
	for len(progress) > 0 {
		events = append(events, <-progress)
	}
	require.Len(t, events, 2, "must report downloading and success")
	require.Equal(t, "downloading", events[0].Stage)
	require.Equal(t, 50, events[0].Percent)
	require.Equal(t, "success", events[1].Stage)
	require.Equal(t, 100, events[1].Percent)
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
		if !assert.True(t, ok, "test response does not support flushing") {
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
	require.NoError(t, err, "pull model")
	require.Greater(t, time.Since(started), idleTimeout, "pull must last longer than one idle interval")
	require.Contains(t, string(result), `"status":"success"`, "pull must report terminal success")
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
		if !assert.True(t, ok, "test response does not support flushing") {
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
	require.NoError(t, err, "pull model")
	require.Greater(t, time.Since(startedAt), idleTimeout, "pull must last longer than one idle interval")
	require.Contains(t, string(result), `"success":true`, "pull must report an accepted download")
}

func TestPullModelDuplicateProgressDoesNotRefreshTimeout(t *testing.T) {
	const idleTimeout = 150 * time.Millisecond
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !assert.True(t, ok, "test response does not support flushing") {
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
	require.ErrorContains(t, err, "model download made no progress for 150ms", "pull model succeeded despite duplicate-only progress")
}

func TestPullProgressWatchdogPreservesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, watchdog := newPullProgressWatchdog(parent, time.Hour)
	defer watchdog.stop()

	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		require.FailNow(t, "watchdog context did not observe parent cancellation")
	}
	require.ErrorIs(t, context.Cause(ctx), context.Canceled)
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
	require.NoError(t, ex.Start(ctx, "fake"), "start")

	var out bytes.Buffer
	mgr := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("7")
	mgr.runAction(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: "engine:action",
		Params: json.RawMessage(`{"engine":"fake","action":"pull_model","params":{"name":"demo:1b"}}`)})

	require.Contains(t, out.String(), "success", "expected the pull's terminal result in the response")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, pulls, "engine:action{action:pull_model} emitted no engine:pull-progress notifications")
	sawPercent := false
	for _, p := range pulls {
		require.Equal(t, "pull", p["op"], "expected op=pull on every frame (%v)", p)
		if pct, ok := p["percent"].(int); ok && pct > 0 {
			sawPercent = true
		}
	}
	require.True(t, sawPercent, "expected at least one frame with a computed percent > 0 (%v)", pulls)
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

	require.Contains(t, out.String(), "pulled:demo:1b", "expected the cmd pull's terminal result in the response")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, pulls, 1, "expected exactly one pulling marker for a CLI pull")
	require.Equal(t, "pull", pulls[0]["op"], "unexpected CLI pull marker")
	require.Equal(t, "pulling", pulls[0]["stage"], "unexpected CLI pull marker")
	require.Equal(t, "demo:1b", pulls[0]["message"], "unexpected CLI pull marker")
	require.NotContains(t, pulls[0], "percent", "CLI pull marker must omit indeterminate percent")
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

	require.Contains(t, out.String(), "Fake Engine experienced an error while downloading a model", "expected a formatted JSON-RPC error for the failed pull")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, pulls, 1, "expected exactly one terminal error frame")
	require.Equal(t, "pull", pulls[0]["op"], "expected a terminal error frame")
	require.Equal(t, "error", pulls[0]["stage"], "expected a terminal error frame")
	pct, _ := pulls[0]["percent"].(int)
	require.Equal(t, -1, pct, "expected percent -1 on the error frame")
	msg, _ := pulls[0]["message"].(string)
	require.Contains(t, msg, "Fake Engine experienced an error while downloading a model", "expected formatted error on the progress frame")
	require.Contains(t, msg, `engine "fake" is not running`, "expected engine detail in progress message")

	snap := reporter.snapshot()
	require.Len(t, snap, 1, "expected one errors:report entry")
	require.Equal(t, "pull", snap[0].Operation, "unexpected errors:report entry")
	require.Equal(t, "demo:1b", snap[0].ModelName, "unexpected errors:report entry")
	require.Equal(t, "retry", snap[0].Action, "unexpected errors:report entry")
	require.Contains(t, reportBuf.String(), `"errors:report"`, "expected errors:report on the wire")
}
