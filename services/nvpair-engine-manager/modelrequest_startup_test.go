// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	settings "nvpair-shared/enginesettings"
	"nvpair-shared/modelrequest"
)

func modelPreparationReply(t *testing.T, executor *Executor, model string) modelrequest.Reply {
	t.Helper()
	managerConn, clientConn := net.Pipe()
	t.Cleanup(func() { assert.NoError(t, managerConn.Close()) })
	t.Cleanup(func() { assert.NoError(t, clientConn.Close()) })
	require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	manager := &Manager{codec: NewCodec(managerConn), exec: executor}
	params, err := json.Marshal(modelrequest.Preparation{ID: t.Name(), Engine: "fake", Model: model})
	require.NoError(t, err)
	require.True(t, manager.handleModelPreparation(t.Context(), &Message{Method: modelrequest.EngineRequest, Params: params}))
	message, err := NewCodec(clientConn).Read()
	require.NoError(t, err)
	require.Equal(t, modelrequest.EngineReply, message.Method)
	var reply modelrequest.Reply
	require.NoError(t, json.Unmarshal(message.Params, &reply))
	return reply
}

// The child announces that it is waiting and binds its HTTP port only after
// the test releases it, so startup stays in progress throughout the assertion.
func holdModelReadiness(t *testing.T, executor *Executor) (func(), func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "start")
	before, err := executor.LaunchSettings("fake")
	require.NoError(t, err)
	_, err = executor.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{
		ServerPort: before.ServerPort,
		LaunchText: fmt.Sprintf("FAKE_START_GATE=%q %s", gate, before.LaunchText),
	}}, func() error { return nil })
	require.NoError(t, err)
	wait := func() {
		require.Eventually(t, func() bool {
			_, err := os.Stat(gate + ".waiting")
			return err == nil
		}, 5*time.Second, time.Millisecond, "the child never reached its startup gate")
	}
	release := func() { require.NoError(t, os.WriteFile(gate, nil, 0o600)) }
	return wait, release
}

func TestModelPreparation_ReportsExplicitStartupAsTemporarilyUnavailable(t *testing.T) {
	test := func(name, model string, start func(context.Context, *Executor) error) {
		t.Run(name, func(t *testing.T) {
			executor := requestedModelExecutor(t)
			require.NoError(t, executor.Start(t.Context(), "fake"))
			_, err := loadModel(t, executor, "org/first")
			require.NoError(t, err)
			require.NoError(t, executor.Stop("fake"))
			wait, release := holdModelReadiness(t, executor)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			completed := make(chan error, 1)
			go func() { completed <- start(ctx, executor) }()
			wait()
			require.True(t, mustStatus(t, executor).Enabled)
			require.True(t, mustStatus(t, executor).Starting)

			reply := modelPreparationReply(t, executor, model)

			assert.Equal(t, http.StatusServiceUnavailable, reply.Status)
			assert.Equal(t, `engine is starting: "fake"`, reply.Error)
			enabled, known, err := executor.desired.get("fake")
			require.NoError(t, err)
			assert.True(t, known)
			assert.False(t, enabled)
			release()
			require.NoError(t, <-completed)
			after := modelPreparationReply(t, executor, model)
			assert.Empty(t, after.Error)
			assert.Positive(t, after.Port)
			assert.True(t, mustStatus(t, executor).Running)
		})
	}

	test("re-enable the saved model", "org/first", func(ctx context.Context, executor *Executor) error {
		return executor.Start(ctx, "fake")
	})
	test("explicitly load another model while OFF", "org/second", func(ctx context.Context, executor *Executor) error {
		_, err := executor.Action(ctx, "fake", "load_model", json.RawMessage(`{"model":"org/second"}`))
		return err
	})
}

func TestModelPreparation_ReportsDisabledEngineAsUnavailableWithoutStartingIt(t *testing.T) {
	executor := requestedModelExecutor(t)
	require.NoError(t, executor.Start(t.Context(), "fake"))
	require.NoError(t, executor.Stop("fake"))

	reply := modelPreparationReply(t, executor, "org/first")

	assert.Equal(t, http.StatusServiceUnavailable, reply.Status)
	assert.Equal(t, `engine is disabled: "fake"`, reply.Error)
	assert.False(t, mustStatus(t, executor).Enabled)
	assert.False(t, mustStatus(t, executor).Running)
}

func TestModelPreparation_DoesNotPreventStopFromCancellingStartup(t *testing.T) {
	executor := requestedModelExecutor(t)
	require.NoError(t, executor.Start(t.Context(), "fake"))
	_, err := loadModel(t, executor, "org/first")
	require.NoError(t, err)
	require.NoError(t, executor.Stop("fake"))
	wait, _ := holdModelReadiness(t, executor)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- executor.Start(ctx, "fake") }()
	wait()
	require.Equal(t, http.StatusServiceUnavailable, modelPreparationReply(t, executor, "org/first").Status)

	require.NoError(t, executor.Stop("fake"))

	assert.ErrorIs(t, <-completed, errStartStopped)
	assert.False(t, mustStatus(t, executor).Enabled)
	assert.False(t, mustStatus(t, executor).Running)
}

func TestServingRequestModel_AllowsPreparationWhileAnEnabledEngineStarts(t *testing.T) {
	executor := requestedModelExecutor(t)
	before, err := executor.LaunchSettings("fake")
	require.NoError(t, err)
	gate := filepath.Join(t.TempDir(), "startup")
	_, err = executor.ConfigureLaunch(t.Context(), settings.Configure{Engine: "fake", Settings: settings.Config{
		ServerPort: before.ServerPort,
		LaunchText: fmt.Sprintf("FAKE_START_GATE=%q %s --model org/first", gate, before.LaunchText),
	}}, func() error { return nil })
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(gate, nil, 0o600))
	require.NoError(t, executor.Start(t.Context(), "fake"))
	state, err := executor.state("fake")
	require.NoError(t, err)
	state.mu.Lock()
	process := state.proc
	state.mu.Unlock()
	require.NotNil(t, process)
	require.NoError(t, process.cmd.Process.Kill())
	require.Eventually(t, func() bool {
		return !executor.snapshot("fake", state).Running
	}, 5*time.Second, time.Millisecond)
	require.NoError(t, os.Remove(gate))
	require.NoError(t, os.Remove(gate+".waiting"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- executor.Start(ctx, "fake") }()
	require.Eventually(t, func() bool {
		_, err := os.Stat(gate + ".waiting")
		return err == nil
	}, 5*time.Second, time.Millisecond)

	_, serving, err := executor.servingRequestModel(ctx, state, "fake", "org/first")

	assert.NoError(t, err, "an already enabled engine keeps the shared model preparation path")
	assert.False(t, serving)
	require.NoError(t, os.WriteFile(gate, nil, 0o600))
	require.NoError(t, <-completed)
}
