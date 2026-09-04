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
)

// hostModel returns the effective host-platform runtime.model for an engine.
func hostModel(t *testing.T, reg *Registry, engine string) string {
	t.Helper()
	m, ok := reg.Get(engine)
	if !ok {
		t.Fatalf("engine %q not in registry", engine)
	}
	p, ok := m.HostPlatform()
	if !ok {
		t.Fatalf("engine %q has no host platform block", engine)
	}
	return p.Runtime.Model
}

// vllmManifest is the bundled vLLM manifest re-keyed onto the running host so
// the Linux-only engine's lifecycle can be exercised on any developer machine.
// Only the platform key changes; every field under test is the bundled one.
func vllmManifest(t *testing.T) *Manifest {
	t.Helper()
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("LoadFS bundled: %v", err)
	}
	m, ok := reg.Get("vllm")
	if !ok {
		t.Fatal("vllm manifest not bundled")
	}
	linux, ok := m.PlatformFor("linux", "amd64")
	if !ok {
		t.Fatal("vllm manifest has no linux/amd64 block")
	}
	clone := *m
	clone.Platforms = map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: *linux}
	return &clone
}

// TestVLLMManifestLoadsAndValidates proves the bundled vLLM manifest passes
// schema validation, including the {model} placeholder in its launch args, and
// that it is offered on Linux only.
func TestVLLMManifestLoadsAndValidates(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("LoadFS bundled: %v", err)
	}
	m, ok := reg.Get("vllm")
	if !ok {
		t.Fatal("vllm manifest not bundled")
	}
	if m.DisplayName != "vLLM" {
		t.Errorf("display_name = %q, want %q", m.DisplayName, "vLLM")
	}
	want := map[string]bool{"linux/amd64": true, "linux/arm64": true}
	for key := range m.Platforms {
		if !want[key] {
			t.Errorf("unexpected platform %q: vLLM ships on Linux only", key)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("missing platform %q", key)
	}

	p, ok := m.PlatformFor("linux", "amd64")
	if !ok {
		t.Fatal("no linux/amd64 block")
	}
	if p.Runtime.modeOrDefault() != "process" {
		t.Errorf("runtime.mode = %q, want process (vllm serve is a foreground server, not a bring-up command)", p.Runtime.modeOrDefault())
	}
	if p.Runtime.Port != 8000 {
		t.Errorf("runtime.port = %d, want 8000", p.Runtime.Port)
	}
	if !p.Runtime.referencesModel() {
		t.Error("vLLM's launch template must substitute {model}")
	}
	if p.Runtime.Ready == nil || p.Runtime.Ready.TimeoutS != 1800 {
		t.Errorf("runtime.ready timeout = %+v, want 1800s (first start downloads weights)", p.Runtime.Ready)
	}
	if p.Runtime.Health == nil || !strings.HasSuffix(p.Runtime.Health.HTTP, "/health") {
		t.Errorf("runtime.health = %+v, want the /health endpoint", p.Runtime.Health)
	}
	for _, name := range []string{"list_models", "loaded_models", "chat"} {
		if _, ok := m.Actions[name]; !ok {
			t.Errorf("missing action %q", name)
		}
	}
	for _, name := range []string{"pull_model", "load_model", "unload_model", "delete_model"} {
		if _, ok := m.Actions[name]; ok {
			t.Errorf("action %q must not be declared: vLLM has no model management surface", name)
		}
	}
}

// TestVLLMHasNoHostPlatformOffHostLinux proves an engine with no block for the
// running platform is simply not offered there: it still lists (so the UI can
// say "unavailable on this system") but every lifecycle op refuses.
func TestVLLMHasNoHostPlatformOffHostLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("vLLM has a platform block on Linux; this covers the other hosts")
	}
	ex := newBundledExecutor(t, t.TempDir())
	var found *EngineStatus
	for _, st := range ex.GetInstalled() {
		if st.Engine == "vllm" {
			s := st
			found = &s
		}
	}
	if found == nil {
		t.Fatal("vllm missing from engine:get-installed")
	}
	if found.Installed || found.Running {
		t.Errorf("vllm reported present on %s: %+v", runtime.GOOS, *found)
	}
	if _, err := ex.state("vllm"); err == nil {
		t.Error("expected vllm to have no host platform block on this OS")
	}
	if err := ex.Start(context.Background(), "vllm"); err == nil {
		t.Error("expected Start to refuse an engine with no host platform block")
	}
}

// TestStartWithoutConfiguredModelIsActionable proves an engine whose launch
// template needs {model} refuses to spawn until a model is chosen, with a
// message that tells the user what to do rather than leaking a placeholder.
func TestStartWithoutConfiguredModelIsActionable(t *testing.T) {
	m := vllmManifest(t)
	// Move off 8000 so an unrelated listener on this host cannot pre-empt the
	// model check with a port-occupied error.
	key := runtime.GOOS + "/" + runtime.GOARCH
	plat := m.Platforms[key]
	free, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	plat.Runtime.Port = free
	// Point detect at a real file so the refusal is about the missing model,
	// not about the engine being uninstalled.
	plat.Detect = []string{fakeEngineBin}
	m.Platforms[key] = plat

	ex := newTestExecutor(t, m)
	err = ex.Start(context.Background(), "vllm")
	if err == nil {
		t.Fatal("expected Start to refuse with no model configured")
	}
	for _, want := range []string{"vLLM", "one model per process", "Engine settings"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "{model}") {
		t.Errorf("error leaks the raw placeholder: %q", err.Error())
	}
}

