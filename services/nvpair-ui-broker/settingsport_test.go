// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
)

func callBrokerPortRequest(t *testing.T, b *Broker, method string, params json.RawMessage) *Message {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, server.Close())
	})
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)), "set response deadline")
	b.codec = NewCodec(server)
	id := json.RawMessage(`1`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	codec := NewCodec(client)
	for {
		response, err := codec.Read()
		require.NoError(t, err, "read %s response", method)
		if response.IsNotification() {
			continue
		}
		require.NotNil(t, response.ID, "response ID")
		require.Equal(t, string(id), string(*response.ID), "response ID")
		return response
	}
}

func TestBrokerProxySetPortRejectsInvalidPorts(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			test := func(name, params string) {
				t.Run(name, func(t *testing.T) {
					response := callBrokerPortRequest(t, &Broker{}, profile.ComponentName()+":set-port", json.RawMessage(params))
					require.NotNil(t, response.Error, "invalid-port error")
					assert.Equal(t, -32602, response.Error.Code)
					assert.Equal(t, "port must be between 1 and 65535", response.Error.Message)
				})
			}
			test("malformed parameters", `{`)
			test("missing port", `{}`)
			test("zero port", `{"port":0}`)
			test("negative port", `{"port":-1}`)
			test("oversized port", `{"port":65536}`)
		})
	}
}

func TestBrokerProxySetPortRejectsInheritedAlias(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			b := &Broker{}
			b.setOllamaHostAlias(ollamaHostAlias{Port: 11433})
			response := callBrokerPortRequest(t, b, profile.ComponentName()+":set-port", json.RawMessage(`{"port":11433}`))
			require.NotNil(t, response.Error, "alias-port rejection")
			assert.Equal(t, -32000, response.Error.Code)
			assert.Contains(t, response.Error.Message, "OLLAMA_HOST proxy alias")
		})
	}
}

func TestBrokerLlamaCPPProxySetPortRejectsConflicts(t *testing.T) {
	test := func(name string, port func(*testing.T, *settingsHarness, settings.Snapshot) int) {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHarnessForEngine(t, "llamacpp")
			before, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "llamacpp"}, "")
			require.NoError(t, err, "read initial settings")
			target := port(t, h, before)
			response := callBrokerPortRequest(t, h.b, "llamacpp-proxy:set-port", settingsJSON(map[string]int{"port": target}))
			require.NotNil(t, response.Error, "settings-conflict rejection")
			assert.Equal(t, -32000, response.Error.Code)
			assert.Equal(t, "resolve settings errors and port conflicts before applying", response.Error.Message)
			assert.Equal(t, int32(0), h.applies.Load(), "rejected port request changed runtime")
			assert.Equal(t, int32(0), h.proxyRebinds.Load(), "rejected port request rebound the proxy")
			h.b.engineConfigMu.Lock()
			after := h.b.engineSettings["llamacpp"].Snapshot
			h.b.engineConfigMu.Unlock()
			assert.Equal(t, before.Settings, after.Settings, "rejection changed desired settings")
			assert.Equal(t, before.Revision, after.Revision, "rejection changed settings revision")
		})
	}
	test("PAIR service port", func(t *testing.T, h *settingsHarness, before settings.Snapshot) int {
		return engineControlPort
	})
	test("same engine server port", func(t *testing.T, h *settingsHarness, before settings.Snapshot) int {
		return before.Settings.ServerPort
	})
	test("another configured engine", func(t *testing.T, h *settingsHarness, before settings.Snapshot) int {
		h.otherEnginePort.Store(25001)
		return 25001
	})
	test("another proxy listener", func(t *testing.T, h *settingsHarness, before settings.Snapshot) int {
		_, port := h.b.getProxy().Status("ollama")
		return port
	})
	test("occupied listener", func(t *testing.T, h *settingsHarness, before settings.Snapshot) int {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err, "bind occupied port")
		t.Cleanup(func() { assert.NoError(t, ln.Close()) })
		return ln.Addr().(*net.TCPAddr).Port
	})
}

func TestBrokerLlamaCPPProxySetPortPersistsOnlyRequestedFacade(t *testing.T) {
	h := newSettingsHarnessForEngine(t, "llamacpp")
	before, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "llamacpp"}, "")
	require.NoError(t, err, "read initial settings")
	otherPorts := make(map[string]int)
	for _, engine := range []string{"ollama", "lmstudio"} {
		_, otherPorts[engine] = h.b.getProxy().Status(engine)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "allocate target port")
	target := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close(), "release target port")
	// The namespace determines the engine, even if parameters name another.
	response := callBrokerPortRequest(t, h.b, "llamacpp-proxy:set-port", settingsJSON(map[string]any{"engine": "ollama", "port": target}))
	require.Nil(t, response.Error, "set proxy port")
	var result struct {
		Port int `json:"port"`
	}
	require.NoError(t, json.Unmarshal(response.Result, &result), "decode port result")
	assert.Equal(t, target, result.Port)
	assert.Equal(t, int32(1), h.proxyRebinds.Load(), "proxy rebind count")
	ready, actual := h.b.getProxy().Status("llamacpp")
	assert.True(t, ready, "llama.cpp facade must be ready")
	assert.Equal(t, target, actual, "llama.cpp facade port")
	for engine, want := range otherPorts {
		_, actual := h.b.getProxy().Status(engine)
		assert.Equal(t, want, actual, "%s facade moved", engine)
	}
	path, err := h.b.engineSettingsPath()
	require.NoError(t, err, "resolve journal path")
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read journal")
	var records map[string]*engineSettingsRecord
	require.NoError(t, json.Unmarshal(data, &records), "decode journal")
	record := records["llamacpp"]
	want := before.Settings
	want.ProxyPort = target
	require.NotNil(t, record, "persisted llama.cpp record")
	assert.True(t, record.Explicit, "persisted proxy-port change must be explicit")
	assert.Equal(t, want, record.Snapshot.Settings)
	assert.Equal(t, "succeeded", record.Snapshot.Phase)
	assert.Equal(t, before.Revision+1, record.Snapshot.Revision)
}
