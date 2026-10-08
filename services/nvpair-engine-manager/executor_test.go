// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testEngineManifest returns a manifest whose host-platform block runs
// the prebuilt fake-engine, so lifecycle tests exercise the real spawn
// + probe + action + stop path with no real engine.
func testEngineManifest(bin string) *Manifest {
	key := runtime.GOOS + "/" + runtime.GOARCH
	return &Manifest{
		Engine:          "fake",
		DisplayName:     "Fake Engine",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect: []string{bin},
				Runtime: Runtime{
					Bin:    bin,
					Env:    map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}"},
					Port:   0, // auto-assign a free loopback port
					Ready:  &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 10},
					Health: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, IntervalS: 1},
					Stop:   &StopSpec{Signal: "term", GraceS: 3},
				},
			},
		},
		Actions: map[string]Action{
			"list_models": {HTTP: &ActionHTTP{Method: "GET", Path: "/api/tags"}},
		},
	}
}

func newTestExecutor(t *testing.T, m *Manifest) *Executor {
	t.Helper()
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	return NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
}

func responseHeaderTimeout(t *testing.T, client *http.Client) time.Duration {
	t.Helper()
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok, "transport type")
	return transport.ResponseHeaderTimeout
}

func TestEngineHTTPClientsBoundResponseHeaders(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	require.Equal(t, engineResponseHeaderTimeout, responseHeaderTimeout(t, ex.client), "ordinary response-header timeout")
	require.Equal(t, ollamaLoadResponseHeaderTimeout, responseHeaderTimeout(t, ex.ollamaLoadClient), "Ollama load response-header timeout")
}

func TestOnlyOllamaRunModelUsesSlowResponseHeaderBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := testEngineManifest(fakeEngineBin)
	m.Engine = "ollama"
	platform := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	platform.Runtime.Port = port
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = platform
	m.Actions = map[string]Action{
		"run_model":    {HTTP: &ActionHTTP{Method: "POST", Path: "/api/generate"}},
		"delete_model": {HTTP: &ActionHTTP{Method: "DELETE", Path: "/api/delete"}},
	}
	ex := newTestExecutor(t, m)
	ex.client = newEngineHTTPClient(20 * time.Millisecond)
	ex.ollamaLoadClient = newEngineHTTPClient(500 * time.Millisecond)
	st, err := ex.state("ollama")
	require.NoError(t, err)
	st.running = true

	_, err = ex.Action(context.Background(), "ollama", "run_model", json.RawMessage(`{"model":"tiny"}`))
	require.NoError(t, err, "Ollama load was cut off by the ordinary response-header budget")
	_, err = ex.Action(context.Background(), "ollama", "delete_model", json.RawMessage(`{"name":"tiny"}`))
	require.ErrorContains(t, err, "timeout awaiting response headers", "ordinary action error")

	other := testEngineManifest(fakeEngineBin)
	other.Engine = "other"
	otherPlatform := other.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	otherPlatform.Runtime.Port = port
	other.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = otherPlatform
	other.Actions = map[string]Action{
		"run_model": {HTTP: &ActionHTTP{Method: "POST", Path: "/api/generate"}},
	}
	otherEx := newTestExecutor(t, other)
	otherEx.client = newEngineHTTPClient(20 * time.Millisecond)
	otherEx.ollamaLoadClient = newEngineHTTPClient(500 * time.Millisecond)
	otherState, err := otherEx.state("other")
	require.NoError(t, err)
	otherState.running = true
	_, err = otherEx.Action(context.Background(), "other", "run_model", json.RawMessage(`{"model":"tiny"}`))
	require.ErrorContains(t, err, "timeout awaiting response headers", "non-Ollama run_model error")
}

