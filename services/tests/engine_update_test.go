// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/engines"
)

func TestBroker_RelaysLocalEngineUpdateRequests(t *testing.T) {
	configDir := t.TempDir()
	installDir := t.TempDir()
	writeUserManifest(t, configDir, map[string]any{
		"engine": engines.NameOllama,
		"platforms": map[string]any{runtime.GOOS + "/" + runtime.GOARCH: map[string]any{
			"detect":  []string{filepath.Join(installDir, engines.NameOllama)},
			"install": nil,
			"runtime": map[string]any{"port": freePorts(t, 1)[0]},
		}},
	})
	stdin, messages, _, cleanup := startBrokerWithConfigDir(t, configDir, "--engine-manager-path", engineMgrBin)
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 15*time.Second)

	t.Run("absent Ollama has no update", func(t *testing.T) {
		reply := callRPC(t, stdin, messages, "engine:check-updates", map[string]string{"engine": engines.NameOllama}, 10*time.Second)
		require.Nil(t, reply.Error)
		var result map[string]any
		require.NoError(t, json.Unmarshal(reply.Result, &result))
		require.Contains(t, result, "updateAvailable")
		assert.Nil(t, result["updateAvailable"])
	})
	t.Run("an update cannot install absent Ollama", func(t *testing.T) {
		reply := callRPC(t, stdin, messages, "engine:update", map[string]string{"engine": engines.NameOllama}, 10*time.Second)
		require.NotNil(t, reply.Error)
		assert.Equal(t, -32000, reply.Error.Code)
	})

	t.Run("a broken Ollama executable reports a check error", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(installDir, engines.NameOllama), []byte("invalid executable"), 0o600))
		reply := callRPC(t, stdin, messages, "engine:check-updates", map[string]string{"engine": engines.NameOllama}, 10*time.Second)
		require.NotNil(t, reply.Error)
		assert.Equal(t, -32000, reply.Error.Code)
	})

	for _, method := range []string{"engine:check-updates", "engine:update"} {
		t.Run(method+" rejects an unknown engine", func(t *testing.T) {
			reply := callRPC(t, stdin, messages, method, map[string]string{"engine": "unknown"}, 10*time.Second)
			require.NotNil(t, reply.Error)
			assert.Equal(t, -32602, reply.Error.Code)
		})
		t.Run(method+" requires an engine name", func(t *testing.T) {
			reply := callRPC(t, stdin, messages, method, map[string]string{}, 10*time.Second)
			require.NotNil(t, reply.Error)
			assert.Equal(t, -32602, reply.Error.Code)
		})
	}
}
