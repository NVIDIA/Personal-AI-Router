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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVenv lays out a Python virtual environment: a pyvenv.cfg beside a bin
// directory holding the given executable scripts. It returns the bin directory.
func fakeVenv(t *testing.T, scripts map[string]string) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "venv", "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "venv", "pyvenv.cfg"), []byte("home = /usr/bin\n"), 0o644))
	for name, body := range scripts {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755))
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

	path, found := lastValue(processEnvironment(filepath.Join(bin, "vllm"), nil), "PATH")

	assert.True(t, found, "the engine must receive PATH")
	assert.Equal(t, bin+":/usr/bin:/bin", path)
}

func TestProcessEnvironment_InheritsTheEnvironmentForAnEngineOutsideAVirtualEnvironment(t *testing.T) {
	dir := t.TempDir()

	assert.Nil(t, processEnvironment(filepath.Join(dir, "ollama"), nil), "nil lets the process inherit its environment")
}

func TestProcessEnvironment_KeepsTheManifestVariablesAndTheirPath(t *testing.T) {
	bin := fakeVenv(t, map[string]string{"vllm": "#!/bin/sh\n"})

	environment := processEnvironment(filepath.Join(bin, "vllm"), map[string]string{"HF_HOME": "/models", "PATH": "/custom"})

	test := func(name, key, want string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			value, found := lastValue(environment, key)

			assert.True(t, found, "the engine must receive %s", key)
			assert.Equal(t, want, value)
		})
	}
	test("preserves the model cache directory", "HF_HOME", "/models")
	test("prepends the virtual environment to the manifest PATH", "PATH", bin+":/custom")
}

func TestVenvBinDir_FollowsALinkIntoTheVirtualEnvironment(t *testing.T) {
	bin := fakeVenv(t, map[string]string{"vllm": "#!/bin/sh\n"})
	shim := filepath.Join(t.TempDir(), "vllm")
	require.NoError(t, os.Symlink(filepath.Join(bin, "vllm"), shim))

	assert.Equal(t, bin, venvBinDir(shim), "a launcher link must use its target's virtual environment")
}

// FlashInfer compiles GPU code by running ninja by name. The engine must find
// the copy installed in its Python environment even when it is absent from the
// service's PATH.
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
	require.NoError(t, err)
	t.Cleanup(func() { proc.stop(0) })

	select {
	case <-proc.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the engine did not exit")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"ninja-was-found"}, lines)
}
