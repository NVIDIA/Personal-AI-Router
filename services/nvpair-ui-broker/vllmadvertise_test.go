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

// openAIProxyFixture returns a broker with a fake OpenAI proxy on the given
// listen port, plus a channel receiving every node/set-local-backend it is sent.
func openAIProxyFixture(t *testing.T, listenPort int) (*Broker, <-chan proxyLocalBackend) {
	t.Helper()
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() { proxyClient.Close(); proxyServer.Close() })
	proxy := &proxyProcess{
		peer:  NewPeer(NewCodec(proxyClient)),
		ready: true,
		port:  listenPort,
	}
	go proxy.peer.Serve(nil, nil)

	backends := make(chan proxyLocalBackend, 8)
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			var got proxyLocalBackend
			if json.Unmarshal(msg.Params, &got) == nil {
				backends <- got
			}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()

	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLMStudioProxy(proxy)
	return b, backends
}

func waitBackend(t *testing.T, ch <-chan proxyLocalBackend, engine string) proxyLocalBackend {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-ch:
			if got.Engine == engine {
				return got
			}
		case <-deadline:
			t.Fatalf("no node/set-local-backend for %q", engine)
		}
	}
}

// TestVLLMAdvertisesTheProxyPortNotTheEnginePort proves a healthy local vLLM
// registers the vl service at the OpenAI proxy's listen port — peers must reach
// this node through the proxy, never the engine — while the engine's own
// loopback port is handed to that proxy as the vllm backend.
func TestVLLMAdvertisesTheProxyPortNotTheEnginePort(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.Error(w, "not vllm", http.StatusNotFound)
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
	if !checkVLLMHealth(client, enginePort) {
		t.Fatal("stub did not answer /health")
	}

	b.reconcileAdvertiseVLLMAt(client, enginePort, true)

	snapshot := b.regCache.Snapshot()
	reg, ok := registrationFor(snapshot, noderec.ServiceVLLM)
	if !ok {
		t.Fatalf("vl was not registered: %+v", snapshot)
	}
	if reg.Port != proxyPort {
		t.Errorf("vl advertised port = %d, want the proxy's %d", reg.Port, proxyPort)
	}
	got := waitBackend(t, backends, "vllm")
	if got.Port != enginePort || !got.Healthy {
		t.Errorf("vllm local backend = %+v, want the healthy engine port %d", got, enginePort)
	}
}

// TestVLLMUnregistersWhenTheEngineIsDown proves the vl key is withdrawn and the
// proxy's vllm backend cleared when the engine stops answering, without any
// health request reaching a listener that is not vLLM.
func TestVLLMUnregistersWhenTheEngineIsDown(t *testing.T) {
	b, backends := openAIProxyFixture(t, 1234)
	b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceVLLM, Port: 1234})

	b.reconcileAdvertiseVLLMAt(nil, defaultVLLMPort, false)

	if _, ok := registrationFor(b.regCache.Snapshot(), noderec.ServiceVLLM); ok {
		t.Error("vl stayed registered with the engine down")
	}
	got := waitBackend(t, backends, "vllm")
	if got.Healthy {
		t.Errorf("vllm backend = %+v, want cleared", got)
	}
}

// TestVLLMNeverAdvertisesItsOwnProxy is the self-forward guard: when the
// resolved engine port equals the proxy's own listener, the node must not
// advertise vl and must not be handed its own listener as a backend.
func TestVLLMNeverAdvertisesItsOwnProxy(t *testing.T) {
	const proxyPort = 1234
	b, backends := openAIProxyFixture(t, proxyPort)

	// A nil client is intentional: the collision check must short-circuit before
	// any health request could mistake the proxy for the engine.
	b.reconcileAdvertiseVLLMAt(nil, proxyPort, true)

	if got := b.regCache.Snapshot(); len(got) != 0 {
		t.Fatalf("the OpenAI proxy was advertised as vLLM: %+v", got)
	}
	if got := waitBackend(t, backends, "vllm"); got.Healthy {
		t.Errorf("proxy listener was retained as the vLLM backend: %+v", got)
	}
}

