// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	settings "nvpair-shared/enginesettings"
)

func waitingModelExecutor(t *testing.T) *Executor {
	t.Helper()
	e := settingsExecutor(t, false)
	m, ok := e.reg.Get("fake")
	require.True(t, ok)
	m.Actions["load_model"] = Action{LaunchModel: &ActionLaunchModel{Flag: "--model"}}
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.WaitForModel = true
	m.Platforms[key] = p
	st, err := e.state("fake")
	require.NoError(t, err)
	st.plat.Runtime.WaitForModel = true
	require.NoError(t, m.Validate())
	return e
}

func TestStart_AwaitsModelWithoutLaunchingAProcess(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))

	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.True(t, status.Installed)
	assert.True(t, status.AwaitingModel)
	assert.False(t, status.Running)
	assert.False(t, status.Starting)
	assert.False(t, status.Healthy)
	enabled, known, err := e.desired.get("fake")
	require.NoError(t, err)
	assert.True(t, enabled)
	assert.True(t, known)
}

func TestStop_DisablesAnEngineAwaitingModel(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	require.NoError(t, e.Stop("fake"))

	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.False(t, status.AwaitingModel)
	assert.False(t, status.Running)
	enabled, known, err := e.desired.get("fake")
	require.NoError(t, err)
	assert.True(t, known)
	assert.False(t, enabled)
}

func TestStop_CancelsFirstModelLoadAndLeavesEngineDisabled(t *testing.T) {
	e := waitingModelExecutor(t)
	before, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{ServerPort: before.ServerPort, LaunchText: "FAKE_START_DELAY=8s " + before.LaunchText}}, func() error { return nil })
	require.NoError(t, err)
	require.NoError(t, e.Start(t.Context(), "fake"))
	result := make(chan error, 1)
	go func() {
		_, err := e.Action(t.Context(), "fake", "load_model", json.RawMessage(`{"model":"org/first"}`))
		result <- err
	}()
	waitForStartInFlight(t, e)
	require.NoError(t, e.Stop("fake"))
	require.NoError(t, <-result)
	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.False(t, status.Running)
	assert.False(t, status.AwaitingModel)
	enabled, known, err := e.desired.get("fake")
	require.NoError(t, err)
	assert.True(t, known)
	assert.False(t, enabled)
}

func TestStop_ReportsStoppedAfterARepeatedStop(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	require.NoError(t, e.Stop("fake"))
	var states []EngineStatus
	e.emit = func(method string, params any) {
		if method != "engine:state-changed" {
			return
		}
		status, ok := params.(EngineStatus)
		require.True(t, ok, "state notification must carry EngineStatus")
		states = append(states, status)
	}
	require.NoError(t, e.Stop("fake"))
	require.NotEmpty(t, states, "an explicit Stop must resolve a client's stopping state even when already off")
	assert.False(t, states[len(states)-1].Running)
	assert.False(t, states[len(states)-1].Starting)
	assert.False(t, states[len(states)-1].AwaitingModel)
}

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
