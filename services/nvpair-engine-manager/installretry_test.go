// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type installFixture struct {
	ex       *Executor
	manifest *Manifest
	counter  string
	marker   string
	requests atomic.Int32
	progress installProgressRecorder
}

func newInstallFixture(t *testing.T, engine, diagnostic string, failures int) *installFixture {
	t.Helper()
	f := &installFixture{counter: filepath.Join(t.TempDir(), "attempts"), marker: filepath.Join(t.TempDir(), "installed")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.requests.Add(1)
		writeTestResponse(t, w, []byte(diagnostic))
	}))
	t.Cleanup(server.Close)
	f.manifest = testEngineManifest(fakeEngineBin)
	f.manifest.Engine = engine
	p := f.manifest.Platforms[hostKey()]
	p.Detect = []string{f.marker}
	p.Install = &Install{
		Fetch: &Fetch{URL: server.URL + "/install.sh"},
		Run:   []string{fakeEngineBin, "installsim", f.counter, f.marker, strconv.Itoa(failures), "{download}"},
	}
	f.manifest.Platforms[hostKey()] = p
	require.NoError(t, f.manifest.Validate())
	f.ex = newTestExecutor(t, f.manifest)
	f.ex.installRetryBackoff = time.Millisecond
	f.ex.emit = f.progress.emit
	return f
}

func TestLMStudioInstall_RecoversWithoutReportingIntermediateFailure(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "install.sh: line 228: _checksum_value: unbound variable", 2)
	f.ex.emit = func(method string, params any) {
		f.progress.emit(method, params)
		if method == "engine:install-progress" {
			p := params.(map[string]any)
			if _, measured := p["percent"]; p["stage"] == "downloading" && !measured {
				assertNoArtifactTemps(t, "lmstudio")
			}
		}
	}
	require.NoError(t, f.ex.Install(t.Context(), "lmstudio"))
	require.Equal(t, 3, readCount(t, f.counter))
	require.Equal(t, int32(3), f.requests.Load())
	f.progress.assertSingleTerminal(t, "done")
	require.Empty(t, f.ex.reporter.snapshot())
	st, err := f.ex.state("lmstudio")
	require.NoError(t, err)
	require.True(t, installedByPAIR(st.installDir))
	assertNoArtifactTemps(t, "lmstudio")
}

func TestLMStudioInstall_ReportsFailureAfterRetryBudget(t *testing.T) {
	test := func(name, diagnostic, want string) {
		t.Run(name, func(t *testing.T) {
			f := newInstallFixture(t, "lmstudio", diagnostic, 10)
			err := f.ex.Install(t.Context(), "lmstudio")
			require.EqualError(t, err, want)
			require.Equal(t, 3, readCount(t, f.counter))
			require.Equal(t, int32(3), f.requests.Load())
			f.progress.assertSingleTerminal(t, "failed")
			require.Equal(t, want, f.ex.reporter.snapshot()[0].Message)
			require.NoFileExists(t, f.marker)
			assertNoArtifactTemps(t, "lmstudio")
		})
	}
	test("timeout", "curl: (28) Operation timed out\n_checksum_value: unbound variable", "LM Studio download timed out after 3 attempts. Check your connection and try again.")
	test("checksum fetch", "line 213: _checksum_value: unbound variable", "LM Studio’s installer could not download its checksum after 3 attempts. Check your connection and try again.")
}

func TestLMStudioInstall_DoesNotRetryPermanentFailures(t *testing.T) {
	for _, diagnostic := range []string{"permission denied", "need curl (command not found)", "no compatible downloads were found", "unexpected installer failure"} {
		t.Run(diagnostic, func(t *testing.T) {
			f := newInstallFixture(t, "lmstudio", diagnostic, 10)
			require.ErrorContains(t, f.ex.Install(t.Context(), "lmstudio"), diagnostic)
			require.Equal(t, 1, readCount(t, f.counter))
			f.progress.assertSingleTerminal(t, "failed")
		})
	}
}

func TestLMStudioInstall_RejectsChecksumMismatchWithoutRunningInstaller(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "payload", 0)
	f.manifest.Platforms[hostKey()].Install.Fetch.SHA256 = strings.Repeat("0", 64)
	require.ErrorContains(t, f.ex.Install(t.Context(), "lmstudio"), "checksum mismatch")
	require.Equal(t, int32(1), f.requests.Load())
	require.NoFileExists(t, f.counter)
	assertNoArtifactTemps(t, "lmstudio")
}

func TestOtherEngineInstall_DoesNotRetryDownloadFailure(t *testing.T) {
	f := newInstallFixture(t, "other", "curl: (28) Operation timed out", 10)
	require.ErrorContains(t, f.ex.Install(t.Context(), "other"), "curl: (28)")
	require.Equal(t, 1, readCount(t, f.counter))
	f.progress.assertSingleTerminal(t, "failed")
}

func TestLMStudioInstall_RecoversFromTransientScriptFetchFailure(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "payload", 0)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeTestResponse(t, w, []byte("payload"))
	}))
	defer server.Close()
	f.manifest.Platforms[hostKey()].Install.Fetch.URL = server.URL
	require.NoError(t, f.ex.Install(t.Context(), "lmstudio"))
	require.Equal(t, int32(3), requests.Load())
	require.Equal(t, 1, readCount(t, f.counter))
	f.progress.assertSingleTerminal(t, "done")
}

func TestLMStudioInstall_CancelsPendingRetry(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "payload", 0)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	f.manifest.Platforms[hostKey()].Install.Fetch.URL = server.URL
	f.ex.installRetryBackoff = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, f.ex.Install(ctx, "lmstudio"), context.DeadlineExceeded)
	require.Equal(t, int32(1), requests.Load())
	require.NoFileExists(t, f.counter)
	f.progress.assertSingleTerminal(t, "failed")
}

func TestLMStudioInstall_CancelsActiveScriptFetch(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "payload", 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestResponse(t, w, []byte("partial"))
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	f.manifest.Platforms[hostKey()].Install.Fetch.URL = server.URL
	require.ErrorIs(t, f.ex.Install(ctx, "lmstudio"), context.Canceled)
	require.NoFileExists(t, f.counter)
	assertNoArtifactTemps(t, "lmstudio")
	require.NoFileExists(t, f.marker)
}
