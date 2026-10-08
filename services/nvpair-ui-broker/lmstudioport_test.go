// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
)

// The wire names of the facade-scoped methods the broker sends LM Studio's
// proxy. Asserted in their addressed form on purpose: a regression that drops
// the address would otherwise reach a process hosting several facades and be
// applied to whichever one happened to be first.
//
// engine:status and the engine-manager restore method are not here — they go to
// engine-manager, which has no facades and takes no address.
var (
	lmstudioSetPort         = lmstudioProxyProfile.addressed("set-port")
	lmstudioSetLocalBackend = lmstudioProxyProfile.addressed("node/set-local-backend")
)

func TestPlanManagedLMStudioPorts(t *testing.T) {
	free := func(ports ...int) func(int) bool {
		set := map[int]bool{}
		for _, port := range ports {
			set[port] = true
		}
		return func(port int) bool { return set[port] }
	}
	tests := []struct {
		name string
		on   bool
		st   ollamaPortStatus
		free func(int) bool
		want managedPortPlan
	}{
		{"disabled", false, ollamaPortStatus{Port: 1234}, free(1234, 1235), managedPortPlan{}},
		{"stopped default moves", true, ollamaPortStatus{Port: 1234}, free(1234, 1235), managedPortPlan{Enabled: true, BackendPort: 1235}},
		{"running identified default moves", true, ollamaPortStatus{Running: true, Port: 1234}, free(1235), managedPortPlan{Enabled: true, BackendPort: 1235}},
		{"occupied stopped backend advances", true, ollamaPortStatus{Port: 1235}, free(1234, 1236), managedPortPlan{Enabled: true, BackendPort: 1236}},
		{"running backend is preserved", true, ollamaPortStatus{Running: true, Port: 1235}, free(1234, 1236), managedPortPlan{Enabled: true}},
		{"custom backend preserved", true, ollamaPortStatus{Running: true, Port: 12400}, free(1234), managedPortPlan{Enabled: true}},
		{"unknown facade owner blocks", true, ollamaPortStatus{Port: 1235}, free(1235), managedPortPlan{Blocked: "the compatibility port is already in use"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, planManagedLMStudioPorts(tc.on, tc.st, tc.free), "planManagedLMStudioPorts")
		})
	}
}

// The engine, its port, and the ignore-persisted decision travel in
// facade/enable rather than on argv, because the broker plans a different port
// per engine and a single-valued flag cannot carry two plans.
func TestManagedLMStudioFacadeSpec(t *testing.T) {
	b := &Broker{}
	b.lmstudioState().startupPort.Store(managedLMStudioFacadePort)
	want := enableFacadeRequest{
		Engine:              "lmstudio",
		Port:                managedLMStudioFacadePort,
		IgnorePersistedPort: true,
	}
	require.Equal(t, want, b.lmstudioFacadeSpec(), "managed facade spec")

	// No startup port means the child decides: its own persisted port, else the
	// engine's standalone default. Naming a port here would override the port a
	// user chose through set-port.
	b.lmstudioState().startupPort.Store(0)
	want = enableFacadeRequest{Engine: "lmstudio"}
	require.Equal(t, want, b.lmstudioFacadeSpec(), "opt-out facade spec preserves persisted-port behavior")

	// The alias stands in for an inherited host variable, which LM Studio does
	// not have; the child rejects alias addresses for it outright.
	require.Empty(t, b.lmstudioFacadeSpec().AliasAddresses, "LM Studio facade spec carried alias addresses")
}