// TestSetModelPersistsAsManifestOverride proves engine:set-model survives a
// "restart" through the same override-dir merge engine:set-port uses, and that
// clearing it removes the override again. It drives ollama because the writer is
// engine-agnostic and ollama has a platform block on every host; vLLM's own
// Linux-only manifest is covered by the manifest test above.
func TestSetModelPersistsAsManifestOverride(t *testing.T) {
	dir := t.TempDir()
	ex := newBundledExecutor(t, dir)
	if err := ex.persistModel("ollama", "Qwen/Qwen3-8B"); err != nil {
		t.Fatalf("persistModel: %v", err)
	}
	if got := hostModel(t, loadWithOverrides(t, dir), "ollama"); got != "Qwen/Qwen3-8B" {
		t.Errorf("model after reload = %q, want Qwen/Qwen3-8B", got)
	}

	if err := ex.persistModel("ollama", ""); err != nil {
		t.Fatalf("persistModel clear: %v", err)
	}
	if got := hostModel(t, loadWithOverrides(t, dir), "ollama"); got != "" {
		t.Errorf("model after clear = %q, want empty", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "ollama.json")); !os.IsNotExist(err) {
		t.Errorf("override file should be gone once no override remains: %v", err)
	}
}

// TestSetModelAndSetPortDoNotClobberEachOther is the regression guard for the
// two persistent setters sharing one override file: each writes its own
// runtime field and must leave the other's alone, in either order.
func TestSetModelAndSetPortDoNotClobberEachOther(t *testing.T) {
	dir := t.TempDir()
	ex := newBundledExecutor(t, dir)
	bundledPort := hostPort(t, ex.reg, "ollama")

	if err := ex.persistModel("ollama", "Qwen/Qwen3-8B"); err != nil {
		t.Fatalf("persistModel: %v", err)
	}
	if err := ex.persistPort("ollama", 9001); err != nil {
		t.Fatalf("persistPort: %v", err)
	}
	reg := loadWithOverrides(t, dir)
	if got := hostModel(t, reg, "ollama"); got != "Qwen/Qwen3-8B" {
		t.Errorf("model lost when the port was set: got %q", got)
	}
	if got := hostPort(t, reg, "ollama"); got != 9001 {
		t.Errorf("port = %d, want 9001", got)
	}

	// Back to the bundled default port: the port override drops, the model stays.
	if err := ex.persistPort("ollama", bundledPort); err != nil {
		t.Fatalf("persistPort default: %v", err)
	}
	reg = loadWithOverrides(t, dir)
	if got := hostModel(t, reg, "ollama"); got != "Qwen/Qwen3-8B" {
		t.Errorf("model lost when the port returned to default: got %q", got)
	}
	if got := hostPort(t, reg, "ollama"); got != bundledPort {
		t.Errorf("port = %d, want the bundled %d", got, bundledPort)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "ollama.json"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc struct {
		Runtime map[string]any `json:"runtime"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Runtime["port"]; ok {
		t.Errorf("default port should not stay pinned in the override: %s", raw)
	}
}

// TestSetModelAppliesToTheLaunchArgs proves the configured model reaches the
// spawned process's argv, and that runtime.extra_args is appended after it.
func TestSetModelAppliesToTheLaunchArgs(t *testing.T) {
	m := vllmManifest(t)
	key := runtime.GOOS + "/" + runtime.GOARCH
	plat := m.Platforms[key]
	// Drive the fake engine instead of a real vLLM: it takes FAKE_ADDR, so the
	// vLLM argv is carried purely to prove substitution, and extra_args adds a
	// flag the fake ignores.
	plat.Detect = []string{fakeEngineBin}
	plat.Runtime.Bin = fakeEngineBin
	plat.Runtime.Model = "Qwen/Qwen3-8B"
	plat.Runtime.Args = []string{"echo", "{model}", "--port", "{port}"}
	plat.Runtime.ExtraArgs = []string{"--gpu-memory-utilization", "0.9"}
	plat.Runtime.Ready = nil
	plat.Runtime.Health = nil
	m.Platforms[key] = plat

	ex := newTestExecutor(t, m)
	st, err := ex.state("vllm")
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.Start(context.Background(), "vllm"); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = ex.Stop("vllm") })

	// `echo` prints its args and exits, so the captured stdout is the resolved argv.
	captured := func() string {
		var line string
		for _, l := range st.logs.snapshot() {
			line += l.Text + "\n"
		}
		return line
	}
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(captured(), "Qwen/Qwen3-8B") })
	line := captured()
	if !strings.Contains(line, "--gpu-memory-utilization 0.9") {
		t.Errorf("extra_args not appended to the launch argv: %q", line)
	}
}
