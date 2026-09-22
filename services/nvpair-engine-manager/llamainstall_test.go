// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLlamaManagedReplacementRetainsModels(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	write("runtime/llama", "old")
	write("models/model.gguf", "model")
	write("settings/config.json", `{"port":18082,"preference":"retained"}`)
	write("stage/llama", "new")
	if err := promoteLlamaRuntime(root, filepath.Join(root, "missing")); err == nil {
		t.Fatal("promoted missing candidate")
	}
	if read("runtime/llama") != "old" {
		t.Fatal("failed promotion lost prior runtime")
	}
	if err := promoteLlamaRuntime(root, filepath.Join(root, "stage")); err != nil {
		t.Fatal(err)
	}
	if read("runtime/llama") != "new" || read("previous/llama") != "old" {
		t.Fatal("replacement slots disagree")
	}
	if err := removeLlamaRuntime(&engineState{installDir: root}); err != nil {
		t.Fatal(err)
	}
	if read("models/model.gguf") != "model" {
		t.Fatal("uninstall touched models")
	}
	if read("settings/config.json") != `{"port":18082,"preference":"retained"}` {
		t.Fatal("runtime-only uninstall touched separate settings")
	}
	for _, slot := range []string{"runtime", "previous"} {
		if _, err := os.Stat(filepath.Join(root, slot)); !os.IsNotExist(err) {
			t.Fatalf("%s slot survived uninstall", slot)
		}
	}
}

func TestLlamaInstallerUsesOnlyProcessScopedWindowsPolicy(t *testing.T) {
	script := `C:\isolated\verified installer.ps1`
	want := []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}
	if got := llamaInstallerArgs("windows", script); !slices.Equal(got, want) {
		t.Fatalf("Windows installer argv = %q, want only process-scoped policy", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := llamaInstallerArgs(goos, "/isolated/pinned.sh"); !slices.Equal(got, []string{"sh", "/isolated/pinned.sh"}) {
			t.Fatalf("%s installer argv changed: %q", goos, got)
		}
	}
}

func TestLlamaStartupRecoversInterruptedReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "llamacpp")
	previous := filepath.Join(root, "previous")
	if err := os.MkdirAll(previous, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, llamaExecutable()), []byte("retained runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(root, "models", "retained.gguf")
	if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("GGUFretained model"), 0600); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, base)
	if installed, err := e.Detect("llamacpp"); err != nil || !installed {
		t.Fatalf("restarted manager did not recover installation: %v %v", installed, err)
	}
	if got, err := os.ReadFile(model); err != nil || string(got) != "GGUFretained model" {
		t.Fatalf("recovery changed model: %v", err)
	}
	// A retained older slot must never replace an already present runtime.
	if err := os.MkdirAll(previous, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, llamaExecutable()), []byte("older runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	e = NewExecutor(buildRegistry(""), NewReporter(nil), nil, base)
	if _, err := e.Detect("llamacpp"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "runtime", llamaExecutable())); err != nil || string(got) != "retained runtime" {
		t.Fatalf("recovery replaced current runtime: %v", err)
	}
}

func TestLlamaHeadlessMutationDetectsInstalledRuntime(t *testing.T) {
	base := t.TempDir()
	runtimeDir := filepath.Join(base, "llamacpp", "runtime")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, llamaExecutable()), []byte("owned runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "local-Q4_K_M.gguf")
	if err := os.WriteFile(source, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, base)
	params, _ := json.Marshal(map[string]string{"path": source})
	if _, err := e.Action(context.Background(), "llamacpp", "import_model", params); err != nil {
		t.Fatalf("first headless action requires an unrelated status call: %v", err)
	}
}

func TestLlamaRecoveryErrorIsNotUnsupportedPlatform(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "llamacpp")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	previous, outside := filepath.Join(root, "previous"), t.TempDir()
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", previous, outside)
		configureSysProcAttr(cmd)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create harmless junction fixture: %v: %s", err, output)
		}
	} else if err := os.Symlink(outside, previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(previous) })
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, base)
	if _, err := e.Status("llamacpp"); err == nil || !strings.Contains(err.Error(), "recover interrupted") {
		t.Fatalf("status hid recovery failure as platform support: %v", err)
	}
	for _, status := range e.GetInstalled() {
		if status.Engine == "llamacpp" && !strings.Contains(status.InstallReason, "recover interrupted") {
			t.Fatalf("inventory hid recovery failure: %+v", status)
		}
	}
}

