// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const latestOllamaReleaseURL = "https://api.github.com/repos/ollama/ollama/releases/latest"

func TestCompareOllamaVersions_OrdersReleasesBySemanticVersion(t *testing.T) {
	check := func(name, current, latest string, expected int) {
		t.Run(name, func(t *testing.T) {
			order, err := compareOllamaVersions(current, latest)
			require.NoError(t, err)
			assert.Equal(t, expected, order)
		})
	}
	check("minor version crosses a decimal place", "0.9.5", "v0.10.0", -1)
	check("installed version is ahead", "0.10.1", "0.10.0", 1)
	check("stable release supersedes an installed release candidate", "0.10.0-rc.1", "0.10.0", -1)
	check("build metadata does not affect ordering", "0.10.0+build.1", "0.10.0", 0)
}

func TestParseOllamaBinaryVersion_ReturnsTheSelectedBinaryVersion(t *testing.T) {
	version, err := parseOllamaBinaryVersion("ollama version is 0.10.0\nWarning: client version is 0.9.5\n")
	require.NoError(t, err)
	assert.Equal(t, "0.9.5", version)
}

func TestLatestOllamaRelease_RejectsIncompleteOrUnstableFeedEntries(t *testing.T) {
	reject := func(name, body string) {
		t.Run(name, func(t *testing.T) {
			ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
			mockOllamaFeed(t, ex, http.StatusOK, body)
			_, err := ex.latestOllamaRelease(t.Context())
			require.Error(t, err)
		})
	}
	reject("missing tag", `{}`)
	reject("incomplete version", `{"tag_name":"v0.9"}`)
	reject("draft release", `{"tag_name":"v0.10.0","draft":true}`)
	reject("prerelease flag", `{"tag_name":"v0.10.0","prerelease":true}`)
	reject("prerelease version", `{"tag_name":"v0.10.0-rc.1"}`)
}

func TestGitHubRelease_AcceptsOnlyThePublishedTagRequestedByTheProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantTag string
	}{
		{"vendor selected a tag GitHub labels prerelease", `{"tag_name":"b12345","prerelease":true}`, "b12345"},
		{"draft", `{"tag_name":"b12345","draft":true}`, ""},
		{"different tag", `{"tag_name":"b12346"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestExecutor(t, testEngineManifest(fakeEngineBin))
			e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/b12345", req.URL.String())
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})

			release, err := e.githubRelease(t.Context(), "ggml-org/llama.cpp", "tags/b12345")

			if tc.wantTag == "" {
				require.Error(t, err)
				assert.Nil(t, release)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, release)
			assert.Equal(t, tc.wantTag, release.Tag)
		})
	}
}

func mockOllamaFeed(t *testing.T, ex *Executor, status int, body string) {
	t.Helper()
	transport := ex.client.Transport
	ex.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "127.0.0.1" {
			return transport.RoundTrip(req)
		}
		if req.URL.String() != latestOllamaReleaseURL {
			return nil, fmt.Errorf("unexpected release request: %s", req.URL)
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
}

func TestCheckOllamaUpdate_ReportsAnUpdateForAStoppedExternalBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), filepath.Base(fakeEngineBin))
	copyFile(t, fakeEngineBin, bin)
	require.NoError(t, os.WriteFile(bin+".version", []byte("0.9.5"), 0o600))
	manifest := testEngineManifest(bin)
	manifest.Engine = "ollama"
	ex := newTestExecutor(t, manifest)
	mockOllamaFeed(t, ex, http.StatusOK, `{"tag_name":"v0.10.0"}`)

	update, err := ex.CheckOllamaUpdate(t.Context())

	require.NoError(t, err)
	assert.Equal(t, &EngineUpdateInfo{
		CurrentVersion: "0.9.5",
		LatestVersion:  "0.10.0",
		ReleaseURL:     "https://github.com/ollama/ollama/releases/tag/v0.10.0",
		InstallType:    "external",
		DownloadURL:    "https://ollama.com/download",
	}, update)
}

func TestCheckOllamaUpdate_OffersManagedUpdatesWhenThePlatformArchiveIsReady(t *testing.T) {
	manifest := testEngineManifest(filepath.Join("{install_dir}", filepath.Base(fakeEngineBin)))
	manifest.Engine = "ollama"
	key := runtime.GOOS + "/" + runtime.GOARCH
	platform := manifest.Platforms[key]
	platform.Install = &Install{
		Fetch: &Fetch{URL: "https://ollama.com/download/ollama-test.zip"},
		Run:   []string{"unzip", "{download}", "-d", "{install_dir}"},
	}
	manifest.Platforms[key] = platform
	ex := newTestExecutor(t, manifest)
	bundled, err := json.Marshal(manifest)
	require.NoError(t, err)
	ex.reg.bundledRaw["ollama"] = bundled
	st, err := ex.state("ollama")
	require.NoError(t, err)
	require.NoError(t, writeInstallMarker(st.installDir, "ollama"))
	bin := filepath.Join(st.installDir, filepath.Base(fakeEngineBin))
	copyFile(t, fakeEngineBin, bin)
	require.NoError(t, os.WriteFile(bin+".version", []byte("0.9.5"), 0o600))
	t.Setenv("OLLAMA_MODELS", t.TempDir())

	t.Run("platform archive published", func(t *testing.T) {
		mockOllamaFeed(t, ex, http.StatusOK, `{
			"tag_name": "v0.10.0",
			"assets": [{
				"name": "ollama-test.zip",
				"digest": "sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
				"browser_download_url": "https://github.com/ollama/ollama/releases/download/v0.10.0/ollama-test.zip"
			}]
		}`)
		update, err := ex.CheckOllamaUpdate(t.Context())
		require.NoError(t, err)
		require.NotNil(t, update)
		assert.Equal(t, "managed", update.InstallType)
		assert.Empty(t, update.DownloadURL)
	})
	t.Run("platform archive missing", func(t *testing.T) {
		mockOllamaFeed(t, ex, http.StatusOK, `{"tag_name":"v0.10.0","assets":[]}`)
		update, err := ex.CheckOllamaUpdate(t.Context())
		require.NoError(t, err)
		assert.Nil(t, update)
	})
}

func TestCheckOllamaUpdate_ChecksAnAdoptedServerWithNoLocalBinary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/version" || req.Header.Get(engineIdentityProbeHeader) != "1" {
			http.NotFound(w, req)
			return
		}
		_, err := io.WriteString(w, `{"version":"0.9.5"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	port, err := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	require.NoError(t, err)
	manifest := testEngineManifest(filepath.Join(t.TempDir(), "absent"))
	manifest.Engine = "ollama"
	key := runtime.GOOS + "/" + runtime.GOARCH
	platform := manifest.Platforms[key]
	platform.Runtime.Port = port
	platform.Runtime.Ready.HTTP = "http://127.0.0.1:{port}/api/version"
	manifest.Platforms[key] = platform
	ex := newTestExecutor(t, manifest)
	mockOllamaFeed(t, ex, http.StatusOK, `{"tag_name":"v0.10.0"}`)

	update, err := ex.CheckOllamaUpdate(t.Context())

	require.NoError(t, err)
	require.NotNil(t, update)
	assert.Equal(t, "0.9.5", update.CurrentVersion)
	assert.Equal(t, "external", update.InstallType)
}

func TestCheckOllamaUpdate_SkipsASecondInstallationWhileTheOriginalIsRunning(t *testing.T) {
	preferred := filepath.Join(t.TempDir(), filepath.Base(fakeEngineBin))
	copyFile(t, fakeEngineBin, preferred)
	manifest := testEngineManifest(preferred)
	manifest.Engine = "ollama"
	ex := newTestExecutor(t, manifest)
	st, err := ex.state("ollama")
	require.NoError(t, err)
	st.proc = &managedProc{cmd: exec.Command(fakeEngineBin)}
	st.running = true

	update, err := ex.CheckOllamaUpdate(t.Context())

	require.NoError(t, err)
	assert.Nil(t, update)
}

func TestCheckOllamaUpdate_ChecksTheFeedOnlyForInstalledEngines(t *testing.T) {
	manifest := testEngineManifest(filepath.Join(t.TempDir(), "absent"))
	manifest.Engine = "ollama"
	ex := newTestExecutor(t, manifest)
	ex.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("unexpected network request for an absent engine: %s", req.URL)
		return nil, errors.New("unexpected network request")
	})

	update, err := ex.CheckOllamaUpdate(t.Context())

	require.NoError(t, err)
	assert.Nil(t, update)
}

