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
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLlamaCPPInstall_PreservesShellArtifactArguments(t *testing.T) {
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

// The vLLM wheel is a CUDA 13 build, so an older driver installs cleanly and
// then fails at startup, after a multi-GB download. The install script has to
// refuse before downloading anything. This runs the manifest's own script, with
// its placeholders resolved the way the executor resolves them, against a fake
// nvidia-smi. Recent drivers print "CUDA UMD Version" and mark the older
// "CUDA Version" field deprecated, so both spellings have to be understood.
func TestVLLMInstall_RefusesDriversWithoutCUDA13(t *testing.T) {
	tools := map[string]string{}
	for _, name := range []string{"sh", "awk", "df", "mkdir", "tar"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not available", name)
		}
		tools[name] = path
	}
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	require.True(t, ok, "vllm manifest not loaded")

	// runInstall runs the Linux install script with only the tools above and,
	// unless smiOutput is nil, a fake nvidia-smi that prints it.
	runInstall := func(t *testing.T, smiOutput *string) (text string, runErr error, installDir string) {
		t.Helper()
		bin := t.TempDir()
		for name, path := range tools {
			if name != "sh" {
				require.NoError(t, os.Symlink(path, filepath.Join(bin, name)))
			}
		}
		if smiOutput != nil {
			script := "#!/bin/sh\nprintf '%s' '" + *smiOutput + "'\n"
			require.NoError(t, os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(script), 0o755))
		}
		installDir = t.TempDir()
		argv, err := resolveArgs(manifest.Platforms["linux/amd64"].Install.Run, map[string]string{
			"download":    filepath.Join(installDir, "missing-uv.tar.gz"),
			"install_dir": installDir,
		})
		require.NoError(t, err)
		// The executor expands OS environment references in every install
		// argument after resolving placeholders (install.go), so "$name" in
		// the script would be replaced before the shell ever saw it. Apply
		// the same expansion, and require it to be a no-op: only positional
		// parameters and "${name:-default}" forms survive it.
		for i := range argv {
			require.Equal(t, argv[i], expandPath(argv[i]), "the executor rewrites install argument %d, which breaks the script", i)
		}
		cmd := exec.Command(tools["sh"], argv[1:]...)
		cmd.Env = []string{"PATH=" + bin}
		out, runErr := cmd.CombinedOutput()
		return string(out), runErr, installDir
	}
	accepted := func(name, smiOutput string) {
		t.Run(name, func(t *testing.T) {
			text, _, _ := runInstall(t, &smiOutput)

			// The driver check passed. The script goes on to fail later,
			// because the archive does not exist; it must not blame the driver.
			assert.NotContains(t, text, "NVIDIA driver", "a CUDA 13 driver was refused")
		})
	}
	refused := func(name string, smiOutput *string, wantInText string) {
		t.Run(name, func(t *testing.T) {
			text, runErr, installDir := runInstall(t, smiOutput)

			require.Error(t, runErr, "install was not refused: %s", text)
			assert.Contains(t, text, wantInText)
			assert.NoFileExists(t, filepath.Join(installDir, "uv"), "the script started installing before it refused")
		})
	}
	accepted("current driver format", "CUDA Version                                           : 13.4 [Deprecated; will be removed in CUDA 14.0. Use CUDA UMD Version instead]\nCUDA UMD Version                                       : 13.4\n")
	accepted("older CUDA 13 driver format", "CUDA Version                                           : 13.0\n")
	accepted("UMD only", "CUDA UMD Version                                       : 14.0\n")
	refused("no nvidia-smi", nil, "nvidia-smi not found")
	refused("old driver", ptr("CUDA Version                                           : 12.4\n"), "CUDA 13 or newer (found: 12)")
	refused("unrecognised output", ptr("Driver Version : 1.2.3\n"), "CUDA 13 or newer (found: none)")
}

func ptr(s string) *string { return &s }
