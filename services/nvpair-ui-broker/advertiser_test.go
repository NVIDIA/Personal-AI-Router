// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"nvpair-ui-broker/relay"
)

// TestLocalEnginePortFallback: with no engine-manager supervised, the broker
// falls back to the engine's stock port rather than failing — so a broker
// running without engine-manager still advertises at the sensible default.
func TestLocalEnginePortFallback(t *testing.T) {
	b := &Broker{} // no engine-manager worker
	if got, ok := b.localEnginePort("ollama", defaultOllamaPort); !ok || got != defaultOllamaPort {
		t.Errorf("no engine-manager: localEnginePort = (%d, %v), want (%d, true)", got, ok, defaultOllamaPort)
	}
	if got, ok := b.localEnginePort("lmstudio", defaultLMStudioPort); !ok || got != defaultLMStudioPort {
		t.Errorf("no engine-manager: localEnginePort = (%d, %v), want (%d, true)", got, ok, defaultLMStudioPort)
	}
	if got, ok := b.localEnginePort("llamacpp", defaultLlamaCppPort); !ok || got != defaultLlamaCppPort {
		t.Errorf("no engine-manager: localEnginePort = (%d, %v), want (%d, true)", got, ok, defaultLlamaCppPort)
	}
}

func TestRunningEnginePort(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		port int
		ok   bool
	}{
		{name: "running", raw: `{"running":true,"port":1235}`, port: 1235, ok: true},
		{name: "explicitly routable", raw: `{"running":true,"routable":true,"port":8001}`, port: 8001, ok: true},
		{name: "disabled is not routable", raw: `{"running":true,"routable":false,"port":8001}`},
		{name: "stopped is authoritative", raw: `{"running":false,"port":1235}`},
		{name: "running without a port", raw: `{"running":true,"port":0}`},
		{name: "malformed response", raw: `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, ok := runningEnginePort([]byte(tc.raw))
			if port != tc.port || ok != tc.ok {
				t.Fatalf("runningEnginePort = (%d, %v), want (%d, %v)", port, ok, tc.port, tc.ok)
			}
		})
	}
}

func TestRoutableEnginePortRequiresStandaloneOrReadyCoordinator(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "standalone", raw: `{"engine":"vllm","running":true,"healthy":true,"routable":true,"port":8001}`, ok: true},
		{name: "ready coordinator while standalone disabled", raw: `{"engine":"vllm","running":true,"healthy":true,"enabled":false,"routable":false,"port":8001,"serving_group":{"role":"coordinator","state":"ready"}}`, ok: true},
		{name: "wrong engine", raw: `{"engine":"ollama","running":true,"healthy":true,"routable":true,"port":8001}`},
		{name: "disabled standalone", raw: `{"engine":"vllm","running":true,"healthy":true,"routable":false,"port":8001}`},
		{name: "ready participant", raw: `{"engine":"vllm","running":true,"healthy":true,"routable":false,"port":8001,"serving_group":{"role":"participant","state":"ready"}}`},
		{name: "starting coordinator", raw: `{"engine":"vllm","running":true,"healthy":true,"routable":false,"port":8001,"serving_group":{"role":"coordinator","state":"starting"}}`},
		{name: "collectively ready coordinator awaiting its route", raw: `{"engine":"vllm","running":true,"healthy":true,"routable":false,"port":8001,"serving_group":{"role":"coordinator","state":"starting","routing":true}}`, ok: true},
		{name: "routing participant", raw: `{"engine":"vllm","running":true,"healthy":true,"routable":false,"port":8001,"serving_group":{"role":"participant","state":"starting","routing":true}}`},
		{name: "routing flag on a stopping coordinator", raw: `{"engine":"vllm","running":true,"healthy":true,"routable":false,"port":8001,"serving_group":{"role":"coordinator","state":"stopping","routing":true}}`},
		{name: "unhealthy", raw: `{"engine":"vllm","running":true,"healthy":false,"routable":true,"port":8001}`},
		{name: "unhealthy ready coordinator", raw: `{"engine":"vllm","running":true,"healthy":false,"routable":false,"port":8001,"serving_group":{"role":"coordinator","state":"ready"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, ok := routableEnginePort([]byte(tc.raw), "vllm")
			if ok != tc.ok || (ok && port != 8001) {
				t.Fatalf("routableEnginePort = (%d, %v), want (8001, %v)", port, ok, tc.ok)
			}
		})
	}
}

