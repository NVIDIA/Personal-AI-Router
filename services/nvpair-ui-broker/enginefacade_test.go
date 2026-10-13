// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	settings "nvpair-shared/enginesettings"
)

func testDefaultEngineProxyProfile() engineProxyProfile {
	return engineProxyProfile{
		Engine: engines.Engine{
			Name:           "fixedtest",
			DisplayName:    "Fixed Test",
			FacadePort:     1233,
			EnginePortBase: 1234,
			PortFile:       "fixedtest-proxy-port.json",
		},
		Ownership:       managedEngine,
		HealthProbePath: "/health",
	}
}

func brokerWithEngineProxyProfile(profile engineProxyProfile) *Broker {
	b := &Broker{}
	b.engineProxiesOnce.Do(func() {
		b.engineProxies = map[string]*engineProxyRuntime{
			profile.Name: {profile: profile},
		}
	})
	return b
}

func TestDefaultEngineFacadeRetriesAwayFromReservedPorts(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	profile := testDefaultEngineProxyProfile()
	b := brokerWithEngineProxyProfile(profile)

	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	attempts := make(chan enableFacadeRequest, 2)
	serveFacadeEnable(t, proxyServer, map[int]bool{profile.FacadePort: true}, attempts)

	err := b.enableEngineFacadeWithPortCheck(
		context.Background(),
		proxy,
		profile,
		ollamaHostAlias{},
		func(int) bool { return true },
	)
	require.NoError(t, err, "enable engine facade")
	first, second := <-attempts, <-attempts
	assert.Equal(t, profile.FacadePort, first.Port, "first attempt must use the stock facade port")
	// 1234 is this backend and LM Studio's facade; 1235 is LM Studio's backend.
	assert.Equal(t, 1236, second.Port, "fallback must exclude backend and sibling ports")
	assert.True(t, second.IgnorePersistedPort, "fallback retry could restore the port that just failed")

	restart := b.defaultEngineFacadeSpec(profile)
	assert.Equal(t, second.Port, restart.Port, "restart must use the fallback port")
	assert.True(t, restart.IgnorePersistedPort, "restart must ignore the persisted port")
}

func TestLlamaCPPFacadePreparationPreservesConfiguredPorts(t *testing.T) {
	profile := mustEngineProxyProfile("llamacpp")
	for _, tc := range []struct {
		name       string
		serverPort int
		proxyPort  int
		explicit   bool
	}{
		{name: "manifest default", serverPort: profile.EnginePortBase},
		{name: "explicit settings", serverPort: 18081, proxyPort: 18080, explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Broker{
				proxyPath:            "test-proxy",
				proxyEngines:         []string{profile.Name},
				engineSettingsLoaded: true,
			}
			if tc.explicit {
				b.engineSettings = map[string]*engineSettingsRecord{
					profile.Name: {Explicit: true, Snapshot: settings.Snapshot{
						Settings: settings.Config{ServerPort: tc.serverPort, ProxyPort: tc.proxyPort},
					}},
				}
			}
			worker, codec := newTestRPCWorkerPipe(t)
			b.setEngineMgr(worker)
			var calls atomic.Int32
			go func() {
				for {
					msg, err := codec.Read()
					if err != nil {
						return
					}
					calls.Add(1)
					if !assert.NoError(t, codec.Respond(msg.ID, ollamaPortStatus{Running: true, Port: tc.serverPort}), "respond to unexpected engine request %s", msg.Method) {
						return
					}
				}
			}()

			b.prepareEnabledFacades()

			assert.Equal(t, int32(0), calls.Load(), "preparation must not probe or relocate the engine")
			state := b.engineProxy(profile)
			assert.Equal(t, int32(tc.serverPort), state.backendPort.Load(), "engine port")
			assert.Equal(t, int32(tc.proxyPort), state.startupPort.Load(), "startup proxy port")
			assert.Equal(t, tc.explicit, state.explicitSettings.Load(), "explicit settings")
		})
	}
}

func TestLlamaCPPProxyTerminalHandlingPreservesEngineState(t *testing.T) {
	profile := mustEngineProxyProfile("llamacpp")
	b := &Broker{ollamaPortReady: make(chan struct{}), lmstudioPortReady: make(chan struct{})}
	state := b.engineProxy(profile)
	state.backendPort.Store(int32(profile.EnginePortBase))
	state.startupPort.Store(18080)
	state.managedFacade.Store(true)

	b.blockAndFinishEngineProxy(profile)
	b.finishEngineProxyStartup(profile)

	assert.Equal(t, int32(profile.EnginePortBase), state.backendPort.Load(), "engine port")
	assert.Equal(t, int32(18080), state.startupPort.Load(), "terminal handling changed facade port")
	assert.True(t, state.managedFacade.Load(), "terminal handling changed facade ownership")
	for _, gate := range []struct {
		name  string
		ready <-chan struct{}
	}{{"Ollama", b.ollamaPortReady}, {"LM Studio", b.lmstudioPortReady}} {
		select {
		case <-gate.ready:
			require.FailNowf(t, "llama.cpp terminal handling released another engine's gate", "engine %s", gate.name)
		default:
		}
	}
}

func TestLlamaCPPEngineStatusRelaysBeforeOtherPortGates(t *testing.T) {
	profile := mustEngineProxyProfile("llamacpp")
	b := brokerWithEngineStatus(t, profile.Name, profile.EnginePortBase)
	b.ollamaPortReady = make(chan struct{})
	b.lmstudioPortReady = make(chan struct{})
	b.managedOllamaBackend.Store(managedOllamaBackendStart)
	client, server := net.Pipe()
	t.Cleanup(func() {
		close(b.ollamaPortReady)
		close(b.lmstudioPortReady)
		_ = client.Close()
		_ = server.Close()
	})
	b.codec = NewCodec(server)
	id := json.RawMessage(`1`)

	b.relayToEngine(&Message{ID: &id, Method: "engine:status", Params: json.RawMessage(`{"engine":"llamacpp"}`)})

	require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)), "set response deadline")
	response, err := NewCodec(client).Read()
	require.NoError(t, err, "read llama.cpp status while other port gates are pending")
	require.Nil(t, response.Error, "llama.cpp status failed")
	var status ollamaPortStatus
	require.NoError(t, json.Unmarshal(response.Result, &status), "decode llama.cpp status")
	assert.Equal(t, profile.EnginePortBase, status.Port)
}

func TestLlamaCPPProxyNotificationDispatchPreservesFacadeAddress(t *testing.T) {
	profile := mustEngineProxyProfile("llamacpp")
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	b := &Broker{codec: NewCodec(server)}
	b.proxyMu.Lock()
	b.setEngineProxySubscribed(profile, true)
	b.proxyMu.Unlock()

	payload := json.RawMessage(`{"port":8080}`)
	done := make(chan struct{})
	go func() {
		b.forwardProxyProcessNotification(0, 0, profile.addressed("ready"), payload)
		close(done)
	}()
	require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)), "set read deadline")
	msg, err := NewCodec(client).Read()
	require.NoError(t, err, "read forwarded notification")
	assert.Equal(t, profile.ComponentName()+":ready", msg.Method)
	var ready proxyReadyParams
	require.NoError(t, json.Unmarshal(msg.Params, &ready), "decode ready notification")
	assert.Equal(t, 8080, ready.Port)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "notification forwarding did not finish")
	}
}
