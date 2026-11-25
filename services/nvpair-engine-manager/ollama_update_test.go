// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The installer extracts a small, local tar archive on every platform. Its
// executable is the same fake that the service lifecycle tests start/stop.
func ollamaUpdateFixture(t *testing.T, stagedVersion string, failStart bool) (*Executor, string) {
	t.Helper()
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	name := "ollama" + filepath.Ext(fakeEngineBin)
	bin := filepath.Join("{install_dir}", name)
	manifest := testEngineManifest(bin)
	manifest.Engine = "ollama"
	manifest.DisplayName = "Ollama"
	platformKey := runtime.GOOS + "/" + runtime.GOARCH
	platform := manifest.Platforms[platformKey]
	platform.ModelsDir = os.Getenv("OLLAMA_MODELS")
	platform.Uninstall = &Uninstall{Remove: []string{"{install_dir}"}}
	platform.Install = &Install{
		Fetch: &Fetch{URL: "https://ollama.com/download/ollama-test.tar"},
		Run:   []string{"tar", "-xf", "{download}", "-C", "{install_dir}"},
	}
	manifest.Platforms[platformKey] = platform
	e := newTestExecutor(t, manifest)
	bundled, err := json.Marshal(manifest)
	require.NoError(t, err)
	e.reg.bundledRaw["ollama"] = bundled
	st, err := e.state("ollama")
	require.NoError(t, err)
	require.NoError(t, writeInstallMarker(st.installDir, "ollama"))
	installed := filepath.Join(st.installDir, name)
	copyFile(t, fakeEngineBin, installed)
	require.NoError(t, os.WriteFile(installed+".version", []byte("1.0.0"), 0o600))
	t.Cleanup(e.StopAll)

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	add := func(path string, data []byte) {
		t.Helper()
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: path, Size: int64(len(data)), Mode: 0o755}))
		_, err := tw.Write(data)
		require.NoError(t, err)
	}
	exe, err := os.ReadFile(fakeEngineBin)
	require.NoError(t, err)
	add(name, exe)
	add(name+".version", []byte(stagedVersion))
	if failStart {
		add(name+".fail-start", nil)
	}
	require.NoError(t, tw.Close())
	const assetURL = "https://github.com/ollama/ollama/releases/download/v1.1.0/ollama-test.tar"
	release, err := json.Marshal(githubRelease{
		Tag:    "v1.1.0",
		Assets: []githubReleaseAsset{{Name: "ollama-test.tar", Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(archive.Bytes())), BrowserDownloadURL: assetURL}},
	})
	require.NoError(t, err)
	loopback := e.client.Transport
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var data []byte
		switch req.URL.String() {
		case latestOllamaReleaseURL:
			data = release
		case assetURL:
			data = archive.Bytes()
		default:
			if req.URL.Hostname() != "127.0.0.1" {
				return nil, fmt.Errorf("unexpected request to %s", req.URL)
			}
			return loopback.RoundTrip(req)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}, nil
	})
	return e, installed
}

func TestUpdateOllama_PreservesModelsAndLeavesStoppedEngineOff(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	model := filepath.Join(os.Getenv("OLLAMA_MODELS"), "model")
	require.NoError(t, os.WriteFile(model, []byte("model data"), 0o600))

	require.NoError(t, e.UpdateEngine(t.Context(), "ollama"))

	version, err := ollamaBinaryVersion(t.Context(), installed)
	require.NoError(t, err)
	assert.Equal(t, "1.1.0", version)
	status, err := e.Status("ollama")
	require.NoError(t, err)
	assert.True(t, status.Installed)
	assert.False(t, status.Enabled)
	assert.False(t, status.Running)
	data, err := os.ReadFile(model)
	require.NoError(t, err)
	assert.Equal(t, "model data", string(data))
}

func TestUpdateOllama_RestartsRunningEngineAtItsPreviousPort(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	require.NoError(t, e.Start(t.Context(), "ollama"))
	before, err := e.Status("ollama")
	require.NoError(t, err)

	require.NoError(t, e.UpdateOllama(t.Context()))

	status, err := e.Status("ollama")
	require.NoError(t, err)
	assert.True(t, status.Running)
	assert.True(t, status.Enabled)
	assert.True(t, status.Healthy)
	assert.Equal(t, before.Port, status.Port)
	version, err := ollamaBinaryVersion(t.Context(), installed)
	require.NoError(t, err)
	assert.Equal(t, "1.1.0", version)
}

func TestUpdateOllama_RestoresRunningVersionWhenNewReleaseFailsToStart(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", true)
	require.NoError(t, e.Start(t.Context(), "ollama"))

	require.Error(t, e.UpdateOllama(t.Context()))

	version, err := ollamaBinaryVersion(t.Context(), installed)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
	status, err := e.Status("ollama")
	require.NoError(t, err)
	assert.True(t, status.Enabled)
	assert.True(t, status.Running)
	assert.True(t, status.Healthy)
}

