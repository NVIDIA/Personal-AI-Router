// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	settings "nvpair-shared/enginesettings"
)

func TestModelSource_PreviewPreservesSavedChoiceWhenOmitted(t *testing.T) {
	executor, request := llamaSourceExecutor(t)
	request.Settings.ModelSource = "modelscope"
	_, err := executor.ConfigureLaunch(context.Background(), settings.Configure{Engine: request.Engine, Settings: request.Settings}, func() error { return nil })
	require.NoError(t, err)
	request.Settings.ModelSource = ""
	request.PreserveCORS = true

	preview, err := executor.PreviewLaunch(request)
	require.NoError(t, err)
	assert.Empty(t, preview.Errors)
	assert.Equal(t, "modelscope", preview.Settings.ModelSource)
	assert.False(t, preview.Restart)
	assert.Nil(t, executor.modelScope, "preview must not open a listener")
}

func TestModelSource_OmittedChoicePreservesRunningProcessAndSavedSettings(t *testing.T) {
	executor, request := llamaSourceExecutor(t)
	t.Cleanup(executor.StopAll)
	request.Settings.ModelSource = "modelscope"
	_, err := executor.ConfigureLaunch(context.Background(), settings.Configure{Engine: request.Engine, Settings: request.Settings}, func() error { return nil })
	require.NoError(t, err)
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
	overridePath := filepath.Join(executor.overrideDir, "llamacpp.json")
	before, err := os.ReadFile(overridePath)
	require.NoError(t, err)
	request.Settings.ModelSource = ""

	preview, err := executor.PreviewLaunch(request)
	require.NoError(t, err)
	require.Empty(t, preview.Errors)
	assert.False(t, preview.Restart)
	result, err := executor.ConfigureLaunch(context.Background(), settings.Configure{Engine: request.Engine, Settings: request.Settings}, func() error { return nil })
	require.NoError(t, err)
	assert.True(t, result.Running)
	assert.Equal(t, "modelscope", result.ModelSource)
	assert.Equal(t, beforeProcessID, processID())
	after, err := os.ReadFile(overridePath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "omitting the choice must not rewrite the saved settings")
	var saved Manifest
	require.NoError(t, json.Unmarshal(after, &saved))
	assert.Equal(t, "modelscope", saved.Platforms[runtime.GOOS+"/"+runtime.GOARCH].Runtime.ModelSource)
}

func TestModelSource_AdoptedEngineRejectsSourceChanges(t *testing.T) {
	executor, request := llamaSourceExecutor(t)
	state, err := executor.state(request.Engine)
	require.NoError(t, err)
	state.mu.Lock()
	state.running = true
	state.adopted = true
	state.mu.Unlock()
	snapshot, err := executor.LaunchSettings(request.Engine)
	require.NoError(t, err)
	assert.False(t, snapshot.Editable)
	assert.Equal(t, "Stop this engine in its own application before changing settings.", snapshot.Reason)
	request.Settings.ModelSource = "modelscope"

	preview, err := executor.PreviewLaunch(request)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Reason, preview.Errors["launchText"])
	rebound := false
	_, err = executor.ConfigureLaunch(context.Background(), settings.Configure{Engine: request.Engine, Settings: request.Settings}, func() error { rebound = true; return nil })
	require.Error(t, err)
	assert.False(t, rebound)
	after, err := executor.LaunchSettings(request.Engine)
	require.NoError(t, err)
	assert.Equal(t, snapshot, after)
	_, err = os.Stat(filepath.Join(executor.overrideDir, "llamacpp.json"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.Nil(t, executor.modelScope)
}
