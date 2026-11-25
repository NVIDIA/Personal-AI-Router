// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func TestRemoteUpdate_ChecksAndUpdatesOnlyTheSelectedPeer(t *testing.T) {
	controllerDir, targetDir := t.TempDir(), t.TempDir()
	clustertrusttest.Join(t, controllerDir, "cluster", "controller")
	clustertrusttest.Join(t, targetDir, "cluster", "target")
	writePinFromCert(t, controllerDir, "target", filepath.Join(targetDir, "node.crt"))
	writePinFromCert(t, targetDir, "controller", filepath.Join(controllerDir, "node.crt"))

	controller, controllerBinary := ollamaUpdateFixture(t, "1.1.0", false)
	require.NoError(t, os.WriteFile(controllerBinary+".version", []byte("0.9.0"), 0o600))
	target, targetBinary := ollamaUpdateFixture(t, "1.1.0", false)
	mesh := clustertrust.Open(targetDir)
	server := httptest.NewUnstartedServer((&controlServer{exec: target, mesh: mesh}).mux())
	server.TLS = mesh.ServerTLSConfig()
	server.StartTLS()
	t.Cleanup(server.Close)

	var output bytes.Buffer
	manager := NewManager(NewCodec(&output), controller, clustertrust.Open(controllerDir))
	t.Cleanup(manager.remoteHTTP.CloseIdle)
	t.Cleanup(manager.readyHTTP.CloseIdle)
	addr, err := net.ResolveTCPAddr("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	manager.peers.set([]noderec.DirectoryNode{{
		HostUUID: "target-host", ClusterUUID: "target", IP: addr.IP.String(),
		IdentityVerification: noderec.IdentityPinned, Trusted: true,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: addr.Port}},
	}})
	id := json.RawMessage("1")
	request := &Message{ID: &id, Method: "engine:remote-check-updates", Params: json.RawMessage(`{"node":"target-host","engine":"ollama"}`)}
	manager.runRemote(t.Context(), request, remoteOrder{})
	var check Message
	require.NoError(t, json.NewDecoder(&output).Decode(&check))
	require.Nil(t, check.Error)
	assert.JSONEq(t, `{"updateAvailable":{"currentVersion":"1.0.0","latestVersion":"1.1.0","installType":"managed","releaseUrl":"https://github.com/ollama/ollama/releases/tag/v1.1.0"}}`, string(check.Result))

	request.Method = "engine:remote-update"
	manager.runRemote(t.Context(), request, remoteOrder{})
	var progress []remoteProgress
	var updated struct {
		OpID   string       `json:"opId"`
		Status EngineStatus `json:"status"`
	}
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var msg Message
		require.NoError(t, decoder.Decode(&msg))
		require.Nil(t, msg.Error)
		if msg.ID != nil {
			require.NoError(t, json.Unmarshal(msg.Result, &updated))
			assert.False(t, decoder.More(), "the request ends with its result")
			break
		}
		require.Equal(t, "engine:remote-progress", msg.Method)
		var event remoteProgress
		require.NoError(t, json.Unmarshal(msg.Params, &event))
		progress = append(progress, event)
	}
	require.NotEmpty(t, updated.OpID)
	assert.Equal(t, "ollama", updated.Status.Engine)
	assert.True(t, updated.Status.Installed)
	assert.False(t, updated.Status.Running)
	require.NotEmpty(t, progress)
	for _, event := range progress {
		assert.Equal(t, "target-host", event.Node)
		assert.Equal(t, "ollama", event.Engine)
		assert.Equal(t, "update", event.Op)
		assert.Equal(t, updated.OpID, event.OpID)
	}
	targetVersion, err := ollamaBinaryVersion(t.Context(), targetBinary)
	require.NoError(t, err)
	assert.Equal(t, "1.1.0", targetVersion)
	controllerVersion, err := ollamaBinaryVersion(t.Context(), controllerBinary)
	require.NoError(t, err)
	assert.Equal(t, "0.9.0", controllerVersion)
}

