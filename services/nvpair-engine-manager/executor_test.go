// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	return transport.ResponseHeaderTimeout
}

func TestEngineHTTPClientsBoundResponseHeaders(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	if got := responseHeaderTimeout(t, ex.client); got != engineResponseHeaderTimeout {
		t.Fatalf("ordinary response-header timeout = %s, want %s", got, engineResponseHeaderTimeout)
	}
	if got := responseHeaderTimeout(t, ex.ollamaLoadClient); got != ollamaLoadResponseHeaderTimeout {
		t.Fatalf("Ollama load response-header timeout = %s, want %s", got, ollamaLoadResponseHeaderTimeout)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	st.running = true

	if _, err := ex.Action(context.Background(), "ollama", "run_model", json.RawMessage(`{"model":"tiny"}`)); err != nil {
		t.Fatalf("Ollama load was cut off by the ordinary response-header budget: %v", err)
	}
	if _, err := ex.Action(context.Background(), "ollama", "delete_model", json.RawMessage(`{"name":"tiny"}`)); err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("ordinary action error = %v, want bounded response-header timeout", err)
	}

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
	if err != nil {
		t.Fatal(err)
	}
	otherState.running = true
	if _, err := otherEx.Action(context.Background(), "other", "run_model", json.RawMessage(`{"model":"tiny"}`)); err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("non-Ollama run_model error = %v, want ordinary response-header timeout", err)
	}
}

func TestEngineLifecycle(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })

	installed, err := ex.Detect("fake")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !installed {
		t.Fatalf("expected fake engine to be detected (bin=%s)", fakeEngineBin)
	}

	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	st, _ := ex.Status("fake")
	if !st.Running || !st.Healthy {
		t.Fatalf("expected running+healthy, got %+v", st)
	}
	if st.Port == 0 {
		t.Fatalf("expected a non-zero port, got %+v", st)
	}

	res, err := ex.Action(ctx, "fake", "list_models", nil)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	if !strings.Contains(string(res), "llama3.2") {
		t.Fatalf("unexpected action result: %s", res)
	}

	if err := ex.Stop("fake"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	st, _ = ex.Status("fake")
	if st.Running {
		t.Fatalf("expected stopped, got %+v", st)
	}
}

func TestCrashThenExternalServiceIsAdoptedWithoutRespawn(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	m.Platforms[key] = p
	ex := newTestExecutor(t, m)

	if err := ex.Start(context.Background(), m.Engine); err != nil {
		t.Fatalf("initial start: %v", err)
	}
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
	if err != nil {
		t.Fatalf("external listener: %v", err)
	}
	external := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = external.Serve(ln) }()
	defer func() {
		_ = external.Close()
		_, _ = ex.Status(m.Engine)
	}()

	if err := ex.Start(context.Background(), m.Engine); err != nil {
		t.Fatalf("restart should adopt the external service: %v", err)
	}
	state.mu.Lock()
	proc := state.proc
	adopted := state.adopted
	running := state.running
	healthy := state.healthy
	state.mu.Unlock()
	if proc != nil || !adopted || !running || !healthy {
		t.Fatalf("restart spawned over external service: proc=%v adopted=%v running=%v healthy=%v", proc != nil, adopted, running, healthy)
	}
}

func TestActionRequiresRunning(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	if _, err := ex.Action(context.Background(), "fake", "list_models", nil); err == nil {
		t.Fatal("expected error: action on a non-running engine")
	}
}

func TestStartUnknownEngine(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	if err := ex.Start(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown engine")
	}
}

func TestStartPortOverride(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin)) // manifest Port is 0
	t.Cleanup(func() { _ = ex.Stop("fake") })
	const want = 17777
	if err := ex.StartWith(context.Background(), "fake", startOpts{Port: want}); err != nil {
		t.Fatalf("start with port override: %v", err)
	}
	st, _ := ex.Status("fake")
	if st.Port != want {
		t.Fatalf("expected port %d, got %d", want, st.Port)
	}
}

