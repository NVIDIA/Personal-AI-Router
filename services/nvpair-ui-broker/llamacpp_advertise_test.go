// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

func TestReconcileAdvertiseLlamaCppRegistersProxyPort(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(engine.Close)
	enginePort := engine.Listener.Addr().(*net.TCPAddr).Port
	if enginePort == defaultLlamaCppProxyPort {
		t.Fatal("httptest bound the llama.cpp proxy port; cannot distinguish engine from proxy")
	}

	proxy, localBackend := llamaCppProxyPipe(t, defaultLlamaCppProxyPort)
	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLlamaCppProxy(proxy)
	b.setEngineMgr(serveEngineStatus(t, enginePort))

	b.reconcileAdvertiseLlamaCpp(&http.Client{Timeout: 2 * time.Second})

	got, ok := registrationFor(b, noderec.ServiceLlamaCpp)
	if !ok || got.Port != defaultLlamaCppProxyPort {
		t.Fatalf("lc registration = (%+v, %v), want port %d", got, ok, defaultLlamaCppProxyPort)
	}
	select {
	case backend := <-localBackend:
		if backend.Engine != "llamacpp" || backend.Port != enginePort || !backend.Healthy {
			t.Fatalf("local backend = %+v, want healthy llamacpp:%d", backend, enginePort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("llama.cpp proxy did not receive a healthy local backend")
	}
}

func TestLlamaCppFallbackNeverAdvertisesItsProxy(t *testing.T) {
	proxy, localBackend := llamaCppProxyPipe(t, defaultLlamaCppPort)
	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLlamaCppProxy(proxy)

	// A nil client is intentional: collision detection must short-circuit before
	// any health request can mistake the proxy for llama.cpp.
	b.reconcileAdvertiseLlamaCpp(nil)
	if got := b.regCache.Snapshot(); len(got) != 0 {
		t.Fatalf("llama.cpp proxy was advertised as an engine: %+v", got)
	}
	select {
	case got := <-localBackend:
		if got.Port != 0 || got.Healthy {
			t.Fatalf("proxy listener was retained as the local backend: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("llama.cpp proxy did not receive a cleared local backend")
	}
}

func TestReconcileAdvertiseLlamaCppUnregistersWhenEngineDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	proxy, localBackend := llamaCppProxyPipe(t, defaultLlamaCppProxyPort)
	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLlamaCppProxy(proxy)
	b.setEngineMgr(serveEngineStatus(t, deadPort))
	b.registerService(noderec.RegisterParams{Service: noderec.ServiceLlamaCpp, Port: defaultLlamaCppProxyPort})

	b.reconcileAdvertiseLlamaCpp(&http.Client{Timeout: 2 * time.Second})

	if _, ok := registrationFor(b, noderec.ServiceLlamaCpp); ok {
		t.Fatal("lc stayed registered while the engine was down")
	}
	select {
	case got := <-localBackend:
		if got.Healthy {
			t.Fatalf("local backend stayed healthy while the engine was down: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("llama.cpp proxy did not receive an unhealthy local backend")
	}
}

func llamaCppProxyPipe(t *testing.T, listenPort int) (*proxyProcess, <-chan proxyLocalBackend) {
	t.Helper()
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{
		peer:  NewPeer(NewCodec(proxyClient)),
		ready: true,
		port:  listenPort,
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
	return proxy, localBackend
}

func serveEngineStatus(t *testing.T, port int) *rpcWorker {
	t.Helper()
	engineClient, engineServer := net.Pipe()
	t.Cleanup(func() {
		_ = engineClient.Close()
		_ = engineServer.Close()
	})
	engine := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go engine.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(engineServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		_ = codec.Respond(msg.ID, map[string]any{"running": true, "port": port})
	}()
	return engine
}

func registrationFor(b *Broker, svc noderec.ServiceKey) (noderec.RegisterParams, bool) {
	for _, p := range b.regCache.Snapshot() {
		if p.Service == svc {
			return p, true
		}
	}
	return noderec.RegisterParams{}, false
}
