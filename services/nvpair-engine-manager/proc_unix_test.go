// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// (The script ends in a command after the sleep so the shell is not replaced by it.)
// A Python-packaged engine (vLLM) runs as "interpreter script args", so its
// executable never matches the engine path. The script in the command line is
// what proves a leftover process is the engine this service manages.
func TestProcRunsScript_RecognisesAnInterpreterRunningTheEngineScript(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/engine"
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\ntrue\n"), 0o755))
	proc, err := startManagedProc("/bin/sh", []string{script, "serve", "--port", "1"}, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { proc.stop(0) })
	pid := proc.cmd.Process.Pid
	// /proc/<pid>/cmdline reads empty for a moment while a new process finishes
	// exec'ing. A leftover engine has been running far longer than that.
	for i := 0; i < 200; i++ {
		if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recognised := func(name string, pid int, path string) {
		t.Run(name, func(t *testing.T) {
			assert.True(t, procRunsScript(pid, path))
		})
	}
	notRecognised := func(name string, pid int, path string) {
		t.Run(name, func(t *testing.T) {
			assert.False(t, procRunsScript(pid, path))
		})
	}
	recognised("the engine script", pid, script)
	recognised("the engine script through a dot segment", pid, dir+"/./engine")
	notRecognised("an unrelated script", pid, dir+"/other-engine")
	// A bare word is not an engine path, even though it is one of the arguments.
	notRecognised("a relative path among the arguments", pid, "serve")
	notRecognised("pid 0", 0, script)
	notRecognised("an empty path", pid, "")
	notRecognised("a pid that does not exist", 999999999, script)
}