func TestHTTPActionSendsStringParamsInQuery(t *testing.T) {
	type observedRequest struct {
		method      string
		rawQuery    string
		model       string
		contentType string
		body        []byte
		readErr     error
	}

	observed := make(chan observedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		observed <- observedRequest{
			method:      r.Method,
			rawQuery:    r.URL.RawQuery,
			model:       r.URL.Query().Get("model"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
			readErr:     err,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	serverURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	m := testEngineManifest(fakeEngineBin)
	m.Engine = "llamacpp"
	platform := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	platform.Runtime.Port = port
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = platform
	m.Actions = map[string]Action{
		"delete_model": {
			HTTP: &ActionHTTP{
				Method:   http.MethodDelete,
				Path:     "/models",
				ParamsIn: actionHTTPParamsQuery,
			},
		},
	}

	ex := newTestExecutor(t, m)
	st, err := ex.state("llamacpp")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	st.running = true

	const model = "owner/repository:Q4_K_M"
	res, err := ex.Action(
		context.Background(),
		"llamacpp",
		"delete_model",
		json.RawMessage(`{"model":"owner/repository:Q4_K_M"}`),
	)
	if err != nil {
		t.Fatalf("delete_model: %v", err)
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if !result.Success {
		t.Fatalf("delete response = %s, want success", res)
	}

	got := <-observed
	if got.readErr != nil {
		t.Fatalf("read request body: %v", got.readErr)
	}
	if got.method != http.MethodDelete {
		t.Errorf("method = %q, want %q", got.method, http.MethodDelete)
	}
	if got.model != model {
		t.Errorf("model query = %q, want %q", got.model, model)
	}
	if got.rawQuery != "model=owner%2Frepository%3AQ4_K_M" {
		t.Errorf("raw query = %q, want URL-encoded model id", got.rawQuery)
	}
	if len(got.body) != 0 {
		t.Errorf("body = %q, want empty", got.body)
	}
	if got.contentType != "" {
		t.Errorf("Content-Type = %q, want empty without a body", got.contentType)
	}
}

func TestHTTPActionRejectsNonStringQueryParams(t *testing.T) {
	_, err := actionQueryParams(json.RawMessage(`{"model":42}`))
	if err == nil {
		t.Fatal("non-string query param accepted")
	}
	if !strings.Contains(err.Error(), "string values") {
		t.Fatalf("error = %q, want string-values requirement", err)
	}
}

func TestEngineLifecycle(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })

	installed, err := ex.Detect("fake")
	require.NoError(t, err, "detect")
	require.True(t, installed, "expected fake engine to be detected (bin (%v)", fakeEngineBin)

	require.NoError(t, ex.Start(ctx, "fake"), "start")
	st, _ := ex.Status("fake")
	require.True(t, st.Running, "expected running+healthy (%v)", st)
	require.True(t, st.Healthy, "expected running+healthy (%v)", st)
	require.NotEqual(t, 0, st.Port, "expected a non-zero port (%v)", st)

	res, err := ex.Action(ctx, "fake", "list_models", nil)
	require.NoError(t, err, "action")
	require.Contains(t, string(res), "llama3.2", "unexpected action result (%v)", res)

	require.NoError(t, ex.Stop("fake"), "stop")
	st, _ = ex.Status("fake")
	require.False(t, st.Running, "expected stopped (%v)", st)
}

func TestCrashThenExternalServiceIsAdoptedWithoutRespawn(t *testing.T) {
	port, err := freePort()
	require.NoError(t, err)
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	m.Platforms[key] = p
	ex := newTestExecutor(t, m)

	require.NoError(t, ex.Start(context.Background(), m.Engine), "initial start")
	resp, _ := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/exit")
	if resp != nil {
		_ = resp.Body.Close()
	}
	state, _ := ex.state(m.Engine)
	waitFor(t, 5*time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return !state.running
	})

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err, "external listener")
	external := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = external.Serve(ln) }()
	defer func() {
		_ = external.Close()
		_, _ = ex.Status(m.Engine)
	}()

	require.NoError(t, ex.Start(context.Background(), m.Engine), "restart should adopt the external service")
	state.mu.Lock()
	proc := state.proc
	adopted := state.adopted
	running := state.running
	healthy := state.healthy
	state.mu.Unlock()
	require.Nil(t, proc, "restart spawned over external service: proc (%v, %v, %v)", adopted, running, healthy)
	require.True(t, adopted, "restart spawned over external service: proc (%v, %v, %v)", adopted, running, healthy)
	require.True(t, running, "restart spawned over external service: proc (%v, %v, %v)", adopted, running, healthy)
	require.True(t, healthy, "restart spawned over external service: proc (%v, %v, %v)", adopted, running, healthy)
}