func TestLlamaManifestAndInstallerIsolation(t *testing.T) {
	reg := buildRegistry("")
	mf, ok := reg.Get("llamacpp")
	if !ok {
		t.Fatal("manifest missing")
	}
	// These are SHA256 of the raw Git blobs at the URL's commit, not checkout
	// files that Git may have converted to CRLF on Windows.
	for platform, p := range mf.Platforms {
		if platform == "darwin/amd64" {
			if p.Install.Fetch != nil || p.Install.ArchiveRoot != "llama-b10826" || len(p.Install.Archives) != 1 || p.Install.Archives[0].SHA256 != "adcd2066b2a1a3d8e774e36c8f97d166defccd01d947d9e39667b0435e8361b0" {
				t.Fatal("Intel Mac must use the pinned official CPU archive")
			}
			continue
		}
		if platform == "windows/arm64" {
			if p.Install.Fetch != nil || len(p.Install.Archives) != 2 {
				t.Fatal("Windows ARM64 must use the official app and CUDA runtime archives")
			}
			for _, archive := range p.Install.Archives {
				if len(archive.SHA256) != 64 || !strings.HasPrefix(archive.URL, "https://github.com/ggml-org/llama.cpp/releases/download/b10826/") {
					t.Fatal("Windows ARM64 archive lacks pinned official provenance")
				}
			}
			continue
		}
		want := "cccdfcbd1b55bf6003ac3037588c9f5b3b79aa0a75fe991e97bb218ccdb55e4d"
		if strings.HasPrefix(platform, "windows/") {
			want = "455084203db0c864f4eb218bc82792b4304458a96211c385275d8337a5049851"
		}
		if p.Install.Fetch.SHA256 != want || !strings.Contains(p.Install.Fetch.URL, "27a82f3a6e0f259f88c2c31cd6b20d858a975f27") {
			t.Fatalf("%s: installer provenance mismatch", platform)
		}
	}
	if supported, reason := llamaInstallSupport("darwin", "amd64"); !supported || !strings.Contains(reason, "CPU") {
		t.Fatal("Intel Mac CPU support must be explicit")
	}
	st := &engineState{installDir: t.TempDir(), plat: &Platform{Runtime: Runtime{Env: map[string]string{"LLAMA_CACHE": "{install_dir}/models"}}}}
	stage := filepath.Join(st.installDir, "stage")
	env, err := llamaInstallerEnv(st, stage)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA"} {
		if env[key] != stage {
			t.Fatalf("%s escapes stage", key)
		}
	}
	if env["SKIP_INSTALL"] != "1" || filepath.Clean(plainWindowsPath(env["LLAMA_CACHE"])) != filepath.Join(st.installDir, "models") {
		t.Fatal("installer/cache isolation missing")
	}
	for _, version := range []string{"version: 0.0.0 (build 10826, commit abc123)\nbuilt with clang", "b10826-abc123"} {
		if !llamaPinnedVersion.MatchString(version) {
			t.Fatalf("reject actual vendor format %q", version)
		}
	}
	if llamaPinnedVersion.MatchString("version: 0 (build 108260, commit abc)") {
		t.Fatal("accepted different build")
	}
}

func TestLlamaExtendedCachePathAndSafeDiagnostic(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("long-directory-", 12), strings.Repeat("nested-", 12))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "model-Q4_K_M.gguf")
	if err := os.WriteFile(file, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cache, err := llamaCachePath(root)
	if err != nil {
		t.Fatal(err)
	}
	returned, err := llamaCachePath(file)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := llamaOwnedFile(cache, returned)
	ownedInfo, ownedErr := os.Stat(owned)
	fileInfo, fileErr := os.Stat(file)
	if err != nil || ownedErr != nil || fileErr != nil || !os.SameFile(ownedInfo, fileInfo) {
		t.Fatalf("extended vendor path rejected: %q %v", owned, err)
	}
	server, err := resolveChildEnv(map[string]string{"LLAMA_CACHE": "{install_dir}"}, map[string]string{"install_dir": root})
	if err != nil {
		t.Fatal(err)
	}
	if server["LLAMA_CACHE"] != cache {
		t.Fatal("server and downloader caches differ")
	}
	secret := "hf_test_secret_value"
	diagnostic := llamaDownloadError(&exec.ExitError{Stderr: []byte("error opening C:/private/path.downloadInProgress Authorization: Bearer " + secret)})
	if !strings.Contains(diagnostic.Error(), "path length") || strings.Contains(diagnostic.Error(), secret) || strings.Contains(diagnostic.Error(), "private") {
		t.Fatalf("unsafe or unactionable diagnostic: %s", diagnostic)
	}
}

