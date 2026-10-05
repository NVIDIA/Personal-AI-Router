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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadWithOverrides builds a registry the same way buildRegistry does at
// startup: bundled manifests first, then the per-user override dir deep-merged
// on top. Used to prove a persisted port survives a "restart".
func loadWithOverrides(t *testing.T, overrideDir string) *Registry {
	t.Helper()
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"), "LoadFS bundled")
	require.NoError(t, reg.LoadOverrideDir(overrideDir), "LoadOverrideDir")
	return reg
}

func hostPort(t *testing.T, reg *Registry, engine string) int {
	t.Helper()
	m, ok := reg.Get(engine)
	require.True(t, ok, "engine (%v)", engine)
	p, ok := m.HostPlatform()
	require.True(t, ok, "engine (%v)", engine)
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
	require.NoError(t, err)
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ollama.json"), override, 0o644))
	reg := loadWithOverrides(t, dir)

	m, ok := reg.Get("ollama")
	require.True(t, ok, "ollama not loaded")
	p, ok := m.HostPlatform()
	require.True(t, ok, "ollama has no host platform")
	assert.Equal(t, 54321, p.Runtime.Port, "override port:")
	// Bundled fields untouched: the top-level runtime.args ["serve"] is
	// inherited by every platform, and the install fetch URL survives.
	assert.Equal(t, []string{"serve"}, p.Runtime.Args, "bundled runtime.args not preserved through merge")
	if assert.NotNil(t, p.Install, "bundled install fetch not preserved through merge") &&
		assert.NotNil(t, p.Install.Fetch, "bundled install fetch not preserved through merge") {
		assert.NotEqual(t, "", p.Install.Fetch.URL, "bundled install fetch not preserved through merge")
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
			require.NoError(t, err, "SetPort")
			assert.True(t, st.Port == tc.port, "returned status port:")

			// The override file is the minimal port delta.
			data, err := os.ReadFile(filepath.Join(dir, tc.engine+".json"))
			require.NoError(t, err, "override file not written")
			var got map[string]any
			require.NoError(t, json.Unmarshal(data, &got), "override file invalid JSON")
			rt, _ := got["runtime"].(map[string]any)
			if assert.NotNil(t, rt, "override file runtime.port:") {
				assert.True(t, int(rt["port"].(float64)) == tc.port, "override file runtime.port:")
			}

			// Restore: a fresh startup-style load comes up on the chosen port.
			{
				p := hostPort(t, loadWithOverrides(t, dir), tc.engine)
				assert.True(t, p == tc.port, "restored port: (%v)", p)
			}

			// Reverting to the bundled default removes the override entirely.
			{
				_, err := ex.SetPort(context.Background(), tc.engine, tc.bundled)
				require.NoError(t, err, "SetPort revert")
			}
			{
				_, err := os.Stat(filepath.Join(dir, tc.engine+".json"))
				assert.True(t, os.IsNotExist(err), "override file should be removed when reverting to default, stat err (%v)", err)
			}
			{
				p := hostPort(t, loadWithOverrides(t, dir), tc.engine)
				assert.True(t, p == tc.bundled, "port after revert: (%v)", p)
			}
		})
	}
}

// TestSetPortRejectsOutOfRange guards the validation boundary.
func TestSetPortRejectsOutOfRange(t *testing.T) {
	ex := newBundledExecutor(t, t.TempDir())
	for _, bad := range []int{0, -1, 70000} {
		{
			_, err := ex.SetPort(context.Background(), "ollama", bad)
			assert.Error(t, err, "SetPort (%v)", bad)
		}
	}
}

func TestSetPortMovesAdoptedCommandEngine(t *testing.T) {
	ex, stopped, started := adoptedCommandEngineFixture(t, "lmstudio", 1234)
	status, err := ex.SetPort(context.Background(), "lmstudio", 1235)
	require.NoError(t, err)
	require.True(t, stopped(), "identified command engine was not stopped on 1234 and restarted on 1235")
	require.True(t, started(1235), "identified command engine was not stopped on 1234 and restarted on 1235")
	require.Equal(t, 1235, status.Port, "moved status (%v)", status)
	require.True(t, status.Running, "moved status (%v)", status)
}

func TestSetPortStillRejectsAdoptedProcessEngine(t *testing.T) {
	ex := adoptedProcessEngineFixture(t, "ollama", 11434)
	{
		_, err := ex.SetPort(context.Background(), "ollama", 11435)
		require.Error(t, err, "adopted process engine unexpectedly moved")
	}
	{
		status, err := ex.Status("ollama")
		require.NoError(t, err, "rejected process engine changed: status (%v, %v)", status, err)
		require.Equal(t, 11434, status.Port, "rejected process engine changed: status (%v, %v)", status, err)
		require.True(t, status.Running, "rejected process engine changed: status (%v, %v)", status, err)
	}
	{
		_, err := os.Stat(filepath.Join(ex.overrideDir, "ollama.json"))
		require.True(t, os.IsNotExist(err), "rejected process engine persisted an override (%v)", err)
	}
}

func TestSetPortRejectsCommandEngineWithoutStopCommand(t *testing.T) {
	ex := adoptedCommandEngineWithoutStopFixture(t, "external", 1234)
	{
		_, err := ex.SetPort(context.Background(), "external", 1235)
		require.Error(t, err, "command engine without official stop unexpectedly moved")
	}
	{
		status, err := ex.Status("external")
		require.NoError(t, err, "rejected command engine changed: status (%v, %v)", status, err)
		require.Equal(t, 1234, status.Port, "rejected command engine changed: status (%v, %v)", status, err)
		require.True(t, status.Running, "rejected command engine changed: status (%v, %v)", status, err)
	}
	{
		_, err := os.Stat(filepath.Join(ex.overrideDir, "external.json"))
		require.True(t, os.IsNotExist(err), "rejected command engine persisted an override (%v)", err)
	}
}

func TestSetPortRestartsOldPortWhenPersistenceFails(t *testing.T) {
	ex, stopped, started := adoptedCommandEngineFixture(t, "lmstudio", 1234)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("blocked"), 0o644))
	ex.overrideDir = blocked

	{
		_, err := ex.SetPort(context.Background(), "lmstudio", 1235)
		require.Error(t, err, "expected persistence failure")
	}
	require.True(t, stopped(), "persistence failure did not stop and restore the engine on 1234")
	require.True(t, started(1234), "persistence failure did not stop and restore the engine on 1234")
	require.False(t, started(1235), "persistence failure did not stop and restore the engine on 1234")
	{
		status, err := ex.Status("lmstudio")
		require.NoError(t, err, "restored status (%v, %v)", status, err)
		require.Equal(t, 1234, status.Port, "restored status (%v, %v)", status, err)
		require.True(t, status.Running, "restored status (%v, %v)", status, err)
	}
}