func TestUpdateOllama_RejectsUnexpectedArchiveVersionBeforeStoppingEngine(t *testing.T) {
	e, _ := ollamaUpdateFixture(t, "0.9.0", false)
	require.NoError(t, e.Start(t.Context(), "ollama"))
	st, err := e.state("ollama")
	require.NoError(t, err)
	st.mu.Lock()
	process := st.proc
	st.mu.Unlock()

	require.ErrorContains(t, e.UpdateOllama(t.Context()), "reports version 0.9.0, expected 1.1.0")

	st.mu.Lock()
	defer st.mu.Unlock()
	assert.Same(t, process, st.proc)
	assert.True(t, st.running)
}

func TestUpdateOllama_RefusesModelStoreInsideManagedDirectory(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	t.Setenv("OLLAMA_MODELS", filepath.Join(filepath.Dir(installed), "models"))

	require.ErrorContains(t, e.UpdateOllama(t.Context()), "PAIR-managed Ollama installation")
	version, err := ollamaBinaryVersion(t.Context(), installed)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
}

func TestUpdateOllama_LeavesCustomInstallerUnderUserControl(t *testing.T) {
	e, _ := ollamaUpdateFixture(t, "1.1.0", false)
	st, err := e.state("ollama")
	require.NoError(t, err)
	st.plat.Install.Run = []string{"custom-ollama-install"}

	require.ErrorContains(t, e.UpdateOllama(t.Context()), "PAIR-managed Ollama installation")
}

func TestUpdateEngine_LeavesRelocatedInstallationsUnderUserControl(t *testing.T) {
	e, binary := ollamaUpdateFixture(t, "1.1.0", false)
	manifest, ok := e.reg.Get("ollama")
	require.True(t, ok)
	platform := manifest.Platforms[hostKey()]
	platform.InstallDir = t.TempDir()
	manifest.Platforms[hostKey()] = platform
	relocated := filepath.Join(platform.InstallDir, filepath.Base(binary))
	copyFile(t, binary, relocated)
	require.NoError(t, writeInstallMarker(platform.InstallDir, "ollama"))
	restarted := NewExecutor(e.reg, NewReporter(nil), nil, e.baseDir)

	err := restarted.UpdateEngine(t.Context(), "ollama")

	require.ErrorContains(t, err, "PAIR-managed Ollama installation")
	assert.FileExists(t, relocated)
}

func TestUpdateOllama_LeavesInstalledVersionInPlaceWhenArchiveChecksumDiffers(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	transport := e.client.Transport
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "github.com" {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("changed bytes")))}, nil
		}
		return transport.RoundTrip(req)
	})

	require.ErrorContains(t, e.UpdateOllama(t.Context()), "checksum mismatch")
	version, err := ollamaBinaryVersion(t.Context(), installed)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
}

func TestOllamaStatus_RestoresOriginalWhenInterruptedBetweenRenames(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	dir := filepath.Dir(installed)
	require.NoError(t, os.Rename(dir, dir+".update-backup"))
	restarted := NewExecutor(e.reg, NewReporter(nil), nil, e.baseDir)

	status, err := restarted.Status("ollama")

	require.NoError(t, err)
	assert.True(t, status.Installed)
	version, err := ollamaBinaryVersion(context.Background(), installed)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
}

func TestOllamaRecovery_LeavesBackupInPlaceWhileAnotherManagerIsUpdating(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	dir := filepath.Dir(installed)
	guard, err := lockEngineInstall(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, guard.Close()) })
	require.NoError(t, renameEngineDirectory(dir, dir+".update-backup"))
	other := NewExecutor(e.reg, NewReporter(nil), nil, e.baseDir)

	_, err = other.Status("ollama")

	require.Error(t, err)
	assert.NoDirExists(t, dir)
	assert.FileExists(t, filepath.Join(dir+".update-backup", filepath.Base(installed)))
}

func TestOllamaUninstall_PreservesAnInstallationBeingUpdatedByAnotherManager(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	guard, err := lockEngineInstall(filepath.Dir(installed))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, guard.Close()) })

	require.Error(t, e.Uninstall(t.Context(), "ollama"))
	assert.FileExists(t, installed)
}

func TestUninstallOllama_PreventsInterruptedUpdateBackupFromRestoringRemovedEngine(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	dir := filepath.Dir(installed)
	require.NoError(t, writeInstallMarker(dir+".update-backup", "ollama"))

	require.NoError(t, e.Uninstall(t.Context(), "ollama"))

	restarted := NewExecutor(e.reg, NewReporter(nil), nil, e.baseDir)
	status, err := restarted.Status("ollama")
	require.NoError(t, err)
	assert.False(t, status.Installed)
	assert.NoDirExists(t, dir+".update-backup")
}

func TestUpdateOllama_ShutdownCancelsStalledDownload(t *testing.T) {
	e, installed := ollamaUpdateFixture(t, "1.1.0", false)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	started := make(chan struct{})
	transport := e.client.Transport
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "github.com" {
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return transport.RoundTrip(req)
	})
	result := make(chan error, 1)
	go func() { result <- e.UpdateOllama(ctx) }()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("update ended before requesting the archive: %v", err)
	case <-ctx.Done():
		t.Fatal("archive download did not start")
	}

	e.StopAll()

	require.NoError(t, ctx.Err(), "shutdown waited for the download's deadline")
	require.ErrorIs(t, <-result, context.Canceled)
	version, err := ollamaBinaryVersion(t.Context(), installed)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
}
