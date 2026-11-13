// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/jsonrpc"
)

func startModelSelectionBroker(t *testing.T) (io.WriteCloser, <-chan jsonrpc.Message) {
	t.Helper()
	configDir := t.TempDir()
	port := freePort(t)
	// The existing manager executable proves installation, but must not be run
	// as a model server: enabling without a choice has no serving process.
	manifest := map[string]any{
		"engine": "selection-test", "display_name": "Selection Test", "manifest_version": 1,
		"platforms": map[string]any{runtime.GOOS + "/" + runtime.GOARCH: map[string]any{
			"detect": []string{engineMgrBin},
			"runtime": map[string]any{
				"bin": engineMgrBin, "args": []string{"serve"}, "port": port, "wait_for_model": true,
				"env":             map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}"},
				"ready":           map[string]any{"http": "http://127.0.0.1:{port}/health", "status": http.StatusOK, "timeout_s": 1},
				"editable_launch": map[string]any{"fixed_args": []string{"serve"}, "controls": []map[string]any{{"env": []string{"OLLAMA_HOST"}, "value": "{server.host}:{server.port}"}}},
			},
		}},
		"actions": map[string]any{"load_model": map[string]any{"launch_model": map[string]string{"flag": "--model"}}, "unload_model": map[string]any{"stop_engine": true}},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	for _, dir := range []string{
		filepath.Join(configDir, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(configDir, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "selection-test.json"), data, 0o644))
	}
	stdin, messages, _, cleanup := startBrokerWithConfigDir(t, configDir, "--engine-manager-path", engineMgrBin)
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 15*time.Second)
	return stdin, messages
}

func TestBroker_RelaysEnabledEngineAwaitingModel(t *testing.T) {
	stdin, messages := startModelSelectionBroker(t)
	require.NoError(t, json.NewEncoder(stdin).Encode(map[string]any{"jsonrpc": "2.0", "id": 601, "method": "engine:start", "params": map[string]string{"engine": "selection-test"}}))
	reply := waitForResponseID(t, messages, 601, 10*time.Second)
	require.Nil(t, reply.Error)
	var status struct {
		Engine        string `json:"engine"`
		Installed     bool   `json:"installed"`
		Running       bool   `json:"running"`
		Healthy       bool   `json:"healthy"`
		AwaitingModel bool   `json:"awaiting_model"`
	}
	require.NoError(t, json.Unmarshal(reply.Result, &status))
	assert.Equal(t, "selection-test", status.Engine)
	assert.True(t, status.Installed)
	assert.True(t, status.AwaitingModel)
	assert.False(t, status.Running)
	assert.False(t, status.Healthy)
}

func TestBroker_EjectKeepsEngineAwaitingModel(t *testing.T) {
	stdin, messages := startModelSelectionBroker(t)
	encoder := json.NewEncoder(stdin)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 701, "method": "engine:start", "params": map[string]string{"engine": "selection-test"}}))
	require.Nil(t, waitForResponseID(t, messages, 701, 10*time.Second).Error)

	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 702, "method": "engine:action", "params": map[string]any{"engine": "selection-test", "action": "unload_model", "params": map[string]string{"model": "org/selected"}}}))
	require.Nil(t, waitForResponseID(t, messages, 702, 10*time.Second).Error)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 703, "method": "engine:status", "params": map[string]string{"engine": "selection-test"}}))
	reply := waitForResponseID(t, messages, 703, 10*time.Second)
	require.Nil(t, reply.Error)
	var status struct {
		AwaitingModel bool `json:"awaiting_model"`
		Running       bool `json:"running"`
	}
	require.NoError(t, json.Unmarshal(reply.Result, &status))
	assert.True(t, status.AwaitingModel)
	assert.False(t, status.Running)
}
