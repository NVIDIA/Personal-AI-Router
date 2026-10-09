// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Render the legacy full command only for migration and argv regression tests.
func (command launchCommand) text() (string, error) {
	return formatLaunchParts(command.Env, append([]string{command.Bin}, command.Args...))
}

func TestResolvedLaunchMatchesBundledEngines(t *testing.T) {
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

func TestLaunchEnvironmentFormattingIsDeterministic(t *testing.T) {
	launch := launchCommand{Bin: "engine", Env: map[string]string{"Z_SETTING": "z", "A_SETTING": "a b"}}
	text, err := launch.text()
	require.NoError(t, err, "unstable environment order or quoting (%v, %v)", text, err)
	require.Equal(t, `A_SETTING="a b" Z_SETTING="z" engine`, text, "unstable environment order or quoting (%v, %v)", text, err)
}

func TestCommandLaunchRejectsEmptyExecutable(t *testing.T) {
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
func TestLaunchTextReachesChildLiterally(t *testing.T) {
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

func TestLifecycleUsesResolvedLaunchBuilder(t *testing.T) {
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
