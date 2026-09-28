// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLlamaCacheMigrationSurvivesAppReset(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	st := &engineState{installDir: filepath.Join(app, "engine-bin", "llamacpp"), modelDir: filepath.Join(root, "persistent-models", "llamacpp")}
	legacy := filepath.Join(st.installDir, "models")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "retained.gguf"), []byte("GGUFretained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLlamaCache(st); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(st.modelDir, "retained.gguf"))
	if err != nil || string(data) != "GGUFretained" {
		t.Fatalf("application reset lost migrated model: %q %v", data, err)
	}
	if err := migrateLlamaCache(st); err != nil {
		t.Fatalf("migration is not restart-safe: %v", err)
	}
}

func TestLlamaCacheMigrationPreservesBothOnCollision(t *testing.T) {
	root := t.TempDir()
	st := &engineState{installDir: filepath.Join(root, "runtime"), modelDir: filepath.Join(root, "persistent")}
	legacy := filepath.Join(st.installDir, "models")
	for _, dir := range []string{legacy, st.modelDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte(dir), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateLlamaCache(st); err == nil {
		t.Fatal("merged distinct model caches")
	}
	for _, dir := range []string{legacy, st.modelDir} {
		if data, err := os.ReadFile(filepath.Join(dir, "keep")); err != nil || string(data) != dir {
			t.Fatalf("collision changed existing content: %v", err)
		}
	}
}

func TestLlamaServingAndControlSharePersistentCache(t *testing.T) {
	root := t.TempDir()
	st := &engineState{
		installDir: filepath.Join(root, "app", "engine-bin", "llamacpp"),
		modelDir:   filepath.Join(root, "persistent", "llamacpp"),
		manifest:   &Manifest{Engine: "llamacpp"},
		plat: &Platform{Runtime: Runtime{Env: map[string]string{
			"LLAMA_CACHE": "{install_dir}/models", "HF_HUB_CACHE": "{model_dir}", "TEST_PORT": "{port}",
		}}},
	}
	env, err := childEnv(st, map[string]string{"port": "18082"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"LLAMA_CACHE", "HF_HUB_CACHE"} {
		if plainWindowsPath(env[key]) != st.modelDir {
			t.Fatalf("%s escaped persistent cache: %q", key, env[key])
		}
	}
	if env["TEST_PORT"] != "18082" {
		t.Fatal("serving-specific environment was lost")
	}
}
