// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamOpEmitsProgressThenResult verifies streamOp forwards published
// progress frames and ends with the run's terminal result frame.
func TestStreamOpEmitsProgressThenResult(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, nil)

	st := EngineStatus{Engine: "ollama", Installed: true, Running: true}
	s.streamOp(rec, req, "op1", "ollama", "install", func(ctx context.Context) (streamFrame, error) {
		exec.progress.publish(ProgressEvent{Engine: "ollama", Op: "install", Stage: "downloading", Percent: 42})
		return streamFrame{Type: "result", OpID: "op1", Engine: "ollama", Op: "install", Status: &st}, nil
	})

	frames := decodeFrames(t, rec.Body.String())
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames (progress, result), got %d: %s", len(frames), rec.Body.String())
	}
	if frames[0].Type != "progress" || frames[0].Percent != 42 || frames[0].OpID != "op1" {
		t.Fatalf("bad progress frame: %+v", frames[0])
	}
	if frames[1].Type != "result" || frames[1].Status == nil || !frames[1].Status.Running {
		t.Fatalf("bad result frame: %+v", frames[1])
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("expected ndjson content-type, got %q", ct)
	}
}

// TestStreamOpEmitsErrorFrame verifies a failing op yields a terminal error
// frame rather than a result.
func TestStreamOpEmitsErrorFrame(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, nil)

	s.streamOp(rec, req, "op2", "ollama", "install", func(ctx context.Context) (streamFrame, error) {
		return streamFrame{}, context.DeadlineExceeded
	})

	frames := decodeFrames(t, rec.Body.String())
	if len(frames) != 1 || frames[0].Type != "error" || frames[0].Message == "" {
		t.Fatalf("expected one error frame, got %+v", frames)
	}
}

// TestHandleInstallRejectsMissingEngine verifies request-body validation.
func TestHandleInstallRejectsMissingEngine(t *testing.T) {
	s := &controlServer{exec: &Executor{progress: newProgressHub()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, strings.NewReader(`{"opId":"x"}`))
	s.handleInstall(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for missing engine, got %d", rec.Code)
	}
}

func decodeFrames(t *testing.T, body string) []streamFrame {
	t.Helper()
	var out []streamFrame
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f streamFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("bad frame line %q: %v", line, err)
		}
		out = append(out, f)
	}
	return out
}