type llamaFixtureTransport func(*http.Request) (*http.Response, error)

func (f llamaFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLlamaIdentityAndNestedState(t *testing.T) {
	body := `{"data":[{"id":"cold","status":{"value":"unloaded"}},{"id":"hot","status":{"value":"loaded"}},{"id":"uncertain","status":null}]}`
	spec := &ActionResult{Array: "data", Field: "id", Match: &ResultMatch{Field: "status.value", In: []string{"loaded"}}}
	got, ok := extractStringsResult(json.RawMessage(body), spec)
	if !ok || len(got) != 1 || got[0] != "hot" {
		t.Fatalf("loaded state %v %v", got, ok)
	}
	var row map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"value":"wrong","status":{}}`), &row)
	if _, ok := lookupField(row, "status.value"); ok {
		t.Fatal("nested lookup reused top-level value")
	}
	for _, server := range []string{"llama.cpp", "unrelated"} {
		e := &Executor{client: &http.Client{Transport: llamaFixtureTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Server": []string{server}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		if e.probe(context.Background(), &Probe{HTTP: "http://127.0.0.1:{port}/models", Identity: "llamacpp"}, 8082) != (server == "llama.cpp") {
			t.Fatal("identity decision wrong")
		}
	}
}

func TestLlamaShutdownCancelsMutationBeforeWaiting(t *testing.T) {
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, t.TempDir())
	st, err := e.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.opMu.Lock()
	st.mu.Lock()
	st.pullCancel = cancel
	st.mu.Unlock()
	go func() { <-ctx.Done(); st.opMu.Unlock() }()
	done := make(chan struct{})
	go func() { e.StopAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("shutdown waited on a pull it did not cancel")
	}
	_, err = e.actionLlama(context.Background(), st, "import_model", Action{Builtin: "llama-models"}, json.RawMessage(`{"path":"unused"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-shutdown mutation admitted: %v", err)
	}
}