func TestEffectiveBind(t *testing.T) {
	cases := []struct{ manifest, override, want string }{
		{"", "", "127.0.0.1"},                 // ordinary engine: safe default
		{"0.0.0.0", "", "0.0.0.0"},            // inference engine declares open
		{"0.0.0.0", "127.0.0.1", "127.0.0.1"}, // per-call lock-down wins
		{"", "192.168.1.5", "192.168.1.5"},    // per-call specific interface
	}
	for _, c := range cases {
		if got := effectiveBind(c.manifest, c.override); got != c.want {
			t.Errorf("effectiveBind(%q,%q)=%q want %q", c.manifest, c.override, got, c.want)
		}
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
	if !st.Installed || !st.Running || !st.Healthy {
		t.Fatalf("expected externally-running engine adopted as installed and healthy, got %+v", st)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || !st.Running || !st.Healthy || st.Port != legacyPort {
		t.Fatalf("legacy listener was not adopted at %d: %+v", legacyPort, st)
	}
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
	if len(statuses) != 1 || !statuses[0].Installed || !statuses[0].Running || !statuses[0].Healthy {
		t.Fatalf("expected get-installed to adopt the external engine, got %+v", statuses)
	}
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

	if err := ex.Install(context.Background(), m.Engine); err != nil {
		t.Fatalf("install should adopt the external service: %v", err)
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("external service triggered %d installer downloads, want 0", got)
	}
	st, err := ex.Status(m.Engine)
	if err != nil || !st.Installed || !st.Running || !st.Healthy {
		t.Fatalf("external service status = %+v, err=%v", st, err)
	}
	stateSeen := false
	progressSeen := false
	for _, method := range methods {
		stateSeen = stateSeen || method == "engine:state-changed"
		progressSeen = progressSeen || method == "engine:install-progress"
	}
	if !stateSeen || !progressSeen {
		t.Fatalf("install adoption notifications = %v, want progress and state", methods)
	}
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
	err := ex.Install(context.Background(), m.Engine)
	if err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("install over an unknown listener error = %v, want occupied", err)
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("unknown listener triggered %d installer downloads, want 0", got)
	}
	st, _ := ex.Status(m.Engine)
	if st.Installed || st.Running {
		t.Fatalf("unknown listener must not be adopted: %+v", st)
	}
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
	if st, err := ex.Status(m.Engine); err != nil || st.Installed || st.Running {
		t.Fatalf("live API without CLI status = %+v, err=%v; want not installed", st, err)
	}
	if err := ex.Install(context.Background(), m.Engine); err != nil {
		t.Fatalf("install missing command-mode CLI: %v", err)
	}
	if !fileExists(cli) {
		t.Fatal("live API incorrectly suppressed command-mode installer")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if backendPort == portOf(t, facade.URL) {
		t.Fatal("test requires distinct facade and backend ports")
	}

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
	if st, err := ex.Status(m.Engine); err != nil || st.Installed || st.Running {
		t.Fatalf("facade on a different port was adopted: status=%+v err=%v", st, err)
	}
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
	if !st.Running || !st.Healthy {
		t.Fatalf("initial external status = %+v", st)
	}

	status.Store(http.StatusServiceUnavailable)
	st, _ = ex.Status(m.Engine)
	if !st.Running || st.Healthy {
		t.Fatalf("HTTP 503 is live-but-unhealthy, got %+v", st)
	}

	srv.Close()
	st, _ = ex.Status(m.Engine)
	if st.Installed || st.Running || st.Healthy {
		t.Fatalf("closed external listener remained present: %+v", st)
	}
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

	if st, err := ex.Status("ollama"); err != nil || st.Installed || st.Running || st.Healthy {
		t.Fatalf("proxy facade was adopted: status=%+v err=%v", st, err)
	}
	if err := ex.Install(context.Background(), "ollama"); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("install over proxy facade error = %v", err)
	}
	if err := ex.Start(context.Background(), "ollama"); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("start over proxy facade error = %v", err)
	}
	state, _ := ex.state("ollama")
	state.mu.Lock()
	state.running = true // stale pre-transition state must not route an action through the facade
	state.mu.Unlock()
	if _, err := ex.Action(context.Background(), "ollama", "list_models", nil); err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("action through proxy facade error = %v", err)
	}
	if probes.Load() == 0 {
		t.Fatal("engine identity probe did not carry the proxy sentinel")
	}
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

	if st, err := ex.Status("ollama"); err != nil || !st.Running || !st.Healthy {
		t.Fatalf("initial adoption = %+v, err=%v", st, err)
	}
	isOllama.Store(false)
	if st, err := ex.Status("ollama"); err != nil || !st.Running || st.Healthy {
		t.Fatalf("foreign replacement status = %+v, err=%v", st, err)
	}
	if err := ex.Start(context.Background(), "ollama"); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("Start accepted foreign replacement: %v", err)
	}
	if err := ex.Install(context.Background(), "ollama"); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("Install accepted foreign replacement: %v", err)
	}
}

