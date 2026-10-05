// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

// captured records the executor's emitted notifications so tests can
// assert on them without a live parent. The Reporter's own ring (queried
// via Executor.Errors) captures reported errors even with a nil codec.
type captured struct {
	mu    sync.Mutex
	emits []string
}

func (c *captured) emit(method string, _ any) {
	c.mu.Lock()
	c.emits = append(c.emits, method)
	c.mu.Unlock()
}

func (c *captured) has(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.emits {
		if m == method {
			return true
		}
	}
	return false
}

func capturingExecutor(t *testing.T, m *Manifest) (*Executor, *captured) {
	t.Helper()
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	c := &captured{}
	ex := NewExecutor(reg, NewReporter(nil), c.emit, t.TempDir())
	ex.detectTimeout = 2 * time.Second // keep failure paths fast in tests
	return ex, c
}

// TestDownloadUnpinned verifies a fetch with no sha256 succeeds (over
// loopback http) — the unpinned path the bundled Ollama manifest now uses.
func TestDownloadUnpinned(t *testing.T) {
	payload := []byte("unpinned engine bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	p, err := ex.download(context.Background(), "fake", &Fetch{URL: srv.URL}) // no SHA256
	require.NoError(t, err, "unpinned download should succeed")
	defer os.Remove(p)
	{
		got, _ := os.ReadFile(p)
		require.True(t, string(got) == string(payload), "downloaded content mismatch (%v)", got)
	}
}

// TestValidateDownloadURL pins the HTTPS-only policy (loopback http is the
// only plaintext exception — used by the live tests and a LAN mirror).
func TestValidateDownloadURL(t *testing.T) {
	for _, u := range []string{
		"https://ollama.com/x.zip", "https://example.com/y",
		"http://127.0.0.1:8080/x", "http://localhost/x", "http://[::1]:9/x",
	} {
		{
			err := validateDownloadURL(u)
			assert.NoError(t, err, " (%v, %v)", u, err)
		}
	}
	for _, u := range []string{
		"http://example.com/x", "http://10.0.0.5/x", "ftp://h/y", "file:///etc/passwd",
	} {
		assert.Error(t, validateDownloadURL(u), " (%v)", u)
	}
}

// TestUninstallRetries proves a failing uninstall command is retried
// uninstallRetries times (the command-mode daemon-race mitigation).
func TestUninstallRetries(t *testing.T) {
	oldR, oldB := uninstallRetries, uninstallBackoff
	uninstallRetries, uninstallBackoff = 3, 5*time.Millisecond
	defer func() { uninstallRetries, uninstallBackoff = oldR, oldB }()

	dir := t.TempDir()
	binCopy := filepath.Join(dir, "fake"+exeExt())
	copyFile(t, fakeEngineBin, binCopy)
	marker := filepath.Join(dir, "attempts")

	m := testEngineManifest(binCopy)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Mode = "command"                                           // command-mode installs may legitimately live outside installDir
	p.Detect = []string{binCopy}                                         // installed → uninstall runs the command
	p.Uninstall = &Uninstall{Run: []string{binCopy, "failmark", marker}} // always exits 1, appends one byte
	m.Platforms[key] = p

	ex := newTestExecutor(t, m)
	// A command-mode engine's files live wherever its vendor script put them, so
	// uninstall declines one PAIR has no record of installing. Claim it, to reach
	// the retry behavior under test.
	if err := writeInstallMarker(filepath.Join(ex.baseDir, "fake"), "fake"); err != nil {
		t.Fatal(err)
	}
	err := ex.Uninstall(context.Background(), "fake")
	require.Error(t, err, "expected uninstall failure after 3 attempts")
	require.Contains(t, err.Error(), "after 3 attempts", "expected uninstall failure after 3 attempts (%v)", err)
	{
		data, _ := os.ReadFile(marker)
		require.Len(t, data, 3, "expected 3 uninstall attempts, marker has")
	}
}

func hasErr(errs []serviceError, id string) bool {
	for _, e := range errs {
		if e.ID == id {
			return true
		}
	}
	return false
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	p, _ := strconv.Atoi(u.Port())
	return p
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.FailNow(t, "test expectation failed", "condition not met within timeout")
}

func hostKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

// TestActionReservedPlaceholderWins is the regression guard for the
// security fix: a caller-supplied param must NOT be able to override a
// reserved placeholder like {cli} and hijack argv[0].
func TestActionReservedPlaceholderWins(t *testing.T) {
	m := &Manifest{
		Engine: "fake", DisplayName: "Fake", ManifestVersion: 1,
		Platforms: map[string]Platform{hostKey(): {
			Detect:  []string{fakeEngineBin},
			Runtime: Runtime{CLI: fakeEngineBin, Bin: fakeEngineBin},
		}},
		Actions: map[string]Action{"echo": {Cmd: []string{"{cli}", "echo", "{model}"}}},
	}
	ex, _ := capturingExecutor(t, m)

	// Malicious params try to override {cli} (argv[0]). Reserved must win,
	// so the command runs fakeEngineBin (not the bogus path) and echoes
	// the model value.
	res, err := ex.Action(context.Background(), "fake", "echo",
		json.RawMessage(`{"cli":"definitely-not-a-real-binary","model":"safevalue"}`))
	require.NoError(t, err, "expected reserved {cli} to win and the command to run")
	require.Contains(t, string(res), "safevalue", "unexpected result (%v)", res)
}

// TestReadinessTimeoutNoSpuriousExit guards the fix that a readiness
// timeout reports start-failed but NOT a phantom "exited unexpectedly".
func TestReadinessTimeoutNoSpuriousExit(t *testing.T) {
	m := &Manifest{
		Engine: "slow", DisplayName: "Slow", ManifestVersion: 1,
		Platforms: map[string]Platform{hostKey(): {
			Detect: []string{fakeEngineBin},
			Runtime: Runtime{
				Bin:   fakeEngineBin,
				Args:  []string{"noserve"}, // runs but never binds
				Ready: &Probe{TCP: "127.0.0.1:{port}", TimeoutS: 1},
			},
		}},
	}
	ex, _ := capturingExecutor(t, m)
	err := ex.Start(context.Background(), "slow")
	require.Error(t, err, "expected readiness timeout")
	require.Contains(t, err.Error(), "did not become ready", "expected readiness timeout (%v)", err)
	require.True(t, hasErr(ex.Errors(), startFailedID("slow")), "expected a start-failed error")
	// Give the watcher a chance to (wrongly) fire before asserting silence.
	time.Sleep(400 * time.Millisecond)
	require.False(t, hasErr(ex.Errors(), exitedID("slow")), "must NOT report 'exited unexpectedly' for a readiness timeout")
	state, _ := ex.state("slow")
	state.mu.Lock()
	proc, running := state.proc, state.running
	state.mu.Unlock()
	require.Nil(t, proc, "timed-out engine was not cleaned up: proc (%v)", running)
	require.False(t, running, "timed-out engine was not cleaned up: proc")
	{
		st, err := ex.Status("slow")
		require.NoError(t, err, "status after timeout (%v, %v)", st, err)
		require.False(t, st.Running, "status after timeout (%v, %v)", st, err)
	}
}

// TestStartWaitsForDelayedReadinessWithinBudget is a reduced-time regression
// for engines whose initialization finishes after several failed probes. Start
// must keep the owned process alive until the configured finite budget expires.
func TestStartWaitsForDelayedReadinessWithinBudget(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[hostKey()]
	p.Runtime.Env["FAKE_START_DELAY"] = "750ms"
	p.Runtime.Ready.TimeoutS = 5
	m.Platforms[hostKey()] = p

	ex, _ := capturingExecutor(t, m)
	t.Cleanup(func() { _ = ex.Stop("fake") })
	startedAt := time.Now()
	require.NoError(t, ex.Start(context.Background(), "fake"), "delayed start within readiness budget failed")
	{
		elapsed := time.Since(startedAt)
		require.GreaterOrEqual(t, elapsed, 700*time.Millisecond, "fake engine did not exercise delayed readiness: elapsed")
	}
	{
		st, err := ex.Status("fake")
		require.NoError(t, err, "delayed start status (%v, %v)", st, err)
		require.True(t, st.Running, "delayed start status (%v, %v)", st, err)
		require.True(t, st.Healthy, "delayed start status (%v, %v)", st, err)
	}
	{
		enabled, known, err := ex.desired.get("fake")
		require.NoError(t, err, "desired state after delayed start (%v, %v, %v)", enabled, known, err)
		require.True(t, known, "desired state after delayed start (%v, %v, %v)", enabled, known, err)
		require.True(t, enabled, "desired state after delayed start (%v, %v, %v)", enabled, known, err)
	}
	require.False(t, hasErr(ex.Errors(), startFailedID("fake")), "delayed success retained a start-failed error")
}

// TestConcurrentStartIsSerialized guards the per-engine op lock: many
// concurrent Starts must not race or double-spawn. Run under -race.
func TestConcurrentStartIsSerialized(t *testing.T) {
	ex, _ := capturingExecutor(t, testEngineManifest(fakeEngineBin))
	t.Cleanup(func() { _ = ex.Stop("fake") })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = ex.Start(context.Background(), "fake") }()
	}
	wg.Wait()
	{
		st, _ := ex.Status("fake")
		require.True(t, st.Running, "expected running after concurrent starts (%v)", st)
	}
}

func TestInstallChecksumMismatchReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	m := &Manifest{
		Engine: "fake", DisplayName: "Fake", ManifestVersion: 1,
		Platforms: map[string]Platform{hostKey(): {
			Detect:  []string{filepath.Join(t.TempDir(), "never"+exeExt())}, // stays not-installed
			Install: &Install{Fetch: &Fetch{URL: srv.URL, SHA256: "deadbeef"}, Run: []string{fakeEngineBin, "echo", "x"}, Mode: "user"},
			Runtime: Runtime{Bin: fakeEngineBin},
		}},
	}
	ex, c := capturingExecutor(t, m)
	err := ex.Install(context.Background(), "fake")
	require.Error(t, err, "expected checksum mismatch")
	require.Contains(t, err.Error(), "checksum mismatch", "expected checksum mismatch (%v)", err)
	require.True(t, hasErr(ex.Errors(), installFailedID("fake")), "expected install-failed to be reported")
	require.True(t, c.has("engine:install-progress"), "expected an install-progress notification")
}

func TestHealthFlipReportsAndClears(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)

	m := &Manifest{
		Engine: "d", DisplayName: "Daemon", ManifestVersion: 1,
		Platforms: map[string]Platform{hostKey(): {
			Detect: []string{fakeEngineBin},
			Runtime: Runtime{
				Mode:   "command",
				Port:   port,
				Start:  [][]string{{fakeEngineBin, "echo", "up"}},
				Stop:   &StopSpec{Cmd: []string{fakeEngineBin, "echo", "down"}},
				Ready:  &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 5},
				Health: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, IntervalS: 1},
			},
		}},
	}
	ex, _ := capturingExecutor(t, m)
	t.Cleanup(func() { _ = ex.Stop("d") })
	require.NoError(t, ex.Start(context.Background(), "d"), "start")

	healthy.Store(false)
	waitFor(t, 6*time.Second, func() bool { return hasErr(ex.Errors(), unhealthyID("d")) })
	healthy.Store(true)
	waitFor(t, 6*time.Second, func() bool { return !hasErr(ex.Errors(), unhealthyID("d")) })
}