func TestLlamaAdoptedListenerRejectsMutationBeforeHTTP(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "llama")
	if err := os.WriteFile(bin, []byte("owned executable placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	st := &engineState{installDir: root, binPath: bin, running: true, adopted: true, plat: &Platform{Runtime: Runtime{Ready: &Probe{HTTP: "http://127.0.0.1:{port}/models", Identity: "llamacpp"}}}}
	called := false
	e := &Executor{client: &http.Client{Transport: llamaFixtureTransport(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("unexpected HTTP") })}}
	for _, action := range []string{"load_model", "unload_model", "pull_model", "delete_model"} {
		_, err := e.actionLlama(context.Background(), st, action, Action{}, json.RawMessage(`{"model":"owner/model:Q4_K_M"}`))
		if err == nil || !strings.Contains(err.Error(), "externally") {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if called {
		t.Fatal("sent mutation or identity probe to external listener")
	}
}

func TestLlamaLostResidencyPublishesUnknown(t *testing.T) {
	e := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	changed, next, result := e.sweepLoaded(context.Background(), map[string][]string{"llamacpp": {"owner/model:Q4_K_M"}, "other": {"unchanged"}})
	if len(changed) != 1 || changed[0] != "llamacpp" {
		t.Fatalf("missing observation was not published: %v", changed)
	}
	if _, ok := next["llamacpp"]; ok {
		t.Fatal("retained stale llama residency")
	}
	if _, ok := result.LoadedByEngine["llamacpp"]; ok {
		t.Fatal("unknown observation represented as an empty successful set")
	}
	if len(next["other"]) != 1 {
		t.Fatal("changed unrelated engine policy")
	}
}

func TestLlamaLoadWaitsForObservedState(t *testing.T) {
	polls := 0
	e := &Executor{actionTimeout: time.Second, client: &http.Client{Transport: llamaFixtureTransport(func(*http.Request) (*http.Response, error) {
		polls++
		state := "loading"
		if polls >= 2 {
			state = "loaded"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"tiny","status":{"value":"` + state + `"}}]}`))}, nil
	})}}
	st := &engineState{running: true, port: 8082, manifest: &Manifest{Actions: map[string]Action{"list_models": {HTTP: &ActionHTTP{Method: "GET", Path: "/models"}}}}}
	if err := e.waitLlamaModelState(context.Background(), st, "tiny", "loaded"); err != nil {
		t.Fatal(err)
	}
	if polls != 2 {
		t.Fatalf("returned before observed loaded state: %d polls", polls)
	}
	e.client.Transport = llamaFixtureTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"tiny","status":{"value":"unloaded","failed":true,"exit_code":7}}]}`))}, nil
	})
	if err := e.waitLlamaModelState(context.Background(), st, "tiny", "loaded"); err == nil || !strings.Contains(err.Error(), "exit code 7") {
		t.Fatalf("failed load was not surfaced: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.waitLlamaModelState(ctx, st, "tiny", "loaded"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestLlamaBusyStatusAndStopRemainResponsive(t *testing.T) {
	reg := NewRegistry()
	mf, _ := buildRegistry("").Get("llamacpp")
	reg.engines["llamacpp"] = mf
	e := NewExecutor(reg, NewReporter(nil), nil, t.TempDir())
	st, err := e.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.opMu.Lock()
	st.mu.Lock()
	st.mutationCancel = cancel
	st.mu.Unlock()
	statusDone := make(chan error, 1)
	go func() {
		_, err := e.Status("llamacpp")
		if err == nil && len(e.GetInstalled()) != 1 {
			err = errors.New("missing busy engine")
		}
		statusDone <- err
	}()
	select {
	case err := <-statusDone:
		if err != nil {
			st.opMu.Unlock()
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		st.opMu.Unlock()
		t.Fatal("status waited behind model operation")
	}
	go func() { <-ctx.Done(); st.opMu.Unlock() }()
	stopDone := make(chan error, 1)
	go func() { stopDone <- e.Stop("llamacpp") }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("Stop did not cancel the active model operation")
	}
	st.mu.Lock()
	pending := st.stopPending
	st.mu.Unlock()
	if pending != 0 {
		t.Fatal("Stop left mutation admission blocked")
	}
}

func TestLlamaInstallRegistersCancellationAndAdmission(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown-%t", shutdown), func(t *testing.T) {
			reg := NewRegistry()
			mf, _ := buildRegistry("").Get("llamacpp")
			reg.engines["llamacpp"] = mf
			e := NewExecutor(reg, NewReporter(nil), nil, t.TempDir())
			st, err := e.state("llamacpp")
			if err != nil {
				t.Fatal(err)
			}
			st.plat.Runtime.Ready = nil // No listener/probe in this cancellation fixture.
			started := make(chan struct{})
			e.client = &http.Client{Transport: llamaFixtureTransport(func(r *http.Request) (*http.Response, error) {
				close(started)
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			installed := make(chan error, 1)
			go func() { installed <- e.Install(context.Background(), "llamacpp") }()
			select {
			case <-started:
			case err := <-installed:
				t.Fatalf("Install did not reach acquisition: %v", err)
			case <-time.After(time.Second):
				t.Fatal("Install acquisition did not start")
			}
			stopped := make(chan error, 1)
			go func() {
				if shutdown {
					e.StopAll()
					stopped <- nil
				} else {
					stopped <- e.Stop("llamacpp")
				}
			}()
			select {
			case err := <-installed:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("install result: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("actual Install was not cancelled")
			}
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Stop blocked behind Install")
			}
			st.mu.Lock()
			registered := st.mutationCancel != nil
			st.mu.Unlock()
			if registered {
				t.Fatal("install cancellation remained registered")
			}
		})
	}
	t.Run("reject-before-effects", func(t *testing.T) {
		e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, t.TempDir())
		e.StopAll()
		if err := e.Install(context.Background(), "llamacpp"); !errors.Is(err, context.Canceled) {
			t.Fatalf("post-shutdown install: %v", err)
		}
		st, _ := e.state("llamacpp")
		if _, err := os.Stat(st.installDir); !os.IsNotExist(err) {
			t.Fatal("post-shutdown install created staging state")
		}
	})
}

func TestLlamaUnsupportedStatusExplainsWhy(t *testing.T) {
	status := unavailableEngineStatus("llamacpp", "llama.cpp")
	if status.InstallSupported || status.InstallReason == "" {
		t.Fatalf("unsupported engine lacks a reason: %+v", status)
	}
}

