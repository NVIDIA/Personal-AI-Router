// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, m.Validate(), "valid manifest rejected")
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
	require.NoError(t, m.Validate(), "command-mode/cmd-action manifest rejected")
}

func TestValidateAcceptsLlamaCPPModelPullProtocol(t *testing.T) {
	m := validManifest()
	m.Actions[pullModelAction] = Action{
		HTTP:             &ActionHTTP{Method: "POST", Path: "/models"},
		ProgressProtocol: pullProgressProtocolLlamaCPPModelsSSE,
	}
	require.NoError(t, m.Validate(), "llama.cpp model pull protocol rejected")
}

func TestValidateAcceptsHTTPQueryParams(t *testing.T) {
	m := validManifest()
	m.Actions["delete_model"] = Action{
		HTTP: &ActionHTTP{
			Method:   http.MethodDelete,
			Path:     "/models",
			ParamsIn: actionHTTPParamsQuery,
		},
	}
	require.NoError(t, m.Validate(), "HTTP query params rejected")
}

func TestValidateRejectsInvalidHTTPParamsLocation(t *testing.T) {
	m := validManifest()
	m.Actions["delete_model"] = Action{
		HTTP: &ActionHTTP{
			Method:   http.MethodDelete,
			Path:     "/models",
			ParamsIn: "headers",
		},
	}
	require.ErrorContains(t, m.Validate(), "http.params_in", "invalid HTTP params location accepted")
}

func TestValidateAcceptsNestedResultMatch(t *testing.T) {
	m := validManifest()
	m.Actions["list_models"] = Action{
		HTTP: &ActionHTTP{Method: "GET", Path: "/models"},
		Result: &ActionResult{
			Array: "data",
			Field: "id",
			Match: &ResultMatch{Field: "status.value", In: []string{"loaded"}},
		},
	}
	require.NoError(t, m.Validate(), "nested result match rejected")
}

func TestValidateAcceptsProbeJSONMatch(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Runtime.Ready.JSONMatch = &ProbeJSONMatch{Field: "service.role", Value: "router"}
	m.Platforms["linux/amd64"] = p
	require.NoError(t, m.Validate(), "HTTP probe JSON match rejected")
}

func TestValidateAcceptsUnpinnedFetch(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Install.Fetch.SHA256 = "" // unpinned: allowed (download runs HTTPS-only with a loud warning)
	m.Platforms["linux/amd64"] = p
	require.NoError(t, m.Validate(), "unpinned fetch should validate")
}

func TestValidateAcceptsNamedInstallArtifacts(t *testing.T) {
	m := validManifest()
	setInstallArtifacts(&m, validInstallArtifacts())
	require.NoError(t, m.Validate(), "named install artifacts rejected")
}

func TestValidateRejectsDownloadInstallWithoutRun(t *testing.T) {
	test := func(name string, install *Install) {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			p := m.Platforms["linux/amd64"]
			p.Install = install
			m.Platforms["linux/amd64"] = p

			const want = `platform "linux/amd64": install.run is required when install.fetch or install.artifacts is present`
			require.EqualError(t, m.Validate(), want, "download install without run accepted")
		})
	}

	test("fetch with omitted run", &Install{
		Fetch: &Fetch{URL: "https://example/installer.zip"},
	})
	test("fetch with empty run", &Install{
		Fetch: &Fetch{URL: "https://example/installer.zip"},
		Run:   []string{},
	})
	test("artifacts with omitted run", &Install{
		Artifacts: validInstallArtifacts(),
	})
	test("artifacts with empty run", &Install{
		Artifacts: validInstallArtifacts(),
		Run:       []string{},
	})
}

func TestValidateAcceptsScriptOnlyInstall(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Install = &Install{Script: []string{"sh", "installer.sh"}}
	m.Platforms["linux/amd64"] = p
	require.NoError(t, m.Validate(), "script-only install rejected")
}

func TestValidateRejectsArtifactPlaceholderFromAnotherPlatform(t *testing.T) {
	m := validManifest()
	setInstallArtifacts(&m, validInstallArtifacts())
	mac := Platform{
		Install: &Install{
			Fetch: &Fetch{URL: "https://example/server.tar.gz"},
			Run:   []string{"extract", "{download}"},
		},
		Runtime: Runtime{Bin: "{install_dir}/llama-server"},
	}
	m.Platforms["darwin/arm64"] = mac
	require.NoError(t, m.Validate(), "valid multi-platform fixture rejected")

	mac.Install.Run = []string{"extract", "{download_cudart}"}
	m.Platforms["darwin/arm64"] = mac
	const want = `platform "darwin/arm64": unknown placeholder {download_cudart}`
	require.ErrorContains(t, m.Validate(), want, "macOS install references {download_cudart}, but only Linux declares cudart")
}