func TestActionRequiresRunning(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	_, err := ex.Action(context.Background(), "fake", "list_models", nil)
	require.Error(t, err, "expected error: action on a non-running engine")
}

func TestStartUnknownEngine(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	require.Error(t, ex.Start(context.Background(), "nope"), "expected error for unknown engine")
}

func TestStartPortOverride(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin)) // manifest Port is 0
	t.Cleanup(func() { _ = ex.Stop("fake") })
	const want = 17777
	require.NoError(t, ex.StartWith(context.Background(), "fake", startOpts{Port: want}), "start with port override")
	st, _ := ex.Status("fake")
	require.Equal(t, want, st.Port, "expected port")
}

func TestEffectiveBind(t *testing.T) {
	cases := []struct{ manifest, override, want string }{
		{"", "", "127.0.0.1"},                 // ordinary engine: safe default
		{"0.0.0.0", "", "0.0.0.0"},            // inference engine declares open
		{"0.0.0.0", "127.0.0.1", "127.0.0.1"}, // per-call lock-down wins
		{"", "192.168.1.5", "192.168.1.5"},    // per-call specific interface
	}
	for _, c := range cases {
		assert.Equal(t, c.want, effectiveBind(c.manifest, c.override), "effectiveBind")
	}
}

// TestStatusReportsExternallyRunning covers the adoption false-negative:
// Status must probe a fixed-port engine and report an externally-started
// instance as installed and running, even when its binary lives outside every
// NVPAIR-managed detection path. Only the engine-specific readiness probe
// should decide whether the external service is present.
func TestStatusReportsExternallyRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "ext", DisplayName: "External", ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect: []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
				Runtime: Runtime{
					Bin:   filepath.Join(t.TempDir(), "does-not-exist"+exeExt()),
					Port:  port,
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 5},
				},
			},
		},
	}
	ex := newTestExecutor(t, m)
	st, _ := ex.Status("ext") // never started via the executor
	require.True(t, st.Installed, "expected externally-running engine adopted as installed and healthy (%v)", st)
	require.True(t, st.Running, "expected externally-running engine adopted as installed and healthy (%v)", st)
	require.True(t, st.Healthy, "expected externally-running engine adopted as installed and healthy (%v)", st)
}

func TestStatusAtPortAdoptsLegacyListener(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	legacyPort, _ := strconv.Atoi(u.Port())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "legacy", DisplayName: "Legacy", ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect: []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
				Runtime: Runtime{
					Bin:   filepath.Join(t.TempDir(), "does-not-exist"+exeExt()),
					Port:  legacyPort + 1,
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 5},
				},
			},
		},
	}
	ex := newTestExecutor(t, m)
	st, err := ex.StatusAtPort("legacy", legacyPort)
	require.NoError(t, err)
	require.True(t, st.Installed, "legacy listener was not adopted at (%v, %v)", legacyPort, st)
	require.True(t, st.Running, "legacy listener was not adopted at (%v, %v)", legacyPort, st)
	require.True(t, st.Healthy, "legacy listener was not adopted at (%v, %v)", legacyPort, st)
	require.Equal(t, legacyPort, st.Port, "legacy listener was not adopted (%v)", st)
}

func TestGetInstalledAdoptsExternallyRunningEngineWithoutDetectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "ext-list", DisplayName: "External List", ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect: []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
				Runtime: Runtime{
					Bin:   filepath.Join(t.TempDir(), "does-not-exist"+exeExt()),
					Port:  port,
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 5},
				},
			},
		},
	}
	ex := newTestExecutor(t, m)
	statuses := ex.GetInstalled()
	require.Len(t, statuses, 1, "expected get-installed to adopt the external engine")
	require.True(t, statuses[0].Installed, "expected get-installed to adopt the external engine (%v)", statuses)
	require.True(t, statuses[0].Running, "expected get-installed to adopt the external engine (%v)", statuses)
	require.True(t, statuses[0].Healthy, "expected get-installed to adopt the external engine (%v)", statuses)
}