// Install is idempotent: with a managed runtime already detected it reports
// already-installed and touches nothing, so a repeat cannot become a hidden
// replacement of the runtime, its receipt, or its models.
func TestLlamaInstallOnExistingRuntimeIsIdempotent(t *testing.T) {
	reg := NewRegistry()
	mf, _ := buildRegistry("").Get("llamacpp")
	reg.engines["llamacpp"] = mf
	stage := ""
	e := NewExecutor(reg, NewReporter(nil), func(method string, value any) {
		if method == "engine:install-progress" {
			stage = value.(map[string]any)["stage"].(string)
		}
	}, t.TempDir())
	st, err := e.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.installDir, "runtime", llamaExecutable())
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("owned runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(filepath.Dir(path), "pair-install.json")
	if err := os.WriteFile(receipt, []byte(`{"version":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(st.installDir, "models", "retained.gguf")
	if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	e.client = &http.Client{Transport: llamaFixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Error("repeat install fetched an artifact")
		return nil, errors.New("unexpected acquisition")
	})}
	for range 2 {
		if err := e.Install(context.Background(), "llamacpp"); err != nil {
			t.Fatal(err)
		}
		if stage != "already-installed" {
			t.Fatalf("repeat install stage = %q", stage)
		}
	}
	for name, want := range map[string]string{path: "owned runtime", receipt: `{"version":"fixture"}`, model: "model"} {
		if got, err := os.ReadFile(name); err != nil || string(got) != want {
			t.Fatalf("repeat install changed %s: %q %v", filepath.Base(name), got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(st.installDir, "previous")); !os.IsNotExist(err) {
		t.Fatal("repeat install replaced the runtime")
	}
	if matches, _ := filepath.Glob(filepath.Join(st.installDir, ".llama-install-*")); len(matches) != 0 {
		t.Fatalf("repeat install staged an acquisition: %v", matches)
	}
}

// llama has no update action. The manifest does not declare one, the registry
// rejects the builtin, and the executor refuses the request outright rather
// than substituting an uninstall-and-reinstall; callers that offer a generic
// update must refuse this engine before reaching the manager.
func TestLlamaUpdateIsRefusedNotSubstituted(t *testing.T) {
	mf, ok := buildRegistry("").Get("llamacpp")
	if !ok {
		t.Fatal("manifest missing")
	}
	if _, declared := mf.Actions["update"]; declared {
		t.Fatal("llama manifest still declares an update action")
	}
	for name, act := range mf.Actions {
		if act.Builtin == "llama-update" {
			t.Fatalf("action %q still uses the removed llama-update builtin", name)
		}
	}
	if err := (&Action{Builtin: "llama-update"}).validate("update"); err == nil || !strings.Contains(err.Error(), "unknown builtin") {
		t.Fatalf("llama-update builtin accepted: %v", err)
	}
	e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, t.TempDir())
	e.client = &http.Client{Transport: llamaFixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Error("refused update touched the network")
		return nil, errors.New("unexpected request")
	})}
	if _, err := e.Action(context.Background(), "llamacpp", "update", nil); err == nil || !strings.Contains(err.Error(), `no action "update"`) {
		t.Fatalf("update was not refused explicitly: %v", err)
	}
	st, err := e.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.installDir); !os.IsNotExist(err) {
		t.Fatal("refused update created install state")
	}
}

// The receipt's recipe_sha256 must identify the exact pinned archive set, so a
// runtime built from a changed companion bundle is never mistaken for one built
// from the recipe currently shipped.
func TestLlamaArchiveRecipeHashTracksEveryPin(t *testing.T) {
	archives := []Fetch{{URL: "https://example.invalid/app.zip", SHA256: "app-pin"}, {URL: "https://example.invalid/runtime.zip", SHA256: "runtime-pin"}}
	base := llamaArchiveRecipeHash(archives)
	if base != llamaArchiveRecipeHash(slices.Clone(archives)) {
		t.Fatal("identical archive recipe produced different provenance")
	}
	companion := slices.Clone(archives)
	companion[1].SHA256 = "new-runtime-pin"
	if llamaArchiveRecipeHash(companion) == base {
		t.Fatal("changed companion runtime pin was not recorded in provenance")
	}
	if llamaArchiveRecipeHash(archives[:1]) == base {
		t.Fatal("dropped companion archive was not recorded in provenance")
	}
}