func validInstallArtifacts() []InstallArtifact {
	return []InstallArtifact{
		{Name: "server", URL: "https://example/server.zip", SHA256: strings.Repeat("a", 64)},
		{Name: "cudart", URL: "https://example/cudart.zip", SHA256: strings.Repeat("b", 64)},
	}
}

func setInstallArtifacts(m *Manifest, artifacts []InstallArtifact) {
	p := m.Platforms["linux/amd64"]
	p.Install.Fetch = nil
	p.Install.Artifacts = artifacts
	p.Install.Run = []string{"extract", "{download_server}", "{download_cudart}"}
	m.Platforms["linux/amd64"] = p
}

func TestValidateRejectsBadEngineName(t *testing.T) {
	test := func(name, engine string) {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			m.Engine = engine
			assert.Error(t, m.Validate(), "invalid engine name must be rejected")
		})
	}
	test("parent traversal", "../evil")
	test("forward slash", "a/b")
	test("backslash", `a\b`)
	test("parent directory", "..")
	test("current directory", ".")
	test("space", "a b")
	test("empty name", "")
}

func TestValidateRejectsScriptWithFetch(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Install.Script = []string{"sh", "-c", "curl x | sh"} // coexists with fetch+run
	m.Platforms["linux/amd64"] = p
	require.ErrorContains(t, m.Validate(), "mutually exclusive", "expected script+fetch rejection")
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
		{"fetch with artifacts", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Install.Artifacts = validInstallArtifacts()
			m.Platforms["linux/amd64"] = p
		}, "mutually exclusive"},
		{"invalid artifact name", func(m *Manifest) {
			artifacts := validInstallArtifacts()
			artifacts[0].Name = "../server"
			setInstallArtifacts(m, artifacts)
		}, "must match"},
		{"duplicate artifact name", func(m *Manifest) {
			artifacts := validInstallArtifacts()
			artifacts[1].Name = artifacts[0].Name
			setInstallArtifacts(m, artifacts)
		}, "duplicate install artifact"},
		{"insecure artifact URL", func(m *Manifest) {
			artifacts := validInstallArtifacts()
			artifacts[0].URL = "http://example.com/server.zip"
			setInstallArtifacts(m, artifacts)
		}, "must be https"},
		{"invalid artifact checksum", func(m *Manifest) {
			artifacts := validInstallArtifacts()
			artifacts[0].SHA256 = "deadbeef"
			setInstallArtifacts(m, artifacts)
		}, "64-character hexadecimal"},
		{"unknown artifact placeholder", func(m *Manifest) {
			setInstallArtifacts(m, validInstallArtifacts())
			p := m.Platforms["linux/amd64"]
			p.Install.Run = append(p.Install.Run, "{download_gpu}")
			m.Platforms["linux/amd64"] = p
		}, "unknown placeholder {download_gpu}"},
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
		{"JSON match without HTTP", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Runtime.Ready = &Probe{
				TCP:       "127.0.0.1:{port}",
				JSONMatch: &ProbeJSONMatch{Field: "role", Value: "router"},
			}
			m.Platforms["linux/amd64"] = p
		}, "json_match requires http"},
		{"invalid JSON match field path", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Runtime.Ready.JSONMatch = &ProbeJSONMatch{Field: "service..role", Value: "router"}
			m.Platforms["linux/amd64"] = p
		}, "is not a valid object path"},
		{"missing JSON match value", func(m *Manifest) {
			p := m.Platforms["linux/amd64"]
			p.Runtime.Ready.JSONMatch = &ProbeJSONMatch{Field: "role"}
			m.Platforms["linux/amd64"] = p
		}, "json_match.value is required"},
		{"action without http or cmd", func(m *Manifest) {
			m.Actions = map[string]Action{"x": {Description: "neither"}}
		}, "exactly one of http, cmd, or remove_path"},
		{"action missing method", func(m *Manifest) {
			m.Actions = map[string]Action{"x": {HTTP: &ActionHTTP{Path: "/p"}}}
		}, "http.method and http.path"},
		{"unknown progress protocol", func(m *Manifest) {
			m.Actions[pullModelAction] = Action{
				HTTP:             &ActionHTTP{Method: "POST", Path: "/models"},
				ProgressProtocol: "unknown",
			}
		}, "unsupported progress_protocol"},
		{"progress protocol on other action", func(m *Manifest) {
			m.Actions["list_models"] = Action{
				HTTP:             &ActionHTTP{Method: "GET", Path: "/models"},
				ProgressProtocol: pullProgressProtocolLlamaCPPModelsSSE,
			}
		}, "requires the HTTP pull_model action"},
		{"invalid match field path", func(m *Manifest) {
			m.Actions["list_models"] = Action{
				HTTP: &ActionHTTP{Method: "GET", Path: "/models"},
				Result: &ActionResult{
					Array: "models",
					Field: "id",
					Match: &ResultMatch{Field: "status..value", In: []string{"loaded"}},
				},
			}
		}, "is not a valid object path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			require.ErrorContains(t, m.Validate(), tc.want)
		})
	}
}