func TestManagedLMStudioReadyOpensGateAndPushesBackend(t *testing.T) {
	engineClient, engineServer := net.Pipe()
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = engineClient.Close()
		_ = engineServer.Close()
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})

	engine := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go engine.peer.Serve(nil, nil)
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(true)
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)
	b.setEngineMgr(engine)
	b.setLMStudioProxy(proxy)

	engineMethod := make(chan string, 1)
	go func() {
		codec := NewCodec(engineServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		engineMethod <- msg.Method
		_ = codec.Respond(msg.ID, ollamaPortStatus{Running: true, Port: managedLMStudioBackendStart})
	}()

	backend := make(chan proxyLocalBackend, 1)
	go func() {
		codec := NewCodec(proxyServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var got proxyLocalBackend
		if json.Unmarshal(msg.Params, &got) == nil {
			backend <- got
		}
		_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
	}()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"version":"test","port":1234}`))

	requireGateOpens(t, b.lmstudioPortReady, "LM Studio preparation")
	select {
	case got := <-engineMethod:
		require.Equal(t, "engine:status", got, "engine method")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "LM Studio ready did not refresh engine status")
	}
	select {
	case got := <-backend:
		require.Equal(t, "lmstudio", got.Engine, "local backend (%v, %v)", got, managedLMStudioBackendStart)
		require.Equal(t, managedLMStudioBackendStart, got.Port, "local backend (%v, %v)", got, managedLMStudioBackendStart)
		require.True(t, got.Healthy, "local backend (%v, %v)", got, managedLMStudioBackendStart)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "LM Studio ready did not push the local backend")
	}
}

func TestManagedLMStudioWrongReadyEntersFallbackAndWarns(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	errorsClient, errorsServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
		_ = errorsClient.Close()
		_ = errorsServer.Close()
	})

	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	errs := &errorsProcess{peer: NewPeer(NewCodec(errorsClient))}
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(true)
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)
	b.setLMStudioProxy(proxy)
	b.setErrors(errs)

	requestedPorts := make(chan int, 2)
	go func() {
		codec := NewCodec(proxyServer)
		setPortCount := 0
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.Method == lmstudioSetLocalBackend {
				_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
				return
			}
			var p struct {
				Port int `json:"port"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			requestedPorts <- p.Port
			setPortCount++
			if setPortCount == 1 {
				_ = codec.RespondError(msg.ID, -32000, "occupied")
			} else {
				_ = codec.Respond(msg.ID, proxyReadyParams{Port: p.Port})
			}
		}
	}()

	type warning struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	warnings := make(chan warning, 1)
	go func() {
		msg, err := NewCodec(errorsServer).Read()
		if err != nil {
			return
		}
		var got warning
		if json.Unmarshal(msg.Params, &got) == nil {
			warnings <- got
		}
	}()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"version":"test","port":1235}`))

	requireGateOpens(t, b.lmstudioPortReady, "the LM Studio fallback")
	require.False(t, b.lmstudioState().managedFacade.Load(), "managed LM Studio mode remained enabled after compatibility-port failure")
	fallback := int(b.lmstudioState().startupPort.Load())
	require.NotEqual(t, 0, fallback, "unsafe LM Studio fallback port")
	require.NotEqual(t, managedLMStudioFacadePort, fallback, "unsafe LM Studio fallback port")
	require.NotEqual(t, managedLMStudioBackendStart, fallback, "unsafe LM Studio fallback port")

	for i, want := range []int{managedLMStudioFacadePort, fallback} {
		select {
		case got := <-requestedPorts:
			require.Equal(t, want, got, "proxy set-port request %d", i)
		case <-time.After(2 * time.Second):
			require.FailNow(t, fmt.Sprintf("missing proxy set-port request %d", i))
		}
	}
	select {
	case got := <-warnings:
		require.Equal(t, lmstudioPortOwnershipBlockedID, got.ID, "warning (%v)", got)
		require.NotEqual(t, "", got.Message, "warning (%v)", got)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "LM Studio fallback did not report a warning")
	}
}

func TestManagedLMStudioRequestsWaitForPortGate(t *testing.T) {
	tests := []struct {
		method string
		params string
	}{
		{"engine:get-installed", `{}`},
		{"engine:status", `{"engine":"lmstudio"}`},
		{"engine:start", `{"engine":"lmstudio"}`},
		{"engine:restart", `{"engine":"lmstudio"}`},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			engineClient, engineServer := net.Pipe()
			brokerClient, brokerServer := net.Pipe()
			t.Cleanup(func() {
				_ = engineClient.Close()
				_ = engineServer.Close()
				_ = brokerClient.Close()
				_ = brokerServer.Close()
			})

			engine := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
			go engine.peer.Serve(nil, nil)
			b := &Broker{
				codec:             NewCodec(brokerClient),
				lmstudioPortReady: make(chan struct{}),
			}
			b.lmstudioState().managedFacade.Store(true)
			b.setEngineMgr(engine)

			method := make(chan string, 1)
			go func() {
				codec := NewCodec(engineServer)
				msg, err := codec.Read()
				if err != nil {
					return
				}
				method <- msg.Method
				_ = codec.Respond(msg.ID, map[string]any{})
			}()
			response := make(chan error, 1)
			go func() {
				_, err := NewCodec(brokerServer).Read()
				response <- err
			}()

			id := json.RawMessage(`7`)
			b.relayToEngine(&Message{
				JSONRPC: "2.0",
				ID:      &id,
				Method:  tc.method,
				Params:  json.RawMessage(tc.params),
			})

			select {
			case got := <-method:
				require.FailNow(t, fmt.Sprintf("%q was relayed before the LM Studio port gate opened", got))
			case <-time.After(100 * time.Millisecond):
			}
			close(b.lmstudioPortReady)
			select {
			case got := <-method:
				require.Equal(t, tc.method, got, "relayed method")
			case <-time.After(2 * time.Second):
				require.FailNow(t, fmt.Sprintf("%q was not relayed after the LM Studio port gate opened", tc.method))
			}
			select {
			case err := <-response:
				require.NoError(t, err, "read broker response")
			case <-time.After(2 * time.Second):
				require.FailNow(t, "broker did not return the relayed response")
			}
		})
	}
}

func TestManagedLMStudioPortGateRequestMatcher(t *testing.T) {
	for _, tc := range []struct {
		method string
		params string
		want   bool
	}{
		{"engine:get-installed", `{}`, true},
		{"engine:status", `{"engine":"lmstudio"}`, true},
		{"engine:start", `{"engine":"lmstudio"}`, true},
		{"engine:restart", `{"engine":"lmstudio"}`, true},
		// Gated for the same reason Ollama gates it: an install that starts
		// the engine assigns a port, and mid-transition that can be the port
		// the proxy is taking.
		{"engine:install", `{"engine":"lmstudio"}`, true},
		{"engine:install", `{"engine":"ollama"}`, false},
		{"engine:status", `{"engine":"ollama"}`, false},
		{"engine:models", `{"engine":"lmstudio"}`, false},
		{"engine:status", `{`, false},
	} {
		assert.Equal(t, tc.want, needsLMStudioPortGate(tc.method, json.RawMessage(tc.params)), "needsLMStudioPortGate(%q, %s)", tc.method, tc.params)
	}
}

func TestManagedLMStudioPortGateHonorsCancellation(t *testing.T) {
	b := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, b.waitForManagedPortOwnership(ctx), "cancelled ownership wait reported ready")
}

func TestManagedLMStudioFallbackRemainsPendingUntilGateCloses(t *testing.T) {
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(false) // fallback disables managed mode first
	require.True(t, b.lmstudioPortOwnershipPending(), "fallback became observable before its proxy rebind completed")
	b.markLMStudioPortReady()
	require.False(t, b.lmstudioPortOwnershipPending(), "completed fallback still reports pending")
}

func TestManagedLMStudioConcurrentReadyWaitsForFallbackRebind(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	errorsClient, errorsServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
		_ = errorsClient.Close()
		_ = errorsServer.Close()
	})

	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(true)
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)
	b.setLMStudioProxy(proxy)
	b.setErrors(&errorsProcess{peer: NewPeer(NewCodec(errorsClient))})

	requests := make(chan string, 4)
	go func() {
		codec := NewCodec(proxyServer)
		count := 0
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			count++
			requests <- msg.Method
			if msg.Method == lmstudioSetPort && count == 1 {
				_ = codec.RespondError(msg.ID, -32000, "occupied")
			} else {
				_ = codec.Respond(msg.ID, proxyReadyParams{Port: managedLMStudioBackendStart + 1})
			}
		}
	}()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":1235}`))
	select {
	case method := <-requests:
		require.Equal(t, lmstudioSetPort, method, "first proxy request")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "missing compatibility-port rebind")
	}
	deadline := time.After(2 * time.Second)
	for b.lmstudioState().managedFacade.Load() {
		select {
		case <-deadline:
			require.FailNow(t, "managed mode was not disabled after rebind failure")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// The first reconciliation is now blocked writing its warning. A duplicate
	// ready must not overtake it and release the gate before the fallback move.
	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":1235}`))
	requireGateStaysShut(t, b.lmstudioPortReady, "a duplicate ready before the fallback rebind")

	_, err := NewCodec(errorsServer).Read()
	require.NoError(t, err, "read fallback warning")
	requireGateOpens(t, b.lmstudioPortReady, "the fallback rebind")
}

// The whole reconciliation path, driven by a ready notification, when no port
// the broker offers can be bound: it exhausts its fallbacks and then gives up
// on LM Studio alone.
//
// This used to end in a supervised restart of the process. It cannot now — the
// same process hosts Ollama's facade — so the assertions are inverted: the gate
// must open anyway, and the handle and supervisor must be left alone.
func TestManagedLMStudioExhaustedFallbacksFinishOnlyThatEngine(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(true)
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)
	b.lmstudioProxyGeneration.Store(1)
	b.lmstudioProxyPublishedGeneration.Store(1)
	b.setLMStudioProxy(proxy)
	b.proxySup = newSupervisor(engines.ProxyComponent, noRestartPolicy(), nil)

	// Refuse every port offered, so the fallback chain runs to exhaustion.
	attempts := make(chan string, 8)
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			attempts <- msg.Method
			_ = codec.RespondError(msg.ID, -32000, "occupied")
		}
	}()

	b.forwardLMStudioProxyNotificationForGeneration(1, "ready", json.RawMessage(`{"port":1235}`))

	// Releasing the gate is what stops every engine:status for LM Studio from
	// waiting out the call timeout and answering "retry" forever.
	requireGateOpens(t, b.lmstudioPortReady, "exhausted LM Studio fallbacks")

	assert.Equal(t, lmstudioSetPort, <-attempts, "first proxy attempt")
	// Same handle still published: the process was not replaced, so every other
	// facade in it keeps serving. Handle identity is the observable form of
	// "no process restart".
	assert.Same(t, proxy, b.getLMStudioProxy(), "exhausting one facade's ports replaced the shared proxy handle")
	assert.False(t, b.lmstudioState().managedFacade.Load(), "managed mode survived exhausted fallbacks")
}

