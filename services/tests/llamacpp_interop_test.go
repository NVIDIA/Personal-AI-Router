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
)

func TestLlamaCPPProxyIsIncludedInBrokerDefaults(t *testing.T) {
	stdin, msgs, stderr, cleanup := startBrokerWith(t, "--proxy-path", proxyBin)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	if port := waitEngineProxyReady(t, "llamacpp-proxy", stdin, msgs, 15*time.Second); port <= 0 {
		t.Fatalf("default llama.cpp proxy port = %d, want a listening facade", port)
	}
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
	for i, tc := range []struct{ name, params string }{
		{"missing port", `{}`},
		{"zero port", `{"port":0}`},
		{"negative port", `{"port":-1}`},
		{"oversized port", `{"port":65536}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := 7300 + i
			if _, err := fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":%d,"method":"llamacpp-proxy:set-port","params":%s}`+"\n", id, tc.params); err != nil {
				t.Fatalf("write proxy port request: %v", err)
			}
			response := waitForResponse(t, msgs, 10*time.Second)
			if response.ID == nil || string(*response.ID) != fmt.Sprint(id) {
				t.Fatalf("unexpected response ID: %+v", response)
			}
			if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != "port must be between 1 and 65535" {
				t.Fatalf("error = %+v, want broker invalid-port rejection", response.Error)
			}
		})
	}
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
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]string{{"id": model}},
			})
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
	getModelList := func(t *testing.T, path string) {
		t.Helper()
		hitsBefore := modelListHits.Load()
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", proxyPort, path))
		if err != nil {
			t.Fatalf("get model list: %v", err)
		}
		defer response.Body.Close()
		var list struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
			t.Fatalf("decode model list: %v", err)
		}
		found := false
		for _, item := range list.Data {
			if item.ID == model {
				found = true
			}
		}
		if response.StatusCode != http.StatusOK || list.Object != "list" ||
			!found || modelListHits.Load() != hitsBefore+1 {
			t.Fatalf("model list status=%d body=%+v upstreamHits=%d",
				response.StatusCode, list, modelListHits.Load())
		}
	}
	t.Run("remaps OpenAI model list to router inventory", func(t *testing.T) {
		getModelList(t, "/v1/models")
	})
	t.Run("serves router model-list alias", func(t *testing.T) {
		getModelList(t, "/models")
	})

	post := func(t *testing.T, requestedModel string) int {
		t.Helper()
		endpoint := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxyPort)
		response, err := client.Post(endpoint, "application/json",
			bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[]}`, requestedModel)))
		if err != nil {
			t.Fatalf("post inference: %v", err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatalf("read inference response: %v", err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatalf("close inference response: %v", err)
		}
		return response.StatusCode
	}
	t.Run("routes only the exact advertised model id", func(t *testing.T) {
		if status := post(t, model); status != http.StatusOK {
			t.Fatalf("matching model status = %d, want 200", status)
		}
		if status := post(t, "org/router-model-GGUF:q4_k_m"); status != http.StatusBadGateway {
			t.Fatalf("case-changed model status = %d, want 502", status)
		}
		if got := inferenceHits.Load(); got != 1 {
			t.Fatalf("upstream inference hits = %d, want only the exact match", got)
		}
	})
}
