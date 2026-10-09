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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLlamaCPPInstallPreservesShellArtifactArguments(t *testing.T) {
	// Both archives wrap their contents in one top-level directory, as every
	// llama.cpp release tarball does, and the two wrappers are named
	// differently — the server's after the build tag, the CUDA runtime's after
	// the whole artifact. The install strips one component from each so the
	// binaries and their libraries land side by side in the install directory,
	// which is where detect and runtime.bin look.
	serverArchive := testTarGZIP(t, "llama-b11146/llama-server", "server")
	cudartArchive := testTarGZIP(t, "cudart-llama-b11146-bin-ubuntu-cuda-12.8-x64/libcudart.so.12", "runtime")
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
		writeTestResponse(t, w, payload)
	}))
	defer server.Close()

	registry := loadWithOverrides(t, t.TempDir())
	manifest, ok := registry.Get("llamacpp")
	require.True(t, ok, "llamacpp manifest not loaded")
	platform, ok := manifest.Platforms[hostKey()]
	require.True(t, ok, "llamacpp install is unavailable for %s", hostKey())
	require.NotNil(t, platform.Install, "llamacpp install is unavailable for %s", hostKey())
	platform.Install.Artifacts = []InstallArtifact{
		pinnedArtifact("server", server.URL+"/server.tar.gz", serverArchive),
		pinnedArtifact("cudart", server.URL+"/cudart.tar.gz", cudartArchive),
	}
	platform.Runtime.Port = 0
	manifest.Platforms[hostKey()] = platform
	require.NoError(t, manifest.Validate(), "validate llama.cpp test manifest")

	baseDir := filepath.Join(t.TempDir(), "Nvidia Corporation", "Personal AI Router", "engine-bin")
	executor := NewExecutor(registry, NewReporter(nil), nil, baseDir)
	executor.detectTimeout = 2 * time.Second
	require.NoError(t, executor.Install(context.Background(), "llamacpp"), "install llama.cpp with shell artifact arguments")

	for name, want := range map[string]string{
		"llama-server":    "server",
		"libcudart.so.12": "runtime",
	} {
		data, err := os.ReadFile(filepath.Join(baseDir, "llamacpp", name))
		require.NoError(t, err, "read extracted %s", name)
		assert.Equal(t, want, string(data), "%s contents", name)
	}
	assertNoArtifactTemps(t, manifest.Engine)
}

func testTarGZIP(t *testing.T, name, contents string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressor)
	data := []byte(contents)
	require.NoError(t, archive.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(data)),
	}), "write tar header for %s", name)
	_, err := archive.Write(data)
	require.NoError(t, err, "write tar contents for %s", name)
	require.NoError(t, archive.Close(), "close tar archive for %s", name)
	require.NoError(t, compressor.Close(), "close gzip stream for %s", name)
	return buffer.Bytes()
}