func TestPrepareManagedLMStudioFacadeMovesDefaultBackend(t *testing.T) {
	settings, settingsCodec := newTestRPCWorkerPipe(t)
	engine, engineCodec := newTestRPCWorkerPipe(t)
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.setSettings(settings)
	b.setEngineMgr(engine)

	calls := make(chan string, 3)
	go func() {
		msg, err := settingsCodec.Read()
		if err != nil {
			return
		}
		calls <- msg.Method
		_ = settingsCodec.Respond(msg.ID, map[string]bool{"value": true})
	}()
	go func() {
		status, err := engineCodec.Read()
		if err != nil {
			return
		}
		calls <- status.Method
		var statusRequest struct {
			Engine string `json:"engine"`
			Port   int    `json:"port"`
		}
		assert.False(t, json.Unmarshal(status.Params, &statusRequest) != nil ||
			statusRequest.Engine != "lmstudio" || statusRequest.Port != managedLMStudioFacadePort, "engine:status params")
		_ = engineCodec.Respond(status.ID, ollamaPortStatus{Running: true, Port: managedLMStudioFacadePort})

		setPort, err := engineCodec.Read()
		if err != nil {
			return
		}
		calls <- setPort.Method
		var request struct {
			Engine string `json:"engine"`
			Port   int    `json:"port"`
		}
		assert.False(t, json.Unmarshal(setPort.Params, &request) != nil || request.Engine != "lmstudio" || request.Port != managedLMStudioBackendStart, "engine:set-port params")
		_ = engineCodec.Respond(setPort.ID, ollamaPortStatus{Running: true, Port: managedLMStudioBackendStart})
	}()

	b.prepareManagedLMStudioFacadeWithPortCheck(func(port int) bool {
		return port == managedLMStudioFacadePort || port == managedLMStudioBackendStart
	})

	for i, want := range []string{"settings/get-force-ports", "engine:status", "engine:set-port"} {
		select {
		case got := <-calls:
			require.Equal(t, want, got, "preparation call %d", i)
		case <-time.After(2 * time.Second):
			require.FailNow(t, fmt.Sprintf("missing preparation call %d (%s)", i, want))
		}
	}
	require.True(t, b.lmstudioState().managedFacade.Load(), "managed LM Studio ownership was not enabled")
	require.Equal(t, int32(managedLMStudioBackendStart), b.lmstudioState().backendPort.Load(), "backend port")
	require.Equal(t, int32(managedLMStudioFacadePort), b.lmstudioState().startupPort.Load(), "proxy startup port")
	requireGateShutNow(t, b.lmstudioPortReady, "preparation before proxy readiness")
}

