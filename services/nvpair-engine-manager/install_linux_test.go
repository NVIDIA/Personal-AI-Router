// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLlamaCPPInstallPreservesShellArtifactArguments(t *testing.T) {
	serverArchive := testTarGZIP(t, "build/bin/llama-server", "server")
	cudartArchive := testTarGZIP(t, "build/bin/libcudart.so.12", "runtime")
	archives := map[string][]byte{
		"/server.tar.gz": serverArchive,
		"/cudart.tar.gz": cudartArchive,
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
		pinnedArtifact("server", server.URL+"/server.tar.gz", serverArchive),
		pinnedArtifact("cudart", server.URL+"/cudart.tar.gz", cudartArchive),
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
		t.Fatalf("install llama.cpp with shell artifact arguments: %v", err)
	}

	for name, want := range map[string]string{
		"llama-server":    "server",
		"libcudart.so.12": "runtime",
	} {
		data, err := os.ReadFile(filepath.Join(baseDir, "llamacpp", "build", "bin", name))
		if err != nil {
			t.Fatalf("read extracted %s: %v", name, err)
		}
		if string(data) != want {
			t.Errorf("%s contents = %q, want %q", name, data, want)
		}
	}
	assertNoArtifactTemps(t, manifest.Engine)
}

func testTarGZIP(t *testing.T, name, contents string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressor)
	data := []byte(contents)
	if err := archive.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(data)),
	}); err != nil {
		t.Fatalf("write tar header for %s: %v", name, err)
	}
	if _, err := archive.Write(data); err != nil {
		t.Fatalf("write tar contents for %s: %v", name, err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close tar archive for %s: %v", name, err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatalf("close gzip stream for %s: %v", name, err)
	}
	return buffer.Bytes()
}