func TestPlatformFor(t *testing.T) {
	m := validManifest()
	m.Platforms["windows/amd64"] = m.Platforms["linux/amd64"]
	_, ok := m.PlatformFor("linux", "amd64")
	require.True(t, ok, "expected linux/amd64 to resolve")
	_, ok = m.PlatformFor("darwin", "arm64")
	require.False(t, ok, "did not expect darwin/arm64 to resolve")
}

func TestResolvePlaceholders(t *testing.T) {
	vars := map[string]string{"port": "11434", "install_dir": "/opt/x", "bin": "/opt/x/ollama"}
	got, err := resolvePlaceholders("http://127.0.0.1:{port}/", vars)
	require.NoError(t, err, "resolve")
	require.Equal(t, "http://127.0.0.1:11434/", got, "got")
	_, err = resolvePlaceholders("{download}", vars)
	require.Error(t, err, "expected error for unresolved {download}")
}

func TestResolveArgs(t *testing.T) {
	vars := map[string]string{"download": "/tmp/x.tgz", "install_dir": "/opt/x"}
	got, err := resolveArgs([]string{"tar", "xzf", "{download}", "-C", "{install_dir}"}, vars)
	require.NoError(t, err, "resolveArgs")
	require.Equal(t, []string{"tar", "xzf", "/tmp/x.tgz", "-C", "/opt/x"}, got, "resolved args")
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
	require.NoError(t, err, "LoadRegistry")
	require.Equal(t, []string{"ollama", "vllm"}, reg.Names(), "unexpected names")
	m, ok := reg.Get("ollama")
	require.True(t, ok, "user override did not win (%v)", m)
	require.Equal(t, "Ollama (user)", m.DisplayName, "user override did not win (%v)", m)
}

func TestLoadOverrideDirRejectsEmptyInstallRun(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"), "load bundled manifests")
	base, ok := reg.Get("ollama")
	require.True(t, ok, "bundled ollama manifest missing")
	baseRun := base.Platforms["linux/amd64"].Install.Run
	require.NotEmpty(t, baseRun, "bundled ollama install.run is empty")
	dir := t.TempDir()
	const override = `{
  "engine": "ollama",
  "display_name": "Invalid override",
  "platforms": {"linux/amd64": {"install": {"run": []}}}
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ollama.json"), []byte(override), 0o644), "write override")
	require.NoError(t, reg.LoadOverrideDir(dir), "load override directory")
	got, ok := reg.Get("ollama")
	require.True(t, ok, "invalid override removed bundled manifest")
	require.Same(t, base, got, "invalid override replaced bundled manifest")
	require.Equal(t, baseRun, got.Platforms["linux/amd64"].Install.Run, "invalid override changed the bundled install.run")
}

func TestLoadRegistryRejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"engine":"x"}`), 0o644))
	_, err := LoadRegistry(dir)
	require.Error(t, err, "expected LoadRegistry to reject an invalid manifest")
}

