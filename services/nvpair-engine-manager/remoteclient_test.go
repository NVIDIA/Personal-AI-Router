// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteStartUsesReadinessHeaderBudget(t *testing.T) {
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
func TestRemoteSlowModelOperationsUseReadinessHeaderBudget(t *testing.T) {
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

func TestRemoteReadinessBudgetCoversEngineStartupAllowance(t *testing.T) {
	require.Greater(t, remoteReadyResponseHeaderTimeout, remoteResponseHeaderTimeout, "readiness budget")
	cases := []struct {
		path   string
		engine string
		want   bool
	}{
		{controlStartPath, "ollama", true},
		{controlDeletePath, "lmstudio", true},
		{controlLoadPath, "ollama", true},
		{controlLoadPath, "lmstudio", false},
		{controlStopPath, "ollama", false},
		{controlUnloadPath, "ollama", false},
		{controlEnginesPath, "", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, waitsForEngineReadiness(tc.path, tc.engine), "waitsForEngineReadiness")
	}
}

func TestRemoteStartHeaderWaitRemainsBounded(t *testing.T) {
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