func TestPrepareUnmanagedLMStudioPreservesPortsAndWaitsForProxy(t *testing.T) {
	settings, settingsCodec := newTestRPCWorkerPipe(t)
	engine, engineCodec := newTestRPCWorkerPipe(t)
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.setSettings(settings)
	b.setEngineMgr(engine)

	go func() {
		msg, err := settingsCodec.Read()
		if err == nil {
			_ = settingsCodec.Respond(msg.ID, map[string]bool{"value": false})
		}
	}()
	go func() {
		msg, err := engineCodec.Read()
		if err == nil {
			_ = engineCodec.Respond(msg.ID, ollamaPortStatus{Running: true, Port: 12400})
		}
	}()

	portChecked := false
	b.prepareManagedLMStudioFacadeWithPortCheck(func(int) bool {
		portChecked = true
		return true
	})

	require.False(t, portChecked, "opt-out preparation unexpectedly probed compatibility ports")
	require.False(t, b.lmstudioState().managedFacade.Load(), "opt-out preparation enabled managed ownership")
	require.Equal(t, int32(12400), b.lmstudioState().backendPort.Load(), "custom backend")
	require.Equal(t, int32(0), b.lmstudioState().startupPort.Load(), "opt-out forced proxy startup port")
	requireGateShutNow(t, b.lmstudioPortReady, "opt-out before the persisted proxy port was known")
}

