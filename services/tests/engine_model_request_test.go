// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	settings "nvpair-shared/enginesettings"
)

// The fake vLLM waits for a file before opening its HTTP port. This lets the
// test exercise the same broker and proxy before and after local readiness.
func TestBroker_RoutesToAPeerWhileLocalVLLMStarts(t *testing.T) {
	const model = "org/first"
	const localID = "local-vllm"
	gate := filepath.Join(t.TempDir(), "startup")
	config, enginePort := cachedModelRequestConfig(t, []string{model}, "--model", model)
	stdin, messages, _, cleanup := startBrokerWithConfigDirAndEnv(t, config, []string{"FAKE_START_GATE=" + gate},
		"--engine-manager-path", engineMgrBin, "--proxy-path", proxyBin, "--proxy-engines", "vllm")
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 15*time.Second)
	proxyPort := waitEngineProxyReady(t, "vllm-proxy", stdin, messages, 15*time.Second)
	call := func(method string, params any) {
		t.Helper()
		reply := callRPC(t, stdin, messages, method, params, 5*time.Second)
		require.Nil(t, reply.Error, "broker call %s", method)
	}
	call("engine:stop", map[string]string{"engine": "vllm"})
	call("vllm-proxy:node/add-manual", map[string]any{
		"id": localID, "host": "127.0.0.1", "port": proxyPort,
		"addresses": []string{"127.0.0.1"}, "models": []string{model},
	})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"model": model, "node": "peer"}))
	}))
	t.Cleanup(peer.Close)
	peerHost, peerPortText, err := net.SplitHostPort(peer.Listener.Addr().String())
	require.NoError(t, err)
	peerPort, err := strconv.Atoi(peerPortText)
	require.NoError(t, err)
	call("vllm-proxy:node/add-manual", map[string]any{
		"id": "peer", "host": peerHost, "port": peerPort,
		"addresses": []string{peerHost}, "models": []string{model},
	})
	// Select the local candidate so the first request must try it before the peer.
	call("vllm-proxy:node/select", map[string]string{"id": localID})

	startID := int(newRPCID())
	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"engine:start","params":{"engine":"vllm"}}`, startID))
	require.Eventually(t, func() bool {
		_, err := os.Stat(gate + ".waiting")
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the model process never reached its startup gate")
	// Keep the local engine eligible for model preparation while its port is closed.
	call("vllm-proxy:node/set-local-backend", map[string]any{
		"engine": "vllm", "host": "127.0.0.1", "port": enginePort, "healthy": false, "loadOnRequest": true,
	})

	client := &http.Client{Timeout: 3 * time.Second}
	// The peer adds a node marker to identify its response.
	request := func() string {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
		response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxyPort), "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer func() { assert.NoError(t, response.Body.Close()) }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var reply struct {
			Model string `json:"model"`
			Node  string `json:"node"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&reply))
		assert.Equal(t, model, reply.Model)
		return reply.Node
	}

	assert.Equal(t, "peer", request(), "the peer should serve while the local engine is starting")
	require.NoError(t, os.WriteFile(gate, nil, 0o600))
	require.Nil(t, waitForResponseID(t, messages, startID, 15*time.Second).Error)
	assert.Empty(t, request(), "the now-ready local model should serve the next request")
}

// cachedModelRequestConfig builds a fake vLLM and prepares its model cache. It
// returns the broker's config directory and the engine's HTTP port. Optional
// launch arguments let a test start with a model already selected.
func cachedModelRequestConfig(t *testing.T, models []string, launchArgs ...string) (string, int) {
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
			// Overrides inherit bundled defaults. The fake server needs neither
			// the vendor installer nor the real engine's memory-sizing probe.
			"install": nil,
			"runtime": map[string]any{
				"bin": executable, "args": append([]string{"serve"}, launchArgs...), "port": enginePort, "wait_for_model": true,
				"auto_launch": nil,
				"env":         map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}", "HF_HUB_CACHE": cache},
				// The startup test holds the port closed through a broker call and an inference request.
				"ready":           map[string]any{"http": "http://127.0.0.1:{port}/", "status": http.StatusOK, "timeout_s": 20},
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

// The same request path can load a model after Eject and must respect Stop even
// when the proxy's model list is out of date.
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
	// The manual node still advertises both models. Routing can reject a request
	// before it reaches the engine's disabled check. Either way, OFF must prevent
	// the request from starting the engine again.
	call("engine:stop", map[string]string{"engine": "vllm"})
	body := bytes.NewBufferString(`{"model":"org/second","messages":[]}`)
	response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxy.Port), "application/json", body)
	require.NoError(t, err)
	defer response.Body.Close()
	var rejection struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&rejection))
	assert.Contains(t, []int{http.StatusServiceUnavailable, http.StatusBadGateway}, response.StatusCode, "rejection: %s", rejection.Error)
	assert.NotEmpty(t, rejection.Error)
	if response.StatusCode == http.StatusServiceUnavailable {
		assert.Equal(t, `engine is disabled: "vllm"`, rejection.Error)
	}
	var stopped struct {
		Engine        string `json:"engine"`
		Enabled       bool   `json:"enabled"`
		Running       bool   `json:"running"`
		AwaitingModel bool   `json:"awaiting_model"`
	}
	require.NoError(t, json.Unmarshal(call("engine:status", map[string]string{"engine": "vllm"}), &stopped))
	assert.Equal(t, "vllm", stopped.Engine)
	assert.False(t, stopped.Enabled)
	assert.False(t, stopped.Running)
	assert.False(t, stopped.AwaitingModel)
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
