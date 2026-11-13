// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	settings "nvpair-shared/enginesettings"
	"testing"
	"time"
)

func requestedModelExecutor(t *testing.T) *Executor {
	t.Helper()
	e := waitingModelExecutor(t)
	cache := t.TempDir()
	for _, model := range []string{"org/first", "org/second", "org/fails-to-load"} {
		writeHFRepo(t, cache, model, llmFiles(map[string]string{"model.safetensors": "weights"}))
	}
	m, ok := e.reg.Get("fake")
	require.True(t, ok)
	m.AdvertisedModels = advertisedModelsAvailable
	m.Actions["list_models"] = Action{HFCache: &ActionHFCache{Dir: cache}, Result: &ActionResult{Array: "data", Field: "id"}}
	m.Actions["loaded_models"] = Action{HTTP: &ActionHTTP{Method: http.MethodGet, Path: "/v1/models"}, Result: &ActionResult{Array: "data", Field: "id", InventoryField: "root"}}
	require.NoError(t, m.Validate())
	return e
}

func TestPrepareRequestedModel_LoadsACachedModelWhileAwaitingSelection(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	port, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
	require.NoError(t, err)
	status := mustStatus(t, e)
	assert.Equal(t, status.Port, port)
	assert.True(t, status.Running)
	assert.False(t, status.AwaitingModel)
	models := e.ModelsResult(t.Context())
	assert.Equal(t, []string{"org/first"}, models.LoadedByEngine["fake"])
}

func TestPrepareRequestedModel_SwitchesToTheRequestedCachedModel(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
	require.NoError(t, err)
	_, err = e.prepareRequestedModel(t.Context(), "fake", "org/second")
	require.NoError(t, err)
	assert.Equal(t, []string{"org/second"}, e.ModelsResult(t.Context()).LoadedByEngine["fake"])
}

func TestPrepareRequestedModel_DoesNotRestartTheAlreadyLoadedModel(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
	require.NoError(t, err)
	before := startGeneration(t, e)
	_, err = e.prepareRequestedModel(t.Context(), "fake", "org/first")
	require.NoError(t, err)
	assert.Equal(t, before, startGeneration(t, e))
}

func TestPrepareRequestedModel_RefusesAnExplicitlyDisabledEngine(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	require.NoError(t, e.Stop("fake"))
	_, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
	require.ErrorContains(t, err, "disabled")
	assert.False(t, mustStatus(t, e).Running)
	assert.False(t, mustStatus(t, e).AwaitingModel)
}

func TestPrepareRequestedModel_RefusesAnUncachedModelWithoutSelectingIt(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	before := launchTextOf(t, e)
	_, err := e.prepareRequestedModel(t.Context(), "fake", "org/not-downloaded")
	require.ErrorContains(t, err, "not downloaded")
	assert.Equal(t, before, launchTextOf(t, e))
	assert.True(t, mustStatus(t, e).AwaitingModel)
}

func TestPrepareRequestedModel_RestoresThePreviousModelAfterAFailedSwitch(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
	require.NoError(t, err)
	_, err = e.prepareRequestedModel(t.Context(), "fake", "org/fails-to-load")
	require.ErrorContains(t, err, "previous model was restored")
	assert.Equal(t, []string{"org/first"}, e.ModelsResult(t.Context()).LoadedByEngine["fake"])
}

func TestPrepareRequestedModel_CancellationRestoresAwaitingSelection(t *testing.T) {
	e := requestedModelExecutor(t)
	before, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{ServerPort: before.ServerPort, LaunchText: "FAKE_START_DELAY=8s " + before.LaunchText}}, func() error { return nil })
	require.NoError(t, err)
	require.NoError(t, e.Start(t.Context(), "fake"))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err = e.prepareRequestedModel(ctx, "fake", "org/first")
	require.Error(t, err)
	assert.True(t, mustStatus(t, e).AwaitingModel)
	assert.False(t, mustStatus(t, e).Running)
}

func TestModelsResult_AdvertisesCachedModelsOnlyWhileEnabled(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	enabled := e.ModelsResult(t.Context())
	assert.ElementsMatch(t, []string{"org/first", "org/second", "org/fails-to-load"}, enabled.ByEngine["fake"])
	assert.Empty(t, enabled.LoadedByEngine["fake"])
	require.NoError(t, e.Stop("fake"))
	stopped := e.ModelsResult(t.Context())
	assert.Empty(t, stopped.ByEngine["fake"])
	assert.ElementsMatch(t, enabled.RequestableByEngine["fake"], stopped.RequestableByEngine["fake"])
	assert.False(t, mustStatus(t, e).Enabled)
	assert.ElementsMatch(t, enabled.DownloadedByEngine["fake"], stopped.DownloadedByEngine["fake"])
}

func TestPrepareRequestedModel_PreservesAnAlreadyServedAlias(t *testing.T) {
	e := requestedModelExecutor(t)
	before, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{ServerPort: before.ServerPort, LaunchText: "FAKE_SERVED_MODEL_NAME=\"assistant alias\" " + before.LaunchText}}, func() error { return nil })
	require.NoError(t, err)
	require.NoError(t, e.Start(t.Context(), "fake"))
	_, err = loadModel(t, e, "org/first")
	require.NoError(t, err)
	generation := startGeneration(t, e)

	_, err = e.prepareRequestedModel(t.Context(), "fake", "assistant alias")
	require.NoError(t, err)

	assert.Equal(t, generation, startGeneration(t, e))
}

func TestStartWith_PublishesEnabledIntentBeforeReturning(t *testing.T) {
	e := requestedModelExecutor(t)
	var last EngineStatus
	e.emit = func(method string, params any) {
		if method == "engine:state-changed" {
			status, ok := params.(EngineStatus)
			require.True(t, ok)
			last = status
		}
	}
	require.NoError(t, e.Start(t.Context(), "fake"))
	require.NoError(t, e.Stop("fake"))
	require.NoError(t, e.Start(t.Context(), "fake"))
	assert.True(t, last.Enabled)
	assert.True(t, last.AwaitingModel)
}
