// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLlamaCPPInstallPreservesDestinationPathWithSpaces(t *testing.T) {
	serverArchive := testZIP(t, "llama-server.exe", "server")
	cudartArchive := testZIP(t, "cudart64_12.dll", "runtime")
	archives := map[string][]byte{
		"/server.zip": serverArchive,
		"/cudart.zip": cudartArchive,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, ok := archives[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	registry := loadWithOverrides(t, t.TempDir())
	manifest, ok := registry.Get("llamacpp")
	if !ok {
		t.Fatal("llamacpp manifest not loaded")
	}
	platform, ok := manifest.Platforms[hostKey()]
	if !ok || platform.Install == nil {
		t.Fatalf("llamacpp install is unavailable for %s", hostKey())
	}
	platform.Install.Artifacts = []InstallArtifact{
		pinnedArtifact("server", server.URL+"/server.zip", serverArchive),
		pinnedArtifact("cudart", server.URL+"/cudart.zip", cudartArchive),
	}
	platform.Runtime.Port = 0
	manifest.Platforms[hostKey()] = platform
	if err := manifest.Validate(); err != nil {
		t.Fatalf("validate llama.cpp test manifest: %v", err)
	}

	baseDir := filepath.Join(t.TempDir(), "Nvidia Corporation", "Personal AI Router", "engine-bin")
	executor := NewExecutor(registry, NewReporter(nil), nil, baseDir)
	executor.detectTimeout = 2 * time.Second
	if err := executor.Install(context.Background(), "llamacpp"); err != nil {
		t.Fatalf("install llama.cpp into a spaced path: %v", err)
	}

	for name, want := range map[string]string{
		"llama-server.exe": "server",
		"cudart64_12.dll":  "runtime",
	} {
		data, err := os.ReadFile(filepath.Join(baseDir, "llamacpp", name))
		if err != nil {
			t.Fatalf("read extracted %s: %v", name, err)
		}
		if string(data) != want {
			t.Errorf("%s contents = %q, want %q", name, data, want)
		}
	}
}

func testZIP(t *testing.T, name, contents string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create(name)
	if err != nil {
		t.Fatalf("create zip entry %s: %v", name, err)
	}
	if _, err := file.Write([]byte(contents)); err != nil {
		t.Fatalf("write zip entry %s: %v", name, err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close zip archive: %v", err)
	}
	return buffer.Bytes()
}
