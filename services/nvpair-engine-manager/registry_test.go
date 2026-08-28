// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// validManifest is a minimal well-formed manifest used as the base for
// validation tests; individual tests mutate a copy to introduce one
// fault at a time.
func validManifest() Manifest {
	return Manifest{
		Engine:          "ollama",
		DisplayName:     "Ollama",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			"linux/amd64": {
				Detect: []string{"$HOME/.local/bin/ollama"},
				Install: &Install{
					Fetch: &Fetch{URL: "https://example/ollama.tgz", SHA256: "abc123"},
					Run:   []string{"tar", "xzf", "{download}", "-C", "{install_dir}"},
					Mode:  "user",
				},
				Runtime: Runtime{
					Bin:    "{install_dir}/ollama",
					Args:   []string{"serve"},
					Env:    map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}"},
					Port:   11434,
					Ready:  &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 20},
					Stop:   &StopSpec{Signal: "term", GraceS: 5},
					Health: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, IntervalS: 5},
				},
			},
		},
		Actions: map[string]Action{
			"list_models": {Description: "list", HTTP: &ActionHTTP{Method: "GET", Path: "/api/tags"}},
		},
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	m := validManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestValidateAcceptsCommandModeAndCmdAction(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Runtime.Mode = "command"
	p.Runtime.Bin = "" // not required in command mode
	p.Runtime.Start = [][]string{{"lms", "daemon", "up"}, {"lms", "server", "start", "--port", "{port}"}}
	p.Runtime.Stop = &StopSpec{Cmd: []string{"lms", "server", "stop"}}
	m.Platforms["linux/amd64"] = p
	// A CLI action with a param placeholder ({model}) must validate —
	// action templates are resolved from params at call time, not here.
	m.Actions["pull"] = Action{Cmd: []string{"lms", "get", "{model}", "--yes"}}
	if err := m.Validate(); err != nil {
		t.Fatalf("command-mode/cmd-action manifest rejected: %v", err)
	}
}

func TestValidateAcceptsUnpinnedFetch(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Install.Fetch.SHA256 = "" // unpinned: allowed (download runs HTTPS-only with a loud warning)
	m.Platforms["linux/amd64"] = p
	if err := m.Validate(); err != nil {
		t.Fatalf("unpinned fetch should validate, got %v", err)
	}
}

func TestValidateRejectsBadEngineName(t *testing.T) {
	for _, bad := range []string{"../evil", "a/b", `a\b`, "..", ".", "a b", ""} {
		m := validManifest()
		m.Engine = bad
		if err := m.Validate(); err == nil {
			t.Errorf("expected rejection of engine name %q", bad)
		}
	}
}

func TestValidateRejectsScriptWithFetch(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Install.Script = []string{"sh", "-c", "curl x | sh"} // coexists with fetch+run
	m.Platforms["linux/amd64"] = p
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected script+fetch rejection, got %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string // substring expected in the error
	}{
		{"missing engine", func(m *Manifest) { m.Engine = "" }, "engine is required"},
		{"missing display_name", func(m *Manifest) { m.DisplayName = "" }, "display_name is required"},
		{"zero version", func(m *Manifest) { m.ManifestVersion = 0 }, "manifest_version must be"},
		{"future version", func(m *Manifest) { m.ManifestVersion = ManifestSchemaVersion + 1 }, "newer than supported"},
		{"no platforms", func(m *Manifest) { m.Platforms = nil }, "at least one platforms"},
		{"bad platform key", func(m *Manifest) {
			m.Platforms = map[string]Platform{"linuxamd64": m.Platforms["linux/amd64"]}
		}, "must be \"<goos>/<goarch>\""},
		{"missing bin", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Runtime.Bin = ""
			m.Platforms["linux/amd64"] = p
		}, "runtime.bin is required"},
		{"run without fetch", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Install.Fetch = nil
			m.Platforms["linux/amd64"] = p
		}, "requires a fetch"},
		{"bad install mode", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Install.Mode = "root"
			m.Platforms["linux/amd64"] = p
		}, "install.mode"},
		{"unknown placeholder", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Runtime.Args = []string{"serve", "{bogus}"}
			m.Platforms["linux/amd64"] = p
		}, "unknown placeholder {bogus}"},
		{"action without http or cmd", func(m *Manifest) {
			m.Actions = map[string]Action{"x": {Description: "neither"}}
		}, "exactly one of http, cmd, or remove_path"},
		{"action missing method", func(m *Manifest) {
			m.Actions = map[string]Action{"x": {HTTP: &ActionHTTP{Path: "/p"}}}
		}, "http.method and http.path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestPlatformFor(t *testing.T) {
	m := validManifest()
	m.Platforms["windows/amd64"] = m.Platforms["linux/amd64"]
	if _, ok := m.PlatformFor("linux", "amd64"); !ok {
		t.Fatal("expected linux/amd64 to resolve")
	}
	if _, ok := m.PlatformFor("darwin", "arm64"); ok {
		t.Fatal("did not expect darwin/arm64 to resolve")
	}
}

