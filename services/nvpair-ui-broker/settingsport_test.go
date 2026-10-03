// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func callBrokerPortRequest(t *testing.T, b *Broker, method string, params json.RawMessage) *Message {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set response deadline: %v", err)
	}
	b.codec = NewCodec(server)
	id := json.RawMessage(`1`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	codec := NewCodec(client)
	for {
		response, err := codec.Read()
		if err != nil {
			t.Fatalf("read %s response: %v", method, err)
		}
		if response.IsNotification() {
			continue
		}
		if response.ID == nil || string(*response.ID) != string(id) {
			t.Fatalf("unexpected response ID: %+v", response)
		}
		return response
	}
}

func TestBrokerProxySetPortRejectsInvalidPorts(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			for _, tc := range []struct{ name, params string }{
				{"malformed parameters", `{`},
				{"missing port", `{}`},
				{"zero port", `{"port":0}`},
				{"negative port", `{"port":-1}`},
				{"oversized port", `{"port":65536}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					response := callBrokerPortRequest(t, &Broker{}, profile.ComponentName()+":set-port", json.RawMessage(tc.params))
					if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != "port must be between 1 and 65535" {
						t.Fatalf("response = %+v, want invalid-port error", response)
					}
				})
			}
		})
	}
}

func TestBrokerProxySetPortRejectsInheritedAlias(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			b := &Broker{}
			b.setOllamaHostAlias(ollamaHostAlias{Port: 11433})
			response := callBrokerPortRequest(t, b, profile.ComponentName()+":set-port", json.RawMessage(`{"port":11433}`))
			if response.Error == nil || response.Error.Code != -32000 || !strings.Contains(response.Error.Message, "OLLAMA_HOST proxy alias") {
				t.Fatalf("response = %+v, want alias-port rejection", response)
			}
		})
	}
}

func TestBrokerLlamaCPPProxySetPortRejectsConflicts(t *testing.T) {
	test := func(name string, port func(*testing.T, *settingsHarness, settings.Snapshot) int) {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHarnessForEngine(t, "llamacpp")
			before, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "llamacpp"}, "")
			if err != nil {
				t.Fatalf("read initial settings: %v", err)
			}
			target := port(t, h, before)
			response := callBrokerPortRequest(t, h.b, "llamacpp-proxy:set-port", settingsJSON(map[string]int{"port": target}))
			if response.Error == nil || response.Error.Code != -32000 || response.Error.Message != "resolve settings errors and port conflicts before applying" {
				t.Fatalf("error = %+v, want settings-conflict rejection", response.Error)
			}
			if h.applies.Load() != 0 || h.proxyRebinds.Load() != 0 {
				t.Fatal("rejected port request changed runtime")
			}
			h.b.engineConfigMu.Lock()
			after := h.b.engineSettings["llamacpp"].Snapshot
			h.b.engineConfigMu.Unlock()
			if after.Settings != before.Settings || after.Revision != before.Revision {
				t.Fatalf("rejection changed desired settings: before=%+v after=%+v", before, after)
			}
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
		if err != nil {
			t.Fatalf("bind occupied port: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return ln.Addr().(*net.TCPAddr).Port
	})
}

func TestBrokerLlamaCPPProxySetPortPersistsOnlyRequestedFacade(t *testing.T) {
	h := newSettingsHarnessForEngine(t, "llamacpp")
	before, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "llamacpp"}, "")
	if err != nil {
		t.Fatalf("read initial settings: %v", err)
	}
	otherPorts := make(map[string]int)
	for _, engine := range []string{"ollama", "lmstudio"} {
		_, otherPorts[engine] = h.b.getProxy().Status(engine)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate target port: %v", err)
	}
	target := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release target port: %v", err)
	}
	// The namespace determines the engine, even if parameters name another.
	response := callBrokerPortRequest(t, h.b, "llamacpp-proxy:set-port", settingsJSON(map[string]any{"engine": "ollama", "port": target}))
	if response.Error != nil {
		t.Fatalf("set proxy port: %+v", response.Error)
	}
	var result struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("decode port result: %v", err)
	}
	if result.Port != target || h.proxyRebinds.Load() != 1 {
		t.Fatalf("port=%d rebinds=%d, want port=%d and one rebind", result.Port, h.proxyRebinds.Load(), target)
	}
	ready, actual := h.b.getProxy().Status("llamacpp")
	if !ready || actual != target {
		t.Fatalf("llama.cpp facade ready=%v port=%d, want %d", ready, actual, target)
	}
	for engine, want := range otherPorts {
		_, actual := h.b.getProxy().Status(engine)
		if actual != want {
			t.Fatalf("%s facade moved from %d to %d", engine, want, actual)
		}
	}
	path, err := h.b.engineSettingsPath()
	if err != nil {
		t.Fatalf("resolve journal path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var records map[string]*engineSettingsRecord
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("decode journal: %v", err)
	}
	record := records["llamacpp"]
	want := before.Settings
	want.ProxyPort = target
	if record == nil || !record.Explicit || record.Snapshot.Settings != want || record.Snapshot.Phase != "succeeded" || record.Snapshot.Revision != before.Revision+1 {
		t.Fatalf("persisted record = %+v, want successful explicit proxy-port change", record)
	}
}
