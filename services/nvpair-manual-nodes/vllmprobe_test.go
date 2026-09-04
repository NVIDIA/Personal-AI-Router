// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// vllmStub serves the two routes probeVLLM requires. version == "" omits the
// version field, standing in for an OpenAI-compatible server that is not vLLM.
func vllmStub(t *testing.T, version string, models []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			if version == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
		case "/v1/models":
			data := make([]map[string]string, 0, len(models))
			for _, id := range models {
				data = append(data, map[string]string{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stubHostPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port %q: %v", u.Port(), err)
	}
	return u.Hostname(), port
}

// TestProbeVLLMReportsModels covers the vLLM probe against a real HTTP server:
// a node answering both /version and /v1/models reports up with its model ids.
func TestProbeVLLMReportsModels(t *testing.T) {
	m, _, _ := newTestManager()
	m.client = http.DefaultClient
	srv := vllmStub(t, "0.11.0", []string{"Qwen/Qwen3-8B"})
	host, port := stubHostPort(t, srv)

	up, models := m.probeVLLM(host, port)
	if !up {
		t.Fatal("expected vllm up")
	}
	if len(models) != 1 || models[0] != "Qwen/Qwen3-8B" {
		t.Fatalf("models = %#v", models)
	}
}

// TestProbeVLLMRejectsAnOpenAIServerThatIsNotVLLM is the disambiguator guard:
// LM Studio serves the same /v1/models but has no /version, so an OpenAI server
// without one must never be reported as vLLM.
func TestProbeVLLMRejectsAnOpenAIServerThatIsNotVLLM(t *testing.T) {
	m, _, _ := newTestManager()
	m.client = http.DefaultClient
	srv := vllmStub(t, "", []string{"qwen2.5-7b"})
	host, port := stubHostPort(t, srv)

	if up, models := m.probeVLLM(host, port); up || models != nil {
		t.Fatalf("an OpenAI server without /version was reported as vLLM: up=%v models=%#v", up, models)
	}
}

// TestProbeVLLMAbsentNodeIsDown covers the unreachable case.
func TestProbeVLLMAbsentNodeIsDown(t *testing.T) {
	m, _, _ := newTestManager()
	if up, models := m.probeVLLM("absent.local", vllmPort); up || models != nil {
		t.Fatalf("expected absent vllm down, got up=%v models=%#v", up, models)
	}
}

// TestVLLMCountsAsReachable proves a node that runs only vLLM is not treated as
// unreachable, which would otherwise raise a probe-failed error for a healthy
// node and drop it from routing.
func TestVLLMCountsAsReachable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status ManualNodeStatus
		want   bool
	}{
		{name: "vllm only", status: ManualNodeStatus{VLLMUp: true}, want: true},
		{name: "lmstudio only", status: ManualNodeStatus{LMStudioUp: true}, want: true},
		{name: "nothing", status: ManualNodeStatus{}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.status.OllamaUp || tc.status.LMStudioUp || tc.status.VLLMUp || tc.status.NodeInfoUp
			if got != tc.want {
				t.Errorf("reachable = %v, want %v", got, tc.want)
			}
		})
	}
}
