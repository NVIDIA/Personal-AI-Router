// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lmStudioUpdateFixture(t *testing.T, hostStatus string) (*Executor, *engineState, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LMS_API_SERVER_INFO_PATH", "")
	t.Setenv("PAIR_TEST_LMS_HOST", hostStatus)
	t.Setenv("PAIR_TEST_LMS_STOP", filepath.Join(home, "last-server-command.json"))
	if runtime.GOOS == "linux" {
		libDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(libDir, "ldconfig"), []byte("#!/bin/sh\necho libatomic.so.1\n"), 0o755))
		t.Setenv("PATH", libDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	root := filepath.Join(home, ".lmstudio")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	copyFile(t, fakeEngineBin, filepath.Join(root, "bin", "lms"+exeExt()))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".internal"), 0o755))
	pointer := filepath.Join(root, ".internal", "llmster-install-location.json")
	registerLMStudioDaemon(t, root, "0.0.17-4", pointer)

	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	st, err := ex.state("lmstudio")
	require.NoError(t, err)
	st.plat.Runtime.Ready = nil
	st.plat.Runtime.Health = nil
	st.port = 0
	return ex, st, pointer
}

func registerLMStudioDaemon(t *testing.T, root, version, pointer string) {
	t.Helper()
	dir := filepath.Join(root, "llmster", version)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	binary := filepath.Join(dir, "llmster"+exeExt())
	copyFile(t, fakeEngineBin, binary)
	data, err := json.Marshal(map[string]string{"path": binary})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pointer, data, 0o600))
}

func mockLMStudioRelease(t *testing.T, ex *Executor, product, current, latest, build string) {
	t.Helper()
	releaseOS := map[string]string{"windows": "win32", "linux": "linux", "darwin": "darwin"}[runtime.GOOS]
	releaseArch := map[string]string{"amd64": "x86", "arm64": "arm64"}[runtime.GOARCH]
	endpoint := "https://versions-prod.lmstudio.ai/update/" + product + releaseOS + "/" + releaseArch + "/" + current
	body, err := json.Marshal(map[string]string{"os": releaseOS, "arch": releaseArch, "version": latest, "build": build})
	require.NoError(t, err)
	ex.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != endpoint {
			return nil, fmt.Errorf("unexpected release feed %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
}

func TestCheckLMStudioUpdate_UsesTheActiveHostAndItsOwnStableFeed(t *testing.T) {
	check := func(name, status, product, current, core, next, build, action, link string) {
		t.Run(name, func(t *testing.T) {
			ex, st, _ := lmStudioUpdateFixture(t, status)
			require.NoError(t, writeInstallMarker(st.installDir, "lmstudio"))
			mockLMStudioRelease(t, ex, product, core, next, build)

			offer, err := ex.CheckLMStudioUpdate(t.Context())

			require.NoError(t, err)
			require.NotNil(t, offer)
			assert.Equal(t, current, offer.CurrentVersion)
			assert.Equal(t, next+"-"+build, offer.LatestVersion)
			assert.Equal(t, action, offer.InstallType)
			assert.Equal(t, link, offer.DownloadURL)
		})
	}
	check("managed stopped daemon has a newer build", "LM Studio is not running", "daemon/", "0.0.17-4", "0.0.17", "0.0.17", "10", "managed", "")
	check("daemon running independently of PAIR", "llmster v0.0.17+4 is running (PID: 68048)", "daemon/", "0.0.17-4", "0.0.17", "0.1.1", "2", "external", lmStudioDaemonGuide)
	check("desktop app takes precedence over installed daemon", "LM Studio v0.4.25+2 is running (PID: 1234)", "", "0.4.25-2", "0.4.25", "0.4.28", "2", "external", "https://lmstudio.ai/download#lm-studio-download-heading")
}

func TestCheckLMStudioUpdate_ReportsNoUpdateForCurrentBuild(t *testing.T) {
	ex, _, _ := lmStudioUpdateFixture(t, "LM Studio is not running")
	mockLMStudioRelease(t, ex, "daemon/", "0.0.17", "0.0.17", "4")

	offer, err := ex.CheckLMStudioUpdate(t.Context())

	require.NoError(t, err)
	assert.Nil(t, offer)
}

func TestUpdateLMStudio_LeavesAnExternalDaemonWithItsOwner(t *testing.T) {
	ex, _, _ := lmStudioUpdateFixture(t, "LM Studio is not running")
	mockLMStudioRelease(t, ex, "daemon/", "0.0.17", "0.1.1", "2")

	offer, err := ex.CheckLMStudioUpdate(t.Context())

	require.NoError(t, err)
	require.NotNil(t, offer)
	assert.Equal(t, "external", offer.InstallType)
	assert.Equal(t, lmStudioDaemonGuide, offer.DownloadURL)
	require.ErrorContains(t, ex.UpdateLMStudio(t.Context()), "PAIR-managed")
}

func TestCheckLMStudioUpdate_UsesTheVersionInTheRelocatedVendorHome(t *testing.T) {
	ex, st, _ := lmStudioUpdateFixture(t, "LM Studio is not running")
	require.NoError(t, writeInstallMarker(st.installDir, "lmstudio"))
	relocated := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(relocated, ".internal"), 0o755))
	registerLMStudioDaemon(t, relocated, "0.0.27-1", filepath.Join(relocated, ".internal", "llmster-install-location.json"))
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".lmstudio-home-pointer"), []byte(relocated), 0o600))
	mockLMStudioRelease(t, ex, "daemon/", "0.0.27", "0.1.1", "2")

	offer, err := ex.CheckLMStudioUpdate(t.Context())

	require.NoError(t, err)
	require.NotNil(t, offer)
	assert.Equal(t, "0.0.27-1", offer.CurrentVersion)
	assert.Equal(t, "external", offer.InstallType)
}