func TestUnexpectedExitReported(t *testing.T) {
	ex, _ := capturingExecutor(t, testEngineManifest(fakeEngineBin))
	require.NoError(t, ex.Start(context.Background(), "fake"), "start")
	st, _ := ex.Status("fake")
	// The /exit route makes the fake engine os.Exit(1) — a crash.
	_, _ = http.Get(fmt.Sprintf("http://127.0.0.1:%d/exit", st.Port))
	waitFor(t, 6*time.Second, func() bool { s, _ := ex.Status("fake"); return !s.Running })
	require.True(t, hasErr(ex.Errors(), exitedID("fake")), "expected an 'exited' error after a crash")
}

func TestNormalStopIsSilent(t *testing.T) {
	ex, _ := capturingExecutor(t, testEngineManifest(fakeEngineBin))
	require.NoError(t, ex.Start(context.Background(), "fake"), "start")
	require.NoError(t, ex.Stop("fake"), "stop")
	time.Sleep(300 * time.Millisecond) // let any watcher fire
	require.False(t, hasErr(ex.Errors(), exitedID("fake")), "a normal stop must not report 'exited unexpectedly'")
}

func TestRestartProcess(t *testing.T) {
	ex, _ := capturingExecutor(t, testEngineManifest(fakeEngineBin))
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(context.Background(), "fake"), "start")
	require.NoError(t, ex.Restart(context.Background(), "fake"), "restart")
	{
		st, _ := ex.Status("fake")
		require.True(t, st.Running, "expected running+healthy after restart (%v)", st)
		require.True(t, st.Healthy, "expected running+healthy after restart (%v)", st)
	}
}

func TestActionUnknownAndHTTPError(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["err"] = Action{HTTP: &ActionHTTP{Method: "GET", Path: "/api/error"}}
	ex, _ := capturingExecutor(t, m)
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(context.Background(), "fake"), "start")
	{
		_, err := ex.Action(context.Background(), "fake", "nope", nil)
		require.Error(t, err, "expected unknown-action error")
		require.Contains(t, err.Error(), "no action", "expected unknown-action error (%v)", err)
	}
	{
		_, err := ex.Action(context.Background(), "fake", "err", nil)
		require.Error(t, err, "expected HTTP 500 error")
		require.Contains(t, err.Error(), "HTTP 500", "expected HTTP 500 error (%v)", err)
	}
}