func TestLoadRegistryMissingDirIsSkipped(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err, "missing dir should be skipped")
	require.Empty(t, reg.Names(), "expected empty registry")
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.json"), []byte(raw), 0o644))
	reg, err := LoadRegistry(dir)
	require.NoError(t, err, "LoadRegistry")
	m, ok := reg.Get("demo")
	require.True(t, ok, "demo not loaded")

	amd, ok := m.PlatformFor("linux", "amd64")
	require.True(t, ok, "linux/amd64 missing")
	// inherited from the shared base:
	assert.Equal(t, 11434, amd.Runtime.Port, "amd64 port: expected inherited 11434")
	assert.Equal(t, []string{"serve"}, amd.Runtime.Args, "amd64 args: expected inherited [serve]")
	if assert.NotNil(t, amd.Runtime.Ready, "amd64 ready: inherited base probe missing") {
		assert.Equal(t, 200, amd.Runtime.Ready.Status, "amd64 ready: inherited base probe missing")
	}
	if assert.NotNil(t, amd.Install, "amd64 install: expected base mode + platform fetch") {
		assert.Equal(t, "user", amd.Install.ModeOrDefault(), "amd64 install: expected base mode + platform fetch")
		assert.NotNil(t, amd.Install.Fetch, "amd64 install: expected base mode + platform fetch")
	}
	// per-platform override applied:
	assert.Equal(t, "{install_dir}/bin/demo", amd.Runtime.Bin, "amd64 bin override missing")
	// nested object (env) deep-merges base + platform keys:
	assert.Equal(t, "1", amd.Runtime.Env["SHARED"], "amd64 env deep-merge expected {SHARED,EXTRA}")
	assert.Equal(t, "2", amd.Runtime.Env["EXTRA"], "amd64 env deep-merge expected {SHARED,EXTRA}")

	// a platform may override a shared scalar while still inheriting the rest:
	arm, ok := m.PlatformFor("linux", "arm64")
	require.True(t, ok, "linux/arm64 missing")
	assert.Equal(t, 5678, arm.Runtime.Port, "arm64 port override expected 5678")
	assert.Equal(t, "1", arm.Runtime.Env["SHARED"], "arm64 should still inherit base env SHARED")
}

// TestBundledManifestsMerge guards the actual shipped manifests: they must
// load, validate, and produce the right effective platforms after the
// shared-base merge.
func TestBundledManifestsMerge(t *testing.T) {
	reg, err := LoadRegistry("manifests")
	require.NoError(t, err, "load bundled manifests")

	ol, ok := reg.Get("ollama")
	require.True(t, ok, "ollama not loaded")
	if p, ok := ol.PlatformFor("linux", "amd64"); ok {
		// linux env must deep-merge shared OLLAMA_HOST with per-platform LD_LIBRARY_PATH
		assert.NotEqual(t, "", p.Runtime.Env["OLLAMA_HOST"], "ollama linux env merge missing a key")
		assert.NotEqual(t, "", p.Runtime.Env["LD_LIBRARY_PATH"], "ollama linux env merge missing a key")
		assert.Equal(t, 11434, p.Runtime.Port, "ollama linux runtime: port")
		assert.NotEqual(t, "", p.Runtime.Bin, "ollama linux runtime: port")
	} else {
		assert.Fail(t, "ollama linux/amd64 missing")
	}

	lm, ok := reg.Get("lmstudio")
	require.True(t, ok, "lmstudio not loaded")
	if p, ok := lm.PlatformFor("darwin", "arm64"); ok {
		// per-platform cli override + inherited shared runtime (port/start)
		assert.Equal(t, "~/.lmstudio/bin/lms", p.Runtime.CLI, "lmstudio darwin cli")
		assert.Equal(t, 1235, p.Runtime.Port, "lmstudio darwin inherited runtime missing: port")
		assert.NotEmpty(t, p.Runtime.Start, "lmstudio darwin inherited runtime missing: port")
	} else {
		assert.Fail(t, "lmstudio darwin/arm64 missing")
	}
}

// TestBundledOllamaReadinessBudget pins the finite startup allowance used by
// every supported platform. Ollama does not serve /api/version until GPU
// discovery completes, which can exceed the previous 30-second allowance.
func TestBundledOllamaReadinessBudget(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("ollama")
	require.True(t, ok, "ollama manifest not loaded")
	for key, p := range m.Platforms {
		if p.Runtime.Ready == nil {
			assert.Failf(t, "readiness probe missing", "%s", key)
			continue
		}
		assert.Equal(t, 600, p.Runtime.Ready.TimeoutS, " (%v)", key)
		assert.Greater(t, remoteReadyResponseHeaderTimeout, time.Duration(p.Runtime.Ready.TimeoutS)*time.Second, " (%v)", key)
	}
}

func writeManifest(t *testing.T, dir, name string, m Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
}

// TestOllamaManifestBindsLoopback pins the secure-inference policy: the bundled
// Ollama manifest binds 127.0.0.1 so the engine is never directly LAN-reachable
// — cluster peers reach it only through the promoted proxy's pin-gated mTLS
// ingress. OLLAMA_HOST stays templated on {host} (which now resolves to
// loopback).
func TestOllamaManifestBindsLoopback(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("ollama")
	require.True(t, ok, "ollama manifest not loaded")
	for key, p := range m.Platforms {
		assert.Equal(t, "127.0.0.1", p.Runtime.Bind, " (%v)", key)
		assert.Contains(t, p.Runtime.Env["OLLAMA_HOST"], "{host}", " (%v)", key)
	}
}