func TestInstallAdoptsExternalServiceWithoutDownloading(t *testing.T) {
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"test"}`))
	}))
	defer engineSrv.Close()
	engineURL, _ := url.Parse(engineSrv.URL)
	enginePort, _ := strconv.Atoi(engineURL.Port())

	var downloads atomic.Int32
	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write([]byte("must-not-download"))
	}))
	defer downloadSrv.Close()

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "external-install", DisplayName: "External Install", ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect:  []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
				Install: &Install{Fetch: &Fetch{URL: downloadSrv.URL}},
				Runtime: Runtime{
					Bin:   filepath.Join(t.TempDir(), "managed"+exeExt()),
					Port:  enginePort,
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/api/version", Status: http.StatusOK},
				},
			},
		},
	}
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	var methods []string
	ex := NewExecutor(reg, NewReporter(nil), func(method string, _ any) { methods = append(methods, method) }, t.TempDir())

	require.NoError(t, ex.Install(context.Background(), m.Engine), "install should adopt the external service")
	require.Equal(t, int32(0), downloads.Load(), "external service triggered")
	st, err := ex.Status(m.Engine)
	require.NoError(t, err, "external service status (%v, %v)", st, err)
	require.True(t, st.Installed, "external service status (%v, %v)", st, err)
	require.True(t, st.Running, "external service status (%v, %v)", st, err)
	require.True(t, st.Healthy, "external service status (%v, %v)", st, err)
	require.Contains(t, methods, "engine:state-changed", "install adoption notifications")
	require.Contains(t, methods, "engine:install-progress", "install adoption notifications")
}

func TestInstallDoesNotOverwriteUnknownListener(t *testing.T) {
	occupant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer occupant.Close()
	u, _ := url.Parse(occupant.URL)
	port, _ := strconv.Atoi(u.Port())

	var downloads atomic.Int32
	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write([]byte("must-not-download"))
	}))
	defer downloadSrv.Close()

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "occupied", DisplayName: "Expected Engine", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Detect:  []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
			Install: &Install{Fetch: &Fetch{URL: downloadSrv.URL}},
			Runtime: Runtime{Port: port, Ready: &Probe{
				HTTP: "http://127.0.0.1:{port}/api/version", Status: http.StatusOK,
			}},
		}},
	}
	ex := newTestExecutor(t, m)
	require.ErrorContains(t, ex.Install(context.Background(), m.Engine), "occupied", "install over an unknown listener error")
	require.Equal(t, int32(0), downloads.Load(), "unknown listener triggered")
	st, _ := ex.Status(m.Engine)
	require.False(t, st.Installed, "unknown listener must not be adopted (%v)", st)
	require.False(t, st.Running, "unknown listener must not be adopted (%v)", st)
}

func TestCommandModeMissingCLIInstallsDespiteLiveAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	cli := filepath.Join(t.TempDir(), "lms"+exeExt())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "missing-cli", DisplayName: "Missing CLI", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Detect:  []string{cli},
			Install: &Install{Run: []string{fakeEngineBin, "touch", cli}},
			Runtime: Runtime{
				Mode: "command", CLI: cli, Port: port,
				Ready: &Probe{HTTP: "http://127.0.0.1:{port}/v1/models", Status: http.StatusOK},
			},
		}},
	}
	ex := newTestExecutor(t, m)
	st, err := ex.Status(m.Engine)
	require.NoError(t, err, "live API without CLI status (%v, %v)", st, err)
	require.False(t, st.Installed, "live API without CLI status (%v, %v)", st, err)
	require.False(t, st.Running, "live API without CLI status (%v, %v)", st, err)
	require.NoError(t, ex.Install(context.Background(), m.Engine), "install missing command-mode CLI")
	require.FileExists(t, cli, "live API incorrectly suppressed command-mode installer")
}

func TestStatusDoesNotAdoptFacadeOnDifferentConfiguredPort(t *testing.T) {
	facade := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte(`{"version":"facade"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer facade.Close()
	backendPort, err := freePort()
	require.NoError(t, err)
	require.NotEqual(t, backendPort, portOf(t, facade.URL), "test requires distinct facade and backend ports")

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "facade-guard", DisplayName: "Facade Guard", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Detect: []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
			Runtime: Runtime{
				Port:  backendPort,
				Ready: &Probe{HTTP: "http://127.0.0.1:{port}/api/version", Status: http.StatusOK},
			},
		}},
	}
	ex := newTestExecutor(t, m)
	st, err := ex.Status(m.Engine)
	require.NoError(t, err, "facade on a different port was adopted: status (%v, %v)", st, err)
	require.False(t, st.Installed, "facade on a different port was adopted: status (%v, %v)", st, err)
	require.False(t, st.Running, "facade on a different port was adopted: status (%v, %v)", st, err)
}