func TestUpdateLMStudio_VerifiesTheVendorInstalledTheReleaseAndPreservesStoppedState(t *testing.T) {
	ex, st, pointer := lmStudioUpdateFixture(t, "LM Studio is not running")
	require.NoError(t, writeInstallMarker(st.installDir, "lmstudio"))
	root := filepath.Dir(filepath.Dir(pointer))
	registerLMStudioDaemon(t, root, "0.1.1-2", pointer+".next")
	require.NoError(t, os.MkdirAll(st.modelsDir, 0o755))
	model := filepath.Join(st.modelsDir, "downloaded.gguf")
	require.NoError(t, os.WriteFile(model, []byte("downloaded model"), 0o600))
	t.Setenv("PAIR_TEST_LLMSTER_POINTER", pointer)
	mockLMStudioRelease(t, ex, "daemon/", "0.0.17", "0.1.1", "2")
	require.NoError(t, ex.UpdateLMStudio(t.Context()))

	updated, err := lmStudioDaemonInstall(root)
	require.NoError(t, err)
	assert.Equal(t, "0.1.1-2", updated.version)
	assert.FileExists(t, model)
	assert.False(t, st.running)
}

func TestUpdateLMStudio_RestartsTheRunningServerWhenTheVendorSkippedTheUpdate(t *testing.T) {
	ex, st, _ := lmStudioUpdateFixture(t, "llmster v0.0.17+4 is running (PID: 68048)")
	require.NoError(t, writeInstallMarker(st.installDir, "lmstudio"))
	st.installed, st.running, st.healthy = true, true, true
	st.port = 1235
	t.Setenv("PAIR_TEST_LMS_STATUS", `{"status":"running","isDaemon":true}`)
	t.Setenv("PAIR_TEST_LLMSTER_POINTER", "") // The vendor can return success without promoting a new release.
	mockLMStudioRelease(t, ex, "daemon/", "0.0.17", "0.1.1", "2")

	require.ErrorContains(t, ex.UpdateLMStudio(t.Context()), "did not install version 0.1.1-2")

	assert.True(t, st.running)
	data, err := os.ReadFile(os.Getenv("PAIR_TEST_LMS_STOP"))
	require.NoError(t, err)
	var argv []string
	require.NoError(t, json.Unmarshal(data, &argv))
	assert.Equal(t, []string{"server", "start", "--port", "1235", "--bind", "127.0.0.1"}, argv)
}
