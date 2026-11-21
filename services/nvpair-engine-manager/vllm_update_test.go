// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/pythontest"
)

func TestVLLMVersions_ComparePlatformPackagesWithoutOfferingDowngrades(t *testing.T) {
	check := func(name string, installed vllmVersions, target string, metal bool, expected string) {
		t.Run(name, func(t *testing.T) {
			older, err := installed.olderPackage(target, metal)
			require.NoError(t, err)
			assert.Equal(t, expected, older)
		})
	}
	check("release candidate", vllmVersions{Core: "0.34.0rc2"}, "0.34.0", false, "0.34.0rc2")
	check("CPU wheel local version label", vllmVersions{Core: "0.32.0+cpu", Metal: "0.32.0"}, "0.32.0", true, "")
	check("older Metal plugin", vllmVersions{Core: "0.32.0+cpu", Metal: "0.31.1"}, "0.32.0", true, "0.31.1")
	check("newer plugin with an older core", vllmVersions{Core: "0.31.0+cpu", Metal: "0.33.0"}, "0.32.0", true, "")
	check("Python post-release", vllmVersions{Core: "0.10.0.post1"}, "0.10.0", false, "")
}

func TestCheckVLLMRelease_OffersAnExternalUpdateOnlyForAnIdentifiedServer(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	ex.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.String() {
		case "http://127.0.0.1:8001/version":
			assert.Equal(t, "1", req.Header.Get(engineIdentityProbeHeader))
			body = `{"version":"0.30.0"}`
		case "https://api.github.com/repos/vllm-project/vllm/releases/latest":
			body = `{"tag_name":"v0.35.0"}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	offer, err := ex.checkVLLMRelease(t.Context(), nil, engineUpdateSnapshot{running: true, adopted: true, port: 8001})

	require.NoError(t, err)
	if runtime.GOOS == "darwin" {
		assert.Nil(t, offer, "the core endpoint cannot identify a Metal release")
		return
	}
	assert.Equal(t, &EngineUpdateInfo{
		CurrentVersion: "0.30.0", LatestVersion: "0.35.0", InstallType: "external",
		ReleaseURL:  "https://github.com/vllm-project/vllm/releases/tag/v0.35.0",
		DownloadURL: "https://docs.vllm.ai/en/latest/getting_started/installation/gpu/",
	}, offer)
}

func TestUpdateVLLM_RebuildsAtTheCanonicalPathAndPreservesModels(t *testing.T) {
	// Create package metadata in a real, empty venv. The bundled fake server
	// stands in for the vllm executable; no vLLM wheel or GPU is needed.
	python := pythontest.Python3(t)
	const build = `import os, pathlib, shutil, sys, venv
server, root, version = sys.argv[1:]
env = pathlib.Path(root) / "venv"
venv.EnvBuilder(with_pip=False, symlinks=os.name != "nt").create(env)
scripts = env / ("Scripts" if os.name == "nt" else "bin")
shutil.copy2(server, scripts / pathlib.Path(server).name)
site = env / ("Lib" if os.name == "nt" else "lib/python%d.%d" % sys.version_info[:2]) / "site-packages"
for package in ["vllm", "vllm-metal"]:
    dist = site / (package.replace("-", "_") + "-" + version + ".dist-info")
    dist.mkdir()
    (dist / "METADATA").write_text("Metadata-Version: 2.1\nName: " + package + "\nVersion: " + version + "\n")
(env / "built-at").write_text(str(env))`
	scripts := "bin"
	if runtime.GOOS == "windows" {
		scripts = "Scripts"
	}
	engine := testEngineManifest(filepath.Join("{install_dir}", "venv", scripts, filepath.Base(fakeEngineBin)))
	engine.Engine = "vllm"
	engine.DisplayName = "vLLM"
	key := runtime.GOOS + "/" + runtime.GOARCH
	platform := engine.Platforms[key]
	platform.ModelsDir = t.TempDir()
	t.Setenv("HF_HOME", platform.ModelsDir)
	platform.Install = &Install{
		Fetch: &Fetch{URL: "https://example.test/uv", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("test bootstrap")))},
		Run:   []string{python, "-I", "-c", build, fakeEngineBin, "{install_dir}", "0.34.0"},
	}
	engine.Platforms[key] = platform
	ex := newTestExecutor(t, engine)
	raw, err := json.Marshal(engine)
	require.NoError(t, err)
	ex.reg.bundledRaw["vllm"] = raw
	st, err := ex.state("vllm")
	require.NoError(t, err)
	out, err := exec.CommandContext(t.Context(), python, "-I", "-c", build, fakeEngineBin, st.installDir, "0.30.0").CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, writeInstallMarker(st.installDir, "vllm"))
	weights := filepath.Join(platform.ModelsDir, "weights")
	require.NoError(t, os.WriteFile(weights, []byte("model data"), 0o600))
	ex.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != platform.Install.Fetch.URL {
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("test bootstrap"))}, nil
	})

	offer, err := ex.CheckVLLMUpdate(t.Context())
	require.NoError(t, err)
	require.NotNil(t, offer)
	assert.Equal(t, "managed", offer.InstallType)
	assert.Equal(t, "0.34.0", offer.LatestVersion)
	require.NoError(t, ex.UpdateVLLM(t.Context()))

	offer, err = ex.CheckVLLMUpdate(t.Context())
	require.NoError(t, err)
	assert.Nil(t, offer)
	builtAt, err := os.ReadFile(filepath.Join(st.installDir, "venv", "built-at"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(st.installDir, "venv"), filepath.Clean(string(builtAt)))
	model, err := os.ReadFile(weights)
	require.NoError(t, err)
	assert.Equal(t, "model data", string(model))
}

func TestRebuildVLLMInstall_RestoresOriginalWhenCandidateCannotStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vllm")
	require.NoError(t, writeInstallMarker(dir, "vllm"))
	bin := filepath.Join(dir, "engine")
	require.NoError(t, os.WriteFile(bin, []byte("old"), 0o600))
	failure := errors.New("candidate model failed to load")

	err := rebuildVLLMInstall(dir, func() error {
		return os.WriteFile(bin, []byte("new"), 0o600)
	}, func() error { return failure })

	require.ErrorIs(t, err, failure)
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	assert.NoDirExists(t, dir+".update-backup")
}

func TestRecoverVLLMUpdate_LeavesALiveRebuildAloneAndRestoresAnInterruptedOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vllm")
	backup := dir + ".update-backup"
	require.NoError(t, writeInstallMarker(backup, "vllm"))
	marker, err := readEngineInstallMarker(backup, "vllm")
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vllmPendingMarker), marker, 0o600))
	guard, err := lockEngineInstall(dir)
	require.NoError(t, err)

	assert.Error(t, recoverVLLMUpdate(dir))
	assert.DirExists(t, backup)
	require.NoError(t, guard.Close())
	require.NoError(t, recoverVLLMUpdate(dir))

	assert.FileExists(t, installMarkerPath(dir))
	assert.NoFileExists(t, filepath.Join(dir, vllmPendingMarker))
	assert.NoDirExists(t, backup)
}