func TestManagedLMStudioBindFailureWaitsForFallbackReady(t *testing.T) {
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(true)
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)

	b.forwardLMStudioProxyNotification("error", json.RawMessage(`{"code":"bind-failed","port":1234}`))
	requireGateStaysShut(t, b.lmstudioPortReady, "a bind failure before a fallback proxy bound")
	fallback := int(b.lmstudioState().startupPort.Load())
	require.NotEqual(t, 0, fallback, "unsafe fallback port")
	require.NotEqual(t, managedLMStudioFacadePort, fallback, "unsafe fallback port")
	require.NotEqual(t, managedLMStudioBackendStart, fallback, "unsafe fallback port")

	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	b.setLMStudioProxy(proxy)
	go func() {
		codec := NewCodec(proxyServer)
		msg, err := codec.Read()
		if err == nil {
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(fmt.Sprintf(`{"port":%d}`, fallback)))
	requireGateOpens(t, b.lmstudioPortReady, "fallback readiness")
}

func TestManagedLMStudioRestartBindFailureChoosesFallback(t *testing.T) {
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(true)
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)
	b.markLMStudioPortReady() // a later supervised proxy generation failed

	b.forwardLMStudioProxyNotification("error", json.RawMessage(`{"code":"bind-failed","port":1234}`))
	require.False(t, b.lmstudioState().managedFacade.Load(), "restart bind failure left managed ownership enabled")
	fallback := int(b.lmstudioState().startupPort.Load())
	require.NotEqual(t, 0, fallback, "restart bind failure selected unsafe fallback")
	require.NotEqual(t, managedLMStudioFacadePort, fallback, "restart bind failure selected unsafe fallback")
	require.NotEqual(t, managedLMStudioBackendStart, fallback, "restart bind failure selected unsafe fallback")
}

func TestManagedLMStudioStaleReadyDoesNotOpenCurrentGate(t *testing.T) {
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioProxyGeneration.Store(2)
	b.forwardLMStudioProxyNotificationForGeneration(1, "ready", json.RawMessage(`{"port":1234}`))
	requireGateStaysShut(t, b.lmstudioPortReady, "a stale proxy generation's ready")
}