// TestUninstallTerminatesRunningInstance covers a uniquely-named managed
// engine started without an executor process handle. Uninstall must reconcile
// and reclaim that exact managed image before deleting it.
func TestUninstallTerminatesRunningInstance(t *testing.T) {
	baseDir := t.TempDir()
	bin := filepath.Join(baseDir, "fake", "fakeu"+strconv.Itoa(os.Getpid())+exeExt())
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, fakeEngineBin, bin)

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "OLLAMA_HOST=127.0.0.1:"+strconv.Itoa(port))
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
	if err := ex.Uninstall(context.Background(), "fake"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if ok, _ := ex.Detect("fake"); ok {
		t.Fatal("engine still detected after uninstall")
	}
	if portServing(port) {
		t.Fatal("expected uninstall to stop the running instance")
	}
}

func TestUninstallDoesNotKillExternalSameNameProcess(t *testing.T) {
	baseDir := t.TempDir()
	name := "fakeu" + strconv.Itoa(os.Getpid()) + exeExt()
	managedBin := filepath.Join(baseDir, "fake", name)
	externalBin := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Dir(managedBin), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, fakeEngineBin, managedBin)
	copyFile(t, fakeEngineBin, externalBin)

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(externalBin)
	cmd.Env = append(os.Environ(), "OLLAMA_HOST=127.0.0.1:"+strconv.Itoa(port))
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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

	err = ex.Uninstall(context.Background(), m.Engine)
	if err == nil || !strings.Contains(err.Error(), "external management") {
		t.Fatalf("uninstall external same-name owner error = %v", err)
	}
	if !portServing(port) {
		t.Fatal("uninstall killed the externally owned process")
	}
	if !fileExists(managedBin) {
		t.Fatal("uninstall removed managed files after refusing the external owner")
	}
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
	err := ex.Uninstall(context.Background(), m.Engine)
	if err == nil || !strings.Contains(err.Error(), "outside NVPAIR's managed install directory") {
		t.Fatalf("external uninstall error = %v", err)
	}
	if !fileExists(externalBin) {
		t.Fatal("external executable was removed")
	}
}