func TestAdoptedExternalLivenessIsTruthful(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "external-liveness", DisplayName: "External Liveness", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Detect: []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
			Runtime: Runtime{Port: port, Ready: &Probe{
				HTTP: "http://127.0.0.1:{port}/", Status: http.StatusOK,
			}},
		}},
	}
	ex := newTestExecutor(t, m)
	st, _ := ex.Status(m.Engine)
	require.True(t, st.Running, "initial external status (%v)", st)
	require.True(t, st.Healthy, "initial external status (%v)", st)

	status.Store(http.StatusServiceUnavailable)
	st, _ = ex.Status(m.Engine)
	require.True(t, st.Running, "HTTP 503 is live-but-unhealthy (%v)", st)
	require.False(t, st.Healthy, "HTTP 503 is live-but-unhealthy (%v)", st)

	srv.Close()
	st, _ = ex.Status(m.Engine)
	require.False(t, st.Installed, "closed external listener remained present (%v)", st)
	require.False(t, st.Running, "closed external listener remained present (%v)", st)
	require.False(t, st.Healthy, "closed external listener remained present (%v)", st)
}

func TestProxyFacadeCannotIdentifyAsOllama(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(engineIdentityProbeHeader) == "1" {
			probes.Add(1)
			http.Error(w, "proxy facade", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := testEngineManifest(fakeEngineBin)
	m.Engine = "ollama"
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = []string{filepath.Join(t.TempDir(), "missing"+exeExt())}
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/api/version", Status: http.StatusOK}
	m.Platforms[key] = p
	ex := newTestExecutor(t, m)

	st, err := ex.Status("ollama")
	require.NoError(t, err, "proxy facade was adopted: status (%v, %v)", st, err)
	require.False(t, st.Installed, "proxy facade was adopted: status (%v, %v)", st, err)
	require.False(t, st.Running, "proxy facade was adopted: status (%v, %v)", st, err)
	require.False(t, st.Healthy, "proxy facade was adopted: status (%v, %v)", st, err)
	require.ErrorContains(t, ex.Install(context.Background(), "ollama"), "occupied", "install over proxy facade error")
	require.ErrorContains(t, ex.Start(context.Background(), "ollama"), "occupied", "start over proxy facade error")
	state, _ := ex.state("ollama")
	state.mu.Lock()
	state.running = true // stale pre-transition state must not route an action through the facade
	state.mu.Unlock()
	_, err = ex.Action(context.Background(), "ollama", "list_models", nil)
	require.ErrorContains(t, err, "HTTP 409", "action through proxy facade error")
	require.NotEqual(t, int32(0), probes.Load(), "engine identity probe did not carry the proxy sentinel")
}

func TestPreviouslyAdoptedForeignReplacementFailsClosed(t *testing.T) {
	var isOllama atomic.Bool
	isOllama.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" && isOllama.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK) // reachable listener, but no Ollama identity
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := testEngineManifest(fakeEngineBin)
	m.Engine = "ollama"
	m.DisplayName = "Ollama"
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = []string{filepath.Join(t.TempDir(), "missing"+exeExt())}
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/api/version", Status: http.StatusOK}
	m.Platforms[key] = p
	ex := newTestExecutor(t, m)

	st, err := ex.Status("ollama")
	require.NoError(t, err, "initial adoption (%v, %v)", st, err)
	require.True(t, st.Running, "initial adoption (%v, %v)", st, err)
	require.True(t, st.Healthy, "initial adoption (%v, %v)", st, err)
	isOllama.Store(false)
	st, err = ex.Status("ollama")
	require.NoError(t, err, "foreign replacement status (%v, %v)", st, err)
	require.True(t, st.Running, "foreign replacement status (%v, %v)", st, err)
	require.False(t, st.Healthy, "foreign replacement status (%v, %v)", st, err)
	require.ErrorContains(t, ex.Start(context.Background(), "ollama"), "occupied", "Start accepted foreign replacement")
	require.ErrorContains(t, ex.Install(context.Background(), "ollama"), "occupied", "Install accepted foreign replacement")
}

// TestUninstallTerminatesRunningInstance covers a uniquely-named managed
// engine started without an executor process handle. Uninstall must reconcile
// and reclaim that exact managed image before deleting it.
func TestUninstallTerminatesRunningInstance(t *testing.T) {
	baseDir := t.TempDir()
	bin := filepath.Join(baseDir, "fake", "fakeu"+strconv.Itoa(os.Getpid())+exeExt())
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o755))
	copyFile(t, fakeEngineBin, bin)

	port, err := freePort()
	require.NoError(t, err)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "OLLAMA_HOST=127.0.0.1:"+strconv.Itoa(port))
	configureSysProcAttr(cmd)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitPortServing(t, port)

	m := testEngineManifest(bin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = []string{bin}
	p.Runtime.Port = port
	p.Uninstall = &Uninstall{Run: rmCmd(bin)}
	m.Platforms[key] = p

	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)
	require.NoError(t, ex.Uninstall(context.Background(), "fake"), "uninstall")
	ok, _ := ex.Detect("fake")
	require.False(t, ok, "engine still detected after uninstall")
	require.False(t, portServing(port), "expected uninstall to stop the running instance")
}

