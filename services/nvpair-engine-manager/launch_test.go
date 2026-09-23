// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	vars := map[string]string{"host": "127.0.0.1", "port": "12345", "cli": "/test path/lms", "install_dir": "/test path"}
	for _, engine := range []string{"ollama", "lmstudio"} {
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
				} else {
					launch, err = resolveCommandLaunch(platform.Runtime.Start[0], vars)
					want = []string{"/test path/lms", "server", "start", "--port", "12345", "--bind", "127.0.0.1"}
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
	t.Cleanup(proc.stop)
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