func TestResolvePlaceholders(t *testing.T) {
	vars := map[string]string{"port": "11434", "install_dir": "/opt/x", "bin": "/opt/x/ollama"}
	got, err := resolvePlaceholders("http://127.0.0.1:{port}/", vars)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "http://127.0.0.1:11434/" {
		t.Fatalf("got %q", got)
	}
	if _, err := resolvePlaceholders("{download}", vars); err == nil {
		t.Fatal("expected error for unresolved {download}")
	}
}

func TestResolveArgs(t *testing.T) {
	vars := map[string]string{"download": "/tmp/x.tgz", "install_dir": "/opt/x"}
	got, err := resolveArgs([]string{"tar", "xzf", "{download}", "-C", "{install_dir}"}, vars)
	if err != nil {
		t.Fatalf("resolveArgs: %v", err)
	}
	want := []string{"tar", "xzf", "/tmp/x.tgz", "-C", "/opt/x"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestLoadRegistryOverride(t *testing.T) {
	bundled := t.TempDir()
	userDir := t.TempDir()

	base := validManifest()
	base.DisplayName = "Ollama (bundled)"
	writeManifest(t, bundled, "ollama.json", base)

	// A second bundled engine.
	other := validManifest()
	other.Engine = "vllm"
	other.DisplayName = "vLLM"
	writeManifest(t, bundled, "vllm.json", other)

	// User override of ollama wins over bundled.
	override := validManifest()
	override.DisplayName = "Ollama (user)"
	writeManifest(t, userDir, "ollama.json", override)

	reg, err := LoadRegistry(bundled, userDir)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	names := reg.Names()
	if len(names) != 2 || names[0] != "ollama" || names[1] != "vllm" {
		t.Fatalf("unexpected names: %v", names)
	}
	m, ok := reg.Get("ollama")
	if !ok || m.DisplayName != "Ollama (user)" {
		t.Fatalf("user override did not win: %+v", m)
	}
}

func TestLoadRegistryRejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"engine":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(dir); err == nil {
		t.Fatal("expected LoadRegistry to reject an invalid manifest")
	}
}

func TestLoadRegistryMissingDirIsSkipped(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should be skipped, got: %v", err)
	}
	if len(reg.Names()) != 0 {
		t.Fatalf("expected empty registry, got %v", reg.Names())
	}
}