func TestUninstallDoesNotKillExternalSameNameProcess(t *testing.T) {
	baseDir := t.TempDir()
	name := "fakeu" + strconv.Itoa(os.Getpid()) + exeExt()
	managedBin := filepath.Join(baseDir, "fake", name)
	externalBin := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Dir(managedBin), 0o755))
	copyFile(t, fakeEngineBin, managedBin)
	copyFile(t, fakeEngineBin, externalBin)

	port, err := freePort()
	require.NoError(t, err)
	cmd := exec.Command(externalBin)
	cmd.Env = append(os.Environ(), "OLLAMA_HOST=127.0.0.1:"+strconv.Itoa(port))
	configureSysProcAttr(cmd)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitPortServing(t, port)

	m := testEngineManifest(managedBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = []string{managedBin}
	p.Runtime.Bin = managedBin
	p.Runtime.Port = port
	p.Uninstall = &Uninstall{Run: rmCmd(managedBin)}
	m.Platforms[key] = p
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)

	require.ErrorContains(t, ex.Uninstall(context.Background(), m.Engine), "external management", "uninstall external same-name owner error")
	require.True(t, portServing(port), "uninstall killed the externally owned process")
	require.FileExists(t, managedBin, "uninstall removed managed files after refusing the external owner")
}