func TestCheckOllamaUpdate_SkipsHostsWithNoSupportedInstallation(t *testing.T) {
	check := func(name string, manifest *Manifest) {
		t.Run(name, func(t *testing.T) {
			e := newTestExecutor(t, manifest)

			update, err := e.CheckOllamaUpdate(t.Context())

			require.NoError(t, err)
			assert.Nil(t, update)
		})
	}
	check("no Ollama manifest", testEngineManifest(fakeEngineBin))
	check("manifest has no host platform", &Manifest{Engine: "ollama"})
}

func TestCheckOllamaUpdate_DiscardsReleaseWhenInstallationChangesDuringCheck(t *testing.T) {
	bin := filepath.Join(t.TempDir(), filepath.Base(fakeEngineBin))
	copyFile(t, fakeEngineBin, bin)
	require.NoError(t, os.WriteFile(bin+".version", []byte("0.9.5"), 0o600))
	manifest := testEngineManifest(bin)
	manifest.Engine = "ollama"
	e := newTestExecutor(t, manifest)
	mockOllamaFeed(t, e, http.StatusOK, `{"tag_name":"v0.10.0"}`)
	feed := e.client.Transport
	e.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// A status refresh observes that the previously checked executable
		// disappeared while the service was waiting on the release feed.
		require.NoError(t, os.Remove(bin))
		installed, err := e.Detect("ollama")
		require.NoError(t, err)
		require.False(t, installed)
		return feed.RoundTrip(req)
	})

	update, err := e.CheckOllamaUpdate(t.Context())

	require.ErrorContains(t, err, "changed during the update check")
	assert.Nil(t, update)
}

func TestCheckOllamaUpdate_DistinguishesACurrentVersionFromAReleaseCheckFailure(t *testing.T) {
	bin := filepath.Join(t.TempDir(), filepath.Base(fakeEngineBin))
	copyFile(t, fakeEngineBin, bin)
	require.NoError(t, os.WriteFile(bin+".version", []byte("0.10.0"), 0o600))
	manifest := testEngineManifest(bin)
	manifest.Engine = "ollama"
	ex := newTestExecutor(t, manifest)

	t.Run("current version clears the notification", func(t *testing.T) {
		mockOllamaFeed(t, ex, http.StatusOK, `{"tag_name":"v0.10.0"}`)
		update, err := ex.CheckOllamaUpdate(t.Context())
		require.NoError(t, err)
		assert.Nil(t, update)
	})
	t.Run("rate limited check returns an error", func(t *testing.T) {
		mockOllamaFeed(t, ex, http.StatusTooManyRequests, "")
		update, err := ex.CheckOllamaUpdate(t.Context())
		require.ErrorContains(t, err, "HTTP 429")
		assert.Nil(t, update)
	})
}