func TestUninstallRefusesUnidentifiedLiveListener(t *testing.T) {
	baseDir := t.TempDir()
	managedBin := filepath.Join(baseDir, "fake", "fake"+exeExt())
	if err := os.MkdirAll(filepath.Dir(managedBin), 0o755); err != nil {
		t.Fatal(err)
	}
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
	err := ex.Uninstall(context.Background(), m.Engine)
	if err == nil || !strings.Contains(err.Error(), "occupied by an unidentified service") {
		t.Fatalf("uninstall unidentified listener error = %v", err)
	}
	if !fileExists(managedBin) {
		t.Fatal("uninstall removed managed files while the target port was occupied")
	}
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
	t.Fatalf("port %d never started serving", port)
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
	if err != nil {
		t.Fatalf("download (ok): %v", err)
	}
	defer os.Remove(p)
	if filepath.Ext(p) != ".ps1" {
		t.Fatalf("downloaded path %q lost its .ps1 suffix", p)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(payload) {
		t.Fatalf("downloaded content mismatch")
	}

	if _, err := ex.download(ctx, "fake", &Fetch{URL: srv.URL + "/installer.ps1", SHA256: "deadbeef"}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	if _, err := ex.download(ctx, "fake", &Fetch{URL: notFound.URL, SHA256: good}); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404, got %v", err)
	}
}

func TestInstallRefusesAdmin(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Detect = nil // ensure not-detected so install reaches the mode check
	p.Install = &Install{Fetch: &Fetch{URL: "https://example/x", SHA256: "abc"}, Run: []string{"echo"}, Mode: "admin"}
	m.Platforms[key] = p

	ex := newTestExecutor(t, m)
	err := ex.Install(context.Background(), "fake")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected admin-install refusal, got %v", err)
	}
}

func TestEngineUninstall(t *testing.T) {
	// Copy the fake engine so uninstall can delete the copy without
	// disturbing the shared build artifact other tests rely on.
	baseDir := t.TempDir()
	binCopy := filepath.Join(baseDir, "fake", "fake"+exeExt())
	if err := os.MkdirAll(filepath.Dir(binCopy), 0o755); err != nil {
		t.Fatal(err)
	}
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
	if ok, _ := ex.Detect("fake"); !ok {
		t.Fatal("expected installed before uninstall")
	}
	if err := ex.Uninstall(context.Background(), "fake"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if ok, _ := ex.Detect("fake"); ok {
		t.Fatal("expected not-installed after uninstall")
	}
}

func TestUninstallNoOpWhenAbsent(t *testing.T) {
	m := testEngineManifest(filepath.Join(t.TempDir(), "absent"+exeExt()))
	ex := newTestExecutor(t, m)
	if err := ex.Uninstall(context.Background(), "fake"); err != nil {
		t.Fatalf("uninstall of an absent engine should be a no-op, got %v", err)
	}
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

	if err := ex.Start(context.Background(), "daemon"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !fileExists(startMarker) {
		t.Fatal("expected the start command to have run")
	}
	st, _ := ex.Status("daemon")
	if !st.Running || !st.Healthy {
		t.Fatalf("expected running+healthy, got %+v", st)
	}

	if err := ex.Stop("daemon"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !fileExists(stopMarker) {
		t.Fatal("expected the stop command to have run")
	}
	st, _ = ex.Status("daemon")
	if st.Running {
		t.Fatalf("expected stopped, got %+v", st)
	}
}

// TestCmdAction exercises a CLI action with a param placeholder; it must
// run without the engine being "running" and return the command stdout.
func TestCmdAction(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["echo"] = Action{Cmd: []string{fakeEngineBin, "echo", "{model}"}}
	ex := newTestExecutor(t, m)

	res, err := ex.Action(context.Background(), "fake", "echo", json.RawMessage(`{"model":"llama3"}`))
	if err != nil {
		t.Fatalf("cmd action: %v", err)
	}
	if !strings.Contains(string(res), "llama3") {
		t.Fatalf("unexpected cmd-action result: %s", res)
	}
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
	if err := ex.Start(context.Background(), "adopt"); err != nil {
		t.Fatalf("expected adoption of the already-serving instance, got: %v", err)
	}
	st, _ := ex.Status("adopt")
	if !st.Installed || !st.Running || !st.Healthy {
		t.Fatalf("expected running via adoption, got %+v", st)
	}
	state, _ := ex.state("adopt")
	state.mu.Lock()
	binPath := state.binPath
	state.mu.Unlock()
	if binPath != "" {
		t.Fatalf("service-only adoption retained killable binPath %q", binPath)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func rmCmd(path string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "del", "/q", path}
	}
	return []string{"rm", "-f", path}
}
