// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// pull.go is the streaming model-pull path. It runs the same manifest-declared
// pull_model action as engine:action, but instead of only returning the final
// blob it tees the engine's progress to both consumers via emitPullProgress:
// the local engine:pull-progress notification (this node's UI) and the progress
// hub (so an ec streaming handler can relay live download progress to a remote
// initiator). engine:action with action "pull_model" is routed here for exactly
// this reason — a local pull streams progress just like a remote one.
//
// Ollama's /api/pull streams newline-delimited JSON status objects
// ({"status":...,"total":N,"completed":M}); each line maps to a progress event,
// coalesced so only changes in stage/percent are emitted (a single layer streams
// many byte-progress lines at the same rendered percent). CLI-driven pulls (LM
// Studio's `lms get`) don't expose structured line progress here, so they emit a
// single "pulling" marker and return the final result — the security/trust
// boundary and result contract are identical.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// pullModelAction is the manifest action name every engine uses for model pulls.
const pullModelAction = "pull_model"

// modelFromParams extracts a human-readable model name from an engine:action
// pull_model params object, preferring Ollama's "name" body key then the generic
// {model} placeholder. Used only for the progress message — the params are still
// passed through to the action verbatim.
func modelFromParams(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Name != "" {
		return p.Name
	}
	return p.Model
}

// PullModelStream runs the engine's pull_model action, publishing progress to
// the hub as it advances, and returns the action's final JSON result. When
// params is empty it defaults to {"name","model"} (covering Ollama's `name` body
// key and the {model} CLI placeholder), so a caller can pass just a model name.
func (e *Executor) PullModelStream(ctx context.Context, engine, model string, params json.RawMessage) (json.RawMessage, error) {
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	act, ok := st.manifest.Actions[pullModelAction]
	if !ok {
		return nil, fmt.Errorf("engine %q has no action %q", engine, pullModelAction)
	}
	if len(params) == 0 || string(params) == "null" {
		params, _ = json.Marshal(map[string]string{"name": model, "model": model})
	}

	ctx, cancel := context.WithTimeout(ctx, e.actionTimeout)
	defer cancel()

	// CLI action (e.g. lms get): no structured line progress; emit a start
	// marker and return the final result via the existing runner.
	if len(act.Cmd) > 0 {
		st.mu.Lock()
		port := st.port
		st.mu.Unlock()
		e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "pulling", Message: model})
		return e.runCmdAction(ctx, st, act, port, params)
	}

	// HTTP action (e.g. Ollama /api/pull): stream NDJSON progress.
	st.mu.Lock()
	running := st.running
	port := st.port
	st.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("engine %q is not running", engine)
	}
	path, err := resolvePlaceholders(act.HTTP.Path, map[string]string{"port": strconv.Itoa(port)})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(act.HTTP.Method), url, bytes.NewReader(params))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, fmt.Errorf("pull %q: engine returned HTTP %d: %s", model, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	// Coalesce redundant frames: a chatty engine streams many byte-progress
	// lines per layer, most of which map to the same (stage, percent) pair. We
	// keep the newest raw line for the terminal result but only notify/publish
	// when the rendered stage or percent actually changes, bounding the event
	// volume a subscriber (local UI or remote relay) has to absorb. lastPct=-1
	// is a sentinel pullProgressFromLine never yields, so the first frame emits.
	last := json.RawMessage("null")
	lastStage, lastPct := "", -1
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // status lines are small; cap generously
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		last = append(json.RawMessage(nil), line...)
		ev := pullProgressFromLine(engine, line)
		if ev.Stage == lastStage && ev.Percent == lastPct {
			continue
		}
		lastStage, lastPct = ev.Stage, ev.Percent
		e.emitPullProgress(ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("pull %q: %w", model, err)
	}
	return last, nil
}

// engineDisplayName returns the manifest display name for user-facing copy.
func (e *Executor) engineDisplayName(engine string) string {
	st, err := e.state(engine)
	if err == nil && strings.TrimSpace(st.manifest.DisplayName) != "" {
		return st.manifest.DisplayName
	}
	return engine
}

// reportPullFailed records a pull failure for the errors pipeline and returns
// the user-facing message for progress frames and JSON-RPC errors.
func (e *Executor) reportPullFailed(engine, model string, err error) string {
	msg := formatEnginePullError(e.engineDisplayName(engine), err)
	e.reporter.report(serviceError{
		ID: pullFailedID(engine, model), Message: msg,
		Severity: "error", Action: "retry", EngineType: engine, Operation: "pull", ModelName: model,
	})
	return msg
}

// pullProgressFromLine maps an Ollama /api/pull status line into a ProgressEvent,
// computing a percentage when the line carries total/completed byte counts.
func pullProgressFromLine(engine string, line []byte) ProgressEvent {
	var p struct {
		Status    string `json:"status"`
		Total     int64  `json:"total"`
		Completed int64  `json:"completed"`
	}
	_ = json.Unmarshal(line, &p)
	pct := 0
	if p.Total > 0 {
		pct = int(p.Completed * 100 / p.Total)
	}
	return ProgressEvent{Engine: engine, Op: "pull", Stage: p.Status, Percent: pct, Message: p.Status}
}
