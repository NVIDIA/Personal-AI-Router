// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
