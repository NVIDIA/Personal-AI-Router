// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	settings "nvpair-shared/enginesettings"
)

func requestedModelExecutor(t *testing.T) *Executor {
	t.Helper()
	e := waitingModelExecutor(t)
	cache := t.TempDir()
	for _, model := range []string{"org/first", "org/second", "org/fails-to-load"} {
		repo := writeRequestedModelSnapshot(t, cache, model, "abc123", llmFiles(map[string]string{"model.safetensors": "weights"}))
		require.NoError(t, os.MkdirAll(filepath.Join(repo, "refs"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(repo, "refs", "main"), []byte("abc123"), 0o644))
	}
	m, ok := e.reg.Get("fake")
	require.True(t, ok)
	m.AdvertisedModels = advertisedModelsAvailable
	m.Actions["list_models"] = Action{HFCache: &ActionHFCache{Dir: cache}, Result: &ActionResult{Array: "data", Field: "id"}}
	m.Actions["loaded_models"] = Action{HTTP: &ActionHTTP{Method: http.MethodGet, Path: "/v1/models"}, Result: &ActionResult{Array: "data", Field: "id", InventoryField: "root"}}
	require.NoError(t, m.Validate())
	return e
}

// Regular files keep these request-admission fixtures usable without symlink
// privileges on Windows. Completeness depends on the snapshot's files.
func writeRequestedModelSnapshot(t *testing.T, cache, model, revision string, files map[string]string) string {
	t.Helper()
	repo := filepath.Join(cache, "models--"+strings.ReplaceAll(model, "/", "--"))
	snapshot := filepath.Join(repo, "snapshots", revision)
	require.NoError(t, os.MkdirAll(snapshot, 0o755))
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(snapshot, name), []byte(content), 0o644))
	}
	return repo
}

// Another complete revision can make the model appear downloaded even when
// the launch's selected revision is missing files. Reject the request before
// changing settings or starting an unexpected download.
func TestPrepareRequestedModel_RefusesAMissingOrIncompleteEffectiveRevision(t *testing.T) {
	test := func(name, launchOptions, revision string, selectedFiles map[string]string) {
		t.Run(name, func(t *testing.T) {
			e := requestedModelExecutor(t)
			manifest, ok := e.reg.Get("fake")
			require.True(t, ok)
			cache := manifest.Actions["list_models"].HFCache.Dir
			repo := filepath.Join(cache, "models--org--second")
			require.NoError(t, os.WriteFile(filepath.Join(repo, "refs", revision), []byte("requested456"), 0o644))
			if selectedFiles != nil {
				writeRequestedModelSnapshot(t, cache, "org/second", "requested456", selectedFiles)
			}
			before, err := e.LaunchSettings("fake")
			require.NoError(t, err)
			_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{ServerPort: before.ServerPort, LaunchText: launchOptions + " " + before.LaunchText}}, func() error { return nil })
			require.NoError(t, err)
			require.NoError(t, e.Start(t.Context(), "fake"))
			unchanged := launchTextOf(t, e)
			// Another complete snapshot keeps the model in the disk inventory.
			require.Contains(t, e.ModelsResult(t.Context()).DownloadedByEngine["fake"], "org/second")

			_, err = e.prepareRequestedModel(t.Context(), "fake", "org/second")

			require.ErrorIs(t, err, errRequestModelNotCached)
			assert.Equal(t, unchanged, launchTextOf(t, e))
			assert.True(t, mustStatus(t, e).AwaitingModel)
			assert.False(t, mustStatus(t, e).Running)
		})
	}
	test("default main snapshot is missing", "", "main", nil)
	test("default main snapshot is incomplete", "", "main", map[string]string{"config.json": "{}", "tokenizer.json": "{}"})
	test("explicit revision is missing", "--revision release", "release", nil)
	test("explicit revision is incomplete", "--revision=release", "release", map[string]string{"config.json": "{}", "tokenizer.json": "{}"})
}

// Checking the selected revision must still allow automatic loading when its
// files are complete, whether the launch names a revision or a commit ID.
func TestPrepareRequestedModel_LoadsTheEffectiveCachedRevision(t *testing.T) {
	test := func(name, launchOptions string) {
		t.Run(name, func(t *testing.T) {
			e := requestedModelExecutor(t)
			manifest, ok := e.reg.Get("fake")
			require.True(t, ok)
			cache := manifest.Actions["list_models"].HFCache.Dir
			require.NoError(t, os.WriteFile(filepath.Join(cache, "models--org--second", "refs", "release"), []byte("abc123"), 0o644))
			before, err := e.LaunchSettings("fake")
			require.NoError(t, err)
			_, err = e.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{ServerPort: before.ServerPort, LaunchText: launchOptions + " " + before.LaunchText}}, func() error { return nil })
			require.NoError(t, err)
			require.NoError(t, e.Start(t.Context(), "fake"))

			_, err = e.prepareRequestedModel(t.Context(), "fake", "org/second")

			require.NoError(t, err)
			assert.Equal(t, []string{"org/second"}, e.ModelsResult(t.Context()).LoadedByEngine["fake"])
		})
	}
	test("named revision", "--revision release")
	test("snapshot revision", "--revision=abc123")
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

func TestPrepareRequestedModel_ServesTheLoadedModelWithoutALaunchTransaction(t *testing.T) {
	e := requestedModelExecutor(t)
	require.NoError(t, e.Start(t.Context(), "fake"))
	wantPort, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
	require.NoError(t, err)
	st, err := e.state("fake")
	require.NoError(t, err)
	// A settings read holds opMu without starting a lifecycle operation.
	st.opMu.Lock()
	defer st.opMu.Unlock()
	type result struct {
		port int
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		port, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
		finished <- result{port, err}
	}()

	select {
	case got := <-finished:
		require.NoError(t, got.err)
		assert.Equal(t, wantPort, got.port)
	case <-time.After(time.Second):
		t.Fatal("an already-serving model waited for the launch operation lock")
	}
}

func TestPrepareRequestedModel_RecoversAnEnabledEngineAfterACrash(t *testing.T) {
	test := func(name, requestedModel string) {
		t.Run(name, func(t *testing.T) {
			e := requestedModelExecutor(t)
			require.NoError(t, e.Start(t.Context(), "fake"))
			_, err := e.prepareRequestedModel(t.Context(), "fake", "org/first")
			require.NoError(t, err)
			st, err := e.state("fake")
			require.NoError(t, err)
			st.mu.Lock()
			proc := st.proc
			st.mu.Unlock()
			require.NotNil(t, proc)

			require.NoError(t, proc.cmd.Process.Kill())

			require.Eventually(t, func() bool {
				return !e.snapshot("fake", st).Running && hasErr(e.Errors(), exitedID("fake"))
			}, 5*time.Second, time.Millisecond)
			status, err := e.Status("fake")
			require.NoError(t, err)
			assert.True(t, status.Enabled)
			assert.True(t, status.AwaitingModel)
			assert.False(t, status.Running)
			assert.False(t, status.Healthy)

			_, err = e.prepareRequestedModel(t.Context(), "fake", requestedModel)

			require.NoError(t, err)
			assert.True(t, mustStatus(t, e).Running)
			assert.Equal(t, []string{requestedModel}, e.ModelsResult(t.Context()).LoadedByEngine["fake"])
		})
	}
	test("the previously loaded model", "org/first")
	test("another cached model", "org/second")
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
