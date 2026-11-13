// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func cachedModelRequestConfig(t *testing.T, models []string) (string, int) {
	t.Helper()
	config := t.TempDir()
	executable := filepath.Join(t.TempDir(), "fake-engine")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	require.NoError(t, goBuild(filepath.Join("..", "nvpair-engine-manager", "testdata", "fakeengine"), executable))
	cache := t.TempDir()
	for _, model := range models {
		repo := filepath.Join(cache, "models--"+strings.ReplaceAll(model, "/", "--"))
		snapshot := filepath.Join(repo, "snapshots", "test-revision")
		require.NoError(t, os.MkdirAll(snapshot, 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(repo, "refs"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(repo, "refs", "main"), []byte("test-revision"), 0o644))
		for name, content := range map[string]string{"config.json": "{}", "tokenizer.json": "{}", "model.safetensors": "weights"} {
			require.NoError(t, os.WriteFile(filepath.Join(snapshot, name), []byte(content), 0o644))
		}
	}
	enginePort := freePort(t)
	manifest := map[string]any{
		"engine": "vllm", "display_name": "Request Model Test", "manifest_version": 1, "advertised_models": "available",
		"platforms": map[string]any{runtime.GOOS + "/" + runtime.GOARCH: map[string]any{
			"detect": []string{executable},
			"runtime": map[string]any{
				"bin": executable, "args": []string{"serve"}, "port": enginePort, "wait_for_model": true,
				"env":             map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}", "HF_HUB_CACHE": cache},
				"ready":           map[string]any{"http": "http://127.0.0.1:{port}/", "status": http.StatusOK, "timeout_s": 5},
				"editable_launch": map[string]any{"fixed_args": []string{"serve"}, "controls": []map[string]any{{"env": []string{"OLLAMA_HOST"}, "value": "{server.host}:{server.port}"}}},
			},
		}},
		"actions": map[string]any{
			"load_model":    map[string]any{"launch_model": map[string]string{"flag": "--model"}},
			"unload_model":  map[string]any{"stop_engine": true},
			"list_models":   map[string]any{"hf_cache": map[string]string{"dir": "{hf_hub_cache}"}, "result": map[string]string{"array": "data", "field": "id"}},
			"loaded_models": map[string]any{"http": map[string]string{"method": "GET", "path": "/v1/models"}, "result": map[string]string{"array": "data", "field": "id", "inventory_field": "root"}},
		},
	}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	for _, dir := range []string{filepath.Join(config, "Nvidia Corporation", "Personal AI Router", "engines"), filepath.Join(config, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines")} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "vllm.json"), raw, 0o644))
	}
	return config, enginePort
}

func TestBroker_ServesRequestedCachedModelsAcrossProcesses(t *testing.T) {
	config, enginePort := cachedModelRequestConfig(t, []string{"org/first", "org/second"})
	stdin, messages, _, cleanup := startBrokerWithConfigDir(t, config, "--engine-manager-path", engineMgrBin, "--proxy-path", proxyBin, "--proxy-engines", "vllm")
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 15*time.Second)
	id := 800
	call := func(method string, params any) json.RawMessage {
		t.Helper()
		id++
		require.NoError(t, json.NewEncoder(stdin).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
		reply := waitForResponseID(t, messages, id, 15*time.Second)
		require.Nil(t, reply.Error)
		return reply.Result
	}
	call("engine:start", map[string]string{"engine": "vllm"})
	var proxy struct {
		Ready bool `json:"ready"`
		Port  int  `json:"port"`
	}
	require.NoError(t, json.Unmarshal(call("vllm-proxy:get-status", nil), &proxy))
	require.True(t, proxy.Ready)
	call("vllm-proxy:node/set-local-backend", map[string]any{"engine": "vllm", "host": "127.0.0.1", "port": enginePort, "healthy": false, "loadOnRequest": true})
	call("vllm-proxy:node/add-manual", map[string]any{"id": "request-self", "host": "127.0.0.1", "port": proxy.Port, "addresses": []string{"127.0.0.1"}, "models": []string{"org/first", "org/second"}})
	client := &http.Client{Timeout: 15 * time.Second}
	request := func(model string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply briefly."}}, "max_tokens": 8})
		require.NoError(t, err)
		response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxy.Port), "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var reply struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&reply))
		assert.Equal(t, model, reply.Model)
		served, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", enginePort))
		require.NoError(t, err)
		defer served.Body.Close()
		var inventory struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.NewDecoder(served.Body).Decode(&inventory))
		require.Len(t, inventory.Data, 1)
		assert.Equal(t, model, inventory.Data[0].ID)
	}
	request("org/first")
	request("org/second")
	call("engine:action", map[string]any{"engine": "vllm", "action": "unload_model", "params": map[string]string{"model": "org/second"}})
	request("org/first")
	// Stop remains authoritative even if discovery still carries a stale positive inventory.
	call("engine:stop", map[string]string{"engine": "vllm"})
	call("vllm-proxy:node/set-local-backend", map[string]any{"engine": "vllm", "host": "127.0.0.1", "port": enginePort, "healthy": false, "loadOnRequest": true})
	body := bytes.NewBufferString(`{"model":"org/second","messages":[]}`)
	response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxy.Port), "application/json", body)
	require.NoError(t, err)
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, response.StatusCode)
}

