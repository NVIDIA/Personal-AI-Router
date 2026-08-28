// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// loadWithOverrides builds a registry the same way buildRegistry does at
// startup: bundled manifests first, then the per-user override dir deep-merged
// on top. Used to prove a persisted port survives a "restart".
func loadWithOverrides(t *testing.T, overrideDir string) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("LoadFS bundled: %v", err)
	}
	if err := reg.LoadOverrideDir(overrideDir); err != nil {
		t.Fatalf("LoadOverrideDir: %v", err)
	}
	return reg
}

func hostPort(t *testing.T, reg *Registry, engine string) int {
	t.Helper()
	m, ok := reg.Get(engine)
	if !ok {
		t.Fatalf("engine %q not in registry", engine)
	}
	p, ok := m.HostPlatform()
	if !ok {
		t.Fatalf("engine %q has no host platform block", engine)
	}
	return p.Runtime.Port
}

func newBundledExecutor(t *testing.T, overrideDir string) *Executor {
	t.Helper()
	reg := loadWithOverrides(t, overrideDir)
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	ex.overrideDir = overrideDir
	return ex
}

func adoptedEngineFixture(t *testing.T, engine string, port int, rt Runtime) *Executor {
	t.Helper()
	m := testEngineManifest(fakeEngineBin)
	m.Engine = engine
	m.DisplayName = engine
	for key, platform := range m.Platforms {
		platform.Runtime = rt
		m.Platforms[key] = platform
	}
	ex := newTestExecutor(t, m)
	ex.overrideDir = t.TempDir()
	st, err := ex.state(engine)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.installed = true
	st.running = true
	st.healthy = true
	st.adopted = true
	st.port = port
	st.mu.Unlock()
	return ex
}

func adoptedCommandEngineFixture(t *testing.T, engine string, port int) (*Executor, func() bool, func(int) bool) {
	t.Helper()
	dir := t.TempDir()
	stopMarker := filepath.Join(dir, "stopped-{port}")
	startMarker := filepath.Join(dir, "started-{port}")
	ex := adoptedEngineFixture(t, engine, port, Runtime{
		Mode:  "command",
		Port:  port,
		Start: [][]string{{fakeEngineBin, "touch", startMarker}},
		Stop:  &StopSpec{Cmd: []string{fakeEngineBin, "touch", stopMarker}},
	})
	return ex,
		func() bool { return fileExists(filepath.Join(dir, "stopped-"+strconv.Itoa(port))) },
		func(port int) bool { return fileExists(filepath.Join(dir, "started-"+strconv.Itoa(port))) }
}

func adoptedProcessEngineFixture(t *testing.T, engine string, port int) *Executor {
	t.Helper()
	return adoptedEngineFixture(t, engine, port, Runtime{
		Bin:  fakeEngineBin,
		Port: port,
		Stop: &StopSpec{Signal: "term", GraceS: 3},
	})
}

func adoptedCommandEngineWithoutStopFixture(t *testing.T, engine string, port int) *Executor {
	t.Helper()
	return adoptedEngineFixture(t, engine, port, Runtime{
		Mode:  "command",
		Port:  port,
		Start: [][]string{{fakeEngineBin, "echo", "up"}},
	})
}

// TestOverrideDeepMergePreservesBundled proves a partial port-only override
// file pins runtime.port while inheriting the rest of the bundled manifest
// (the single-source-of-truth merge that backs engine:set-port persistence).
func TestOverrideDeepMergePreservesBundled(t *testing.T) {
	dir := t.TempDir()
	override := []byte(`{"engine":"ollama","runtime":{"port":54321}}`)
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), override, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := loadWithOverrides(t, dir)

	m, ok := reg.Get("ollama")
	if !ok {
		t.Fatal("ollama not loaded")
	}
	p, ok := m.HostPlatform()
	if !ok {
		t.Fatal("ollama has no host platform")
	}
	if p.Runtime.Port != 54321 {
		t.Errorf("override port: got %d, want 54321", p.Runtime.Port)
	}
	// Bundled fields untouched: the top-level runtime.args ["serve"] is
	// inherited by every platform, and the install fetch URL survives.
	if len(p.Runtime.Args) != 1 || p.Runtime.Args[0] != "serve" {
		t.Errorf("bundled runtime.args not preserved through merge: %v", p.Runtime.Args)
	}
	if p.Install == nil || p.Install.Fetch == nil || p.Install.Fetch.URL == "" {
		t.Errorf("bundled install fetch not preserved through merge: %+v", p.Install)
	}
}

