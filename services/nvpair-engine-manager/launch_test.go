// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Render the legacy full command only for migration and argv regression tests.
func (command launchCommand) text() (string, error) {
	return formatLaunchParts(command.Env, append([]string{command.Bin}, command.Args...))
}

func TestResolvedLaunch_MatchesBundledEngines(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	vars := map[string]string{"host": "127.0.0.1", "port": "12345", "cli": "/test path/lms", "install_dir": "/test path", "models_dir": "/test models"}
	for _, engine := range []string{"ollama", "lmstudio", "llamacpp"} {
		manifest, ok := reg.Get(engine)
		require.True(t, ok, "missing bundled engine (%v)", engine)
		// Exercise every platform even when running on Windows or macOS, so
		// Linux-only launch environment requirements are covered locally too.
		for platformKey, platform := range manifest.Platforms {
			t.Run(engine+"/"+platformKey, func(t *testing.T) {
				var launch launchCommand
				var err error
				var want []string
				if engine == "ollama" {
					launch, err = resolveProcessLaunch(platform.Runtime, "/test path/ollama", vars)
					want = []string{"OLLAMA_HOST=127.0.0.1:12345", "/test path/ollama", "serve"}
					if strings.HasPrefix(platformKey, "linux/") {
						want = append([]string{"LD_LIBRARY_PATH=/test path/lib/ollama"}, want...)
					}
				} else if engine == "lmstudio" {
					launch, err = resolveCommandLaunch(platform.Runtime.Start[0], vars)
					want = []string{"/test path/lms", "server", "start", "--port", "12345", "--bind", "127.0.0.1"}
				} else {
					var bin string
					bin, err = resolvePlaceholders(platform.Runtime.Bin, vars)
					if err == nil {
						launch, err = resolveProcessLaunch(platform.Runtime, bin, vars)
					}
					want = []string{"LLAMA_CACHE=/test models", bin, "--sleep-idle-seconds", "300", "--host", "127.0.0.1", "--port", "12345", "--cors-origins", ""}
					if strings.HasPrefix(platformKey, "linux/") {
						want = append([]string{"LD_LIBRARY_PATH=/test path"}, want...)
					}
				}
				require.NoError(t, err)
				text, err := launch.text()
				require.NoError(t, err)
				got, err := parseLaunchText(text)
				require.NoError(t, err, "launch description differs from executable inputs: (%v, %v, %v)", got, want, err)
				require.Equal(t, want, got, "launch description differs from executable inputs: (%v, %v, %v)", got, want, err)
			})
		}
	}
	require.NotContains(t, vars, "bin", "process builder mutated caller's resolution context")
}

func TestLaunchEnvironmentFormatting_IsDeterministic(t *testing.T) {
	launch := launchCommand{Bin: "engine", Env: map[string]string{"Z_SETTING": "z", "A_SETTING": "a b"}}
	text, err := launch.text()
	require.NoError(t, err, "unstable environment order or quoting (%v, %v)", text, err)
	require.Equal(t, `A_SETTING="a b" Z_SETTING="z" engine`, text, "unstable environment order or quoting (%v, %v)", text, err)
}

func TestLiteralEnvironment_ParsesAssignmentsAndRejectsInvalidNames(t *testing.T) {
	accept := func(name string, assignments []string, want map[string]string) {
		t.Run(name, func(t *testing.T) {
			got, err := literalEnvironment(assignments)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
	reject := func(name string, assignments []string, wantMessage string) {
		t.Run(name, func(t *testing.T) {
			_, err := literalEnvironment(assignments)
			require.Error(t, err)
			assert.Contains(t, err.Error(), wantMessage)
		})
	}

	accept("empty assignments", nil, map[string]string{})
	accept("parses multiple keys", []string{
		"MODEL_DIR=/models",
		"TOKEN=secret",
		"CACHE_DIR=/cache",
	}, map[string]string{
		"MODEL_DIR": "/models",
		"TOKEN":     "secret",
		"CACHE_DIR": "/cache",
	})
	accept("preserves templates and shell syntax literally", []string{`MODEL_DIR={install_dir}/models`, `LITERAL=$HOME "two words" C:\new\tools`}, map[string]string{
		"MODEL_DIR": "{install_dir}/models",
		"LITERAL":   `$HOME "two words" C:\new\tools`,
	})
	accept("splits assignments at the first equals", []string{"TOKEN=prefix=suffix"}, map[string]string{
		"TOKEN": "prefix=suffix",
	})
	accept("accepts an empty value", []string{"EMPTY="}, map[string]string{
		"EMPTY": "",
	})

	reject("requires an equals sign", []string{"MISSING_VALUE"}, "environment variable names")
	reject("rejects an empty name", []string{"=value"}, "environment variable names")
	reject("rejects a leading digit", []string{"9MODEL_DIR=/models"}, "environment variable names")
	reject("requires a valid environment name", []string{"MODEL-DIR=/models"}, "environment variable names")
	reject("rejects duplicate names", []string{"TOKEN=first", "TOKEN=second"}, "assigned more than once")
	reject("rejects NUL in values", []string{"TOKEN=before\x00after"}, "unsupported control character")
	reject("rejects invalid UTF-8", []string{"TOKEN=\xff"}, "valid UTF-8")
}

func TestLiteralEnvironment_UsesPlatformNameCaseSensitivity(t *testing.T) {
	assignments := []string{"MODEL_DIR=/first", "model_dir=/second"}
	env, err := literalEnvironment(assignments)
	if runtime.GOOS == "windows" {
		require.Error(t, err)
		assert.Contains(t, err.Error(), "assigned more than once")
		return
	}
	require.NoError(t, err)
	want := map[string]string{"MODEL_DIR": "/first", "model_dir": "/second"}
	assert.Equal(t, want, env)
}

func TestRuntimeLaunchOverrides_RoundTripsJSONAndPreservesEmptySlices(t *testing.T) {
	test := func(name string, args, env []string) {
		t.Run(name, func(t *testing.T) {
			original := Runtime{LaunchArgs: args, LaunchEnv: env}
			data, err := json.Marshal(original)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &fields))
			_, argsPresent := fields["launch_args"]
			assert.Equal(t, args != nil, argsPresent, "launch_args presence")
			_, envPresent := fields["launch_env"]
			assert.Equal(t, env != nil, envPresent, "launch_env presence")
			var decoded Runtime
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, args, decoded.LaunchArgs)
			assert.Equal(t, env, decoded.LaunchEnv)
		})
	}
	test("absent overrides inherit defaults", nil, nil)
	test("empty overrides stay declared", []string{}, []string{})
	test("only arguments are declared", []string{}, nil)
	test("only environment is declared", nil, []string{})
	test("literal overrides stay intact", []string{"{port}", "", "two words"}, []string{"MODEL_DIR={install_dir}/models", "EMPTY="})
}

