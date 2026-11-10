// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteStream_AcceptsAccumulatedCLIOutput(t *testing.T) {
	// LM Studio returns accumulated CLI output in one terminal frame. A
	// representative 6.4 MiB result catches the former 1 MiB frame cap.
	wantOutput := strings.Repeat("x", (64<<20)/10)
	resultJSON, err := json.Marshal(wantOutput)
	require.NoError(t, err, "encode command output")
	c := newRemoteStreamTestClient(t, streamFrame{
		Type: "result", Result: resultJSON,
	})
	result, err := c.stream(t.Context(), controlPullPath, pullRequest{Engine: "lmstudio", Model: "test-model"}, nil)
	require.NoError(t, err, "read 6.4 MiB command result")
	var gotOutput string
	require.NoError(t, json.Unmarshal(result.Result, &gotOutput), "decode command output")
	assert.Equal(t, wantOutput, gotOutput)
}

func TestRemoteStream_RejectsFramesAboveWorkerLimit(t *testing.T) {
	// Raising the cap for long downloads must still reject an oversized frame
	// before relaying it to progress consumers.
	c := newRemoteStreamTestClient(t,
		streamFrame{Type: "progress", Message: strings.Repeat("x", maxFrameBytes+1)},
		streamFrame{Type: "result", Result: json.RawMessage(`{"downloaded":true}`)},
	)
	progressCount := 0
	_, err := c.stream(t.Context(), controlPullPath, pullRequest{Engine: "lmstudio", Model: "test-model"}, func(streamFrame) {
		progressCount++
	})
	assert.ErrorIs(t, err, bufio.ErrTooLong)
	assert.Zero(t, progressCount, "oversized progress frames must not be relayed")
}

func TestRemoteStream_AllowsStreamLargerThanFrameLimit(t *testing.T) {
	// Individually small progress frames can exceed the cap in total. A
	// cumulative response limit would incorrectly fail a long operation.
	const messageBytes = 512 << 10
	const progressCount = maxFrameBytes/messageBytes + 1
	wantMessage := strings.Repeat("x", messageBytes)
	frames := make([]streamFrame, 0, progressCount+1)
	for range progressCount {
		frames = append(frames, streamFrame{
			Type: "progress", Message: wantMessage,
		})
	}
	frames = append(frames, streamFrame{Type: "result", Result: json.RawMessage(`{"downloaded":true}`)})
	c := newRemoteStreamTestClient(t, frames...)
	received := 0
	result, err := c.stream(t.Context(), controlPullPath, pullRequest{Engine: "lmstudio", Model: "test-model"}, func(frame streamFrame) {
		assert.Equal(t, wantMessage, frame.Message, "progress frame %d", received)
		received++
	})
	require.NoError(t, err, "read stream exceeding the per-frame limit in total")
	assert.Equal(t, progressCount, received)
	var gotResult struct {
		Downloaded bool `json:"downloaded"`
	}
	require.NoError(t, json.Unmarshal(result.Result, &gotResult), "decode terminal result")
	assert.True(t, gotResult.Downloaded, "terminal result must report a completed download")
}

// newRemoteStreamTestClient serves deterministic NDJSON in memory so size tests
// exercise the production response reader without binding ports or running an engine.
func newRemoteStreamTestClient(t *testing.T, frames ...streamFrame) *remoteClient {
	t.Helper()
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, frame := range frames {
		require.NoError(t, enc.Encode(frame), "encode %q frame", frame.Type)
	}
	return &remoteClient{
		http: &http.Client{Transport: remoteStreamTestTransport{body: body.Bytes()}},
		base: "http://peer.test",
	}
}

type remoteStreamTestTransport struct {
	body []byte
}

func (tr remoteStreamTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/x-ndjson"}},
		Body:       io.NopCloser(bytes.NewReader(tr.body)),
		Request:    req,
	}, nil
}

