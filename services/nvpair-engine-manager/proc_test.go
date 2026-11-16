// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsOurEngineImage_RecognizesManagedExecutable(t *testing.T) {
	bin := filepath.Join("/opt", "nvpair", "ollama.exe")
	test := func(name, image string, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, isOurEngineImage(image, bin))
		})
	}
	test("matching binary", bin, true)
	test("case insensitive", `/opt/nvpair/OLLAMA.EXE`, true)
	test("deleted binary", bin+` (deleted)`, true)
	test("other binary", `/opt/nvpair/other.exe`, false)
	test("empty image", ``, false)
	require.False(t, isOurEngineImage(bin, ``), "empty binPath must not match")
}

func TestIsManagedInstallPath_RejectsPathsOutsideManagedRoot(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "ollama", "bin", "ollama"+exeExt())
	outside := filepath.Join(t.TempDir(), "ollama"+exeExt())
	require.True(t, isManagedInstallPath(inside, filepath.Join(base, "ollama")), "managed child path (%v)", inside)
	require.False(t, isManagedInstallPath(outside, filepath.Join(base, "ollama")), "external path (%v)", outside)
	require.False(t, isManagedInstallPath("", filepath.Join(base, "ollama")), "empty binary path must not be managed")
}

// processGone reports whether pid is gone. A killed process whose parent has
// not reaped it yet is a zombie, which is as dead as a process gets.
func processGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// "pid (comm) S ..." — comm may contain spaces and parentheses, so cut after the last ")".
	fields := strings.Fields(string(data[strings.LastIndex(string(data), ")")+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processGone(pid)
}

// vLLM's main process can exit on a polite stop while the helper it forked
// (EngineCore, holding the GPU) is still alive, or never gets to answer one.
// Stopping an engine has to take the helpers with it: they are in the
// engine's process group and no one else will ever clean them up.
func TestManagedProcStop_KillsHelpersTheMainProcessLeftBehind(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process groups and /proc")
	}

	// The main process exits on SIGTERM; its helper ignores it and has closed the
	// engine's output pipes, so the stop sees the main process end and nothing
	// else would ever notice the helper.
	script := `trap 'exit 0' TERM; (trap '' TERM; exec sleep 300 >/dev/null 2>&1) & echo $!; while true; do sleep 0.1; done`
	childPID := make(chan int, 1)
	proc, err := startManagedProc("sh", []string{"-c", script}, nil, func(stream, line string) {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && stream == "stdout" {
			select {
			case childPID <- pid:
			default:
			}
		}
	})
	require.NoError(t, err)
	var helper int
	select {
	case helper = <-childPID:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the helper process never started")
	}
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })
	require.False(t, processGone(helper), "the helper died before the stop")

	started := time.Now()
	proc.stop(5 * time.Second)

	// The main process answered the polite signal, so this must not have waited
	// out the grace: what is under test is the cleanup after it exits.
	assert.LessOrEqual(t, time.Since(started), 2*time.Second, "the main process should have exited on the graceful signal")
	assert.True(t, waitGone(helper, 3*time.Second), "helper process %d survived the engine being stopped", helper)
}

// A process that does not answer the polite signal is force-killed after the
// grace, together with its helpers.
func TestManagedProcStop_ForceKillsAProcessThatIgnoresTheGracefulSignal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process groups and /proc")
	}

	script := `trap '' TERM; (trap '' TERM; exec sleep 300) & echo $!; while true; do sleep 1; done`
	childPID := make(chan int, 1)
	proc, err := startManagedProc("sh", []string{"-c", script}, nil, func(stream, line string) {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && stream == "stdout" {
			select {
			case childPID <- pid:
			default:
			}
		}
	})
	require.NoError(t, err)
	helper := <-childPID
	t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })

	started := time.Now()
	proc.stop(500 * time.Millisecond)

	assert.LessOrEqual(t, time.Since(started), 3*time.Second, "the stop must not wait much past the grace")
	assert.True(t, waitGone(proc.cmd.Process.Pid, time.Second), "the process survived the forced stop")
	assert.True(t, waitGone(helper, 3*time.Second), "the helper survived the forced stop")
}

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
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux virtual-environment paths")
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	bin := fakeVenv(t, map[string]string{"vllm": "#!/bin/sh\n"})

	path, found := lastValue(processEnvironment(filepath.Join(bin, "vllm"), nil), "PATH")

	assert.True(t, found, "the engine must receive PATH")
	assert.Equal(t, bin+":/usr/bin:/bin", path)
}

func TestProcessEnvironment_InheritsTheEnvironmentForAnEngineOutsideAVirtualEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux virtual-environment paths")
	}
	dir := t.TempDir()

	assert.Nil(t, processEnvironment(filepath.Join(dir, "ollama"), nil), "nil lets the process inherit its environment")
}

func TestProcessEnvironment_KeepsTheManifestVariablesAndTheirPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux virtual-environment paths")
	}
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
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux virtual-environment paths")
	}
	bin := fakeVenv(t, map[string]string{"vllm": "#!/bin/sh\n"})
	shim := filepath.Join(t.TempDir(), "vllm")
	require.NoError(t, os.Symlink(filepath.Join(bin, "vllm"), shim))

	assert.Equal(t, bin, venvBinDir(shim), "a launcher link must use its target's virtual environment")
}

// FlashInfer compiles GPU code by running ninja by name. The engine must find
// the copy installed in its Python environment even when it is absent from the
// service's PATH.
func TestStartManagedProc_FindsToolsInTheEnginesVirtualEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux executable scripts and virtual-environment paths")
	}
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