func TestUnmanagedLMStudioProxyCollisionRebindsBeforeGate(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().backendPort.Store(12400)
	b.setLMStudioProxy(proxy)

	methods := make(chan string, 2)
	requestedFallback := make(chan int, 1)
	localBackend := make(chan proxyLocalBackend, 1)
	go func() {
		codec := NewCodec(proxyServer)
		for i := 0; i < 2; i++ {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			methods <- msg.Method
			switch msg.Method {
			case lmstudioSetPort:
				var request struct {
					Port int `json:"port"`
				}
				_ = json.Unmarshal(msg.Params, &request)
				requestedFallback <- request.Port
				_ = codec.Respond(msg.ID, proxyReadyParams{Port: request.Port})
			case lmstudioSetLocalBackend:
				var backend proxyLocalBackend
				_ = json.Unmarshal(msg.Params, &backend)
				localBackend <- backend
				_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
			default:
				_ = codec.RespondError(msg.ID, -32601, "unexpected method")
			}
		}
	}()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":12400}`))
	requireGateOpens(t, b.lmstudioPortReady, "the opt-out collision's proxy fallback")

	for i, want := range []string{lmstudioSetPort, lmstudioSetLocalBackend} {
		select {
		case got := <-methods:
			require.Equal(t, want, got, "proxy method %d", i)
		case <-time.After(2 * time.Second):
			require.FailNow(t, fmt.Sprintf("missing proxy method %d (%s)", i, want))
		}
	}
	fallback := <-requestedFallback
	require.NotEqual(t, 0, fallback, "collision fallback")
	require.NotEqual(t, 12400, fallback, "collision fallback")
	require.Equal(t, fallback, int(b.lmstudioState().startupPort.Load()), "cached proxy fallback")
	require.Equal(t, int32(12400), b.lmstudioState().backendPort.Load(), "custom backend changed")
	got := <-localBackend
	require.Equal(t, 12400, got.Port, "local backend (%v)", got)
}

func TestUnmanagedLMStudioCollisionRebindsBeforeStatusProbe(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	engine, engineCodec := newTestRPCWorkerPipe(t)

	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().backendPort.Store(12400)
	b.setLMStudioProxy(proxy)
	b.setEngineMgr(engine)

	order := make(chan string, 3)
	go func() {
		codec := NewCodec(proxyServer)
		for i := 0; i < 2; i++ {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			order <- msg.Method
			if msg.Method == lmstudioSetPort {
				var request struct {
					Port int `json:"port"`
				}
				_ = json.Unmarshal(msg.Params, &request)
				_ = codec.Respond(msg.ID, proxyReadyParams{Port: request.Port})
			} else {
				_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
			}
		}
	}()
	go func() {
		msg, err := engineCodec.Read()
		if err != nil {
			return
		}
		order <- msg.Method
		_ = engineCodec.Respond(msg.ID, ollamaPortStatus{Port: 12400})
	}()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":12400}`))
	requireGateOpens(t, b.lmstudioPortReady, "the opt-out collision")
	require.Equal(t, []string{lmstudioSetPort, "engine:status", lmstudioSetLocalBackend}, []string{<-order, <-order, <-order}, "collision reconciliation order")
}

func TestUnknownLMStudioBackendFallbackSkipsUnprovenPorts(t *testing.T) {
	b := &Broker{}
	fallback := b.setLMStudioProxyFallback()
	require.NotEqual(t, 0, fallback, "unknown-backend fallback selected unproven port")
	require.NotEqual(t, managedLMStudioFacadePort, fallback, "unknown-backend fallback selected unproven port")
	require.NotEqual(t, managedLMStudioBackendStart, fallback, "unknown-backend fallback selected unproven port")
}

func TestUnknownLMStudioBackendMovesProxyBeforeStatusProbe(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	engine, engineCodec := newTestRPCWorkerPipe(t)

	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.setLMStudioProxy(proxy)
	b.setEngineMgr(engine)

	order := make(chan string, 3)
	fallbackPort := make(chan int, 1)
	go func() {
		codec := NewCodec(proxyServer)
		for i := 0; i < 2; i++ {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			order <- msg.Method
			if msg.Method == lmstudioSetPort {
				var request struct {
					Port int `json:"port"`
				}
				_ = json.Unmarshal(msg.Params, &request)
				fallbackPort <- request.Port
				_ = codec.Respond(msg.ID, proxyReadyParams{Port: request.Port})
			} else {
				_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
			}
		}
	}()
	go func() {
		msg, err := engineCodec.Read()
		if err != nil {
			return
		}
		order <- msg.Method
		_ = engineCodec.Respond(msg.ID, ollamaPortStatus{Port: managedLMStudioBackendStart})
	}()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":1235}`))
	requireGateOpens(t, b.lmstudioPortReady, "the unknown-backend fallback")
	require.Equal(t, []string{lmstudioSetPort, "engine:status", lmstudioSetLocalBackend}, []string{<-order, <-order, <-order}, "unknown-backend reconciliation order")
	fallback := <-fallbackPort
	require.NotEqual(t, managedLMStudioFacadePort, fallback, "unknown-backend fallback used unsafe port")
	require.NotEqual(t, managedLMStudioBackendStart, fallback, "unknown-backend fallback used unsafe port")
}

