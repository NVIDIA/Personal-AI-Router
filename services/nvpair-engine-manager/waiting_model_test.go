// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"runtime"
	"testing"

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

func TestRestoreEnabled_PreservesWaitingForModelAcrossShutdown(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	e.StopAll()
	fresh := NewExecutor(e.reg, NewReporter(nil), func(string, any) {}, e.baseDir)
	t.Cleanup(fresh.StopAll)
	require.NoError(t, fresh.RestoreEnabled(t.Context()))

	status, err := fresh.Status("fake")
	require.NoError(t, err)
	assert.True(t, status.AwaitingModel)
	assert.False(t, status.Running)
}

func TestLaunchModel_StartsTheFirstSelectedModel(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err := loadModel(t, e, "org/selected")
	require.NoError(t, err)

	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.True(t, status.Running)
	assert.False(t, status.AwaitingModel)
	tokens, err := parseLaunchText(launchTextOf(t, e))
	require.NoError(t, err)
	model, found := launchFlagValue(tokens, "--model")
	assert.True(t, found)
	assert.Equal(t, "org/selected", model)
}

func TestLaunchModel_RestoresWaitingWhenTheFirstModelFails(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	before := launchTextOf(t, e)
	_, err := loadModel(t, e, "org/fails-to-load")
	require.ErrorContains(t, err, "previous model was restored")

	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.True(t, status.AwaitingModel)
	assert.False(t, status.Running)
	assert.Equal(t, before, launchTextOf(t, e))
}

func TestConfigureLaunch_StartsASelectedModelWhenEnabled(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	before, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	text, _, err := withLaunchFlag(before.LaunchText, "--model", "org/settings-choice")
	require.NoError(t, err)
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{ServerPort: before.ServerPort, LaunchText: text}}, func() error { return nil })
	require.NoError(t, err)

	status, err := e.Status("fake")
	require.NoError(t, err)
	assert.True(t, status.Running)
	assert.False(t, status.AwaitingModel)
}

func TestEngineStatus_ReportsAwaitingModelOnTheWire(t *testing.T) {
	e := waitingModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	status, err := e.Status("fake")
	require.NoError(t, err)
	raw, err := json.Marshal(status)
	require.NoError(t, err)
	var wire struct {
		AwaitingModel bool `json:"awaiting_model"`
		Running       bool `json:"running"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	assert.True(t, wire.AwaitingModel)
	assert.False(t, wire.Running)
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

func TestManifest_RejectsWaitingWithoutModelSelection(t *testing.T) {
	m := validManifest()
	p := m.Platforms["linux/amd64"]
	p.Runtime.WaitForModel = true
	m.Platforms["linux/amd64"] = p
	require.ErrorContains(t, m.Validate(), "wait_for_model requires process mode and a launch_model action")
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
