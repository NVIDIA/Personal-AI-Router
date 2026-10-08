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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// TestLocalEnginePortFallback: with no engine-manager supervised, the broker
// falls back to the engine's stock port rather than failing — so a broker
// running without engine-manager still advertises at the sensible default.
func TestLocalEnginePortFallback(t *testing.T) {
	b := &Broker{} // no engine-manager worker
	got, ok := b.localEnginePort("ollama", defaultOllamaPort)
	assert.True(t, ok, "no engine-manager: Ollama port fallback")
	assert.Equal(t, defaultOllamaPort, got, "no engine-manager: Ollama port fallback")
	got, ok = b.localEnginePort("lmstudio", defaultLMStudioPort)
	assert.True(t, ok, "no engine-manager: LM Studio port fallback")
	assert.Equal(t, defaultLMStudioPort, got, "no engine-manager: LM Studio port fallback")
}

func TestRunningEnginePort(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		port int
		ok   bool
	}{
		{name: "running", raw: `{"running":true,"port":1235}`, port: 1235, ok: true},
		{name: "stopped is authoritative", raw: `{"running":false,"port":1235}`},
		{name: "running without a port", raw: `{"running":true,"port":0}`},
		{name: "malformed response", raw: `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, ok := runningEnginePort([]byte(tc.raw))
			require.Equal(t, tc.port, port, "runningEnginePort")
			require.Equal(t, tc.ok, ok, "runningEnginePort")
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
	require.Empty(t, b.regCache.Snapshot(), "LM Studio proxy was advertised as an engine")
	select {
	case got := <-localBackend:
		require.Equal(t, 0, got.Port, "proxy listener was retained as the local backend (%v)", got)
		require.False(t, got.Healthy, "proxy listener was retained as the local backend (%v)", got)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "LM Studio proxy did not receive a cleared local backend")
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

	require.Equal(t, managedLMStudioBackendStart, int(b.lmstudioState().backendPort.Load()), "fallback must not overwrite the confirmed backend")
}

func TestEngineAdvertiserTracksEngineHealth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	_, portText, err := net.SplitHostPort(backend.Listener.Addr().String())
	require.NoError(t, err)
	backendPort, err := strconv.Atoi(portText)
	require.NoError(t, err)
	proxyPort := 44000
	if backendPort == proxyPort {
		proxyPort++
	}

	profile := testDefaultEngineProxyProfile()
	profile.DiscoveryService = noderec.ServiceLMStudio
	b := brokerWithEngineProxyProfile(profile)
	b.regCache = relay.NewRegistrationCache()
	b.engineProxy(profile).backendPort.Store(int32(backendPort))
	updates := attachAdvertiserProxy(t, b, profile, proxyPort)

	b.reconcileAdvertiseEngine(profile, backend.Client())
	registrations := b.regCache.Snapshot()
	require.Len(t, registrations, 1, "healthy registration")
	assert.Equal(t, profile.DiscoveryService, registrations[0].Service)
	assert.Equal(t, proxyPort, registrations[0].Port)
	update := <-updates
	assert.True(t, update.Healthy, "healthy local backend")
	assert.Equal(t, backendPort, update.Port)
	assert.Equal(t, profile.Name, update.Engine)

	backend.Close()
	b.reconcileAdvertiseEngine(profile, backend.Client())
	assert.Empty(t, b.regCache.Snapshot(), "unhealthy engine remained advertised")
	update = <-updates
	assert.False(t, update.Healthy, "unhealthy local backend")
	assert.Equal(t, backendPort, update.Port)
}

func TestEngineAdvertiserRejectsSelfForwardLoop(t *testing.T) {
	profile := testDefaultEngineProxyProfile()
	profile.DiscoveryService = noderec.ServiceLMStudio
	b := brokerWithEngineProxyProfile(profile)
	b.regCache = relay.NewRegistrationCache()
	b.regCache.Register(noderec.RegisterParams{Service: profile.DiscoveryService, Port: 44000})
	b.engineProxy(profile).backendPort.Store(44000)
	updates := attachAdvertiserProxy(t, b, profile, 44000)

	// A nil client proves the collision check short-circuits before probing the
	// facade as though it were the backend.
	b.reconcileAdvertiseEngine(profile, nil)
	assert.Empty(t, b.regCache.Snapshot(), "self-forwarding facade remained advertised")
	assert.False(t, (<-updates).Healthy, "self-forwarding backend remained healthy")
}

func attachAdvertiserProxy(
	t *testing.T,
	b *Broker,
	profile engineProxyProfile,
	port int,
) <-chan proxyLocalBackend {
	t.Helper()
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{
		peer:        NewPeer(NewCodec(proxyClient)),
		facadeState: readyFacade(profile.Name, port),
	}
	go proxy.peer.Serve(nil, nil)
	b.setEngineProxyHandle(profile, proxy)

	updates := make(chan proxyLocalBackend, 4)
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			var update proxyLocalBackend
			if json.Unmarshal(msg.Params, &update) == nil {
				updates <- update
			}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()
	return updates
}

// TestProxyListenPortNoProxy: with no proxy supervised, proxyListenPort is 0,
// so the self-forward collision check (port == proxy port) never falsely trips.
func TestProxyListenPortNoProxy(t *testing.T) {
	b := &Broker{} // no proxy worker
	assert.Equal(t, 0, b.proxyListenPort(), "no proxy: proxyListenPort")
}

func TestOllamaFacadeIsPendingBackend(t *testing.T) {
	b := &Broker{}
	b.ollamaState().managedFacade.Store(true)
	b.ollamaState().backendPort.Store(managedOllamaFacadePort)
	require.True(t, b.ollamaFacadeIsPendingBackend(), "managed facade must block liveness probes while the backend still points at 11434")
	b.ollamaState().backendPort.Store(11435)
	require.False(t, b.ollamaFacadeIsPendingBackend(), "liveness probes should resume after the backend moves off 11434")
	b.managedOllamaBackend.Store(11436)
	require.True(t, b.ollamaFacadeIsPendingBackend(), "a pending 11435 to 11436 move must keep liveness probes gated")
	b.managedOllamaBackend.Store(0)
	b.ollamaMoveInFlight.Store(true)
	require.True(t, b.ollamaFacadeIsPendingBackend(), "an in-flight backend move must keep liveness probes gated")
	b.ollamaMoveInFlight.Store(false)

	b.ollamaState().managedFacade.Store(false)
	b.ollamaState().backendPort.Store(managedOllamaFacadePort)
	b.setProxy(&proxyProcess{
		facadeState: readyFacade(ollamaProxyProfile.Name, managedOllamaFacadePort),
	})
	require.True(t, b.ollamaFacadeIsPendingBackend(), "recovery must keep probes blocked until the proxy vacates 11434")
}
