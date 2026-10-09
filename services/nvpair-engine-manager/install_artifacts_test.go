// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type installProgressRecorder struct {
	mu     sync.Mutex
	events []any
}

func (r *installProgressRecorder) emit(method string, params any) {
	if method != "engine:install-progress" {
		return
	}
	r.mu.Lock()
	r.events = append(r.events, params)
	r.mu.Unlock()
}

func (r *installProgressRecorder) assertSingleTerminal(t *testing.T, want string) {
	t.Helper()
	r.mu.Lock()
	events := append([]any(nil), r.events...)
	r.mu.Unlock()

	stages := make([]string, 0, len(events))
	terminal := make([]string, 0, 1)
	for index, params := range events {
		event, ok := params.(map[string]any)
		require.True(t, ok, "install progress event %d params must be map[string]any", index)
		stage, ok := event["stage"].(string)
		require.True(t, ok, "install progress event %d stage must be a string", index)
		stages = append(stages, stage)
		switch stage {
		case "done", "already-installed", "failed":
			terminal = append(terminal, stage)
		}
	}
	require.Equal(t, []string{want}, terminal, "all install progress stages: %v", stages)
}

func TestInstallDownloadsAllNamedArtifactsBeforeRunning(t *testing.T) {
	payload := []byte("artifact")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	marker := filepath.Join(t.TempDir(), "installed.json")
	artifacts := []InstallArtifact{
		pinnedArtifact("server", server.URL+"/server.zip", payload),
		pinnedArtifact("cudart", server.URL+"/cudart.zip", payload),
	}
	manifest := artifactInstallManifest(t, "artifact-success", marker, artifacts)
	executor := newTestExecutor(t, manifest)
	progress := &installProgressRecorder{}
	executor.emit = progress.emit

	require.NoError(t, executor.Install(context.Background(), manifest.Engine), "install named artifacts")
	progress.assertSingleTerminal(t, "done")
	data, err := os.ReadFile(marker)
	require.NoError(t, err, "read captured install arguments")
	var downloads []string
	require.NoError(t, json.Unmarshal(data, &downloads), "decode captured install arguments")
	require.Len(t, downloads, 2)
	require.NotEqual(t, downloads[0], downloads[1], "artifacts must resolve to distinct paths")
	assertNoArtifactTemps(t, manifest.Engine)
}

func TestInstallRejectsBadSecondArtifactBeforeCommand(t *testing.T) {
	payload := []byte("artifact")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	marker := filepath.Join(t.TempDir(), "must-not-exist")
	artifacts := []InstallArtifact{
		pinnedArtifact("server", server.URL+"/server.zip", payload),
		{Name: "cudart", URL: server.URL + "/cudart.zip", SHA256: strings.Repeat("0", 64)},
	}
	manifest := artifactInstallManifest(t, "artifact-bad-checksum", marker, artifacts)
	executor := newTestExecutor(t, manifest)
	progress := &installProgressRecorder{}
	executor.emit = progress.emit

	require.ErrorContains(t, executor.Install(context.Background(), manifest.Engine), "checksum mismatch")
	progress.assertSingleTerminal(t, "failed")
	require.False(t, fileExists(marker), "install command ran after an artifact checksum failed")
	assertNoArtifactTemps(t, manifest.Engine)
}

func TestInstallCancellationRemovesDownloadedArtifacts(t *testing.T) {
	firstPayload := []byte("server")
	secondStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/server.zip" {
			_, _ = w.Write(firstPayload)
			return
		}
		_, _ = w.Write([]byte("partial"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		secondStarted <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()

	marker := filepath.Join(t.TempDir(), "must-not-exist")
	artifacts := []InstallArtifact{
		pinnedArtifact("server", server.URL+"/server.zip", firstPayload),
		{Name: "cudart", URL: server.URL + "/cudart.zip", SHA256: strings.Repeat("c", 64)},
	}
	manifest := artifactInstallManifest(t, "artifact-cancel", marker, artifacts)
	executor := newTestExecutor(t, manifest)
	progress := &installProgressRecorder{}
	executor.emit = progress.emit
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- executor.Install(ctx, manifest.Engine)
	}()

	select {
	case <-secondStarted:
		cancel()
	case <-time.After(5 * time.Second):
		require.FailNow(t, "second artifact download did not start")
	}
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "cancelled install did not return")
	}
	progress.assertSingleTerminal(t, "failed")
	assertNoArtifactTemps(t, manifest.Engine)
}

func pinnedArtifact(name, url string, payload []byte) InstallArtifact {
	sum := sha256.Sum256(payload)
	return InstallArtifact{Name: name, URL: url, SHA256: hex.EncodeToString(sum[:])}
}

func artifactInstallManifest(t *testing.T, engine, marker string, artifacts []InstallArtifact) *Manifest {
	t.Helper()
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Engine = engine
	manifest.DisplayName = "Artifact Test"
	platform := manifest.Platforms[hostKey()]
	platform.Detect = []string{marker}
	platform.Install = &Install{
		Artifacts: artifacts,
		Run:       []string{fakeEngineBin, "captureargs", marker, "{download_server}", "{download_cudart}"},
	}
	manifest.Platforms[hostKey()] = platform
	require.NoError(t, manifest.Validate(), "validate fixture")
	return manifest
}

func assertNoArtifactTemps(t *testing.T, engine string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "nvpair-engine-"+engine+"-*"))
	require.NoError(t, err, "glob temporary artifacts")
	require.Empty(t, matches, "temporary artifacts remain")
}