// TestSetPortPersistsAndRestores covers the full engine:set-port contract for
// a bundled engine that isn't running: the chosen port is written as a
// manifest override and a fresh (startup-style) load comes back up on it.
func TestSetPortPersistsAndRestores(t *testing.T) {
	for _, tc := range []struct {
		engine  string
		port    int
		bundled int
	}{
		{"ollama", 12345, 11434},
		{"lmstudio", 4321, 1235},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			dir := t.TempDir()
			ex := newBundledExecutor(t, dir)

			st, err := ex.SetPort(context.Background(), tc.engine, tc.port)
			if err != nil {
				t.Fatalf("SetPort: %v", err)
			}
			if st.Port != tc.port {
				t.Errorf("returned status port: got %d, want %d", st.Port, tc.port)
			}

			// The override file is the minimal port delta.
			data, err := os.ReadFile(filepath.Join(dir, tc.engine+".json"))
			if err != nil {
				t.Fatalf("override file not written: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("override file invalid JSON: %v", err)
			}
			rt, _ := got["runtime"].(map[string]any)
			if rt == nil || int(rt["port"].(float64)) != tc.port {
				t.Errorf("override file runtime.port: got %v, want %d", got["runtime"], tc.port)
			}

			// Restore: a fresh startup-style load comes up on the chosen port.
			if p := hostPort(t, loadWithOverrides(t, dir), tc.engine); p != tc.port {
				t.Errorf("restored port: got %d, want %d", p, tc.port)
			}

			// Reverting to the bundled default removes the override entirely.
			if _, err := ex.SetPort(context.Background(), tc.engine, tc.bundled); err != nil {
				t.Fatalf("SetPort revert: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, tc.engine+".json")); !os.IsNotExist(err) {
				t.Errorf("override file should be removed when reverting to default, stat err = %v", err)
			}
			if p := hostPort(t, loadWithOverrides(t, dir), tc.engine); p != tc.bundled {
				t.Errorf("port after revert: got %d, want bundled default %d", p, tc.bundled)
			}
		})
	}
}

// TestSetPortRejectsOutOfRange guards the validation boundary.
func TestSetPortRejectsOutOfRange(t *testing.T) {
	ex := newBundledExecutor(t, t.TempDir())
	for _, bad := range []int{0, -1, 70000} {
		if _, err := ex.SetPort(context.Background(), "ollama", bad); err == nil {
			t.Errorf("SetPort(%d) should have been rejected", bad)
		}
	}
}

func TestSetPortMovesAdoptedCommandEngine(t *testing.T) {
	ex, stopped, started := adoptedCommandEngineFixture(t, "lmstudio", 1234)
	status, err := ex.SetPort(context.Background(), "lmstudio", 1235)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped() || !started(1235) {
		t.Fatal("identified command engine was not stopped on 1234 and restarted on 1235")
	}
	if status.Port != 1235 || !status.Running {
		t.Fatalf("moved status = %+v, want running on 1235", status)
	}
}

func TestSetPortStillRejectsAdoptedProcessEngine(t *testing.T) {
	ex := adoptedProcessEngineFixture(t, "ollama", 11434)
	if _, err := ex.SetPort(context.Background(), "ollama", 11435); err == nil {
		t.Fatal("adopted process engine unexpectedly moved")
	}
	if status, err := ex.Status("ollama"); err != nil || status.Port != 11434 || !status.Running {
		t.Fatalf("rejected process engine changed: status=%+v err=%v", status, err)
	}
	if _, err := os.Stat(filepath.Join(ex.overrideDir, "ollama.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected process engine persisted an override: %v", err)
	}
}

func TestSetPortRejectsCommandEngineWithoutStopCommand(t *testing.T) {
	ex := adoptedCommandEngineWithoutStopFixture(t, "external", 1234)
	if _, err := ex.SetPort(context.Background(), "external", 1235); err == nil {
		t.Fatal("command engine without official stop unexpectedly moved")
	}
	if status, err := ex.Status("external"); err != nil || status.Port != 1234 || !status.Running {
		t.Fatalf("rejected command engine changed: status=%+v err=%v", status, err)
	}
	if _, err := os.Stat(filepath.Join(ex.overrideDir, "external.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected command engine persisted an override: %v", err)
	}
}

func TestSetPortRestartsOldPortWhenPersistenceFails(t *testing.T) {
	ex, stopped, started := adoptedCommandEngineFixture(t, "lmstudio", 1234)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex.overrideDir = blocked

	if _, err := ex.SetPort(context.Background(), "lmstudio", 1235); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !stopped() || !started(1234) || started(1235) {
		t.Fatal("persistence failure did not stop and restore the engine on 1234")
	}
	if status, err := ex.Status("lmstudio"); err != nil || status.Port != 1234 || !status.Running {
		t.Fatalf("restored status = %+v err=%v, want running on 1234", status, err)
	}
}
