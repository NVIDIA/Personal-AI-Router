// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// TestSGLangAdvertisesTheProxyPortNotTheEnginePort proves a healthy local SGLang
// registers the sg service at the OpenAI proxy's listen port — peers must reach
// this node through the proxy, never the engine — while the engine's own
// loopback port is handed to that proxy as the sglang backend.
func TestSGLangAdvertisesTheProxyPortNotTheEnginePort(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_model_info" {
			http.Error(w, "not sglang", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer engine.Close()
	_, portStr, err := net.SplitHostPort(engine.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	enginePort, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	const proxyPort = 1234
	b, backends := openAIProxyFixture(t, proxyPort)
	// No engine-manager, so localEnginePort answers with the fallback; point the
	// fallback at the stub by probing it directly.
	client := &http.Client{Timeout: 2 * time.Second}
	if !checkSGLangHealth(client, enginePort) {
		t.Fatal("stub did not answer /get_model_info")
	}

	b.reconcileAdvertiseSGLangAt(client, enginePort, true)

	snapshot := b.regCache.Snapshot()
	reg, ok := registrationFor(snapshot, noderec.ServiceSGLang)
	if !ok {
		t.Fatalf("sg was not registered: %+v", snapshot)
	}
	if reg.Port != proxyPort {
		t.Errorf("sg advertised port = %d, want the proxy's %d", reg.Port, proxyPort)
	}
	got := waitBackend(t, backends, "sglang")
	if got.Port != enginePort || !got.Healthy {
		t.Errorf("sglang local backend = %+v, want the healthy engine port %d", got, enginePort)
	}
}

// TestSGLangUnregistersWhenTheEngineIsDown proves the sg key is withdrawn and
// the proxy's sglang backend cleared when the engine stops answering, without
// any health request reaching a listener that is not SGLang.
func TestSGLangUnregistersWhenTheEngineIsDown(t *testing.T) {
	b, backends := openAIProxyFixture(t, 1234)
	b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceSGLang, Port: 1234})

	b.reconcileAdvertiseSGLangAt(nil, defaultSGLangPort, false)

	if _, ok := registrationFor(b.regCache.Snapshot(), noderec.ServiceSGLang); ok {
		t.Error("sg stayed registered with the engine down")
	}
	got := waitBackend(t, backends, "sglang")
	if got.Healthy {
		t.Errorf("sglang backend = %+v, want cleared", got)
	}
}

// TestSGLangNeverAdvertisesItsOwnProxy is the self-forward guard: when the
// resolved engine port equals the proxy's own listener, the node must not
// advertise sg and must not be handed its own listener as a backend.
func TestSGLangNeverAdvertisesItsOwnProxy(t *testing.T) {
	const proxyPort = 1234
	b, backends := openAIProxyFixture(t, proxyPort)

	// A nil client is intentional: the collision check must short-circuit before
	// any health request could mistake the proxy for the engine.
	b.reconcileAdvertiseSGLangAt(nil, proxyPort, true)

	if got := b.regCache.Snapshot(); len(got) != 0 {
		t.Fatalf("the OpenAI proxy was advertised as SGLang: %+v", got)
	}
	if got := waitBackend(t, backends, "sglang"); got.Healthy {
		t.Errorf("proxy listener was retained as the SGLang backend: %+v", got)
	}
}

// TestManualSGLangNodeIsBridgedWithItsEngine proves a manual node whose
// sglang_up is set is bridged into the OpenAI proxy tagged sglang, and that the
// vLLM entry on the same host is a separate entry on its own port rather than
// being overwritten.
func TestManualSGLangNodeIsBridgedWithItsEngine(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient)), ready: true, port: 1234}
	go proxy.peer.Serve(nil, nil)

	type call struct {
		method string
		node   proxyManualNode
	}
	calls := make(chan call, 16)
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			var n proxyManualNode
			_ = json.Unmarshal(msg.Params, &n)
			calls <- call{method: msg.Method, node: n}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()

	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLMStudioProxy(proxy)
	b.bridgeManualNode(manualNodeStatus{
		ID:           "n1",
		Address:      "192.0.2.7",
		VLLMUp:       true,
		VLLMPort:     8000,
		VLLMModels:   []string{"Qwen/Qwen3-8B"},
		SGLangUp:     true,
		SGLangPort:   30000,
		SGLangModels: []string{"/models/my-model"},
	}, "host-uuid")

	byEngine := map[string]proxyManualNode{}
	deadline := time.After(2 * time.Second)
	for len(byEngine) < 2 {
		select {
		case c := <-calls:
			if c.method == "node/add-manual" {
				byEngine[c.node.Engine] = c.node
			}
		case <-deadline:
			t.Fatalf("only bridged %v", byEngine)
		}
	}
	got := byEngine["sglang"]
	if got.Port != 30000 || got.ID != "host-uuid" {
		t.Errorf("sglang bridge = %+v, want SGLang's port on the node's operational key", got)
	}
	// A model id that is a bare local path is what SGLang actually reports, so it
	// has to survive the bridge unaltered.
	if len(got.Models) != 1 || got.Models[0] != "/models/my-model" {
		t.Errorf("sglang models = %v, want the path-shaped id carried verbatim", got.Models)
	}
	if got := byEngine["vllm"]; got.Port != 8000 {
		t.Errorf("vllm bridge = %+v, want its own port preserved", got)
	}
}

// TestManualNodeWithoutSGLangIsRemovedFromThatEngine proves an unreachable
// SGLang leg clears only its own entry, so the other engines on that host keep
// routing.
func TestManualNodeWithoutSGLangIsRemovedFromThatEngine(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient)), ready: true, port: 1234}
	go proxy.peer.Serve(nil, nil)

	type call struct {
		method string
		ref    proxyManualRef
	}
	calls := make(chan call, 16)
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			var ref proxyManualRef
			_ = json.Unmarshal(msg.Params, &ref)
			calls <- call{method: msg.Method, ref: ref}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()

	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLMStudioProxy(proxy)
	b.bridgeManualNode(manualNodeStatus{
		ID: "n1", Address: "192.0.2.7",
		VLLMUp: true, VLLMPort: 8000,
	}, "host-uuid")

	deadline := time.After(2 * time.Second)
	for {
		select {
		case c := <-calls:
			if c.method == "node/remove-manual" && c.ref.Engine == "sglang" {
				if c.ref.ID != "host-uuid" {
					t.Fatalf("remove ref = %+v", c.ref)
				}
				return
			}
			if c.method == "node/remove-manual" && c.ref.Engine == "vllm" {
				t.Fatal("a reachable vLLM must not be removed")
			}
		case <-deadline:
			t.Fatal("SGLang entry was never cleared")
		}
	}
}

// TestManualModelsByEngineCarriesSGLang proves a manual node's SGLang inventory
// is attributed under the same engine name discovered nodes use, and that an
// engine reporting nothing still adds no key.
func TestManualModelsByEngineCarriesSGLang(t *testing.T) {
	got := manualModelsByEngine(manualNodeStatus{
		OllamaModels: []string{"llama3.2:1b"},
		SGLangModels: []string{"/models/my-model"},
	})
	if len(got["sglang"]) != 1 || got["sglang"][0] != "/models/my-model" {
		t.Errorf("modelsByEngine = %+v, want an sglang key", got)
	}
	for _, engine := range []string{"lmstudio", "vllm"} {
		if _, ok := got[engine]; ok {
			t.Errorf("an engine with no models must add no key: %+v", got)
		}
	}
}
