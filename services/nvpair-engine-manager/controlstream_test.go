// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	s.streamOp(rec, req, "op1", "ollama", "install", nil, func(ctx context.Context) (streamFrame, error) {
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

	s.streamOp(rec, req, "op2", "ollama", "install", nil, func(ctx context.Context) (streamFrame, error) {
		return streamFrame{}, context.DeadlineExceeded
	})

	frames := decodeFrames(t, rec.Body.String())
	require.Len(t, frames, 1, "expected one error frame")
	require.Equal(t, "error", frames[0].Type, "expected one error frame (%v)", frames)
	require.NotEqual(t, "", frames[0].Message, "expected one error frame (%v)", frames)
}

// A subscription is per engine, so a pull stream also sees the engine's
// install steps, any other model's pull, and other downloads of its own model.
// Only the download its request runs as belongs to the initiator that asked
// for it.
func TestStreamOp_CarriesOnlyTheDownloadItsRequestRunsAs(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}
	claimed, release := exec.claimPull(t.Context(), "ollama", "demo")
	defer release()
	exec.pullMu.Lock()
	claimed.Value(pullClaimKey{}).(*pullClaim).download = 7
	exec.pullMu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlPullPath, nil).WithContext(claimed)

	s.streamOp(rec, req, "op3", "ollama", "pull", exec.claimedDownload(claimed), func(ctx context.Context) (streamFrame, error) {
		// An install carries no model, so matching on the model alone let it
		// through and stamped it with this pull's opID.
		exec.progress.publish(ProgressEvent{Engine: "ollama", Op: "install", Stage: "downloading", Percent: 10})
		exec.progress.publish(ProgressEvent{Engine: "ollama", Model: "other", Op: "pull", Stage: "downloading", Percent: 20, Download: 6})
		exec.progress.publish(ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: "cancelled", Download: 5})
		exec.progress.publish(ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: "downloading", Percent: 30, Download: 7})
		return streamFrame{Type: "result", OpID: "op3", Engine: "ollama", Op: "pull"}, nil
	})

	frames := decodeFrames(t, rec.Body.String())
	require.Len(t, frames, 2, "want the request's own download's progress and the result")
	assert.Equal(t, streamFrame{Type: "progress", OpID: "op3", Engine: "ollama", Model: "demo", Op: "pull",
		Stage: "downloading", Percent: 30, Download: 7}, frames[0])
	assert.Equal(t, "result", frames[1].Type)
}

// pullStream runs a peer's pull request for demo on ex, as handlePull does,
// with download as the request's download, and returns the stream it wrote.
func pullStream(t *testing.T, ex *Executor, claimed context.Context, download func(context.Context) (json.RawMessage, error)) []streamFrame {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, controlPullPath, nil).WithContext(claimed)
	(&controlServer{exec: ex}).streamOp(rec, req, "op", "ollama", "pull", ex.claimedDownload(claimed), func(ctx context.Context) (streamFrame, error) {
		res, err := ex.trackedPull(ctx, "ollama", "demo", download)
		return streamFrame{Type: "result", OpID: "op", Engine: "ollama", Op: "pull", Result: res}, err
	})
	return decodeFrames(t, rec.Body.String())
}

func frameStages(frames []streamFrame) []string {
	stages := make([]string, 0, len(frames))
	for _, f := range frames {
		stages = append(stages, f.Type+":"+f.Stage)
	}
	return stages
}

// A retry sent after a cancel waits on the peer for the cancelled download to
// stop, with its stream already open, so the cancelled download's last frames
// reach that stream too. Relayed as the retry's, they told the initiator the
// retry had been cancelled before it began.
func TestStreamOp_CarriesNoFrameOfTheDownloadARetryReplaces(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	original, releaseOriginal := ex.claimPull(ctx, "ollama", "demo")
	defer releaseOriginal()
	downloading := make(chan struct{})
	stopping := make(chan struct{})
	finishStopping := make(chan struct{})
	originalDone := make(chan []streamFrame, 1)
	go func() {
		originalDone <- pullStream(t, ex, original, func(runCtx context.Context) (json.RawMessage, error) {
			ex.emitPullProgress(ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: "downloading", Percent: 40})
			close(downloading)
			<-runCtx.Done()
			close(stopping)
			<-finishStopping
			return nil, runCtx.Err()
		})
	}()
	<-downloading
	cancelled := make(chan error, 1)
	go func() { cancelled <- ex.CancelModelPull(ctx, "ollama", "demo") }()
	<-stopping
	retry, releaseRetry := ex.claimPull(ctx, "ollama", "demo")
	defer releaseRetry()
	retryDone := make(chan []streamFrame, 1)
	go func() {
		retryDone <- pullStream(t, ex, retry, func(runCtx context.Context) (json.RawMessage, error) {
			ex.emitPullProgress(ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: "downloading", Percent: 10})
			return succeedingPull(runCtx)
		})
	}()
	// Once the retry's stream is subscribed, the cancelled download's last
	// frames reach it.
	require.Eventually(t, func() bool {
		ex.progress.mu.Lock()
		defer ex.progress.mu.Unlock()
		return len(ex.progress.subs) == 2
	}, 10*time.Second, 10*time.Millisecond, "the retry's stream never opened")
	close(finishStopping)
	require.NoError(t, <-cancelled)

	assert.Equal(t, []string{"progress:downloading", "progress:cancelled", "result:"}, frameStages(<-originalDone))
	retryFrames := <-retryDone
	assert.Equal(t, []string{"progress:downloading", "progress:success", "result:"}, frameStages(retryFrames))
	assert.Equal(t, 10, retryFrames[0].Percent, "the retry's stream carried the cancelled download's progress")
}

// A duplicate request joins the download already in flight, and from then on
// that download's frames are its own.
func TestStreamOp_CarriesTheFramesOfTheDownloadADuplicateJoined(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	first, releaseFirst := ex.claimPull(ctx, "ollama", "demo")
	defer releaseFirst()
	downloading := make(chan struct{})
	finish := make(chan struct{})
	firstDone := make(chan []streamFrame, 1)
	go func() {
		firstDone <- pullStream(t, ex, first, func(runCtx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-finish
			ex.emitPullProgress(ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: "downloading", Percent: 90})
			return succeedingPull(runCtx)
		})
	}()
	<-downloading
	duplicate, releaseDuplicate := ex.claimPull(ctx, "ollama", "demo")
	defer releaseDuplicate()
	duplicateDone := make(chan []streamFrame, 1)
	go func() {
		duplicateDone <- pullStream(t, ex, duplicate, func(context.Context) (json.RawMessage, error) {
			return nil, errors.New("a duplicate ran a download of its own")
		})
	}()
	require.Eventually(t, func() bool { return ex.claimedDownload(duplicate)() != 0 }, 10*time.Second, 10*time.Millisecond,
		"the duplicate never joined the download")
	close(finish)

	firstFrames, duplicateFrames := <-firstDone, <-duplicateDone
	assert.Equal(t, []string{"progress:downloading", "progress:success", "result:"}, frameStages(duplicateFrames))
	assert.Equal(t, frameStages(firstFrames), frameStages(duplicateFrames))
	assert.Equal(t, firstFrames[0].Download, duplicateFrames[0].Download, "the two requests run as different downloads")
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
