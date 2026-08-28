// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