func TestSavedLaunchArguments_ReplaceManifestDefaults(t *testing.T) {
	test := func(name, mode string, args, want []string) {
		t.Run(name, func(t *testing.T) {
			rt := Runtime{
				Mode:       mode,
				Bin:        "engine",
				Args:       []string{"serve", "--manifest-default"},
				Start:      [][]string{{"engine", "serve", "--manifest-default"}},
				LaunchArgs: args,
				EditableLaunch: &EditableLaunch{
					FixedArgs: []string{"serve"},
					Controls:  []LaunchControl{{Value: "{server.host}:{server.port}", Env: []string{"ENGINE_LISTEN"}}},
				},
			}
			vars := map[string]string{"host": "127.0.0.1", "port": "12345"}
			var launch launchCommand
			var err error
			if mode == "process" {
				launch, err = resolveProcessLaunch(rt, "", vars)
			} else {
				launch, err = resolveRuntimeCommand(rt, 0, vars)
			}
			require.NoError(t, err)
			assert.Equal(t, want, launch.Args)
		})
	}
	test("process inherits defaults", "process", nil, []string{"serve", "--manifest-default"})
	test("process clears defaults", "process", []string{}, []string{"serve"})
	test("process replaces defaults", "process", []string{"--user-option"}, []string{"serve", "--user-option"})
	test("command inherits defaults", "command", nil, []string{"serve", "--manifest-default"})
	test("command clears defaults", "command", []string{}, []string{"serve"})
	test("command replaces defaults", "command", []string{"--user-option"}, []string{"serve", "--user-option"})
}

func TestCommandLaunch_RejectsEmptyExecutable(t *testing.T) {
	test := func(name string, template []string) {
		t.Run(name, func(t *testing.T) {
			_, err := resolveCommandLaunch(template, map[string]string{"cli": ""})
			require.Error(t, err, "empty executable must fail rather than skip the start command")
		})
	}
	test("literal empty executable", []string{"", "start"})
	test("expanded empty executable", []string{"{cli}", "start"})
}

// The child receives the parser's output directly. This catches escaping
// differences in the real Windows/POSIX argv path, not just parser symmetry.
func TestLaunchText_ReachesChildLiterally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "captured args.json")
	want := []string{"", "two words", "日本語", `C:\new\tools\`, `\\server\share`, `a"b`, "$HOME", "%USERPROFILE%", "{port}", "$(ignored)", "line\nbreak"}
	tokens := append([]string{fakeEngineBin, "captureargs", path}, want...)
	text, err := formatLaunchText(tokens)
	require.NoError(t, err)
	parsed, err := parseLaunchText(text)
	require.NoError(t, err)
	proc, err := startManagedProc(parsed[0], parsed[1:], nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { proc.stop(0) })
	select {
	case <-proc.done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "argument-capture child did not exit")
	}
	assertCapturedArgs(t, path, want)
}

func TestLifecycle_UsesResolvedLaunchBuilder(t *testing.T) {
	test := func(mode string) {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "captured args.json")
			manifest := testEngineManifest(fakeEngineBin)
			for key, platform := range manifest.Platforms {
				platform.Runtime = Runtime{Mode: mode, Bin: fakeEngineBin, Port: 25001,
					Args:  []string{"captureargs", path, "{host}", "{port}", "two words", ""},
					Start: [][]string{{fakeEngineBin, "captureargs", path, "{host}", "{port}", "two words", ""}},
				}
				manifest.Platforms[key] = platform
			}
			ex := newTestExecutor(t, manifest)
			require.NoError(t, ex.Start(context.Background(), "fake"))
			t.Cleanup(func() { _ = ex.Stop("fake") })
			// Process mode has no readiness probe here; wait for the short-lived
			// fixture to finish writing before inspecting its actual argv.
			st, err := ex.state("fake")
			require.NoError(t, err)
			st.mu.Lock()
			proc := st.proc
			st.mu.Unlock()
			if proc != nil {
				select {
				case <-proc.done:
				case <-time.After(10 * time.Second):
					require.FailNow(t, "capture process did not exit")
				}
			}
			assertCapturedArgs(t, path, []string{"127.0.0.1", "25001", "two words", ""})
		})
	}
	test("process")
	test("command")
}

func assertCapturedArgs(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var got []string
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, want, got, "child received")
}
