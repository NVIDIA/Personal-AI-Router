// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

func TestLlamaCPPPullRemoteDisconnectStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	stopped := make(chan struct{})
	f.unload = func(w http.ResponseWriter, _ *http.Request) {
		f.downloading.Store(false)
		_, err := fmt.Fprint(w, `{"success":true}`)
		assert.NoError(t, err, "write stop response")
		close(stopped)
	}
	s := &controlServer{exec: f.ex}
	server := httptest.NewServer(http.HandlerFunc(s.handlePull))
	t.Cleanup(server.Close)
	requestLlamaCPPPullAndDisconnect(t, server.Client(), server.URL, f.started)
	waitLlamaCPPPullSignal(t, stopped)
	require.False(t, f.downloading.Load(), "remote disconnect must stop download")
	require.Equal(t, int32(1), f.unloads.Load())
}

// Exercise the real ec listener, pinned mTLS, request cancellation, and cleanup
// in the compiled manager, without a real engine or model download.
func TestE2ELlamaCPPPullRemoteDisconnectStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	stopped := make(chan struct{})
	f.unload = func(w http.ResponseWriter, _ *http.Request) {
		f.downloading.Store(false)
		_, err := fmt.Fprint(w, `{"success":true}`)
		assert.NoError(t, err, "write stop response")
		close(stopped)
	}
	serverCert, serverKey := mintLeaf(t, "pull-server")
	clientCert, clientKey := mintLeaf(t, "pull-client")
	serverDir := clusterDirFor(t, serverCert, serverKey, map[string][]byte{"pull-client": clientCert})
	clientDir := clusterDirFor(t, clientCert, clientKey, map[string][]byte{"pull-server": serverCert})
	controlPort, err := freePort()
	require.NoError(t, err, "allocate ec port")
	state, err := f.ex.state("fake")
	require.NoError(t, err, "resolve fake router")
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Actions[pullModelAction] = Action{
		HTTP:             &ActionHTTP{Method: http.MethodPost, Path: "/models"},
		ProgressProtocol: pullProgressProtocolLlamaCPPModelsSSE,
	}
	for key, platform := range manifest.Platforms {
		platform.Runtime.Port = state.port
		platform.Runtime.Ready.HTTP = "http://127.0.0.1:{port}/health"
		platform.Runtime.Health = nil
		manifest.Platforms[key] = platform
	}
	cfg, home := t.TempDir(), t.TempDir()
	for _, dir := range []string{
		filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		writeE2EManifest(t, dir, manifest)
	}
	manager := startE2EManager(t, cfg, home, "--control-port", strconv.Itoa(controlPort), "--cluster-dir", serverDir, "--loaded-poll-interval", "0")
	// The fake router is already listening. Start adopts it using its readiness
	// probe; the child never spawns a real engine or another fake listener.
	send(t, manager.stdin, 1, "engine:start", map[string]string{"engine": "fake"})
	var status EngineStatus
	require.NoError(t, json.Unmarshal(waitResult(t, manager.frames, "1", 10*time.Second), &status), "decode start status")
	require.True(t, status.Running, "fake router must be running")
	require.Equal(t, state.port, status.Port)
	waitPortServing(t, controlPort)
	tlsConfig, ok := clustertrust.Open(clientDir).ClientTLSConfig("pull-server")
	require.True(t, ok, "client could not resolve pinned server TLS configuration")
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	requestLlamaCPPPullAndDisconnect(t, client, fmt.Sprintf("https://127.0.0.1:%d", controlPort), f.started)
	waitLlamaCPPPullSignal(t, stopped)
	require.False(t, f.downloading.Load(), "compiled manager must stop download")
	require.Equal(t, int32(1), f.unloads.Load())
	// Shut the fixture down first: the adopted listener is owned by this test,
	// and manager shutdown must not attempt to terminate the test process.
	f.server.Close()
	manager.stop(t)
}

func requestLlamaCPPPullAndDisconnect(t *testing.T, client *http.Client, baseURL string, started <-chan struct{}) {
	t.Helper()
	body, err := json.Marshal(pullRequest{OpID: "disconnect-test", Engine: "fake", Model: llamaCPPPullTestModel})
	require.NoError(t, err, "encode remote pull")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+controlPullPath, bytes.NewReader(body))
	require.NoError(t, err, "create remote pull")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err, "start remote pull")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	waitLlamaCPPPullSignal(t, started)
	require.NoError(t, resp.Body.Close(), "disconnect remote pull")
}
