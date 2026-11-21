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
	names := []string{engines.NameOllama, engines.NameLMStudio, engines.NameLlamaCPP, engines.NameVLLM}
	ports := freePorts(t, len(names))
	// Give each engine an empty installation on the host platform, including
	// engines whose bundled manifests normally omit this OS or architecture.
	for i, name := range names {
		writeUserManifest(t, configDir, map[string]any{
			"engine": name,
			"platforms": map[string]any{runtime.GOOS + "/" + runtime.GOARCH: map[string]any{
				"detect":  []string{filepath.Join(installDir, name)},
				"install": nil,
				"runtime": map[string]any{"port": ports[i]},
			}},
		})
	}
	stdin, messages, _, cleanup := startBrokerWithConfigDir(t, configDir, "--engine-manager-path", engineMgrBin)
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 15*time.Second)

	expectNoUpdate := func(t *testing.T, name string) {
		t.Helper()
		reply := callRPC(t, stdin, messages, "engine:check-updates", map[string]string{"engine": name}, 10*time.Second)
		require.Nil(t, reply.Error)
		var result map[string]any
		require.NoError(t, json.Unmarshal(reply.Result, &result))
		require.Contains(t, result, "updateAvailable")
		assert.Nil(t, result["updateAvailable"])
	}

	for _, name := range names {
		t.Run(name+" has no update when absent", func(t *testing.T) { expectNoUpdate(t, name) })
		t.Run(name+" cannot be installed by an update", func(t *testing.T) {
			reply := callRPC(t, stdin, messages, "engine:update", map[string]string{"engine": name}, 10*time.Second)
			require.NotNil(t, reply.Error)
			assert.Equal(t, -32000, reply.Error.Code)
		})
	}

	t.Run("a broken Ollama binary leaves the LM Studio check independent", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(installDir, engines.NameOllama), []byte("invalid executable"), 0o600))
		reply := callRPC(t, stdin, messages, "engine:check-updates", map[string]string{"engine": engines.NameOllama}, 10*time.Second)
		require.NotNil(t, reply.Error)
		assert.Equal(t, -32000, reply.Error.Code)
		expectNoUpdate(t, engines.NameLMStudio)
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