func TestRemoteStart_UsesReadinessHeaderBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, []byte(`{"engine":"remote-only","running":true,"healthy":true}`))
	}))
	defer srv.Close()

	base := http.DefaultTransport.(*http.Transport)
	c := &remoteClient{
		http:      newRemoteHTTPClient(base, 20*time.Millisecond),
		readyHTTP: newRemoteHTTPClient(base, time.Second),
		base:      srv.URL,
	}
	_, err := c.postJSON(context.Background(), controlStartPath, "remote-only", startRequest{Engine: "remote-only"})
	require.NoError(t, err, "remote start was cut off by the ordinary response-header budget")
	_, err = c.postJSON(context.Background(), controlStopPath, "ollama", stopRequest{Engine: "ollama"})
	require.ErrorContains(t, err, "timeout awaiting response headers", "ordinary remote call error")
}

// A remote Ollama load can withhold headers while a cold model loads. A remote
// LM Studio delete can likewise restart the peer's engine before it replies.
// Both need the longer response-header budget; other model calls remain bounded.
func TestRemoteModelOperations_AllowReadinessResponseHeaderBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, []byte(`{"ok":true}`))
	}))
	defer srv.Close()

	base := http.DefaultTransport.(*http.Transport)
	c := &remoteClient{
		http:      newRemoteHTTPClient(base, 20*time.Millisecond),
		readyHTTP: newRemoteHTTPClient(base, time.Second),
		base:      srv.URL,
	}
	ollama := modelActionRequest{Engine: "ollama", Model: "qwen2.5"}
	_, err := c.postJSON(context.Background(), controlLoadPath, "ollama", ollama)
	require.NoError(t, err, "remote load was cut off by the ordinary response-header budget")
	lmstudio := modelActionRequest{Engine: "lmstudio", Model: "qwen2.5"}
	_, err = c.postJSON(context.Background(), controlDeletePath, "lmstudio", lmstudio)
	require.NoError(t, err, "remote delete was cut off by the ordinary response-header budget")
	_, err = c.postJSON(context.Background(), controlLoadPath, "lmstudio", lmstudio)
	require.ErrorContains(t, err, "timeout awaiting response headers", "remote LM Studio load error")
	_, err = c.postJSON(context.Background(), controlUnloadPath, "ollama", ollama)
	require.ErrorContains(t, err, "timeout awaiting response headers", "remote unload error")
}

func TestRemoteReadinessBudget_CoversEngineStartupAllowance(t *testing.T) {
	require.Greater(t, remoteReadyResponseHeaderTimeout, remoteResponseHeaderTimeout, "readiness budget")
	test := func(name, path, engine string, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, waitsForEngineReadiness(path, engine))
		})
	}
	test("start waits", controlStartPath, "ollama", true)
	test("delete waits", controlDeletePath, "lmstudio", true)
	test("Ollama load waits", controlLoadPath, "ollama", true)
	test("vLLM load waits", controlLoadPath, "vllm", true)
	test("LM Studio load does not wait", controlLoadPath, "lmstudio", false)
	test("stop does not wait", controlStopPath, "ollama", false)
	test("unload does not wait", controlUnloadPath, "ollama", false)
	test("listing does not wait", controlEnginesPath, "", false)
}

func TestRemoteStartHeaderWait_RemainsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		writeTestResponse(t, w, []byte(`{"engine":"ollama"}`))
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	base := http.DefaultTransport.(*http.Transport)
	c := &remoteClient{
		http:      newRemoteHTTPClient(base, time.Second),
		readyHTTP: newRemoteHTTPClient(base, 50*time.Millisecond),
		base:      srv.URL,
	}
	started := time.Now()
	_, err := c.postJSON(context.Background(), controlStartPath, "ollama", startRequest{Engine: "ollama"})
	require.ErrorContains(t, err, "timeout awaiting response headers", "remote start error")
	require.LessOrEqual(t, time.Since(started), time.Second, "remote start took")
}
