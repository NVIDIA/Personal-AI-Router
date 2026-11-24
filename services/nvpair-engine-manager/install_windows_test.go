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

func TestOllamaDirectoryRename_WaitsForWindowsToReleaseTheDirectory(t *testing.T) {
	parent := t.TempDir()
	staged := filepath.Join(parent, "staged")
	installed := filepath.Join(parent, "ollama")
	require.NoError(t, os.Mkdir(staged, 0o700))
	// A directory opened without delete sharing reproduces the handle left
	// briefly open after Windows finishes executing the staged binary.
	handle, err := os.Open(staged)
	require.NoError(t, err)
	closed := make(chan error, 1)
	time.AfterFunc(200*time.Millisecond, func() { closed <- handle.Close() })

	err = renameEngineDirectory(staged, installed)

	require.NoError(t, <-closed)
	require.NoError(t, err)
	assert.DirExists(t, installed)
}

func TestReplaceOllamaInstall_RestoresCurrentVersionWhenStagingRemainsLocked(t *testing.T) {
	e, binary := ollamaUpdateFixture(t, "1.1.0", false)
	st, err := e.state("ollama")
	require.NoError(t, err)
	staged := t.TempDir()
	handle, err := os.Open(staged)
	require.NoError(t, err)

	err = e.replaceEngineInstall(t.Context(), st, staged, false)

	require.NoError(t, handle.Close())
	require.Error(t, err)
	version, err := ollamaBinaryVersion(t.Context(), binary)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
	assert.NoDirExists(t, st.installDir+".update-backup")
}

func TestLlamaCPPInstall_ExtractsBothArchivesWithWindowsPowerShell(t *testing.T) {
	// A parent shell can supply an Archive module incompatible with powershell.exe.
	modulePath := t.TempDir()
	module := filepath.Join(modulePath, "Microsoft.PowerShell.Archive")
	require.NoError(t, os.Mkdir(module, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(module, "Microsoft.PowerShell.Archive.psm1"),
		[]byte("function Expand-Archive { throw 'Incompatible Archive module' }"), 0o600))
	t.Setenv("PSModulePath", modulePath)

	serverArchive := testZIP(t, map[string]string{"llama-server.exe": "server", "LICENSE": "server license"})
	cudartArchive := testZIP(t, map[string]string{"cudart64_12.dll": "runtime", "LICENSE": "runtime license"})
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
		"LICENSE":          "runtime license",
	} {
		data, err := os.ReadFile(filepath.Join(baseDir, "llamacpp", name))
		require.NoError(t, err, "read extracted %s", name)
		assert.Equal(t, want, string(data), "%s contents", name)
	}
}

func testZIP(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, contents := range files {
		file, err := archive.Create(name)
		require.NoError(t, err, "create zip entry %s", name)
		_, err = file.Write([]byte(contents))
		require.NoError(t, err, "write zip entry %s", name)
	}
	require.NoError(t, archive.Close(), "close zip archive")
	return buffer.Bytes()
}
