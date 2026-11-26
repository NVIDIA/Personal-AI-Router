// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mockLlamaRelease(t *testing.T, e *Executor, tag string, assets []githubReleaseAsset, downloads map[string][]byte) {
	t.Helper()
	release, err := json.Marshal(githubRelease{Tag: tag, Prerelease: true, Assets: assets})
	require.NoError(t, err)
	transport := e.client.Transport
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		switch req.URL.String() {
		case latestLlamaBuildURL:
			body = []byte(tag + "\n")
		case "https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/" + tag:
			body = release
		default:
			if req.URL.Hostname() == "127.0.0.1" {
				return transport.RoundTrip(req)
			}
			download, exists := downloads[req.URL.String()]
			if !exists {
				return nil, fmt.Errorf("unexpected download: %s", req.URL)
			}
			body = download
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
	})
}

func llamaTestAsset(tag, name string, data []byte) githubReleaseAsset {
	return githubReleaseAsset{Name: name, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), BrowserDownloadURL: llamaReleasesURL + "/download/" + tag + "/" + name}
}

func TestCheckLlamaCppUpdate_LinksAnAdoptedServerToTheStableBinaryRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/props" || req.Header.Get(engineIdentityProbeHeader) != "1" {
			http.NotFound(w, req)
			return
		}
		_, err := io.WriteString(w, `{"role":"router","build_info":"b99-abc123"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	manifest := testEngineManifest(filepath.Join(t.TempDir(), "absent"))
	manifest.Engine = "llamacpp"
	platform := manifest.Platforms[hostKey()]
	port, err := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	require.NoError(t, err)
	platform.Runtime.Port = port
	platform.Runtime.Ready.HTTP = "http://127.0.0.1:{port}/props"
	manifest.Platforms[hostKey()] = platform
	e := newTestExecutor(t, manifest)
	mockLlamaRelease(t, e, "b100", nil, nil)

	update, err := e.CheckEngineUpdate(t.Context(), "llamacpp")

	require.NoError(t, err)
	wantURL := llamaReleasesURL + "/tag/b100"
	assert.Equal(t, &EngineUpdateInfo{CurrentVersion: "b99", LatestVersion: "b100", InstallType: "external", ReleaseURL: wantURL, DownloadURL: wantURL}, update)
}

func TestLlamaUpdateRelease_RequiresEveryCompatibleCUDAArchive(t *testing.T) {
	pinURL := llamaReleasesURL + "/download/b99/"
	pinned := &Install{
		Artifacts: []InstallArtifact{
			pinnedArtifact("server", pinURL+"llama-b99-bin-win-cuda-12.4-x64.zip", nil),
			pinnedArtifact("cudart", pinURL+"cudart-llama-bin-win-cuda-12.4-x64.zip", nil),
		},
		Run: []string{"unpack"},
	}
	server := llamaTestAsset("b100", "llama-b100-bin-win-cuda-12.4-x64.zip", []byte("server"))
	runtime := llamaTestAsset("b100", "cudart-llama-bin-win-cuda-12.4-x64.zip", []byte("cudart"))
	t.Run("both matching artifacts", func(t *testing.T) {
		e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
		mockLlamaRelease(t, e, "b100", []githubReleaseAsset{server, runtime}, nil)

		target, inst, err := e.llamaUpdateRelease(t.Context(), 98, pinned)

		require.NoError(t, err)
		assert.Equal(t, llamaBuild(100), target)
		require.Len(t, inst.Artifacts, 2)
		assert.Equal(t, server.BrowserDownloadURL, inst.Artifacts[0].URL)
		assert.Equal(t, strings.TrimPrefix(runtime.Digest, "sha256:"), inst.Artifacts[1].SHA256)
		assert.Equal(t, pinURL+"cudart-llama-bin-win-cuda-12.4-x64.zip", pinned.Artifacts[1].URL)
	})
	t.Run("runtime requires a different CUDA version", func(t *testing.T) {
		e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
		newRuntime := llamaTestAsset("b100", "cudart-llama-bin-win-cuda-13.5-x64.zip", []byte("cudart"))
		mockLlamaRelease(t, e, "b100", []githubReleaseAsset{server, newRuntime}, nil)

		target, inst, err := e.llamaUpdateRelease(t.Context(), 98, pinned)

		require.NoError(t, err)
		assert.Equal(t, llamaBuild(99), target)
		assert.Equal(t, pinned, inst)
	})
}

func llamaManagedFixture(t *testing.T, stagedVersion string) (*Executor, string) {
	t.Helper()
	name := "llama-server" + filepath.Ext(fakeEngineBin)
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	add := func(path string, data []byte) {
		t.Helper()
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: path, Mode: 0755, Size: int64(len(data))}))
		_, err := writer.Write(data)
		require.NoError(t, err)
	}
	binary, err := os.ReadFile(fakeEngineBin)
	require.NoError(t, err)
	add(name, binary)
	add(name+".version-output", []byte("version: 1.0.0 (build "+stagedVersion+", commit 12abcd)\n"))
	require.NoError(t, writer.Close())
	asset := llamaTestAsset("b100", "llama-b100-bin-test.tar", archive.Bytes())
	pin := llamaTestAsset("b90", "llama-b90-bin-test.tar", nil)

	manifest := testEngineManifest(filepath.Join("{install_dir}", name))
	manifest.Engine = "llamacpp"
	manifest.DisplayName = "llama.cpp"
	platform := manifest.Platforms[hostKey()]
	platform.ModelsDir = t.TempDir()
	platform.Install = &Install{
		Fetch: &Fetch{URL: pin.BrowserDownloadURL, SHA256: strings.TrimPrefix(pin.Digest, "sha256:")},
		Run:   []string{"tar", "-xf", "{download}", "-C", "{install_dir}"},
	}
	manifest.Platforms[hostKey()] = platform
	e := newTestExecutor(t, manifest)
	bundled, err := json.Marshal(manifest)
	require.NoError(t, err)
	e.reg.bundledRaw["llamacpp"] = bundled
	st, err := e.state("llamacpp")
	require.NoError(t, err)
	require.NoError(t, writeInstallMarker(st.installDir, "llamacpp"))
	installed := filepath.Join(st.installDir, name)
	copyFile(t, fakeEngineBin, installed)
	require.NoError(t, os.WriteFile(installed+".version-output", []byte("version: 99 (349cd7)\r\nbuilt with cc for test\n"), 0600))
	mockLlamaRelease(t, e, "b100", []githubReleaseAsset{asset}, map[string][]byte{asset.BrowserDownloadURL: archive.Bytes()})
	return e, installed
}

func TestUpdateLlamaCpp_UpdatesAStoppedEngineToTheAdvertisedBuild(t *testing.T) {
	e, _ := llamaManagedFixture(t, "100")
	st, err := e.state("llamacpp")
	require.NoError(t, err)
	model := filepath.Join(st.modelsDir, "saved.gguf")
	require.NoError(t, os.WriteFile(model, []byte("weights"), 0600))
	update, err := e.CheckEngineUpdate(t.Context(), "llamacpp")
	require.NoError(t, err)
	require.NotNil(t, update)
	assert.Equal(t, "managed", update.InstallType)
	assert.Equal(t, "b100", update.LatestVersion)

	// Applying the advertised build still works if GitHub's API has since
	// rate-limited the node. The archive is available on the download host.
	downloads := e.client.Transport
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.github.com" {
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Retry-After": {"60"}}, Body: http.NoBody}, nil
		}
		return downloads.RoundTrip(req)
	})
	require.NoError(t, e.UpdateEngine(t.Context(), "llamacpp"))

	update, err = e.CheckEngineUpdate(t.Context(), "llamacpp")
	require.NoError(t, err)
	assert.Nil(t, update)
	status, err := e.Status("llamacpp")
	require.NoError(t, err)
	assert.False(t, status.Running)
	saved, err := os.ReadFile(model)
	require.NoError(t, err)
	assert.Equal(t, "weights", string(saved))
}

func TestUpdateLlamaCpp_RejectsAnArchiveReportingADifferentBuild(t *testing.T) {
	e, binPath := llamaManagedFixture(t, "98")
	err := e.UpdateEngine(t.Context(), "llamacpp")
	require.ErrorContains(t, err, "reports build b98, expected b100")

	build, err := llamaBinaryBuild(t.Context(), binPath)
	require.NoError(t, err)
	assert.Equal(t, llamaBuild(99), build)
}

func TestUpdateLlamaCpp_RestartsARunningEngineAtItsPreviousPort(t *testing.T) {
	e, installed := llamaManagedFixture(t, "100")
	t.Cleanup(e.StopAll)
	require.NoError(t, e.Start(t.Context(), "llamacpp"))
	before, err := e.Status("llamacpp")
	require.NoError(t, err)

	require.NoError(t, e.UpdateEngine(t.Context(), "llamacpp"))

	after, err := e.Status("llamacpp")
	require.NoError(t, err)
	assert.True(t, after.Running)
	assert.Equal(t, before.Port, after.Port)
	build, err := llamaBinaryBuild(t.Context(), installed)
	require.NoError(t, err)
	assert.Equal(t, llamaBuild(100), build)
}

func TestUpdateLlamaCpp_RefusesACacheInsideTheEngineDirectory(t *testing.T) {
	e, binary := llamaManagedFixture(t, "100")
	t.Setenv("LLAMA_CACHE", filepath.Join(filepath.Dir(binary), "cached-models"))
	require.ErrorContains(t, e.UpdateEngine(t.Context(), "llamacpp"), "PAIR-managed")
}
