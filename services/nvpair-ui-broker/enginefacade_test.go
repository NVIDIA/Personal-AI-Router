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
	if err != nil {
		t.Fatalf("enable engine facade: %v", err)
	}
	first, second := <-attempts, <-attempts
	if first.Port != profile.FacadePort {
		t.Fatalf("first port = %d, want stock facade %d", first.Port, profile.FacadePort)
	}
	// 1234 is this backend and LM Studio's facade; 1235 is LM Studio's backend.
	if second.Port != 1236 {
		t.Fatalf("fallback port = %d, want 1236 after backend and sibling exclusions", second.Port)
	}
	if !second.IgnorePersistedPort {
		t.Fatal("fallback retry could restore the port that just failed")
	}

	restart := b.defaultEngineFacadeSpec(profile)
	if restart.Port != second.Port || !restart.IgnorePersistedPort {
		t.Fatalf("restart spec = %+v, want explicit fallback port %d", restart, second.Port)
	}
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
					if err := codec.Respond(msg.ID, ollamaPortStatus{Running: true, Port: tc.serverPort}); err != nil {
						t.Errorf("respond to unexpected engine request %s: %v", msg.Method, err)
						return
					}
				}
			}()

			b.prepareEnabledFacades()

			if got := calls.Load(); got != 0 {
				t.Fatalf("preparation issued %d engine requests, want no probing or relocation", got)
			}
			state := b.engineProxy(profile)
			if got := int(state.backendPort.Load()); got != tc.serverPort {
				t.Fatalf("engine port = %d, want %d", got, tc.serverPort)
			}
			if got := int(state.startupPort.Load()); got != tc.proxyPort {
				t.Fatalf("startup proxy port = %d, want %d", got, tc.proxyPort)
			}
			if got := state.explicitSettings.Load(); got != tc.explicit {
				t.Fatalf("explicit settings = %v, want %v", got, tc.explicit)
			}
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

	if got := int(state.backendPort.Load()); got != profile.EnginePortBase {
		t.Fatalf("engine port = %d, want %d", got, profile.EnginePortBase)
	}
	if got := state.startupPort.Load(); got != 18080 || !state.managedFacade.Load() {
		t.Fatalf("terminal handling changed facade state: port=%d managed=%v", got, state.managedFacade.Load())
	}
	for _, gate := range []struct {
		name  string
		ready <-chan struct{}
	}{{"Ollama", b.ollamaPortReady}, {"LM Studio", b.lmstudioPortReady}} {
		select {
		case <-gate.ready:
			t.Fatalf("llama.cpp terminal handling released %s's gate", gate.name)
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

	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set response deadline: %v", err)
	}
	response, err := NewCodec(client).Read()
	if err != nil {
		t.Fatalf("read llama.cpp status while other port gates are pending: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("llama.cpp status failed: %+v", response.Error)
	}
	var status ollamaPortStatus
	if err := json.Unmarshal(response.Result, &status); err != nil {
		t.Fatalf("decode llama.cpp status: %v", err)
	}
	if status.Port != profile.EnginePortBase {
		t.Fatalf("status port = %d, want %d", status.Port, profile.EnginePortBase)
	}
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
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	msg, err := NewCodec(client).Read()
	if err != nil {
		t.Fatalf("read forwarded notification: %v", err)
	}
	if msg.Method != profile.ComponentName()+":ready" {
		t.Fatalf("notification method = %s, want %s:ready", msg.Method, profile.ComponentName())
	}
	var ready proxyReadyParams
	if err := json.Unmarshal(msg.Params, &ready); err != nil {
		t.Fatalf("decode ready notification: %v", err)
	}
	if ready.Port != 8080 {
		t.Fatalf("ready port = %d, want 8080", ready.Port)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notification forwarding did not finish")
	}
}