// TestApplyPlatformDefaults covers the shared-base merge: a top-level
// runtime/install block is inherited by each platform, per-platform keys
// override, nested objects (env) deep-merge, and a platform can override
// a shared scalar.
func TestApplyPlatformDefaults(t *testing.T) {
	dir := t.TempDir()
	raw := `{
  "engine": "demo",
  "display_name": "Demo",
  "manifest_version": 1,
  "install": { "mode": "user" },
  "runtime": {
    "args": ["serve"],
    "env": { "SHARED": "1" },
    "port": 11434,
    "ready": { "http": "http://127.0.0.1:{port}/", "status": 200 }
  },
  "platforms": {
    "linux/amd64": {
      "detect": ["{install_dir}/bin/demo"],
      "install": { "fetch": { "url": "https://x/demo.tgz" }, "run": ["tar", "xf", "{download}", "-C", "{install_dir}"] },
      "uninstall": { "run": ["rm", "-rf", "{install_dir}"] },
      "runtime": { "bin": "{install_dir}/bin/demo", "env": { "EXTRA": "2" } }
    },
    "linux/arm64": {
      "detect": ["{install_dir}/bin/demo"],
      "install": { "fetch": { "url": "https://x/demo.tgz" }, "run": ["tar", "xf", "{download}", "-C", "{install_dir}"] },
      "uninstall": { "run": ["rm", "-rf", "{install_dir}"] },
      "runtime": { "bin": "{install_dir}/bin/demo", "port": 5678 }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "demo.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(dir)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	m, ok := reg.Get("demo")
	if !ok {
		t.Fatal("demo not loaded")
	}

	amd, ok := m.PlatformFor("linux", "amd64")
	if !ok {
		t.Fatal("linux/amd64 missing")
	}
	// inherited from the shared base:
	if amd.Runtime.Port != 11434 {
		t.Errorf("amd64 port: expected inherited 11434, got %d", amd.Runtime.Port)
	}
	if len(amd.Runtime.Args) != 1 || amd.Runtime.Args[0] != "serve" {
		t.Errorf("amd64 args: expected inherited [serve], got %v", amd.Runtime.Args)
	}
	if amd.Runtime.Ready == nil || amd.Runtime.Ready.Status != 200 {
		t.Errorf("amd64 ready: inherited base probe missing")
	}
	if amd.Install == nil || amd.Install.ModeOrDefault() != "user" || amd.Install.Fetch == nil {
		t.Errorf("amd64 install: expected base mode + platform fetch, got %+v", amd.Install)
	}
	// per-platform override applied:
	if amd.Runtime.Bin != "{install_dir}/bin/demo" {
		t.Errorf("amd64 bin override missing: %q", amd.Runtime.Bin)
	}
	// nested object (env) deep-merges base + platform keys:
	if amd.Runtime.Env["SHARED"] != "1" || amd.Runtime.Env["EXTRA"] != "2" {
		t.Errorf("amd64 env deep-merge expected {SHARED,EXTRA}, got %v", amd.Runtime.Env)
	}

	// a platform may override a shared scalar while still inheriting the rest:
	arm, ok := m.PlatformFor("linux", "arm64")
	if !ok {
		t.Fatal("linux/arm64 missing")
	}
	if arm.Runtime.Port != 5678 {
		t.Errorf("arm64 port override expected 5678, got %d", arm.Runtime.Port)
	}
	if arm.Runtime.Env["SHARED"] != "1" {
		t.Errorf("arm64 should still inherit base env SHARED, got %v", arm.Runtime.Env)
	}
}

// TestBundledManifestsMerge guards the actual shipped manifests: they must
// load, validate, and produce the right effective platforms after the
// shared-base merge.
func TestBundledManifestsMerge(t *testing.T) {
	reg, err := LoadRegistry("manifests")
	if err != nil {
		t.Fatalf("load bundled manifests: %v", err)
	}

	ol, ok := reg.Get("ollama")
	if !ok {
		t.Fatal("ollama not loaded")
	}
	if p, ok := ol.PlatformFor("linux", "amd64"); ok {
		// linux env must deep-merge shared OLLAMA_HOST with per-platform LD_LIBRARY_PATH
		if p.Runtime.Env["OLLAMA_HOST"] == "" || p.Runtime.Env["LD_LIBRARY_PATH"] == "" {
			t.Errorf("ollama linux env merge missing a key: %v", p.Runtime.Env)
		}
		if p.Runtime.Port != 11434 || p.Runtime.Bin == "" {
			t.Errorf("ollama linux runtime: port=%d bin=%q", p.Runtime.Port, p.Runtime.Bin)
		}
	} else {
		t.Error("ollama linux/amd64 missing")
	}

	lm, ok := reg.Get("lmstudio")
	if !ok {
		t.Fatal("lmstudio not loaded")
	}
	if p, ok := lm.PlatformFor("darwin", "arm64"); ok {
		// per-platform cli override + inherited shared runtime (port/start)
		if p.Runtime.CLI != "~/.lmstudio/bin/lms" {
			t.Errorf("lmstudio darwin cli: %q", p.Runtime.CLI)
		}
		if p.Runtime.Port != 1235 || len(p.Runtime.Start) == 0 {
			t.Errorf("lmstudio darwin inherited runtime missing: port=%d start=%v", p.Runtime.Port, p.Runtime.Start)
		}
	} else {
		t.Error("lmstudio darwin/arm64 missing")
	}
}

// TestBundledOllamaReadinessBudget pins the finite startup allowance used by
// every supported platform. Ollama does not serve /api/version until GPU
// discovery completes, which can exceed the previous 30-second allowance.
func TestBundledOllamaReadinessBudget(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, ok := reg.Get("ollama")
	if !ok {
		t.Fatal("ollama manifest not loaded")
	}
	for key, p := range m.Platforms {
		if p.Runtime.Ready == nil {
			t.Errorf("%s: readiness probe missing", key)
			continue
		}
		if got, want := p.Runtime.Ready.TimeoutS, 600; got != want {
			t.Errorf("%s: readiness timeout = %ds, want %ds", key, got, want)
		}
		if readiness := time.Duration(p.Runtime.Ready.TimeoutS) * time.Second; remoteReadyResponseHeaderTimeout <= readiness {
			t.Errorf("%s: remote readiness response-header timeout %s must exceed readiness timeout %s",
				key, remoteReadyResponseHeaderTimeout, readiness)
		}
	}
}

func writeManifest(t *testing.T, dir, name string, m Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOllamaManifestBindsLoopback pins the secure-inference policy: the bundled
// Ollama manifest binds 127.0.0.1 so the engine is never directly LAN-reachable
// — cluster peers reach it only through the promoted proxy's pin-gated mTLS
// ingress. OLLAMA_HOST stays templated on {host} (which now resolves to
// loopback).
func TestOllamaManifestBindsLoopback(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, ok := reg.Get("ollama")
	if !ok {
		t.Fatal("ollama manifest not loaded")
	}
	for key, p := range m.Platforms {
		if p.Runtime.Bind != "127.0.0.1" {
			t.Errorf("%s: runtime.bind = %q, want 127.0.0.1", key, p.Runtime.Bind)
		}
		if !strings.Contains(p.Runtime.Env["OLLAMA_HOST"], "{host}") {
			t.Errorf("%s: OLLAMA_HOST %q should use {host}", key, p.Runtime.Env["OLLAMA_HOST"])
		}
	}
}

// TestLMStudioManifestBindsLoopback pins LM Studio to the same loopback-only
// policy as Ollama: runtime.bind is 127.0.0.1 and the start command still
// passes `--bind {host}` (which resolves to loopback), so the engine is reached
// only through the promoted proxy's mTLS ingress, never directly on the LAN.
func TestLMStudioManifestBindsLoopback(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, ok := reg.Get("lmstudio")
	if !ok {
		t.Fatal("lmstudio manifest not loaded")
	}
	for key, p := range m.Platforms {
		if p.Runtime.Bind != "127.0.0.1" {
			t.Errorf("%s: runtime.bind = %q, want 127.0.0.1", key, p.Runtime.Bind)
		}
		var hasBindFlag, hasHostToken bool
		for _, cmd := range p.Runtime.Start {
			for i, arg := range cmd {
				if arg == "--bind" {
					hasBindFlag = true
					if i+1 < len(cmd) && cmd[i+1] == "{host}" {
						hasHostToken = true
					}
				}
			}
		}
		if !hasBindFlag || !hasHostToken {
			t.Errorf("%s: start %v should pass --bind {host}", key, p.Runtime.Start)
		}
	}
}

func TestLMStudioManifestUsesNativeSystemInventory(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, ok := reg.Get("lmstudio")
	if !ok {
		t.Fatal("lmstudio manifest not loaded")
	}
	action, ok := m.Actions["list_models"]
	if !ok || action.HTTP == nil || action.Result == nil {
		t.Fatalf("lmstudio list_models action is incomplete: %+v", action)
	}
	if action.HTTP.Method != "GET" || action.HTTP.Path != "/api/v1/models" {
		t.Errorf("lmstudio list_models HTTP = %s %s, want GET /api/v1/models", action.HTTP.Method, action.HTTP.Path)
	}
	if action.Result.Array != "models" || action.Result.Field != "key" {
		t.Errorf("lmstudio list_models result = %+v, want models[].key", action.Result)
	}
}

// TestLMStudioInstallBootstrapSafety verifies that bootstrap fetch failures are
// visible and Windows executes a downloaded .ps1
// file rather than pipe remote content through Invoke-Expression.
func TestLMStudioInstallBootstrapSafety(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, ok := reg.Get("lmstudio")
	if !ok {
		t.Fatal("lmstudio manifest not loaded")
	}
	for key, p := range m.Platforms {
		if p.Install == nil {
			continue
		}
		if strings.HasPrefix(key, "windows/") {
			if len(p.Install.Script) != 0 {
				t.Errorf("%s: install must not evaluate a remote script inline; got %v", key, p.Install.Script)
			}
			if p.Install.Fetch == nil || p.Install.Fetch.URL != "https://lmstudio.ai/install.ps1" {
				t.Errorf("%s: install must download the official script through engine-manager; got %+v", key, p.Install.Fetch)
			}
			wantRun := []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "RemoteSigned", "-File", "{download}"}
			if !slices.Equal(p.Install.Run, wantRun) {
				t.Errorf("%s: install run = %v, want %v", key, p.Install.Run, wantRun)
			}
			continue
		}
		if len(p.Install.Script) != 0 {
			t.Errorf("%s: install must not interpolate paths into shell text; got %v", key, p.Install.Script)
		}
		if p.Install.Fetch == nil || p.Install.Fetch.URL != "https://lmstudio.ai/install.sh" {
			t.Errorf("%s: install must download the official script through engine-manager; got %+v", key, p.Install.Fetch)
		}
		wantRun := []string{"bash", "{download}"}
		if !slices.Equal(p.Install.Run, wantRun) {
			t.Errorf("%s: install run = %v, want %v", key, p.Install.Run, wantRun)
		}
	}
}