func TestExpandPathForms(t *testing.T) {
	t.Setenv("NVPAIR_TEST_VAR", "xyz")
	for _, tc := range []struct {
		name, goos, input, want string
	}{
		{"windows percent", "windows", "a/%NVPAIR_TEST_VAR%/b", "a/xyz/b"},
		{"windows dollar", "windows", "a/$NVPAIR_TEST_VAR/b", "a/$NVPAIR_TEST_VAR/b"},
		{"windows braced dollar", "windows", "a/${NVPAIR_TEST_VAR}/b", "a/${NVPAIR_TEST_VAR}/b"},
		{"unix dollar", "linux", "a/$NVPAIR_TEST_VAR/b", "a/xyz/b"},
		{"unix braced dollar", "linux", "a/${NVPAIR_TEST_VAR}/b", "a/xyz/b"},
		{"unix percent", "linux", "a/%NVPAIR_TEST_VAR%/b", "a/%NVPAIR_TEST_VAR%/b"},
		{"unix shell parameters", "linux", `tar -xzf "$1" -C "$3"`, `tar -xzf "$1" -C "$3"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			{
				got := expandPathForOS(tc.input, tc.goos)
				assert.True(t, got == tc.want, "expandPathForOS (%v)", got)
			}
		})
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, goos := range []string{"windows", "linux"} {
			{
				got, want := expandPathForOS("~/sub", goos), filepath.Join(home, "sub")
				assert.True(t, got == want, " (%v, %v, %v)", goos, got, want)
			}
		}
	}
}

func TestBundledWindowsLMStudioUninstallExpansion(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("lmstudio")
	require.True(t, ok, "lmstudio manifest not loaded")
	for _, arch := range []string{"amd64", "arm64"} {
		p, ok := m.PlatformFor("windows", arch)
		require.True(t, ok, "windows/ (%v)", arch)
		require.NotNil(t, p.Uninstall, "windows/ (%v)", arch)
		require.NotEqual(t, 0, len(p.Uninstall.Run), "windows/ (%v)", arch)
		command := p.Uninstall.Run[len(p.Uninstall.Run)-1]
		{
			got := expandPathForOS(command, "windows")
			assert.True(t, got == command, "windows/ (%v, %v, %v)", arch, got, command)
		}
		for _, variable := range []string{"$root", "$_", "$env:USERPROFILE"} {
			assert.Contains(t, command, variable, "windows/ (%v, %v)", arch, variable)
		}
	}
}

// TestBundledManifestsGolden validates the shipped manifests load and
// validate (a typo would otherwise only surface as a runtime fatal).
func TestBundledManifestsGolden(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"), "bundled manifests invalid")
	for _, want := range []string{"ollama", "lmstudio", "llamacpp"} {
		m, ok := reg.Get(want)
		require.True(t, ok, "missing bundled engine (%v)", want)
		{
			_, ok := m.Actions["list_models"]
			assert.True(t, ok, "bundled (%v)", want)
		}
	}
}

func TestDownloadSizeCap(t *testing.T) {
	old := maxDownloadBytes
	maxDownloadBytes = 64
	t.Cleanup(func() { maxDownloadBytes = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 256)) // larger than the cap
	}))
	defer srv.Close()

	ex, _ := capturingExecutor(t, testEngineManifest(fakeEngineBin))
	{
		_, err := ex.download(context.Background(), "x", &Fetch{URL: srv.URL, SHA256: "abc"})
		require.Error(t, err, "expected size-cap error")
		require.Contains(t, err.Error(), "exceeds", "expected size-cap error (%v)", err)
	}
}

// TestStopClearsUnhealthy guards that a deliberate stop clears a lingering
// unhealthy error (the same fix-family as clearing exited on start).
func TestStopClearsUnhealthy(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	port := portOf(t, srv.URL)
	marker := filepath.Join(t.TempDir(), "stop-attempted")

	m := &Manifest{
		Engine: "d", DisplayName: "Daemon", ManifestVersion: 1,
		Platforms: map[string]Platform{hostKey(): {
			Detect: []string{fakeEngineBin},
			Runtime: Runtime{
				Mode:   "command",
				Port:   port,
				Start:  [][]string{{fakeEngineBin, "echo", "up"}},
				Stop:   &StopSpec{Cmd: []string{fakeEngineBin, "touch", marker}, GraceS: 2},
				Ready:  &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 5},
				Health: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, IntervalS: 1},
			},
		}},
	}
	ex, _ := capturingExecutor(t, m)
	require.NoError(t, ex.Start(context.Background(), "d"), "start")
	healthy.Store(false)
	waitFor(t, 6*time.Second, func() bool { return hasErr(ex.Errors(), unhealthyID("d")) })
	closed := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fileExists(marker) {
				srv.Close()
				close(closed)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	require.NoError(t, ex.Stop("d"), "stop")
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test expectation failed", "stop command did not close the test endpoint")
	}
	require.False(t, hasErr(ex.Errors(), unhealthyID("d")), "stop must clear the lingering unhealthy error")
}