func TestUninstallRefusesExternalProcessEngine(t *testing.T) {
	externalBin := filepath.Join(t.TempDir(), "external"+exeExt())
	copyFile(t, fakeEngineBin, externalBin)
	m := testEngineManifest(externalBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = []string{externalBin}
	p.Uninstall = &Uninstall{Run: rmCmd(externalBin)}
	m.Platforms[key] = p

	ex := newTestExecutor(t, m) // managed base is a different temp directory
	require.ErrorContains(t, ex.Uninstall(context.Background(), m.Engine), "outside NVPAIR's managed install directory", "external uninstall error")
	require.FileExists(t, externalBin, "external executable was removed")
}

func TestUninstallRefusesUnidentifiedLiveListener(t *testing.T) {
	baseDir := t.TempDir()
	managedBin := filepath.Join(baseDir, "fake", "fake"+exeExt())
	require.NoError(t, os.MkdirAll(filepath.Dir(managedBin), 0o755))
	copyFile(t, fakeEngineBin, managedBin)

	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := testEngineManifest(managedBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = []string{managedBin}
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/api/version", Status: http.StatusOK}
	p.Uninstall = &Uninstall{Run: rmCmd(managedBin)}
	m.Platforms[key] = p

	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)
	require.ErrorContains(t, ex.Uninstall(context.Background(), m.Engine), "occupied by an unidentified service", "uninstall unidentified listener error")
	require.FileExists(t, managedBin, "uninstall removed managed files while the target port was occupied")
}

func waitPortServing(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if portServing(port) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.FailNowf(t, "port never started serving", "port %d", port)
}

func portServing(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func TestDownloadVerify(t *testing.T) {
	payload := []byte("fake engine payload bytes")
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	ctx := context.Background()

	p, err := ex.download(ctx, "fake", &Fetch{URL: srv.URL + "/installer.ps1", SHA256: good})
	require.NoError(t, err, "download (ok)")
	defer os.Remove(p)
	require.Equal(t, ".ps1", filepath.Ext(p), "downloaded path (%v)", p)
	got, _ := os.ReadFile(p)
	require.Equal(t, string(payload), string(got), "downloaded content mismatch")

	_, err = ex.download(ctx, "fake", &Fetch{URL: srv.URL + "/installer.ps1", SHA256: "deadbeef"})
	require.ErrorContains(t, err, "checksum mismatch", "expected checksum mismatch")

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	_, err = ex.download(ctx, "fake", &Fetch{URL: notFound.URL, SHA256: good})
	require.ErrorContains(t, err, "HTTP 404", "expected HTTP 404")
}

func TestInstallRefusesAdmin(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = nil // ensure not-detected so install reaches the mode check
	p.Install = &Install{Fetch: &Fetch{URL: "https://example/x", SHA256: "abc"}, Run: []string{"echo"}, Mode: "admin"}
	m.Platforms[key] = p

	ex := newTestExecutor(t, m)
	require.ErrorContains(t, ex.Install(context.Background(), "fake"), "refused", "expected admin-install refusal")
}

func TestEngineUninstall(t *testing.T) {
	// Copy the fake engine so uninstall can delete the copy without
	// disturbing the shared build artifact other tests rely on.
	baseDir := t.TempDir()
	binCopy := filepath.Join(baseDir, "fake", "fake"+exeExt())
	require.NoError(t, os.MkdirAll(filepath.Dir(binCopy), 0o755))
	copyFile(t, fakeEngineBin, binCopy)

	m := testEngineManifest(binCopy)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = []string{binCopy}
	p.Uninstall = &Uninstall{Run: rmCmd(binCopy)}
	m.Platforms[key] = p

	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)
	ok, _ := ex.Detect("fake")
	require.True(t, ok, "expected installed before uninstall")
	require.NoError(t, ex.Uninstall(context.Background(), "fake"), "uninstall")
	ok, _ = ex.Detect("fake")
	require.False(t, ok, "expected not-installed after uninstall")
}

func TestUninstallNoOpWhenAbsent(t *testing.T) {
	m := testEngineManifest(filepath.Join(t.TempDir(), "absent"+exeExt()))
	ex := newTestExecutor(t, m)
	require.NoError(t, ex.Uninstall(context.Background(), "fake"), "uninstall of an absent engine should be a no-op")
}

// TestCommandModeLifecycle exercises the "command" runtime mode: start
// commands run, readiness is determined by a probe (here a stand-in
// HTTP server for the daemon's API), and the stop command runs. Marker
// files prove the commands executed.
func TestCommandModeLifecycle(t *testing.T) {
	dir := t.TempDir()
	startMarker := filepath.Join(dir, "started")
	stopMarker := filepath.Join(dir, "stopped")

	// The stand-in daemon only "serves" once the start command has run
	// (created startMarker), so the adoption probe doesn't short-circuit
	// the start sequence.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fileExists(startMarker) && !fileExists(stopMarker) {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	// Model a real command-mode daemon: once the stop command has run (created
	// stopMarker) the daemon exits and its port stops accepting connections.
	// waitUnavailable confirms a stop by an actual TCP refusal, not a 503 from a
	// still-listening socket, so the stand-in must genuinely close its listener.
	var closeOnce sync.Once
	closeServer := func() { closeOnce.Do(srv.Close) }
	defer closeServer()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(50 * time.Millisecond):
				if fileExists(stopMarker) {
					closeServer()
					return
				}
			}
		}
	}()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "daemon", DisplayName: "Daemon Engine", ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect: []string{fakeEngineBin},
				Runtime: Runtime{
					Mode:   "command",
					Port:   port,
					Start:  [][]string{{fakeEngineBin, "touch", startMarker}},
					Stop:   &StopSpec{Cmd: []string{fakeEngineBin, "touch", stopMarker}},
					Ready:  &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 10},
					Health: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, IntervalS: 1},
				},
			},
		},
	}
	ex := newTestExecutor(t, m)

	require.NoError(t, ex.Start(context.Background(), "daemon"), "start")
	require.FileExists(t, startMarker, "expected the start command to have run")
	st, _ := ex.Status("daemon")
	require.True(t, st.Running, "expected running+healthy (%v)", st)
	require.True(t, st.Healthy, "expected running+healthy (%v)", st)

	require.NoError(t, ex.Stop("daemon"), "stop")
	require.FileExists(t, stopMarker, "expected the stop command to have run")
	st, _ = ex.Status("daemon")
	require.False(t, st.Running, "expected stopped (%v)", st)
}