func TestControlUpdate_OffersVendorDownloadAndRefusesToReplaceExternalEngine(t *testing.T) {
	target, bin := ollamaUpdateFixture(t, "1.1.0", false)
	require.NoError(t, os.Remove(installMarkerPath(filepath.Dir(bin))))
	server := &controlServer{exec: target}
	check := httptest.NewRecorder()
	server.handleCheckUpdates(check, httptest.NewRequest(http.MethodPost, controlCheckUpdatesPath, strings.NewReader(`{"engine":"ollama"}`)))
	require.Equal(t, http.StatusOK, check.Code)
	assert.JSONEq(t, `{"updateAvailable":{"currentVersion":"1.0.0","latestVersion":"1.1.0","installType":"external","downloadUrl":"https://ollama.com/download","releaseUrl":"https://github.com/ollama/ollama/releases/tag/v1.1.0"}}`, check.Body.String())

	update := httptest.NewRecorder()
	server.handleUpdate(update, httptest.NewRequest(http.MethodPost, controlUpdatePath, strings.NewReader(`{"engine":"ollama","opId":"external-update"}`)))
	frames := decodeFrames(t, update.Body.String())
	require.NotEmpty(t, frames)
	terminal := frames[len(frames)-1]
	assert.Equal(t, "error", terminal.Type)
	assert.Equal(t, "external-update", terminal.OpID)
	assert.Contains(t, terminal.Message, "PAIR-managed")
	version, err := ollamaBinaryVersion(t.Context(), bin)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
}

func TestRemoteUpdate_RelaysOnlyProgressFromItsOwnOperation(t *testing.T) {
	exec := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	var localProgress installProgressRecorder
	exec.emit = localProgress.emit
	state, err := exec.state("fake")
	require.NoError(t, err)
	// An install already holds the engine lock when the peer subscribes.
	state.lockOperation()
	installEvents, unsubscribe := exec.progress.subscribe("fake")
	defer unsubscribe()
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, controlUpdatePath, nil)
	(&controlServer{exec: exec}).streamOp(rec, request, "op1", "fake", "update", nil, func(ctx context.Context) (streamFrame, error) {
		exec.emitInstallProgress("fake", "installing", 80)
		state.unlockOperation()
		err := exec.withEngineUpdate(ctx, "fake", func(context.Context, *engineState) error {
			exec.emitPullProgress(ProgressEvent{Engine: "fake", Op: "pull", Model: "demo", Stage: "downloading", Percent: 30})
			exec.emitInstallProgress("fake", "verified", 50)
			return nil
		})
		exec.emitInstallProgress("fake", "downloading", 10)
		return streamFrame{Type: "result"}, err
	})

	require.Equal(t, []streamFrame{
		{Type: "progress", OpID: "op1", Engine: "fake", Op: "update", Stage: "verified", Percent: 50},
		{Type: "progress", OpID: "op1", Engine: "fake", Op: "update", Stage: "done", Percent: 100},
		{Type: "result"},
	}, decodeFrames(t, rec.Body.String()))
	assert.Equal(t, []ProgressEvent{
		{Engine: "fake", Op: "install", Stage: "installing", Percent: 80},
		{Engine: "fake", Op: "pull", Model: "demo", Stage: "downloading", Percent: 30},
		{Engine: "fake", Op: "install", Stage: "downloading", Percent: 10},
	}, publishedFrames(installEvents), "an install subscriber must not receive the update's installer frames")
	assert.Contains(t, localProgress.events, map[string]any{"engine": "fake", "stage": "verified", "percent": 50})
	assert.Contains(t, localProgress.events, map[string]any{"engine": "fake", "stage": "done", "percent": 100})
}

func TestControlUpdateRoutes_RejectUnpinnedRequests(t *testing.T) {
	mux := (&controlServer{mesh: clustertrust.Open(t.TempDir())}).mux()
	for _, path := range []string{controlCheckUpdatesPath, controlUpdatePath} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"engine":"ollama"}`)))
			assert.Equal(t, http.StatusForbidden, rec.Code)
		})
	}
}