// TestOpenAIProxyServesBothEngines proves both OpenAI engines resolve to the one
// proxy process, which is what lets a node advertise lm and vl at the same port.
func TestOpenAIProxyServesBothEngines(t *testing.T) {
	b, _ := openAIProxyFixture(t, 1234)
	lm := b.proxyForEngine("lmstudio")
	vl := b.proxyForEngine("vllm")
	if lm == nil || vl == nil {
		t.Fatalf("proxyForEngine: lmstudio=%v vllm=%v", lm, vl)
	}
	if lm != vl {
		t.Error("both OpenAI engines must be fronted by the same proxy process")
	}
	if b.proxyForEngine("ollama") == vl {
		t.Error("Ollama must keep its own proxy")
	}
}

// TestManualVLLMNodeIsBridgedWithItsEngine proves a manual node whose vllm_up is
// set is bridged into the OpenAI proxy tagged vllm, and that an LM Studio entry
// on the same host is a separate entry rather than overwriting it.
func TestManualVLLMNodeIsBridgedWithItsEngine(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient)), ready: true, port: 1234}
	go proxy.peer.Serve(nil, nil)

	type call struct {
		method string
		node   proxyManualNode
	}
	calls := make(chan call, 8)
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
		ID:             "n1",
		Address:        "192.0.2.7",
		LMStudioUp:     true,
		LMStudioPort:   1234,
		LMStudioModels: []string{"qwen2.5-7b"},
		VLLMUp:         true,
		VLLMPort:       8000,
		VLLMModels:     []string{"Qwen/Qwen3-8B"},
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
	if got := byEngine["vllm"]; got.Port != 8000 || got.ID != "host-uuid" {
		t.Errorf("vllm bridge = %+v, want the vLLM port on the node's operational key", got)
	}
	if got := byEngine["lmstudio"]; got.Port != 1234 {
		t.Errorf("lmstudio bridge = %+v, want its own port preserved", got)
	}
}

// TestManualNodeWithoutVLLMIsRemovedFromThatEngine proves an unreachable engine
// leg clears only its own entry, so the other engine on that host keeps routing.
func TestManualNodeWithoutVLLMIsRemovedFromThatEngine(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient)), ready: true, port: 1234}
	go proxy.peer.Serve(nil, nil)

	type call struct {
		method string
		ref    proxyManualRef
	}
	calls := make(chan call, 8)
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
		LMStudioUp: true, LMStudioPort: 1234,
	}, "host-uuid")

	deadline := time.After(2 * time.Second)
	for {
		select {
		case c := <-calls:
			if c.method == "node/remove-manual" && c.ref.Engine == "vllm" {
				if c.ref.ID != "host-uuid" {
					t.Fatalf("remove ref = %+v", c.ref)
				}
				return
			}
			if c.method == "node/remove-manual" && c.ref.Engine == "lmstudio" {
				t.Fatal("a reachable LM Studio must not be removed")
			}
		case <-deadline:
			t.Fatal("vLLM entry was never cleared")
		}
	}
}

// TestManualModelsByEngineCarriesVLLM proves a manual node's vLLM inventory is
// attributed under the same engine name discovered nodes use.
func TestManualModelsByEngineCarriesVLLM(t *testing.T) {
	got := manualModelsByEngine(manualNodeStatus{
		OllamaModels: []string{"llama3.2:1b"},
		VLLMModels:   []string{"Qwen/Qwen3-8B"},
	})
	if len(got["vllm"]) != 1 || got["vllm"][0] != "Qwen/Qwen3-8B" {
		t.Errorf("modelsByEngine = %+v, want a vllm key", got)
	}
	if _, ok := got["lmstudio"]; ok {
		t.Errorf("an engine with no models must add no key: %+v", got)
	}
}

func registrationFor(regs []noderec.RegisterParams, svc noderec.ServiceKey) (noderec.RegisterParams, bool) {
	for _, r := range regs {
		if r.Service == svc {
			return r, true
		}
	}
	return noderec.RegisterParams{}, false
}
