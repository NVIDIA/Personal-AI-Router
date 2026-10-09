// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
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
		if !ok {
			t.Fatalf("missing bundled engine %q", engine)
		}
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
				if err != nil {
					t.Fatal(err)
				}
				text, err := launch.text()
				if err != nil {
					t.Fatal(err)
				}
				got, err := parseLaunchText(text)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("launch description differs from executable inputs: got %#v, want %#v, error %v", got, want, err)
				}
			})
		}
	}
	if _, changed := vars["bin"]; changed {
		t.Fatal("process builder mutated caller's resolution context")
	}
}

func TestLaunchEnvironmentFormattingIsDeterministic(t *testing.T) {
	launch := launchCommand{Bin: "engine", Env: map[string]string{"Z_SETTING": "z", "A_SETTING": "a b"}}
	text, err := launch.text()
	if err != nil || text != `A_SETTING="a b" Z_SETTING="z" engine` {
		t.Fatalf("unstable environment order or quoting: %q, %v", text, err)
	}
}

func TestLiteralEnvironment(t *testing.T) {
	accept := func(name string, assignments []string, want map[string]string) {
		t.Run(name, func(t *testing.T) {
			got, err := literalEnvironment(assignments)
			if err != nil {
				t.Fatalf("literalEnvironment: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("literalEnvironment(%q) = %v, want %v", assignments, got, want)
			}
		})
	}
	reject := func(name string, assignments []string, wantMessage string) {
		t.Run(name, func(t *testing.T) {
			_, err := literalEnvironment(assignments)
			if err == nil || !strings.Contains(err.Error(), wantMessage) {
				t.Fatalf("literalEnvironment(%q) error = %v, want %q", assignments, err, wantMessage)
			}
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

func TestLiteralEnvironmentNameCaseSensitivity(t *testing.T) {
	assignments := []string{"MODEL_DIR=/first", "model_dir=/second"}
	env, err := literalEnvironment(assignments)
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "assigned more than once") {
			t.Fatalf("case-insensitive duplicate error = %v, want duplicate assignment", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("distinct environment names rejected: %v", err)
	}
	want := map[string]string{"MODEL_DIR": "/first", "model_dir": "/second"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("environment = %v, want %v", env, want)
	}
}

func TestRuntimeLaunchOverridesJSONRoundTrip(t *testing.T) {
	test := func(name string, args, env []string) {
		t.Run(name, func(t *testing.T) {
			original := Runtime{LaunchArgs: args, LaunchEnv: env}
			data, err := json.Marshal(original)
			if err != nil {
				t.Fatalf("encode runtime: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatalf("decode runtime fields: %v", err)
			}
			if _, present := fields["launch_args"]; present != (args != nil) {
				t.Fatalf("launch_args presence = %v, want %v", present, args != nil)
			}
			if _, present := fields["launch_env"]; present != (env != nil) {
				t.Fatalf("launch_env presence = %v, want %v", present, env != nil)
			}
			var decoded Runtime
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("decode runtime: %v", err)
			}
			if !reflect.DeepEqual(decoded.LaunchArgs, args) || !reflect.DeepEqual(decoded.LaunchEnv, env) {
				t.Fatalf("launch overrides changed: args=%#v env=%#v, want args=%#v env=%#v", decoded.LaunchArgs, decoded.LaunchEnv, args, env)
			}
		})
	}
	test("absent overrides inherit defaults", nil, nil)
	test("empty overrides stay declared", []string{}, []string{})
	test("only arguments are declared", []string{}, nil)
	test("only environment is declared", nil, []string{})
	test("literal overrides stay intact", []string{"{port}", "", "two words"}, []string{"MODEL_DIR={install_dir}/models", "EMPTY="})
}

func TestSavedLaunchArgumentsReplaceManifestDefaults(t *testing.T) {
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
			if err != nil {
				t.Fatalf("resolve launch: %v", err)
			}
			if !reflect.DeepEqual(launch.Args, want) {
				t.Fatalf("launch arguments = %v, want %v", launch.Args, want)
			}
		})
	}
	test("process inherits defaults", "process", nil, []string{"serve", "--manifest-default"})
	test("process clears defaults", "process", []string{}, []string{"serve"})
	test("process replaces defaults", "process", []string{"--user-option"}, []string{"serve", "--user-option"})
	test("command inherits defaults", "command", nil, []string{"serve", "--manifest-default"})
	test("command clears defaults", "command", []string{}, []string{"serve"})
	test("command replaces defaults", "command", []string{"--user-option"}, []string{"serve", "--user-option"})
}

func TestCommandLaunchRejectsEmptyExecutable(t *testing.T) {
	for _, template := range [][]string{{"", "start"}, {"{cli}", "start"}} {
		if _, err := resolveCommandLaunch(template, map[string]string{"cli": ""}); err == nil {
			t.Fatal("empty executable must fail rather than skip the start command")
		}
	}
}

// The child receives the parser's output directly. This catches escaping
// differences in the real Windows/POSIX argv path, not just parser symmetry.
func TestLaunchTextReachesChildLiterally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "captured args.json")
	want := []string{"", "two words", "日本語", `C:\new\tools\`, `\\server\share`, `a"b`, "$HOME", "%USERPROFILE%", "{port}", "$(ignored)", "line\nbreak"}
	tokens := append([]string{fakeEngineBin, "captureargs", path}, want...)
	text, err := formatLaunchText(tokens)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseLaunchText(text)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := startManagedProc(parsed[0], parsed[1:], nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proc.stop(0) })
	select {
	case <-proc.done:
	case <-time.After(10 * time.Second):
		t.Fatal("argument-capture child did not exit")
	}
	assertCapturedArgs(t, path, want)
}

func TestLifecycleUsesResolvedLaunchBuilder(t *testing.T) {
	for _, mode := range []string{"process", "command"} {
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
			if err := ex.Start(context.Background(), "fake"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ex.Stop("fake") })
			// Process mode has no readiness probe here; wait for the short-lived
			// fixture to finish writing before inspecting its actual argv.
			st, err := ex.state("fake")
			if err != nil {
				t.Fatal(err)
			}
			st.mu.Lock()
			proc := st.proc
			st.mu.Unlock()
			if proc != nil {
				select {
				case <-proc.done:
				case <-time.After(10 * time.Second):
					t.Fatal("capture process did not exit")
				}
			}
			assertCapturedArgs(t, path, []string{"127.0.0.1", "25001", "two words", ""})
		})
	}
}

func assertCapturedArgs(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child received %#v; want %#v", got, want)
	}
}
