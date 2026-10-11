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
	"testing"
)

func TestSetPortPreservesOtherOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ollama.json")
	host := runtime.GOOS + "/" + runtime.GOARCH
	original := map[string]any{
		"engine":         "ollama",
		"display_name":   "Custom Ollama",
		"custom_counter": json.Number("9007199254740993"),
		"runtime": map[string]any{
			"port": 21001,
			"args": []string{"serve", "--custom-option"},
			"env":  map[string]string{"CUSTOM_SETTING": "kept"},
		},
		"platforms": map[string]any{
			host: map[string]any{"runtime": map[string]any{
				"port": 21002, "env": map[string]string{"HOST_SETTING": "kept"},
			}},
		},
	}
	if err := writeJSONAtomic(path, original); err != nil {
		t.Fatal(err)
	}
	ex := newBundledExecutor(t, dir)
	for _, port := range []int{21003, 11434} {
		if _, err := ex.SetPort(context.Background(), "ollama", port); err != nil {
			t.Fatal(err)
		}
		reg := loadWithOverrides(t, dir)
		manifest, _ := reg.Get("ollama")
		platform, _ := manifest.HostPlatform()
		if platform.Runtime.Port != port {
			t.Fatalf("restart restored %d instead of %d", platform.Runtime.Port, port)
		}
		if manifest.DisplayName != "Custom Ollama" ||
			!reflect.DeepEqual(platform.Runtime.Args, []string{"serve", "--custom-option"}) ||
			platform.Runtime.Env["CUSTOM_SETTING"] != "kept" ||
			platform.Runtime.Env["HOST_SETTING"] != "kept" ||
			platform.Runtime.Env["OLLAMA_HOST"] != "{host}:{port}" {
			t.Fatalf("port change lost unrelated overrides or inherited defaults: %+v", platform.Runtime)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, pinned := saved["runtime"].(map[string]any)["port"]; pinned {
		t.Fatal("reset retained a shared port override")
	}
	hostRuntime := saved["platforms"].(map[string]any)[host].(map[string]any)["runtime"].(map[string]any)
	if _, pinned := hostRuntime["port"]; pinned {
		t.Fatal("reset retained a host port override")
	}
	var exact map[string]json.RawMessage
	if err := json.Unmarshal(data, &exact); err != nil || string(exact["custom_counter"]) != "9007199254740993" {
		t.Fatalf("unrelated numeric setting lost precision: %s, %v", exact["custom_counter"], err)
	}
}

func TestPersistPortOverridesBundledPlatformPort(t *testing.T) {
	host := runtime.GOOS + "/" + runtime.GOARCH
	raw, err := json.Marshal(map[string]any{
		"engine": "platform-engine", "display_name": "Platform engine", "manifest_version": 1,
		"runtime":   map[string]any{"bin": "fake", "port": 22000},
		"platforms": map[string]any{host: map[string]any{"runtime": map[string]any{"port": 22001}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	if _, err := reg.addManifest("test", raw); err != nil {
		t.Fatal(err)
	}
	reg.bundledRaw["platform-engine"] = raw
	ex := NewExecutor(reg, NewReporter(nil), nil, t.TempDir())
	ex.overrideDir = t.TempDir()
	for _, port := range []int{22002, 22001} {
		if err := ex.persistPort("platform-engine", port); err != nil {
			t.Fatal(err)
		}
		reloaded := NewRegistry()
		if _, err := reloaded.addManifest("test", raw); err != nil {
			t.Fatal(err)
		}
		reloaded.bundledRaw["platform-engine"] = raw
		if err := reloaded.LoadOverrideDir(ex.overrideDir); err != nil {
			t.Fatal(err)
		}
		if got := hostPort(t, reloaded, "platform-engine"); got != port {
			t.Fatalf("host default shadowed saved port: got %d, want %d", got, port)
		}
	}
}

func TestPersistPortRefusesMalformedOverrideWithoutClobbering(t *testing.T) {
	for _, data := range []string{
		`{`, `null`, `[]`, `{}`, `{"runtime":{}}`, `{"engine":"another-engine"}`,
		`{"engine":"ollama","runtime":null}`,
		`{"engine":"ollama","platforms":[]}`,
	} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ollama.json")
			ex := newBundledExecutor(t, dir)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ex.SetPort(context.Background(), "ollama", 23001); err == nil {
				t.Fatal("malformed override was overwritten")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("invalid override changed: %q, %v", got, err)
			}
			if got, _ := ex.Status("ollama"); got.Port != 11434 {
				t.Fatalf("failed persistence changed runtime port: %+v", got)
			}
		})
	}
}

func TestWriteJSONAtomicReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	for _, port := range []int{24001, 24002} {
		if err := writeJSONAtomic(path, map[string]int{"port": port}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]int
		if err := json.Unmarshal(data, &got); err != nil || got["port"] != port {
			t.Fatalf("replacement not readable: %q, %v", data, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("atomic writer left temporary files: %v, %v", entries, err)
	}
}

// bundledHostPlatform returns an engine's bundled platform for this host, or
// skips the test where the engine has none.
func bundledHostPlatform(t *testing.T, engine string) Platform {
	t.Helper()
	bundled, ok := buildBundledRegistry().Get(engine)
	if !ok {
		t.Fatalf("no bundled %s manifest", engine)
	}
	platform, ok := bundled.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Skipf("%s has no manifest for this host", engine)
	}
	return platform
}

// TestUninstallerTakesOnlyLocationsFromAnOverride checks the uninstaller looks
// for an engine on the port the user moved it to and keeps the model store they
// moved it to, and takes nothing else from the override. The uninstaller runs
// elevated on Windows, and the override is a file any process running as the
// user can write.
func TestUninstallerTakesOnlyLocationsFromAnOverride(t *testing.T) {
	host := runtime.GOOS + "/" + runtime.GOARCH
	want := bundledHostPlatform(t, "ollama")
	dir := t.TempDir()
	override := map[string]any{
		"engine":  "ollama",
		"runtime": map[string]any{"port": 21001},
		"platforms": map[string]any{
			host: map[string]any{
				"models_dir": "~/somewhere-else",
				"uninstall":  map[string]any{"run": []string{"rm", "-rf", "/"}},
				"runtime":    map[string]any{"port": 21002},
			},
		},
	}
	if err := writeJSONAtomic(filepath.Join(dir, "ollama.json"), override); err != nil {
		t.Fatal(err)
	}

	reg := buildBundledRegistry()
	reg.applyLocationOverrides(dir)
	got, ok := reg.Get("ollama")
	if !ok {
		t.Fatal("ollama vanished from the registry")
	}
	platform := got.Platforms[host]

	if platform.Runtime.Port != 21002 {
		t.Errorf("port %d, want the host platform's override, 21002", platform.Runtime.Port)
	}
	if platform.ModelsDir != "~/somewhere-else" {
		t.Errorf("models_dir %q, want the override's store", platform.ModelsDir)
	}
	if !reflect.DeepEqual(platform.Uninstall, want.Uninstall) {
		t.Errorf("took the uninstall commands from the override: %+v", platform.Uninstall)
	}
}

// TestUninstallerIgnoresAnOverrideStoreHoldingWhatItRemoves checks an override
// whose model store would contain a removal target is ignored whole, as startup
// ignores it, rather than leaving the uninstaller with a store it must refuse
// to remove around.
func TestUninstallerIgnoresAnOverrideStoreHoldingWhatItRemoves(t *testing.T) {
	host := runtime.GOOS + "/" + runtime.GOARCH
	want := bundledHostPlatform(t, "lmstudio")
	dir := t.TempDir()
	override := map[string]any{
		"engine":     "lmstudio",
		"models_dir": "~",
		"runtime":    map[string]any{"port": 21003},
	}
	if err := writeJSONAtomic(filepath.Join(dir, "lmstudio.json"), override); err != nil {
		t.Fatal(err)
	}

	reg := buildBundledRegistry()
	reg.applyLocationOverrides(dir)
	got, ok := reg.Get("lmstudio")
	if !ok {
		t.Fatal("lmstudio vanished from the registry")
	}
	platform := got.Platforms[host]

	if platform.ModelsDir != want.ModelsDir || platform.Runtime.Port != want.Runtime.Port {
		t.Errorf("applied an invalid override: models_dir %q, port %d; want the bundled %q, %d",
			platform.ModelsDir, platform.Runtime.Port, want.ModelsDir, want.Runtime.Port)
	}
}
