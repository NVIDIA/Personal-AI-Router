// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStreamOpEmitsProgressThenResult verifies streamOp forwards published
// progress frames and ends with the run's terminal result frame.
func TestStreamOp_EmitsProgressThenResult(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, nil)

	st := EngineStatus{Engine: "ollama", Installed: true, Running: true}
	s.streamOp(rec, req, "op1", "ollama", "install", "", func(ctx context.Context) (streamFrame, error) {
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
func TestStreamOp_EmitsErrorFrame(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, nil)

	s.streamOp(rec, req, "op2", "ollama", "install", "", func(ctx context.Context) (streamFrame, error) {
		return streamFrame{}, context.DeadlineExceeded
	})

	frames := decodeFrames(t, rec.Body.String())
	require.Len(t, frames, 1, "expected one error frame")
	require.Equal(t, "error", frames[0].Type, "expected one error frame (%v)", frames)
	require.NotEqual(t, "", frames[0].Message, "expected one error frame (%v)", frames)
}

// A subscription is per engine, so a pull stream also sees the engine's
// install steps and any other model's pull. Only the requested model's
// download belongs to the initiator that asked for it.
func TestStreamOpScopedToAModelDropsEveryOtherOperation(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlPullPath, nil)

	s.streamOp(rec, req, "op3", "ollama", "pull", "demo", func(ctx context.Context) (streamFrame, error) {
		// An install carries no model, so matching on the model alone let it
		// through and stamped it with this pull's opID.
		exec.progress.publish(ProgressEvent{Engine: "ollama", Op: "install", Stage: "downloading", Percent: 10})
		exec.progress.publish(ProgressEvent{Engine: "ollama", Model: "other", Op: "pull", Stage: "downloading", Percent: 20})
		exec.progress.publish(ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: "downloading", Percent: 30})
		return streamFrame{Type: "result", OpID: "op3", Engine: "ollama", Op: "pull"}, nil
	})

	frames := decodeFrames(t, rec.Body.String())
	if len(frames) != 2 {
		t.Fatalf("expected the requested model's progress and the result, got %d frames: %s", len(frames), rec.Body.String())
	}
	if frames[0].Type != "progress" || frames[0].Model != "demo" || frames[0].Percent != 30 {
		t.Fatalf("bad progress frame: %+v", frames[0])
	}
	if frames[1].Type != "result" {
		t.Fatalf("bad result frame: %+v", frames[1])
	}
}

// heldHeaderRecorder records a streamed response, holding the first flush —
// the one that sends the stream's header — until released.
type heldHeaderRecorder struct {
	*httptest.ResponseRecorder
	headerSent chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (w *heldHeaderRecorder) Flush() {
	w.once.Do(func() {
		close(w.headerSent)
		<-w.release
	})
	w.ResponseRecorder.Flush()
}

// The peer claims a pull before it sends the stream's header, and the
// initiator holds its cancel until that header arrives, so the cancel always
// finds the claim — though not always the download, which may not have
// registered yet. The claim has to reach the executor with the request for
// that cancel to stop it.
func TestHandlePull_StopsAPullCancelledBeforeItsDownloadRegisters(t *testing.T) {
	ex := newHTTPPullTestExecutor(t, stalledTransfer(t), Action{HTTP: &ActionHTTP{Method: http.MethodPost, Path: "/api/pull"}})
	w := &heldHeaderRecorder{ResponseRecorder: httptest.NewRecorder(), headerSent: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		(&controlServer{exec: ex}).handlePull(w, httptest.NewRequest(http.MethodPost, controlPullPath,
			strings.NewReader(`{"opId":"op","engine":"fake","model":"demo"}`)).WithContext(ctx))
	}()
	select {
	case <-w.headerSent:
	case <-served:
		require.FailNow(t, "the pull ended before its stream opened", w.Body.String())
	}

	// streamOp starts the download only after the header is out, so this
	// cancel reaches the executor before the download can register.
	require.NoError(t, ex.CancelModelPull(ctx, "fake", "demo"))
	close(w.release)
	<-served

	frames := decodeFrames(t, w.Body.String())
	require.NotEmpty(t, frames, "the pull kept running after its cancel was acknowledged")
	terminal := frames[len(frames)-1]
	require.Equal(t, "result", terminal.Type, "the pull should settle as cancelled, not fail: %+v", terminal)
	assertPullStatus(t, terminal.Result, "cancelled")
}

// TestHandleInstallRejectsMissingEngine verifies request-body validation.
func TestHandleInstall_RejectsMissingEngine(t *testing.T) {
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
