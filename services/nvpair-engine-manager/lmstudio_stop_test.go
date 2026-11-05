// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLMStudioStop_EnablesHostDetectionOnEveryPlatform(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	manifest, ok := reg.Get("lmstudio")
	require.True(t, ok)
	require.NotEmpty(t, manifest.Platforms)
	for key, platform := range manifest.Platforms {
		t.Run(key, func(t *testing.T) {
			require.NotNil(t, platform.Runtime.Stop)
			assert.Equal(t, []string{"{cli}", "daemon", "down"}, platform.Runtime.Stop.Cmd)
		})
	}
}

func TestLMStudioStop_UsesHostSpecificShutdown(t *testing.T) {
	test := func(name, status string, want []string) {
		t.Run(name, func(t *testing.T) {
			ex, marker := lmStudioStopExecutor(t, status)

			require.NoError(t, ex.Stop("lmstudio"))

			data, err := os.ReadFile(marker)
			require.NoError(t, err)
			var command []string
			require.NoError(t, json.Unmarshal(data, &command))
			assert.Equal(t, want, command)
		})
	}
	test("headless daemon", `{"status":"running","isDaemon":true}`, []string{"daemon", "down"})
	test("GUI server", `{"status":"running","isDaemon":false}`, []string{"server", "stop"})
}

func TestLMStudioStop_RejectsUnidentifiedHost(t *testing.T) {
	test := func(name, status string) {
		t.Run(name, func(t *testing.T) {
			ex, marker := lmStudioStopExecutor(t, status)

			require.Error(t, ex.Stop("lmstudio"))
			assert.NoFileExists(t, marker, "an unidentified host must not receive a stop command")
		})
	}
	test("status command failed", "command-error")
	test("malformed JSON", "not JSON")
	test("missing status", `{}`)
	test("missing host type", `{"status":"running"}`)
	test("unknown status", `{"status":"unexpected","isDaemon":false}`)
}

func TestLMStudioStop_DoesNotStartAnAbsentHost(t *testing.T) {
	ex, marker := lmStudioStopExecutor(t, `{"status":"not-running"}`)

	require.NoError(t, ex.Stop("lmstudio"))
	assert.NoFileExists(t, marker)
}

func TestLMStudioStop_PreservesCustomStopCommand(t *testing.T) {
	ex, marker := lmStudioStopExecutor(t, "command-error")
	st, err := ex.state("lmstudio")
	require.NoError(t, err)
	st.plat.Runtime.Stop.Cmd = []string{fakeEngineBin, "touch", marker}

	require.NoError(t, ex.Stop("lmstudio"))
	assert.FileExists(t, marker)
}

func TestLMStudioStop_WaitsForDelayedListenerClosure(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	manifest, ok := reg.Get("lmstudio")
	require.True(t, ok)
	platform := manifest.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ex, _ := lmStudioStopExecutor(t, `{"status":"running","isDaemon":true}`)
	st, err := ex.state("lmstudio")
	require.NoError(t, err)
	st.plat.Runtime.Stop = platform.Runtime.Stop
	st.plat.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	st.port = portOf(t, srv.URL)
	closeTimer := time.AfterFunc(7*time.Second, srv.Close)
	defer closeTimer.Stop()

	require.NoError(t, ex.Stop("lmstudio"))
	assert.False(t, st.running)
}

func lmStudioStopExecutor(t *testing.T, status string) (*Executor, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "stop.json")
	t.Setenv("PAIR_TEST_LMS_STATUS", status)
	t.Setenv("PAIR_TEST_LMS_STOP", marker)
	manifest := &Manifest{
		Engine: "lmstudio", DisplayName: "LM Studio", ManifestVersion: 1,
		Platforms: map[string]Platform{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Detect: []string{fakeEngineBin},
				Runtime: Runtime{
					Mode: "command", CLI: fakeEngineBin,
					Stop: &StopSpec{Cmd: []string{"{cli}", "daemon", "down"}},
				},
			},
		},
	}
	ex := newTestExecutor(t, manifest)
	st, err := ex.state("lmstudio")
	require.NoError(t, err)
	st.installed, st.running, st.healthy = true, true, true
	return ex, marker
}
