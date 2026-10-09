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

func TestLlamaCPPProxyIsIncludedInBrokerDefaults(t *testing.T) {
	stdin, msgs, stderr, cleanup := startBrokerWith(t, "--proxy-path", proxyBin)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	assert.Greater(t, waitEngineProxyReady(t, "llamacpp-proxy", stdin, msgs, 15*time.Second), 0, "default llama.cpp facade must listen")
}

func TestBrokerLlamaCPPProxySetPortRejectsInvalidPorts(t *testing.T) {
	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin, "--proxy-engines", "llamacpp",
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()
	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	test := func(name, params string, id int) {
		t.Run(name, func(t *testing.T) {
			_, err := fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":%d,"method":"llamacpp-proxy:set-port","params":%s}`+"\n", id, params)
			require.NoError(t, err, "write proxy port request")
			response := waitForResponse(t, msgs, 10*time.Second)
			require.NotNil(t, response.ID, "response ID")
			assert.Equal(t, fmt.Sprint(id), string(*response.ID), "response ID")
			require.NotNil(t, response.Error, "broker invalid-port rejection")
			assert.Equal(t, -32602, response.Error.Code)
			assert.Equal(t, "port must be between 1 and 65535", response.Error.Message)
		})
	}
	test("missing port", `{}`, 7300)
	test("zero port", `{"port":0}`, 7301)
	test("negative port", `{"port":-1}`, 7302)
	test("oversized port", `{"port":65536}`, 7303)
}

func TestLlamaCPPFacadeUsesRouterInventoryAndExactModelIDs(t *testing.T) {
	const model = "org/router-model-GGUF:Q4_K_M"
	var modelListHits atomic.Int32
	var inferenceHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			modelListHits.Add(1)
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]string{{"id": model}},
			}))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			inferenceHits.Add(1)
			_, err := io.Copy(io.Discard, r.Body)
			assert.NoError(t, err, "read inference request")
			_, err = io.WriteString(w, `{"choices":[]}`)
			assert.NoError(t, err, "write inference response")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin, "--proxy-engines", "llamacpp",
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	proxyPort := waitEngineProxyReady(t, "llamacpp-proxy", stdin, msgs, 15*time.Second)
	callBrokerRPC(t, stdin, msgs, 7200, "llamacpp-proxy:node/add-manual", map[string]any{
		"id":        "llamacpp-owner",
		"host":      "127.0.0.1",
		"port":      portOfURL(t, upstream.URL),
		"addresses": []string{"127.0.0.1"},
		"models":    []string{model},
	})

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	getModelList := func(name, path string) {
		t.Run(name, func(t *testing.T) {
			hitsBefore := modelListHits.Load()
			response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", proxyPort, path))
			require.NoError(t, err, "get model list")
			defer func() { assert.NoError(t, response.Body.Close(), "close model-list response") }()
			var list struct {
				Object string `json:"object"`
				Data   []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&list), "decode model list")
			found := false
			for _, item := range list.Data {
				if item.ID == model {
					found = true
				}
			}
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "list", list.Object)
			assert.True(t, found, "model list must include the advertised model")
			assert.Equal(t, hitsBefore+1, modelListHits.Load(), "upstream model-list requests")
		})
	}
	getModelList("remaps OpenAI model list to router inventory", "/v1/models")
	getModelList("serves router model-list alias", "/models")

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
		assert.Equal(t, http.StatusOK, post(t, model), "matching model status")
		assert.Equal(t, http.StatusBadGateway, post(t, "org/router-model-GGUF:q4_k_m"), "case-changed model status")
		assert.Equal(t, int32(1), inferenceHits.Load(), "only the exact match must reach the upstream")
	})
}
