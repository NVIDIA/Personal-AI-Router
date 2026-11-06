// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVenv lays out a Python virtual environment: a pyvenv.cfg beside a bin
// directory holding the given executable scripts. It returns the bin directory.
func fakeVenv(t *testing.T, scripts map[string]string) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "venv", "pyvenv.cfg"), []byte("home = /usr/bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// lastValue is the value the process will see for key, which is the last entry
// naming it.
func lastValue(environment []string, key string) (string, bool) {
	value, found := "", false
	for _, entry := range environment {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value, found = v, true
		}
	}
	return value, found
}

func TestProcessEnvironment_PutsTheVirtualEnvironmentFirstOnPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	bin := fakeVenv(t, map[string]string{"vllm": "#!/bin/sh\n"})

	got, _ := lastValue(processEnvironment(filepath.Join(bin, "vllm"), nil), "PATH")

	if want := bin + ":/usr/bin:/bin"; got != want {
		t.Fatalf("PATH = %q, want the venv's bin directory ahead of the existing PATH (%q)", got, want)
	}
}

func TestProcessEnvironment_InheritsTheEnvironmentForAnEngineOutsideAVirtualEnvironment(t *testing.T) {
	dir := t.TempDir()
	if got := processEnvironment(filepath.Join(dir, "ollama"), nil); got != nil {
		t.Fatalf("environment = %v, want nil so the process inherits it unchanged", got)
	}
}

func TestProcessEnvironment_KeepsTheManifestVariablesAndTheirPath(t *testing.T) {
	bin := fakeVenv(t, map[string]string{"vllm": "#!/bin/sh\n"})

	environment := processEnvironment(filepath.Join(bin, "vllm"), map[string]string{"HF_HOME": "/models", "PATH": "/custom"})

	if got, _ := lastValue(environment, "HF_HOME"); got != "/models" {
		t.Fatalf("HF_HOME = %q, want /models", got)
	}
	if got, _ := lastValue(environment, "PATH"); got != bin+":/custom" {
		t.Fatalf("PATH = %q, want the venv's bin directory ahead of the PATH the manifest set", got)
	}
}

func TestProcessEnvironment_FollowsALinkIntoTheVirtualEnvironment(t *testing.T) {
	bin := fakeVenv(t, map[string]string{"vllm": "#!/bin/sh\n"})
	shim := filepath.Join(t.TempDir(), "vllm")
	if err := os.Symlink(filepath.Join(bin, "vllm"), shim); err != nil {
		t.Fatal(err)
	}

	if got := venvBinDir(shim); got != bin {
		t.Fatalf("venvBinDir = %q, want %q: a link such as ~/.local/bin/vllm belongs to the venv it points into", got, bin)
	}
}

// vLLM's FlashInfer compiles kernels by running ninja by name, and ninja is in
// the virtual environment's bin directory. Before the engine was started with
// that directory on PATH, loading a model that needs a kernel stopped with
// "No such file or directory: 'ninja'".
func TestStartManagedProc_FindsToolsInTheEnginesVirtualEnvironment(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	bin := fakeVenv(t, map[string]string{
		"engine": "#!/bin/sh\nexec ninja\n",
		"ninja":  "#!/bin/sh\necho ninja-was-found\n",
	})

	var mu sync.Mutex
	var lines []string
	proc, err := startManagedProc(filepath.Join(bin, "engine"), nil, nil, func(_, line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the engine did not exit")
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "ninja-was-found") {
		t.Fatalf("engine output = %q, want it to have run the venv's ninja", got)
	}
}
