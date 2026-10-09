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

	"github.com/stretchr/testify/require"
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
	require.Len(t, frames, 2, "expected 2 frames (progress, result)")
	require.Equal(t, "progress", frames[0].Type, "bad progress frame")
	require.Equal(t, 42, frames[0].Percent, "bad progress frame")
	require.Equal(t, "op1", frames[0].OpID, "bad progress frame")
	require.Equal(t, "result", frames[1].Type, "bad result frame")
	require.NotNil(t, frames[1].Status, "bad result frame")
	require.True(t, frames[1].Status.Running, "bad result frame")
	require.Equal(t, "application/x-ndjson", rec.Header().Get("Content-Type"), "expected ndjson content-type")
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
	require.Len(t, frames, 1, "expected one error frame")
	require.Equal(t, "error", frames[0].Type, "expected one error frame (%v)", frames)
	require.NotEqual(t, "", frames[0].Message, "expected one error frame (%v)", frames)
}

// TestHandleInstallRejectsMissingEngine verifies request-body validation.
func TestHandleInstallRejectsMissingEngine(t *testing.T) {
	s := &controlServer{exec: &Executor{progress: newProgressHub()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, strings.NewReader(`{"opId":"x"}`))
	s.handleInstall(rec, req)
	require.Equal(t, 400, rec.Code, "expected 400 for missing engine")
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
		require.NoError(t, json.Unmarshal([]byte(line), &f), "bad frame line (%v)", line)
		out = append(out, f)
	}
	return out
}