// Persisted ON intent must be routable when the broker reports ready, without
// a UI command, a manually injected proxy target, or the next polling interval.
func TestBroker_RestoresCachedModelRoutingBeforeReady(t *testing.T) {
	config, _ := cachedModelRequestConfig(t, []string{"org/restored"})
	for _, base := range []string{
		filepath.Join(config, "Nvidia Corporation", "Personal AI Router"),
		filepath.Join(config, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, "engine-bin"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(base, "engine-bin", "engine-state.json"), []byte(`{"engines":{"vllm":true}}`), 0o600))
	}
	stdin, messages, _, cleanup := startBrokerWithConfigDir(t, config, "--engine-manager-path", engineMgrBin, "--proxy-path", proxyBin, "--proxy-engines", "vllm")
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 15*time.Second)
	require.NoError(t, json.NewEncoder(stdin).Encode(map[string]any{"jsonrpc": "2.0", "id": 9101, "method": "vllm-proxy:get-status"}))
	reply := waitForResponseID(t, messages, 9101, 5*time.Second)
	require.Nil(t, reply.Error)
	var proxy struct {
		Ready bool `json:"ready"`
		Port  int  `json:"port"`
	}
	require.NoError(t, json.Unmarshal(reply.Result, &proxy))
	require.True(t, proxy.Ready)
	body := bytes.NewBufferString(`{"model":"org/restored","messages":[{"role":"user","content":"Reply briefly."}],"max_tokens":8}`)
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxy.Port), "application/json", body)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, "the first request after readiness must load the persisted cached model")
	var completion struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&completion))
	assert.Equal(t, "org/restored", completion.Model)
}

func TestBroker_RestoresIdleCachedModelRoutingAfterSettingsApply(t *testing.T) {
	config, _ := cachedModelRequestConfig(t, []string{"org/idle-cached"})
	stdin, messages, _, cleanup := startBrokerWithConfigDir(t, config, "--engine-manager-path", engineMgrBin, "--proxy-path", proxyBin, "--proxy-engines", "vllm")
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 15*time.Second)
	id := 9200
	call := func(method string, params any) json.RawMessage {
		t.Helper()
		id++
		require.NoError(t, json.NewEncoder(stdin).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
		reply := waitForResponseID(t, messages, id, 15*time.Second)
		require.Nil(t, reply.Error)
		return reply.Result
	}
	call("engine:start", map[string]string{"engine": "vllm"})
	var before settings.Snapshot
	require.NoError(t, json.Unmarshal(call("engine:get-settings", settings.Request{Engine: "vllm"}), &before))
	require.False(t, before.Running, "enabled without a selection must remain idle")
	empty := before.Settings
	empty.LaunchText = regexp.MustCompile(`HF_HUB_CACHE=(?:"[^"]*"|'[^']*'|[^\s]+)`).ReplaceAllString(before.Settings.LaunchText, fmt.Sprintf("HF_HUB_CACHE=%q", t.TempDir()))
	var preview settings.Preview
	require.NoError(t, json.Unmarshal(call("engine:preview-settings", settings.Request{Engine: "vllm", ExpectedRevision: before.Revision, Settings: empty}), &preview))
	require.Empty(t, preview.Errors)
	require.Nil(t, preview.Conflict)
	var receipt struct {
		Phase string `json:"phase"`
	}
	require.NoError(t, json.Unmarshal(call("engine:apply-settings", settings.Request{Engine: "vllm", ExpectedRevision: before.Revision, RequestID: "empty-cache", Settings: empty}), &receipt))
	require.Equal(t, "succeeded", receipt.Phase)
	call("engine:stop", map[string]string{"engine": "vllm"})
	call("engine:start", map[string]string{"engine": "vllm"})
	var after settings.Snapshot
	require.NoError(t, json.Unmarshal(call("engine:get-settings", settings.Request{Engine: "vllm"}), &after))
	require.NoError(t, json.Unmarshal(call("engine:apply-settings", settings.Request{Engine: "vllm", ExpectedRevision: after.Revision, RequestID: "restore-cache", Settings: before.Settings}), &receipt))
	require.Equal(t, "succeeded", receipt.Phase)

	// Completion must restore request eligibility even when no model is loaded.
	body := bytes.NewBufferString(`{"model":"org/idle-cached","messages":[{"role":"user","content":"Reply briefly."}],"max_tokens":8}`)
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", before.Settings.ProxyPort), "application/json", body)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var completion struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&completion))
	assert.Equal(t, "org/idle-cached", completion.Model)
}
