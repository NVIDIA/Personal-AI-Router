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
