// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"nvpair-shared/engines"
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
}

func TestRunningEngine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		port  int
		slots *engineSlots
		ok    bool
	}{
		{name: "running", raw: `{"running":true,"port":1235}`, port: 1235, ok: true},
		{
			name:  "running with slots",
			raw:   `{"running":true,"port":1235,"slots":{"default":2,"models":{"qwen/qwen3-8b":4}}}`,
			port:  1235,
			slots: &engineSlots{Default: 2, Models: map[string]int{"qwen/qwen3-8b": 4}},
			ok:    true,
		},
		{name: "malformed slots cost only the slots", raw: `{"running":true,"port":1235,"slots":{"default":"2"}}`, port: 1235, ok: true},
		{name: "null slots", raw: `{"running":true,"port":1235,"slots":null}`, port: 1235, ok: true},
		{name: "stopped is authoritative", raw: `{"running":false,"port":1235,"slots":{"default":2}}`},
		{name: "running without a port", raw: `{"running":true,"port":0}`},
		{name: "malformed response", raw: `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, slots, ok := runningEngine([]byte(tc.raw))
			if port != tc.port || ok != tc.ok || !reflect.DeepEqual(slots, tc.slots) {
				t.Fatalf("runningEngine = (%d, %+v, %v), want (%d, %+v, %v)", port, slots, ok, tc.port, tc.slots, tc.ok)
			}
		})
	}
}

// The advertise loop's engine:status read is the only writer of the slot cache
// that setProxyLocalBackend relays.
func TestLocalEnginePortUpdatesCachedSlots(t *testing.T) {
	cached := &engineSlots{Default: 2}
	test := func(name string, reply func(*Codec, *Message) error, wantPort int, wantProbe bool, want *engineSlots) {
		t.Run(name, func(t *testing.T) {
			worker, manager := newTestRPCWorkerPipe(t)
			b := &Broker{}
			b.setEngineMgr(worker)
			b.engineSlots.set("ollama", cached)
			replied := make(chan error, 1)
			go func() {
				msg, err := manager.Read()
				if err != nil {
					replied <- err
					return
				}
				replied <- reply(manager, msg)
			}()

			port, probe := b.localEnginePort("ollama", defaultOllamaPort)
			if err := <-replied; err != nil {
				t.Fatalf("engine-manager reply: %v", err)
			}
			if port != wantPort || probe != wantProbe {
				t.Fatalf("localEnginePort = (%d, %v), want (%d, %v)", port, probe, wantPort, wantProbe)
			}
			if got := b.engineSlots.get("ollama"); !reflect.DeepEqual(got, want) {
				t.Fatalf("cached slots = %+v, want %+v", got, want)
			}
		})
	}
	respond := func(result string) func(*Codec, *Message) error {
		return func(c *Codec, msg *Message) error { return c.Respond(msg.ID, json.RawMessage(result)) }
	}
	test("a running engine's slots replace the cached ones",
		respond(`{"running":true,"port":11435,"slots":{"default":3}}`), 11435, true, &engineSlots{Default: 3})
	test("a running engine without slots clears them",
		respond(`{"running":true,"port":11435}`), 11435, true, nil)
	test("a stopped engine clears them",
		respond(`{"running":false,"port":11435}`), 0, false, nil)
	test("an RPC failure keeps them",
		func(c *Codec, msg *Message) error { return c.RespondError(msg.ID, -32000, "engine-manager busy") },
		defaultOllamaPort, true, cached)
}

// fakeLocalBackendProxy returns a ready proxy for engine and a channel that
// receives every node/set-local-backend payload sent to it.
func fakeLocalBackendProxy(t *testing.T, engine string, port int) (*proxyProcess, <-chan proxyLocalBackend) {
	t.Helper()
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{
		peer:        NewPeer(NewCodec(proxyClient)),
		facadeState: readyFacade(engine, port),
	}
	go proxy.peer.Serve(nil, nil)
	payloads := make(chan proxyLocalBackend, 4)
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			var got proxyLocalBackend
			if msg.Method == engines.AddressMethod(engine, "node/set-local-backend") && json.Unmarshal(msg.Params, &got) == nil {
				payloads <- got
			}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()
	return proxy, payloads
}

func receiveLocalBackend(t *testing.T, payloads <-chan proxyLocalBackend) proxyLocalBackend {
	t.Helper()
	select {
	case got := <-payloads:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("proxy received no node/set-local-backend")
		return proxyLocalBackend{}
	}
}

// Every node/set-local-backend goes through setProxyLocalBackend, so this is
// what each of its callers sends.
func TestSetProxyLocalBackendRelaysCachedSlots(t *testing.T) {
	slots := &engineSlots{Default: 2, Models: map[string]int{"qwen/qwen3-8b": 4}}
	test := func(name string, cached *engineSlots, healthy bool, want *engineSlots) {
		t.Run(name, func(t *testing.T) {
			proxy, payloads := fakeLocalBackendProxy(t, "lmstudio", defaultLMStudioPort)
			b := &Broker{}
			b.engineSlots.set("lmstudio", cached)
			b.setProxyLocalBackend(proxy, "lmstudio", 1235, healthy)
			got := receiveLocalBackend(t, payloads)
			if got.Port != 1235 || got.Healthy != healthy || !reflect.DeepEqual(got.Slots, want) {
				t.Fatalf("node/set-local-backend = %+v with slots %+v, want port 1235, healthy %v, slots %+v",
					got, got.Slots, healthy, want)
			}
		})
	}
	test("a healthy backend carries the cached slots", slots, true, slots)
	test("an unhealthy backend carries none", slots, false, nil)
	test("a healthy backend with nothing cached carries none", nil, true, nil)
}

// TestReconcileAdvertiseRelaysEngineSlots drives the advertise loop end to end:
// engine-manager reports slots, the engine answers its health probe, and the
// proxy's local backend arrives carrying those slots.
func TestReconcileAdvertiseRelaysEngineSlots(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(engine.Close)
	enginePort := engine.Listener.Addr().(*net.TCPAddr).Port

	worker, manager := newTestRPCWorkerPipe(t)
	go func() {
		msg, err := manager.Read()
		if err != nil {
			return
		}
		_ = manager.Respond(msg.ID, map[string]any{
			"running": true,
			"port":    enginePort,
			"slots":   map[string]any{"default": 3},
		})
	}()
	proxy, payloads := fakeLocalBackendProxy(t, ollamaProxyProfile.Name, defaultOllamaPort)
	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setEngineMgr(worker)
	b.setProxy(proxy)

	b.reconcileAdvertise(&http.Client{Timeout: 2 * time.Second})
	got := receiveLocalBackend(t, payloads)
	if !got.Healthy || got.Port != enginePort || !reflect.DeepEqual(got.Slots, &engineSlots{Default: 3}) {
		t.Fatalf("node/set-local-backend = %+v with slots %+v, want healthy port %d with 3 slots", got, got.Slots, enginePort)
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
