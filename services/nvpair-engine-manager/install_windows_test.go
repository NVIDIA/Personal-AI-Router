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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.True(t, ok, "llamacpp manifest not loaded")
	platform, ok := manifest.Platforms[hostKey()]
	require.True(t, ok, "llamacpp install is unavailable for %s", hostKey())
	require.NotNil(t, platform.Install, "llamacpp install is unavailable for %s", hostKey())
	platform.Install.Artifacts = []InstallArtifact{
		pinnedArtifact("server", server.URL+"/server.zip", serverArchive),
		pinnedArtifact("cudart", server.URL+"/cudart.zip", cudartArchive),
	}
	platform.Runtime.Port = 0
	manifest.Platforms[hostKey()] = platform
	require.NoError(t, manifest.Validate(), "validate llama.cpp test manifest")

	baseDir := filepath.Join(t.TempDir(), "Nvidia Corporation", "Personal AI Router", "engine-bin")
	executor := NewExecutor(registry, NewReporter(nil), nil, baseDir)
	executor.detectTimeout = 2 * time.Second
	require.NoError(t, executor.Install(context.Background(), "llamacpp"), "install llama.cpp into a spaced path")

	for name, want := range map[string]string{
		"llama-server.exe": "server",
		"cudart64_12.dll":  "runtime",
	} {
		data, err := os.ReadFile(filepath.Join(baseDir, "llamacpp", name))
		require.NoError(t, err, "read extracted %s", name)
		assert.Equal(t, want, string(data), "%s contents", name)
	}
}

func testZIP(t *testing.T, name, contents string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create(name)
	require.NoError(t, err, "create zip entry %s", name)
	_, err = file.Write([]byte(contents))
	require.NoError(t, err, "write zip entry %s", name)
	require.NoError(t, archive.Close(), "close zip archive")
	return buffer.Bytes()
}