// TestLMStudioManifestBindsLoopback pins LM Studio to the same loopback-only
// policy as Ollama: runtime.bind is 127.0.0.1 and the start command still
// passes `--bind {host}` (which resolves to loopback), so the engine is reached
// only through the promoted proxy's mTLS ingress, never directly on the LAN.
func TestLMStudioManifestBindsLoopback(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("lmstudio")
	require.True(t, ok, "lmstudio manifest not loaded")
	for key, p := range m.Platforms {
		assert.Equal(t, "127.0.0.1", p.Runtime.Bind, " (%v)", key)
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
		assert.True(t, hasBindFlag, " (%v)", key)
		assert.True(t, hasHostToken, " (%v)", key)
	}
}

func TestLlamaCPPManifestRequiresRouterIdentity(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("llamacpp")
	require.True(t, ok, "llamacpp manifest not loaded")
	for key, p := range m.Platforms {
		ready := p.Runtime.Ready
		if !assert.NotNil(t, ready, "%s: readiness JSON identity is missing", key) || !assert.NotNil(t, ready.JSONMatch, "%s: readiness JSON identity is missing", key) {
			continue
		}
		assert.Equal(t, "http://127.0.0.1:{port}/props", ready.HTTP, "%s: readiness URL", key)
		assert.Equal(t, "role", ready.JSONMatch.Field, "%s: readiness JSON identity field", key)
		assert.Equal(t, "router", ready.JSONMatch.Value, "%s: readiness JSON identity value", key)
		if health := p.Runtime.Health; assert.NotNil(t, health, "%s: ongoing health probe is missing", key) {
			assert.Equal(t, "http://127.0.0.1:{port}/health", health.HTTP, "%s: ongoing health probe URL", key)
			assert.Nil(t, health.JSONMatch, "%s: ongoing health probe changed unexpectedly", key)
		}
	}
}

func TestLlamaCPPManifestDeclaresNativeCacheDelete(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("llamacpp")
	require.True(t, ok, "llamacpp manifest not loaded")
	require.Contains(t, m.Actions, "delete_model", "llamacpp delete_model action is incomplete")
	action := m.Actions["delete_model"]
	require.NotNil(t, action.HTTP, "llamacpp delete_model action is incomplete")
	assert.Equal(t, http.MethodDelete, action.HTTP.Method)
	assert.Equal(t, "/models", action.HTTP.Path)
	assert.Equal(t, actionHTTPParamsQuery, action.HTTP.ParamsIn)
	assert.False(t, action.RestartAfter, "llamacpp delete_model must not restart the router")
}

func TestLMStudioManifestUsesNativeSystemInventory(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("lmstudio")
	require.True(t, ok, "lmstudio manifest not loaded")
	require.Contains(t, m.Actions, "list_models", "lmstudio list_models action is incomplete")
	action := m.Actions["list_models"]
	require.NotNil(t, action.HTTP, "lmstudio list_models action is incomplete (%v)", action)
	require.NotNil(t, action.Result, "lmstudio list_models action is incomplete (%v)", action)
	assert.Equal(t, "GET", action.HTTP.Method, "lmstudio list_models HTTP")
	assert.Equal(t, "/api/v1/models", action.HTTP.Path, "lmstudio list_models HTTP")
	assert.Equal(t, "models", action.Result.Array, "lmstudio list_models result")
	assert.Equal(t, "key", action.Result.Field, "lmstudio list_models result")
}

// TestLMStudioInstallBootstrapSafety verifies that bootstrap fetch failures are
// visible and Windows executes a downloaded .ps1
// file rather than pipe remote content through Invoke-Expression.
func TestLMStudioInstallBootstrapSafety(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("lmstudio")
	require.True(t, ok, "lmstudio manifest not loaded")
	for key, p := range m.Platforms {
		if p.Install == nil {
			continue
		}
		if strings.HasPrefix(key, "windows/") {
			assert.Empty(t, p.Install.Script, " (%v)", key)
			if assert.NotNil(t, p.Install.Fetch, " (%v)", key) {
				assert.Equal(t, "https://lmstudio.ai/install.ps1", p.Install.Fetch.URL, " (%v)", key)
			}
			assert.Equal(t, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "RemoteSigned", "-File", "{download}"}, p.Install.Run, " (%v)", key)
			continue
		}
		assert.Empty(t, p.Install.Script, " (%v)", key)
		if assert.NotNil(t, p.Install.Fetch, " (%v)", key) {
			assert.Equal(t, "https://lmstudio.ai/install.sh", p.Install.Fetch.URL, " (%v)", key)
		}
		assert.Equal(t, []string{"bash", "{download}"}, p.Install.Run, " (%v)", key)
	}
}
