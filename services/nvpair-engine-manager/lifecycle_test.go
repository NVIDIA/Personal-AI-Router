// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shutdown is bounded by the parent, so the force kill has to come well inside
// that bound however long the manifest's grace is.
func TestStopGraceFor_CapsTheManifestGraceDuringShutdown(t *testing.T) {
	test := func(name string, shuttingDown bool, stop StopSpec, want time.Duration) {
		t.Run(name, func(t *testing.T) {
			e := &Executor{}
			e.shuttingDown.Store(shuttingDown)

			assert.Equal(t, want, e.stopGraceFor(Runtime{Stop: &stop}))
		})
	}
	test("an ordinary stop gets the manifest's grace", false, StopSpec{Signal: "term", GraceS: 30}, 30*time.Second)
	test("a shutdown stop is capped", true, StopSpec{Signal: "term", GraceS: 30}, shutdownStopGraceCap)
	test("a shutdown keeps a grace below the cap", true, StopSpec{Signal: "term", GraceS: 1}, time.Second)
	test("an immediate-kill spec has no grace", true, StopSpec{Signal: "kill"}, 0)
}

// vLLM that is still reading its weights does not answer a polite stop, and its
// manifest allows a generous grace for an ordinary stop. When the whole service
// shuts down mid-start the parent allows only a few seconds before it kills
// engine-manager, so the engine has to be force-killed inside that time or it is
// orphaned for good (it runs in its own process group, so nothing else will).
func TestStopAll_ForceKillsAStartingEngineThatIgnoresTheGracefulSignal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process groups and /proc")
	}

	// A shell that ignores SIGTERM and forks a helper that does too, like vLLM's
	// API server and its EngineCore while loading.
	script := `trap '' TERM; (trap '' TERM; exec sleep 300) & echo $! > ` + "%s" + `; while true; do sleep 1; done`
	pidFile := t.TempDir() + "/helper.pid"

	m := testEngineManifest("/bin/sh")
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Args = []string{"-c", strings.Replace(script, "%s", pidFile, 1)}
	p.Runtime.Env = nil
	p.Runtime.Stop = &StopSpec{Signal: "term", GraceS: 30} // far longer than a shutdown can wait
	p.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 60}
	p.Runtime.Health = nil
	m.Platforms[key] = p
	m.Actions = nil
	ex := newTestExecutor(t, m)

	started := make(chan error, 1)
	go func() { started <- ex.Start(t.Context(), "fake") }()

	var helper int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				helper = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotZero(t, helper, "the engine never started")
	st, _ := ex.state("fake")
	for i := 0; i < 100; i++ {
		st.mu.Lock()
		proc := st.proc
		st.mu.Unlock()
		if proc != nil {
			t.Cleanup(func() { killGroupRemnants(proc.cmd.Process.Pid) })
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	began := time.Now()
	ex.StopAll()

	assert.LessOrEqual(t, time.Since(began), shutdownStopGraceCap+3*time.Second, "the parent only waits a few seconds for shutdown")
	assert.True(t, waitGone(helper, 2*time.Second), "the engine's helper process %d was left running", helper)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		assert.Fail(t, "the cancelled start never returned")
	}
}
