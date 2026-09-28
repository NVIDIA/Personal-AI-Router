// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestManualModelsByEngineIncludesLlamaCpp(t *testing.T) {
	s := manualNodeStatus{
		ID:             "lab",
		OllamaModels:   []string{"llama3"},
		LMStudioModels: []string{"qwen"},
		LlamaCppModels: []string{"loaded-one"},
	}
	got := manualModelsByEngine(s)
	if len(got["llamacpp"]) != 1 || got["llamacpp"][0] != "loaded-one" {
		t.Fatalf("llamacpp = %#v, want [loaded-one]", got["llamacpp"])
	}
	en := manualToEnriched(s)
	found := false
	for _, m := range en.Models {
		if m == "loaded-one" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Models missing loaded-one: %#v", en.Models)
	}
}

func TestManualNodeBridgesLlamaCppIntoProxy(t *testing.T) {
	proxy, calls := llamaCppManualProxyPipe(t)
	b := newManualTestBroker()
	b.setLlamaCppProxy(proxy)

	b.bridgeManualNode(manualNodeStatus{
		ID:             "lab",
		Address:        "10.0.0.5",
		LlamaCppUp:     true,
		LlamaCppPort:   8082,
		LlamaCppModels: []string{"loaded-one"},
	}, "lab")

	call := readProxyManualCall(t, calls)
	// Addressed to the facade, not bare: one process hosts every engine, so
	// the engine has to travel with the message.
	if want := llamacppProxyProfile.addressed("node/add-manual"); call.method != want {
		t.Fatalf("method = %q, want %q", call.method, want)
	}
	var node proxyManualNode
	if err := json.Unmarshal(call.params, &node); err != nil {
		t.Fatalf("decode add-manual: %v", err)
	}
	if node.ID != "lab" || node.Host != "10.0.0.5" || node.Port != 8082 {
		t.Fatalf("bridged node = %+v", node)
	}
	if len(node.Models) != 1 || node.Models[0] != "loaded-one" {
		t.Fatalf("models = %#v, want [loaded-one]", node.Models)
	}
}

func TestManualNodeRemovesLlamaCppFromProxyWhenDown(t *testing.T) {
	proxy, calls := llamaCppManualProxyPipe(t)
	b := newManualTestBroker()
	b.setLlamaCppProxy(proxy)

	b.bridgeManualNode(manualNodeStatus{
		ID:           "lab",
		Address:      "10.0.0.5",
		LlamaCppUp:   false,
		LlamaCppPort: 8082,
	}, "lab")

	call := readProxyManualCall(t, calls)
	if want := llamacppProxyProfile.addressed("node/remove-manual"); call.method != want {
		t.Fatalf("method = %q, want %q", call.method, want)
	}
	var params map[string]string
	if err := json.Unmarshal(call.params, &params); err != nil {
		t.Fatalf("decode remove-manual: %v", err)
	}
	if params["id"] != "lab" {
		t.Fatalf("remove id = %q, want lab", params["id"])
	}
}

func TestRemoveManualNodeFromProxiesDropsLlamaCpp(t *testing.T) {
	proxy, calls := llamaCppManualProxyPipe(t)
	b := newManualTestBroker()
	b.setLlamaCppProxy(proxy)

	b.removeManualNodeFromProxies("lab")

	call := readProxyManualCall(t, calls)
	if want := llamacppProxyProfile.addressed("node/remove-manual"); call.method != want {
		t.Fatalf("method = %q, want %q", call.method, want)
	}
	var params map[string]string
	if err := json.Unmarshal(call.params, &params); err != nil {
		t.Fatalf("decode remove-manual: %v", err)
	}
	if params["id"] != "lab" {
		t.Fatalf("remove id = %q, want lab", params["id"])
	}
}

type proxyManualCall struct {
	method string
	params json.RawMessage
}

func llamaCppManualProxyPipe(t *testing.T) (*proxyProcess, <-chan proxyManualCall) {
	t.Helper()
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	calls := make(chan proxyManualCall, 1)
	go func() {
		codec := NewCodec(proxyServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		calls <- proxyManualCall{method: msg.Method, params: msg.Params}
		_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
	}()
	return proxy, calls
}

func readProxyManualCall(t *testing.T, calls <-chan proxyManualCall) proxyManualCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for llamacpp-proxy manual-node call")
		return proxyManualCall{}
	}
}