func TestLMStudioFallbackNeverAdvertisesItsProxy(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()
	proxy := &proxyProcess{
		peer:        NewPeer(NewCodec(proxyClient)),
		facadeState: readyFacade(lmstudioProxyProfile.Name, defaultLMStudioPort),
	}
	go proxy.peer.Serve(nil, nil)

	localBackend := make(chan proxyLocalBackend, 1)
	go func() {
		codec := NewCodec(proxyServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var got proxyLocalBackend
		if json.Unmarshal(msg.Params, &got) == nil {
			localBackend <- got
		}
		_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
	}()

	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLMStudioProxy(proxy)

	// A nil client is intentional: collision detection must short-circuit before
	// any health request can mistake the proxy for LM Studio.
	b.reconcileAdvertiseLMStudio(nil)
	if got := b.regCache.Snapshot(); len(got) != 0 {
		t.Fatalf("LM Studio proxy was advertised as an engine: %+v", got)
	}
	select {
	case got := <-localBackend:
		if got.Port != 0 || got.Healthy {
			t.Fatalf("proxy listener was retained as the local backend: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LM Studio proxy did not receive a cleared local backend")
	}
}

// TestLMStudioFallbackDoesNotOverwriteKnownBackend: when engine-manager is
// unavailable, localEnginePort hands back the stock-port fallback (1234, the
// facade port). The advertiser must not promote that guess into the confirmed
// backend cache, or a later proxy-ready reconcile mistakes the compatibility
// proxy on :1234 for the backend and disables managed mode.
func TestLMStudioFallbackDoesNotOverwriteKnownBackend(t *testing.T) {
	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)

	// No engine-manager and no proxy: the fallback path that used to poison
	// the cache with defaultLMStudioPort.
	b.reconcileAdvertiseLMStudio(nil)

	if got := int(b.lmstudioState().backendPort.Load()); got != managedLMStudioBackendStart {
		t.Fatalf("backend cache = %d, want %d (fallback must not overwrite the confirmed backend)", got, managedLMStudioBackendStart)
	}
}

// TestProxyListenPortNoProxy: with no proxy supervised, proxyListenPort is 0,
// so the self-forward collision check (port == proxy port) never falsely trips.
func TestProxyListenPortNoProxy(t *testing.T) {
	b := &Broker{} // no proxy worker
	if got := b.proxyListenPort(); got != 0 {
		t.Errorf("no proxy: proxyListenPort = %d, want 0", got)
	}
}

func TestOllamaFacadeIsPendingBackend(t *testing.T) {
	b := &Broker{}
	b.ollamaState().managedFacade.Store(true)
	b.ollamaState().backendPort.Store(managedOllamaFacadePort)
	if !b.ollamaFacadeIsPendingBackend() {
		t.Fatal("managed facade must block liveness probes while the backend still points at 11434")
	}
	b.ollamaState().backendPort.Store(11435)
	if b.ollamaFacadeIsPendingBackend() {
		t.Fatal("liveness probes should resume after the backend moves off 11434")
	}
	b.managedOllamaBackend.Store(11436)
	if !b.ollamaFacadeIsPendingBackend() {
		t.Fatal("a pending 11435 to 11436 move must keep liveness probes gated")
	}
	b.managedOllamaBackend.Store(0)
	b.ollamaMoveInFlight.Store(true)
	if !b.ollamaFacadeIsPendingBackend() {
		t.Fatal("an in-flight backend move must keep liveness probes gated")
	}
	b.ollamaMoveInFlight.Store(false)

	b.ollamaState().managedFacade.Store(false)
	b.ollamaState().backendPort.Store(managedOllamaFacadePort)
	b.setProxy(&proxyProcess{
		facadeState: readyFacade(ollamaProxyProfile.Name, managedOllamaFacadePort),
	})
	if !b.ollamaFacadeIsPendingBackend() {
		t.Fatal("recovery must keep probes blocked until the proxy vacates 11434")
	}
}
