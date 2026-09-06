// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The two inference proxies are held to deliberate parity, so the liveness
// report a streaming response raises is asserted on both. See ollama-proxy's
// activity_test.go for the reasoning behind the signal itself.

// TestStreamedBytesReportNodeActivity mirrors the ollama-proxy test of the same
// name.
func TestStreamedBytesReportNodeActivity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", upstream.URL, "qwen"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`)))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !rec.has(`"method":"node/activity"`) {
		time.Sleep(5 * time.Millisecond)
	}
	if !rec.has(`"method":"node/activity"`) {
		t.Fatal("no node/activity was reported after the upstream streamed a response")
	}
	if !rec.has(`"hostUuid":"serving-node"`) {
		t.Fatal("node/activity did not name the node that served the request")
	}
}

// A node that never wrote a response byte has proved nothing and must not be
// vouched for.
func TestNoActivityReportedWithoutUpstreamBytes(t *testing.T) {
	rec := &prRec{}
	disc := NewDiscovery()
	// A port nothing is listening on: the dial fails, so no upstream byte can
	// ever reach the client. It still has to advertise the requested model, or
	// candidate pruning rejects the request before anything is dialled and the
	// test passes without exercising the dial failure at all.
	disc.AddManual(Node{
		ID:        "dead-node",
		Addresses: []string{"127.0.0.1"},
		Port:      closedPortFor(t),
		Models:    []string{"qwen"},
	})
	p := NewProxy(NewCodec(rec), disc, 1235)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`)))

	if rec.has(`"method":"node/activity"`) {
		t.Fatal("activity was reported for a node that never answered")
	}
}

// closedPortFor returns a port nothing is listening on, by binding and releasing
// it, so the dial is a prompt refusal rather than a timeout.
func closedPortFor(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	port := nodeFor(t, "probe", srv.URL).Port
	srv.Close()
	return port
}