func TestUnknownCustomBackendStatusFailuresKeepRestoreGated(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	engine, engineCodec := newTestRPCWorkerPipe(t)

	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.setLMStudioProxy(proxy)
	b.setEngineMgr(engine)

	statusCalls := make(chan struct{}, 2)
	go func() {
		for i := 0; i < 2; i++ {
			msg, err := engineCodec.Read()
			if err != nil {
				return
			}
			statusCalls <- struct{}{}
			_ = engineCodec.RespondError(msg.ID, -32000, "status unavailable")
		}
	}()
	// Let the pre-fix local-backend call finish so an incorrect gate close is
	// observable instead of blocking on the test pipe.
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()

	for i := 0; i < 2; i++ {
		b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":12400}`))
		select {
		case <-statusCalls:
		case <-time.After(2 * time.Second):
			require.FailNow(t, fmt.Sprintf("missing status attempt %d", i+1))
		}
		requireGateStaysShut(t, b.lmstudioPortReady,
			fmt.Sprintf("status failure %d with an unknown custom backend", i+1))
	}
}

func TestEngineManagerRespawnReconcilesUnknownCustomBackendBeforeRestore(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{
		peer:        NewPeer(NewCodec(proxyClient)),
		facadeState: readyFacade(lmstudioProxyProfile.Name, 12400),
	}
	go proxy.peer.Serve(nil, nil)
	oldEngine, oldEngineCodec := newTestRPCWorkerPipe(t)

	b := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	close(b.ollamaPortReady)
	b.setLMStudioProxy(proxy)
	b.setEngineMgr(oldEngine)

	oldStatus := make(chan struct{}, 1)
	go func() {
		msg, err := oldEngineCodec.Read()
		if err == nil {
			oldStatus <- struct{}{}
			_ = oldEngineCodec.RespondError(msg.ID, -32000, "manager exiting")
		}
	}()
	restoreDone := make(chan bool, 1)
	go func() { restoreDone <- b.restoreEnabledEnginesAfterPortGate(context.Background()) }()

	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":12400}`))
	select {
	case <-oldStatus:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "initial status failure was not exercised")
	}
	requireGateStaysShut(t, b.lmstudioPortReady, "the initial status failure")

	newEngine, newEngineCodec := newTestRPCWorkerPipe(t)
	b.setEngineMgr(newEngine)
	order := make(chan string, 4)
	go func() {
		status, err := newEngineCodec.Read()
		if err != nil {
			return
		}
		order <- status.Method
		_ = newEngineCodec.Respond(status.ID, ollamaPortStatus{Port: 12400})

		restore, err := newEngineCodec.Read()
		if err == nil {
			order <- restore.Method
		}
	}()
	go func() {
		codec := NewCodec(proxyServer)
		for i := 0; i < 2; i++ {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			order <- msg.Method
			if msg.Method == lmstudioSetPort {
				var request struct {
					Port int `json:"port"`
				}
				_ = json.Unmarshal(msg.Params, &request)
				_ = codec.Respond(msg.ID, proxyReadyParams{Port: request.Port})
			} else {
				_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
			}
		}
	}()

	b.forwardEngineNotification("engine:ready", nil)
	requireGateOpens(t, b.lmstudioPortReady, "the engine-manager respawn")
	require.True(t, <-restoreDone, "restore waiter reported cancellation")
	require.Equal(t, []string{"engine:status", lmstudioSetPort, lmstudioSetLocalBackend, restoreEnabledEnginesMethod}, []string{<-order, <-order, <-order, <-order}, "respawn reconciliation order")
}
