// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

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
			if got := planManagedLMStudioPorts(tc.on, tc.st, tc.free); got != tc.want {
				t.Fatalf("planManagedLMStudioPorts() = %+v, want %+v", got, tc.want)
			}
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
	if got := b.lmstudioFacadeSpec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("managed facade spec = %+v, want %+v", got, want)
	}

	// No startup port means the child decides: its own persisted port, else the
	// engine's standalone default. Naming a port here would override the port a
	// user chose through set-port.
	b.lmstudioState().startupPort.Store(0)
	want = enableFacadeRequest{Engine: "lmstudio"}
	if got := b.lmstudioFacadeSpec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("opt-out facade spec = %+v, want %+v (persisted-port behavior)", got, want)
	}

	// The alias stands in for an inherited host variable, which LM Studio does
	// not have; the child rejects alias addresses for it outright.
	if len(b.lmstudioFacadeSpec().AliasAddresses) != 0 {
		t.Fatal("LM Studio facade spec carried alias addresses")
	}
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
		if got != "engine:status" {
			t.Fatalf("engine method = %q, want engine:status", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LM Studio ready did not refresh engine status")
	}
	select {
	case got := <-backend:
		if got.Engine != "lmstudio" || got.Port != managedLMStudioBackendStart || !got.Healthy {
			t.Fatalf("local backend = %+v, want healthy lmstudio:%d", got, managedLMStudioBackendStart)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LM Studio ready did not push the local backend")
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
	if b.lmstudioState().managedFacade.Load() {
		t.Fatal("managed LM Studio mode remained enabled after compatibility-port failure")
	}
	fallback := int(b.lmstudioState().startupPort.Load())
	if fallback == 0 || fallback == managedLMStudioFacadePort || fallback == managedLMStudioBackendStart {
		t.Fatalf("unsafe LM Studio fallback port %d", fallback)
	}

	for i, want := range []int{managedLMStudioFacadePort, fallback} {
		select {
		case got := <-requestedPorts:
			if got != want {
				t.Fatalf("proxy set-port request %d = %d, want %d", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing proxy set-port request %d", i)
		}
	}
	select {
	case got := <-warnings:
		if got.ID != lmstudioPortOwnershipBlockedID || got.Message == "" {
			t.Fatalf("warning = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LM Studio fallback did not report a warning")
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
				t.Fatalf("%q was relayed before the LM Studio port gate opened", got)
			case <-time.After(100 * time.Millisecond):
			}
			close(b.lmstudioPortReady)
			select {
			case got := <-method:
				if got != tc.method {
					t.Fatalf("method = %q, want %q", got, tc.method)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%q was not relayed after the LM Studio port gate opened", tc.method)
			}
			select {
			case err := <-response:
				if err != nil {
					t.Fatalf("read broker response: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("broker did not return the relayed response")
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
		if got := needsLMStudioPortGate(tc.method, json.RawMessage(tc.params)); got != tc.want {
			t.Errorf("needsLMStudioPortGate(%q, %s) = %v, want %v", tc.method, tc.params, got, tc.want)
		}
	}
}

func TestManagedLMStudioPortGateHonorsCancellation(t *testing.T) {
	b := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if b.waitForManagedPortOwnership(ctx) {
		t.Fatal("cancelled ownership wait reported ready")
	}
}

func TestManagedLMStudioFallbackRemainsPendingUntilGateCloses(t *testing.T) {
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(false) // fallback disables managed mode first
	if !b.lmstudioPortOwnershipPending() {
		t.Fatal("fallback became observable before its proxy rebind completed")
	}
	b.markLMStudioPortReady()
	if b.lmstudioPortOwnershipPending() {
		t.Fatal("completed fallback still reports pending")
	}
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
		if method != lmstudioSetPort {
			t.Fatalf("first proxy request = %q, want %q", method, lmstudioSetPort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing compatibility-port rebind")
	}
	deadline := time.After(2 * time.Second)
	for b.lmstudioState().managedFacade.Load() {
		select {
		case <-deadline:
			t.Fatal("managed mode was not disabled after rebind failure")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// The first reconciliation is now blocked writing its warning. A duplicate
	// ready must not overtake it and release the gate before the fallback move.
	b.forwardLMStudioProxyNotification("ready", json.RawMessage(`{"port":1235}`))
	requireGateStaysShut(t, b.lmstudioPortReady, "a duplicate ready before the fallback rebind")

	if _, err := NewCodec(errorsServer).Read(); err != nil {
		t.Fatalf("read fallback warning: %v", err)
	}
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

	if got := <-attempts; got != lmstudioSetPort {
		t.Errorf("first attempt was %q, want %q", got, lmstudioSetPort)
	}
	// Same handle still published: the process was not replaced, so every other
	// facade in it keeps serving. Handle identity is the observable form of
	// "no process restart".
	if b.getLMStudioProxy() != proxy {
		t.Error("exhausting one facade's ports replaced the shared proxy handle")
	}
	if b.lmstudioState().managedFacade.Load() {
		t.Error("managed mode survived exhausted fallbacks")
	}
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
		if json.Unmarshal(status.Params, &statusRequest) != nil ||
			statusRequest.Engine != "lmstudio" || statusRequest.Port != managedLMStudioFacadePort {
			t.Errorf("engine:status params = %s", status.Params)
		}
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
		if json.Unmarshal(setPort.Params, &request) != nil || request.Engine != "lmstudio" || request.Port != managedLMStudioBackendStart {
			t.Errorf("engine:set-port params = %s", setPort.Params)
		}
		_ = engineCodec.Respond(setPort.ID, ollamaPortStatus{Running: true, Port: managedLMStudioBackendStart})
	}()

	b.prepareManagedLMStudioFacadeWithPortCheck(func(port int) bool {
		return port == managedLMStudioFacadePort || port == managedLMStudioBackendStart
	})

	for i, want := range []string{"settings/get-force-ports", "engine:status", "engine:set-port"} {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("preparation call %d = %q, want %q", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing preparation call %d (%s)", i, want)
		}
	}
	if !b.lmstudioState().managedFacade.Load() {
		t.Fatal("managed LM Studio ownership was not enabled")
	}
	if got := b.lmstudioState().backendPort.Load(); got != managedLMStudioBackendStart {
		t.Fatalf("backend port = %d, want %d", got, managedLMStudioBackendStart)
	}
	if got := b.lmstudioState().startupPort.Load(); got != managedLMStudioFacadePort {
		t.Fatalf("proxy startup port = %d, want %d", got, managedLMStudioFacadePort)
	}
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

	if portChecked {
		t.Fatal("opt-out preparation unexpectedly probed compatibility ports")
	}
	if b.lmstudioState().managedFacade.Load() {
		t.Fatal("opt-out preparation enabled managed ownership")
	}
	if got := b.lmstudioState().backendPort.Load(); got != 12400 {
		t.Fatalf("custom backend = %d, want 12400", got)
	}
	if got := b.lmstudioState().startupPort.Load(); got != 0 {
		t.Fatalf("opt-out forced proxy startup port %d", got)
	}
	requireGateShutNow(t, b.lmstudioPortReady, "opt-out before the persisted proxy port was known")
}

func TestManagedLMStudioBindFailureWaitsForFallbackReady(t *testing.T) {
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().managedFacade.Store(true)
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)

	b.forwardLMStudioProxyNotification("error", json.RawMessage(`{"code":"bind-failed","port":1234}`))
	requireGateStaysShut(t, b.lmstudioPortReady, "a bind failure before a fallback proxy bound")
	fallback := int(b.lmstudioState().startupPort.Load())
	if fallback == 0 || fallback == managedLMStudioFacadePort || fallback == managedLMStudioBackendStart {
		t.Fatalf("unsafe fallback port %d", fallback)
	}

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
	if b.lmstudioState().managedFacade.Load() {
		t.Fatal("restart bind failure left managed ownership enabled")
	}
	fallback := int(b.lmstudioState().startupPort.Load())
	if fallback == 0 || fallback == managedLMStudioFacadePort || fallback == managedLMStudioBackendStart {
		t.Fatalf("restart bind failure selected unsafe fallback %d", fallback)
	}
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
			if got != want {
				t.Fatalf("proxy method %d = %q, want %q", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing proxy method %d (%s)", i, want)
		}
	}
	fallback := <-requestedFallback
	if fallback == 0 || fallback == 12400 {
		t.Fatalf("collision fallback = %d", fallback)
	}
	if got := int(b.lmstudioState().startupPort.Load()); got != fallback {
		t.Fatalf("cached proxy fallback = %d, want %d", got, fallback)
	}
	if got := int(b.lmstudioState().backendPort.Load()); got != 12400 {
		t.Fatalf("custom backend changed to %d", got)
	}
	if got := <-localBackend; got.Port != 12400 {
		t.Fatalf("local backend = %+v, want port 12400", got)
	}
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
	got := []string{<-order, <-order, <-order}
	if got[0] != lmstudioSetPort || got[1] != "engine:status" || got[2] != lmstudioSetLocalBackend {
		t.Fatalf("collision reconciliation order = %v", got)
	}
}

func TestUnknownLMStudioBackendFallbackSkipsUnprovenPorts(t *testing.T) {
	b := &Broker{}
	fallback := b.setLMStudioProxyFallback()
	if fallback == 0 || fallback == managedLMStudioFacadePort || fallback == managedLMStudioBackendStart {
		t.Fatalf("unknown-backend fallback selected unproven port %d", fallback)
	}
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
	got := []string{<-order, <-order, <-order}
	if got[0] != lmstudioSetPort || got[1] != "engine:status" || got[2] != lmstudioSetLocalBackend {
		t.Fatalf("unknown-backend reconciliation order = %v", got)
	}
	fallback := <-fallbackPort
	if fallback == managedLMStudioFacadePort || fallback == managedLMStudioBackendStart {
		t.Fatalf("unknown-backend fallback used unsafe port %d", fallback)
	}
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
			t.Fatalf("missing status attempt %d", i+1)
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
		t.Fatal("initial status failure was not exercised")
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
	if !<-restoreDone {
		t.Fatal("restore waiter reported cancellation")
	}
	got := []string{<-order, <-order, <-order, <-order}
	want := []string{"engine:status", lmstudioSetPort, lmstudioSetLocalBackend, restoreEnabledEnginesMethod}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("respawn reconciliation order = %v, want %v", got, want)
		}
	}
}
