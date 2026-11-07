// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	settings "nvpair-shared/enginesettings"
)

func llamaSourceExecutor(t *testing.T) (*Executor, settings.Request) {
	t.Helper()
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Engine = "llamacpp"
	key := runtime.GOOS + "/" + runtime.GOARCH
	platform := manifest.Platforms[key]
	port, err := freePort()
	require.NoError(t, err)
	platform.Runtime.Port = port
	platform.Runtime.Args = []string{"--port", "{port}"}
	platform.Runtime.EditableLaunch = &EditableLaunch{Controls: []LaunchControl{{Value: "{server.port}", Flags: []string{"--port"}}}}
	manifest.Platforms[key] = platform
	executor := newTestExecutor(t, manifest)
	executor.overrideDir = t.TempDir()
	snapshot, err := executor.LaunchSettings("llamacpp")
	require.NoError(t, err)
	return executor, settings.Request{Engine: "llamacpp", Settings: settings.Config{ServerPort: snapshot.ServerPort, ProxyPort: 54322, LaunchText: snapshot.LaunchText}}
}

func TestModelSource_DefaultsToHuggingFace(t *testing.T) {
	executor, request := llamaSourceExecutor(t)
	preview, err := executor.PreviewLaunch(request)
	require.NoError(t, err)
	assert.Empty(t, preview.Errors)
	assert.Equal(t, "huggingface", preview.Settings.ModelSource)
	assert.Nil(t, executor.modelScope, "preview must not open a listener")
}

func TestModelSource_RejectsUnsupportedSelection(t *testing.T) {
	executor, request := llamaSourceExecutor(t)
	request.Settings.ModelSource = "https://example.com"
	preview, err := executor.PreviewLaunch(request)
	require.NoError(t, err)
	assert.Contains(t, preview.Errors, "modelSource")
}

func TestModelSource_RestartsEngineForSourceOnlyEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environment.json")
	t.Setenv("PAIR_TEST_ENV_FILE", path)
	executor, request := llamaSourceExecutor(t)
	t.Cleanup(executor.StopAll)
	require.NoError(t, executor.Start(context.Background(), request.Engine))
	state, err := executor.state(request.Engine)
	require.NoError(t, err)
	processID := func() int {
		t.Helper()
		state.mu.Lock()
		defer state.mu.Unlock()
		require.NotNil(t, state.proc)
		return state.proc.cmd.Process.Pid
	}
	beforeProcessID := processID()

	request.Settings.ModelSource = "modelscope"
	preview, err := executor.PreviewLaunch(request)
	require.NoError(t, err)
	require.Empty(t, preview.Errors)
	assert.True(t, preview.Restart)
	assert.Equal(t, request.Settings, preview.Settings)
	result, err := executor.ConfigureLaunch(context.Background(), settings.Configure{Engine: request.Engine, Settings: request.Settings}, func() error { return nil })
	require.NoError(t, err)
	assert.True(t, result.Running)
	assert.Equal(t, "modelscope", result.ModelSource)
	assert.Equal(t, request.Settings.ServerPort, result.ServerPort)
	assert.Equal(t, request.Settings.LaunchText, result.LaunchText)
	assert.NotEqual(t, beforeProcessID, processID())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var environment map[string]string
	require.NoError(t, json.Unmarshal(data, &environment))
	require.NotEmpty(t, executor.modelScopeURL)
	assert.Equal(t, executor.modelScopeURL, environment["MODEL_ENDPOINT"])
	data, err = os.ReadFile(filepath.Join(executor.overrideDir, "llamacpp.json"))
	require.NoError(t, err)
	var saved Manifest
	require.NoError(t, json.Unmarshal(data, &saved))
	assert.Equal(t, "modelscope", saved.Platforms[runtime.GOOS+"/"+runtime.GOARCH].Runtime.ModelSource)
}

func TestModelSource_PersistsSelectionWithoutStartingStoppedEngine(t *testing.T) {
	executor, request := llamaSourceExecutor(t)
	request.Settings.ModelSource = "modelscope"
	snapshot, err := executor.ConfigureLaunch(context.Background(), settings.Configure{Engine: request.Engine, Settings: request.Settings}, func() error { return nil })
	require.NoError(t, err)
	assert.Equal(t, "modelscope", snapshot.ModelSource)
	assert.False(t, snapshot.Running)
	assert.Nil(t, executor.modelScope)
	data, err := os.ReadFile(filepath.Join(executor.overrideDir, "llamacpp.json"))
	require.NoError(t, err)
	var saved Manifest
	require.NoError(t, json.Unmarshal(data, &saved))
	assert.Equal(t, "modelscope", saved.Platforms[runtime.GOOS+"/"+runtime.GOARCH].Runtime.ModelSource)
}

func TestModelSource_OverridesLaunchEndpointOnlyForModelScope(t *testing.T) {
	t.Setenv("MODEL_ENDPOINT", "https://inherited-mirror.invalid")
	for _, source := range []string{"huggingface", "modelscope"} {
		t.Run(source, func(t *testing.T) {
			executor, _ := llamaSourceExecutor(t)
			t.Cleanup(executor.StopAll)
			state, err := executor.state("llamacpp")
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "environment.json")
			state.plat.Runtime.Env["PAIR_TEST_ENV_FILE"] = path
			key := "MODEL_ENDPOINT"
			if runtime.GOOS == "windows" {
				key = "model_endpoint"
			}
			state.plat.Runtime.LaunchEnv = []string{key + "=https://unselected.invalid"}
			state.plat.Runtime.ModelSource = source
			require.NoError(t, executor.Start(context.Background(), "llamacpp"))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var environment map[string]string
			require.NoError(t, json.Unmarshal(data, &environment))
			if source == "modelscope" {
				assert.Equal(t, executor.modelScopeURL, environment["MODEL_ENDPOINT"])
				require.NotNil(t, executor.modelScope)
			} else {
				assert.Equal(t, "https://unselected.invalid", environment["MODEL_ENDPOINT"])
				assert.Nil(t, executor.modelScope)
			}
		})
	}
}

func TestModelSource_PreservesInheritedHuggingFaceEndpoint(t *testing.T) {
	t.Setenv("MODEL_ENDPOINT", "https://inherited-mirror.invalid")
	executor, _ := llamaSourceExecutor(t)
	t.Cleanup(executor.StopAll)
	state, err := executor.state("llamacpp")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "environment.json")
	state.plat.Runtime.Env["PAIR_TEST_ENV_FILE"] = path
	state.plat.Runtime.ModelSource = "huggingface"

	require.NoError(t, executor.Start(context.Background(), "llamacpp"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var environment map[string]string
	require.NoError(t, json.Unmarshal(data, &environment))
	assert.Equal(t, "https://inherited-mirror.invalid", environment["MODEL_ENDPOINT"])
	assert.Nil(t, executor.modelScope)
}
