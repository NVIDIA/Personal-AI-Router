// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
		if !ok {
			t.Fatalf("install progress event %d params type = %T, want map[string]any", index, params)
		}
		stage, ok := event["stage"].(string)
		if !ok {
			t.Fatalf("install progress event %d stage = %v, want string", index, event["stage"])
		}
		stages = append(stages, stage)
		switch stage {
		case "done", "already-installed", "failed":
			terminal = append(terminal, stage)
		}
	}
	if len(terminal) != 1 || terminal[0] != want {
		t.Fatalf("terminal install progress stages = %v, want [%s]; all stages = %v", terminal, want, stages)
	}
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

	if err := executor.Install(context.Background(), manifest.Engine); err != nil {
		t.Fatalf("install named artifacts: %v", err)
	}
	progress.assertSingleTerminal(t, "done")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read captured install arguments: %v", err)
	}
	var downloads []string
	if err := json.Unmarshal(data, &downloads); err != nil {
		t.Fatalf("decode captured install arguments: %v", err)
	}
	if len(downloads) != 2 || downloads[0] == downloads[1] {
		t.Fatalf("resolved artifact paths = %v", downloads)
	}
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

	err := executor.Install(context.Background(), manifest.Engine)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("install error = %v, want checksum mismatch", err)
	}
	progress.assertSingleTerminal(t, "failed")
	if fileExists(marker) {
		t.Fatal("install command ran after an artifact checksum failed")
	}
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
		t.Fatal("second artifact download did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("install error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled install did not return")
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
	if err := manifest.Validate(); err != nil {
		t.Fatalf("validate fixture: %v", err)
	}
	return manifest
}

func assertNoArtifactTemps(t *testing.T, engine string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "nvpair-engine-"+engine+"-*"))
	if err != nil {
		t.Fatalf("glob temporary artifacts: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary artifacts remain: %v", matches)
	}
}
