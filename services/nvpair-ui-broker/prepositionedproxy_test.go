// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"nvpair-shared/engines"
)

func testPrepositionedProfile() engineProxyProfile {
	return engineProxyProfile{
		Engine: engines.Engine{
			Name:           "fixedtest",
			DisplayName:    "Fixed Test",
			FacadePort:     1233,
			EnginePortBase: 1234,
			PortFile:       "fixedtest-proxy-port.json",
		},
		Ownership:       prepositionedEngine,
		HealthProbePath: "/health",
	}
}

func brokerWithPrepositionedProfile(profile engineProxyProfile) *Broker {
	b := &Broker{}
	b.engineProxiesOnce.Do(func() {
		b.engineProxies = map[string]*engineProxyRuntime{
			profile.Name: {profile: profile},
		}
	})
	return b
}

func TestPrepositionedFacadeRetriesAwayFromReservedPorts(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	profile := testPrepositionedProfile()
	b := brokerWithPrepositionedProfile(profile)

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
		t.Fatalf("enable prepositioned facade: %v", err)
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

	restart := b.prepositionedFacadeSpec(profile)
	if restart.Port != second.Port || !restart.IgnorePersistedPort {
		t.Fatalf("restart spec = %+v, want explicit fallback port %d", restart, second.Port)
	}
}

func TestPrepositionedProfileNeverTakesBackendOwnership(t *testing.T) {
	profile := testPrepositionedProfile()
	if profile.mayMoveRunningEngine() {
		t.Fatal("prepositioned strategy may move a running backend")
	}
	if profile.blocksOnOccupiedFacade() {
		t.Fatal("prepositioned strategy blocks instead of moving only its facade")
	}

	b := brokerWithPrepositionedProfile(profile)
	b.preparePrepositionedFacade(profile)
	state := b.engineProxy(profile)
	if got := int(state.backendPort.Load()); got != profile.EnginePortBase {
		t.Fatalf("backend port = %d, want fixed base %d", got, profile.EnginePortBase)
	}
	state.managedFacade.Store(true)
	b.blockAndFinishEngineProxy(profile)
	b.finishEngineProxyStartup(profile)
	if !state.managedFacade.Load() {
		t.Fatal("ungated terminal handling mutated ownership state")
	}
}

func TestPrepositionedNotificationPreservesFacadeAddress(t *testing.T) {
	profile := lmstudioProxyProfile
	profile.Ownership = prepositionedEngine
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	b := &Broker{codec: NewCodec(server)}
	b.proxyMu.Lock()
	b.setEngineProxySubscribed(profile, true)
	b.proxyMu.Unlock()

	payload := json.RawMessage(`{"port":1234}`)
	done := make(chan struct{})
	go func() {
		b.forwardPrepositionedProxyNotification(profile, profile.addressed("ready"), payload)
		close(done)
	}()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	msg, err := NewCodec(client).Read()
	if err != nil {
		t.Fatalf("read forwarded notification: %v", err)
	}
	if msg.Method != profile.ComponentName()+":ready" || string(msg.Params) != string(payload) {
		t.Fatalf("forwarded notification = %s %s", msg.Method, msg.Params)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notification forwarding did not finish")
	}
}
