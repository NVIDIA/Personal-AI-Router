// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, writeJSONAtomic(path, original))
	ex := newBundledExecutor(t, dir)
	for _, port := range []int{21003, 11434} {
		{
			_, err := ex.SetPort(context.Background(), "ollama", port)
			require.NoError(t, err)
		}
		reg := loadWithOverrides(t, dir)
		manifest, _ := reg.Get("ollama")
		platform, _ := manifest.HostPlatform()
		require.True(t, platform.Runtime.Port == port, "restart restored (%v)", port)
		require.Equal(t, "Custom Ollama", manifest.DisplayName, "port change lost unrelated overrides or inherited defaults")
		require.Equal(t, []string{"serve", "--custom-option"}, platform.Runtime.Args, "port change lost unrelated overrides or inherited defaults")
		require.Equal(t, "kept", platform.Runtime.Env["CUSTOM_SETTING"], "port change lost unrelated overrides or inherited defaults")
		require.Equal(t, "kept", platform.Runtime.Env["HOST_SETTING"], "port change lost unrelated overrides or inherited defaults")
		require.Equal(t, "{host}:{port}", platform.Runtime.Env["OLLAMA_HOST"], "port change lost unrelated overrides or inherited defaults")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(data, &saved))
	{
		_, pinned := saved["runtime"].(map[string]any)["port"]
		require.False(t, pinned, "reset retained a shared port override")
	}
	hostRuntime := saved["platforms"].(map[string]any)[host].(map[string]any)["runtime"].(map[string]any)
	{
		_, pinned := hostRuntime["port"]
		require.False(t, pinned, "reset retained a host port override")
	}
	var exact map[string]json.RawMessage
	{
		err := json.Unmarshal(data, &exact)
		require.NoError(t, err, "unrelated numeric setting lost precision")
		require.Equal(t, "9007199254740993", string(exact["custom_counter"]), "unrelated numeric setting lost precision (%v)", err)
	}
}

func TestPersistPortOverridesBundledPlatformPort(t *testing.T) {
	host := runtime.GOOS + "/" + runtime.GOARCH
	raw, err := json.Marshal(map[string]any{
		"engine": "platform-engine", "display_name": "Platform engine", "manifest_version": 1,
		"runtime":   map[string]any{"bin": "fake", "port": 22000},
		"platforms": map[string]any{host: map[string]any{"runtime": map[string]any{"port": 22001}}},
	})
	require.NoError(t, err)
	reg := NewRegistry()
	{
		_, err := reg.addManifest("test", raw)
		require.NoError(t, err)
	}
	reg.bundledRaw["platform-engine"] = raw
	ex := NewExecutor(reg, NewReporter(nil), nil, t.TempDir())
	ex.overrideDir = t.TempDir()
	for _, port := range []int{22002, 22001} {
		require.NoError(t, ex.persistPort("platform-engine", port))
		reloaded := NewRegistry()
		{
			_, err := reloaded.addManifest("test", raw)
			require.NoError(t, err)
		}
		reloaded.bundledRaw["platform-engine"] = raw
		require.NoError(t, reloaded.LoadOverrideDir(ex.overrideDir))
		{
			got := hostPort(t, reloaded, "platform-engine")
			require.True(t, got == port, "host default shadowed saved port: (%v, %v)", got, port)
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
			require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
			{
				_, err := ex.SetPort(context.Background(), "ollama", 23001)
				require.Error(t, err, "malformed override was overwritten")
			}
			got, err := os.ReadFile(path)
			require.NoError(t, err, "invalid override changed (%v, %v)", got, err)
			require.True(t, string(got) == data, "invalid override changed (%v, %v)", got, err)
			{
				got, _ := ex.Status("ollama")
				require.Equal(t, 11434, got.Port, "failed persistence changed runtime port (%v)", got)
			}
		})
	}
}

func TestWriteJSONAtomicReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	for _, port := range []int{24001, 24002} {
		require.NoError(t, writeJSONAtomic(path, map[string]int{"port": port}))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var got map[string]int
		{
			err := json.Unmarshal(data, &got)
			require.NoError(t, err, "replacement not readable (%v, %v)", data, err)
			require.True(t, got["port"] == port, "replacement not readable (%v, %v)", data, err)
		}
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "atomic writer left temporary files (%v, %v)", entries, err)
	require.Len(t, entries, 1, "atomic writer left temporary files (%v, %v)", entries, err)
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