// TestCmdAction exercises a CLI action with a param placeholder; it must
// run without the engine being "running" and return the command stdout.
func TestCmdAction(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["echo"] = Action{Cmd: []string{fakeEngineBin, "echo", "{model}"}}
	ex := newTestExecutor(t, m)

	res, err := ex.Action(context.Background(), "fake", "echo", json.RawMessage(`{"model":"llama3"}`))
	require.NoError(t, err, "cmd action")
	require.Contains(t, string(res), "llama3", "unexpected cmd-action result (%v)", res)
}

// TestStartAdoptsAlreadyRunning verifies Start adopts an instance that
// already serves the target port instead of spawning a duplicate. The
// runtime bin is bogus on purpose: without adoption, Start would try to
// spawn it and fail.
func TestStartAdoptsAlreadyRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine: "adopt", DisplayName: "Adopt", ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Detect: []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
				Runtime: Runtime{
					Bin:   filepath.Join(t.TempDir(), "does-not-exist"+exeExt()),
					Port:  port,
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 5},
				},
			},
		},
	}
	ex := newTestExecutor(t, m)
	require.NoError(t, ex.Start(context.Background(), "adopt"), "expected adoption of the already-serving instance")
	st, _ := ex.Status("adopt")
	require.True(t, st.Installed, "expected running via adoption (%v)", st)
	require.True(t, st.Running, "expected running via adoption (%v)", st)
	require.True(t, st.Healthy, "expected running via adoption (%v)", st)
	state, _ := ex.state("adopt")
	state.mu.Lock()
	binPath := state.binPath
	state.mu.Unlock()
	require.Equal(t, "", binPath, "service-only adoption retained killable binPath")
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, data, 0o755))
}

func rmCmd(path string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "del", "/q", path}
	}
	return []string{"rm", "-f", path}
}
