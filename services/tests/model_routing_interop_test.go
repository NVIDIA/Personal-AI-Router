// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type routingUpstream struct {
	server *httptest.Server
	hits   atomic.Int32
}

func newRoutingUpstream(t *testing.T, status int) *routingUpstream {
	t.Helper()
	upstream := &routingUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.hits.Add(1)
		_, copyErr := io.Copy(io.Discard, r.Body)
		assert.NoError(t, copyErr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, writeErr := io.WriteString(w, `{"done":true,"choices":[]}`)
			assert.NoError(t, writeErr)
			return
		}
		_, writeErr := io.WriteString(w, `{"error":"model not found"}`)
		assert.NoError(t, writeErr)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// An eligible owner can fail while a node without the requested model remains
// reachable. Failover must stay within model owners instead of using that other node.
func TestModelRouting_RejectsNodesWithoutRequestedModelAcrossProcesses(t *testing.T) {
	if portBusy(11435) || portBusy(1234) {
		t.Skip("ollama-proxy (11435) or lmstudio-proxy (1234) default port already in use; skipping")
	}

	owner404 := newRoutingUpstream(t, http.StatusNotFound)
	ownerOK := newRoutingUpstream(t, http.StatusOK)
	ineligible := newRoutingUpstream(t, http.StatusOK)

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin, "--proxy-engines", "ollama,lmstudio",
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	ollamaPort := waitProxyReady(t, stdin, msgs, 15*time.Second)
	lmstudioPort := waitLMStudioProxyReady(t, stdin, msgs, 15*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	snapshot := func() [3]int32 {
		return [3]int32{owner404.hits.Load(), ownerOK.hits.Load(), ineligible.hits.Load()}
	}

	test := func(name, rpcPrefix, path string, port, caseIndex int) {
		t.Run(name, func(t *testing.T) {
			targetModel := "strict-routing-" + name
			owner404ID := name + "-owner-404"
			ownerOKID := name + "-owner-ok"
			missingID := name + "-known-missing"
			unknownID := name + "-unknown"
			nodes := []struct {
				id     string
				port   int
				models []string
			}{
				{id: owner404ID, port: portOfURL(t, owner404.server.URL), models: []string{targetModel}},
				{id: ownerOKID, port: portOfURL(t, ownerOK.server.URL), models: []string{targetModel}},
				{id: missingID, port: portOfURL(t, ineligible.server.URL), models: []string{"different-model"}},
				{id: unknownID, port: portOfURL(t, ineligible.server.URL)},
			}

			requestID := 1000 + caseIndex*10
			for _, node := range nodes {
				params := map[string]any{
					"id":        node.id,
					"host":      "127.0.0.1",
					"port":      node.port,
					"addresses": []string{"127.0.0.1"},
				}
				if node.models != nil {
					params["models"] = node.models
				}
				callBrokerRPC(t, stdin, msgs, requestID, rpcPrefix+":node/add-manual", params)
				requestID++
			}
			callBrokerRPC(t, stdin, msgs, requestID, rpcPrefix+":node/set-priority", map[string]any{
				// Both facades share one proxy process, so every snapshot must
				// advance the process-wide generation.
				"generation": caseIndex + 1,
				"nodes":      []string{missingID, unknownID, owner404ID, ownerOKID},
			})

			before := snapshot()
			endpoint := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
			resp, err := client.Post(endpoint, "application/json",
				bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[]}`, targetModel)))
			require.NoError(t, err, "target-model request failed")
			body, readErr := io.ReadAll(resp.Body)
			require.NoError(t, readErr)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, "target-model response (%v)", body)
			after := snapshot()
			require.Equal(t, [3]int32{before[0] + 1, before[1] + 1, before[2]}, after, "target-model hit deltas")

			before = snapshot()
			resp, err = client.Post(endpoint, "application/json",
				bytes.NewBufferString(`{"model":"no-advertised-owner","messages":[]}`))
			require.NoError(t, err, "ownerless request failed")
			body, readErr = io.ReadAll(resp.Body)
			require.NoError(t, readErr)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusBadGateway, resp.StatusCode, "ownerless response (%v)", body)
			require.Contains(t, string(body), "no available node advertises the requested model", "ownerless response")
			after = snapshot()
			require.Equal(t, before, after, "ownerless request reached an upstream")
		})
	}
	test("ollama", "ollama-proxy", "/api/chat", ollamaPort, 0)
	test("ollama-anthropic", "ollama-proxy", "/v1/messages", ollamaPort, 1)
	test("lmstudio", "lmstudio-proxy", "/v1/chat/completions", lmstudioPort, 2)
	test("lmstudio-anthropic", "lmstudio-proxy", "/v1/messages", lmstudioPort, 3)
}

// vLLM serves one model per process under its Hugging Face id, so a facade in
// front of it must route only the exact id a node advertises.
func TestVLLMFacade_ServesTheModelListAndRoutesOnlyExactModelIDs(t *testing.T) {
	const model = "Qwen/Qwen2.5-0.5B-Instruct"
	var modelListHits atomic.Int32
	var inferenceHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			modelListHits.Add(1)
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]string{{"id": model}},
			}))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			inferenceHits.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"choices":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin, "--proxy-engines", "vllm",
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	proxyPort := waitEngineProxyReady(t, "vllm-proxy", stdin, msgs, 15*time.Second)
	callBrokerRPC(t, stdin, msgs, 7500, "vllm-proxy:node/add-manual", map[string]any{
		"id":        "vllm-owner",
		"host":      "127.0.0.1",
		"port":      portOfURL(t, upstream.URL),
		"addresses": []string{"127.0.0.1"},
		"models":    []string{model},
	})

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	t.Run("serves the OpenAI model list", func(t *testing.T) {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", proxyPort))
		require.NoError(t, err, "get model list")
		defer response.Body.Close()
		var list struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&list), "decode model list")
		var ids []string
		for _, item := range list.Data {
			ids = append(ids, item.ID)
		}
		assert.Equal(t, http.StatusOK, response.StatusCode)
		assert.Equal(t, "list", list.Object)
		assert.Contains(t, ids, model)
		assert.NotZero(t, modelListHits.Load(), "the list must come from the upstream engine")
	})

	post := func(t *testing.T, requestedModel string) int {
		t.Helper()
		endpoint := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxyPort)
		response, err := client.Post(endpoint, "application/json",
			bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[]}`, requestedModel)))
		require.NoError(t, err, "post inference")
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err, "read inference response")
		require.NoError(t, response.Body.Close(), "close inference response")
		return response.StatusCode
	}
	t.Run("routes only the exact advertised model id", func(t *testing.T) {
		require.Equal(t, http.StatusOK, post(t, model), "the exact model id")
		require.Equal(t, http.StatusBadGateway, post(t, "qwen/qwen2.5-0.5b-instruct"), "a case-changed model id")
		assert.Equal(t, int32(1), inferenceHits.Load(), "only the exact match reaches the upstream")
	})
}
