// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestEnabledEngineRestoreWaitsForBothPortGates(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(client))}
	broker := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	broker.setEngineMgr(worker)

	method := make(chan string, 1)
	go func() {
		var msg struct {
			Method string `json:"method"`
		}
		if json.NewDecoder(server).Decode(&msg) == nil {
			method <- msg.Method
		}
	}()
	done := make(chan bool, 1)
	go func() { done <- broker.restoreEnabledEnginesAfterPortGate(context.Background()) }()

	select {
	case got := <-method:
		t.Fatalf("restore %q was sent before either port gate opened", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(broker.ollamaPortReady)
	select {
	case got := <-method:
		t.Fatalf("restore %q was sent before the LM Studio port gate opened", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(broker.lmstudioPortReady)
	select {
	case got := <-method:
		if got != restoreEnabledEnginesMethod {
			t.Fatalf("method = %q, want %q", got, restoreEnabledEnginesMethod)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restore was not sent after both port gates opened")
	}
	if !<-done {
		t.Fatal("restore wait reported cancellation")
	}
}

func TestPlanManagedOllamaPorts(t *testing.T) {
	free := func(ports ...int) func(int) bool {
		set := map[int]bool{}
		for _, port := range ports {
			set[port] = true
		}
		return func(port int) bool { return set[port] }
	}

	for _, tc := range []struct {
		name      string
		enabled   bool
		status    ollamaPortStatus
		available func(int) bool
		want      managedPortPlan
	}{
		{
			name:      "policy disabled changes nothing",
			available: free(managedOllamaFacadePort, managedOllamaBackendStart),
		},
		{
			name:      "stopped default engine moves behind facade",
			enabled:   true,
			status:    ollamaPortStatus{Port: managedOllamaFacadePort},
			available: free(managedOllamaFacadePort, managedOllamaBackendStart),
			want:      managedPortPlan{Enabled: true, BackendPort: managedOllamaBackendStart},
		},
		{
			name:      "allocator skips occupied preferred backend",
			enabled:   true,
			status:    ollamaPortStatus{Port: managedOllamaFacadePort},
			available: free(managedOllamaFacadePort, managedOllamaBackendStart+1),
			want:      managedPortPlan{Enabled: true, BackendPort: managedOllamaBackendStart + 1},
		},
		{
			name:      "occupied stopped backend advances",
			enabled:   true,
			status:    ollamaPortStatus{Port: managedOllamaBackendStart},
			available: free(managedOllamaFacadePort, managedOllamaBackendStart+1),
			want:      managedPortPlan{Enabled: true, BackendPort: managedOllamaBackendStart + 1},
		},
		{
			name:      "running backend is preserved",
			enabled:   true,
			status:    ollamaPortStatus{Running: true, Port: managedOllamaBackendStart},
			available: free(managedOllamaFacadePort, managedOllamaBackendStart+1),
			want:      managedPortPlan{Enabled: true},
		},
		{
			name:      "custom backend is preserved",
			enabled:   true,
			status:    ollamaPortStatus{Running: true, Port: 12000},
			available: free(managedOllamaFacadePort),
			want:      managedPortPlan{Enabled: true},
		},
		{
			name:      "running default engine is never moved",
			enabled:   true,
			status:    ollamaPortStatus{Running: true, Port: managedOllamaFacadePort},
			available: free(managedOllamaFacadePort, managedOllamaBackendStart),
			want:      managedPortPlan{Blocked: "Ollama is already running on the compatibility port"},
		},
		{
			name:      "unknown facade owner is never touched",
			enabled:   true,
			status:    ollamaPortStatus{Port: managedOllamaFacadePort},
			available: free(managedOllamaBackendStart),
			want:      managedPortPlan{Blocked: "the compatibility port is already in use"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := planManagedOllamaPorts(tc.enabled, tc.status, tc.available)
			if got != tc.want {
				t.Fatalf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNextAvailablePortExcludingCustomBackend(t *testing.T) {
	available := func(port int) bool { return port == 11435 || port == 11436 }
	if got := nextAvailablePortExcluding(11435, []int{11435}, available); got != 11436 {
		t.Fatalf("fallback = %d, want 11436 (11435 is the configured backend)", got)
	}
}

func TestTakePendingManagedOllamaBackendOnce(t *testing.T) {
	b := &Broker{}
	b.managedOllamaFacade.Store(true)
	b.managedOllamaBackend.Store(11435)

	if got := b.takePendingManagedOllamaBackend(11436); got != 0 {
		t.Fatalf("wrong bound port consumed pending backend: %d", got)
	}
	if got := b.takePendingManagedOllamaBackend(managedOllamaFacadePort); got != 11435 {
		t.Fatalf("first facade reconciliation got %d, want 11435", got)
	}
	if got := b.takePendingManagedOllamaBackend(managedOllamaFacadePort); got != 0 {
		t.Fatalf("duplicate reconciliation consumed backend twice: %d", got)
	}
}

func TestOllamaBackendSourcePort(t *testing.T) {
	b := &Broker{}
	if got := b.ollamaBackendSourcePort(); got != managedOllamaFacadePort {
		t.Fatalf("unset source = %d, want %d", got, managedOllamaFacadePort)
	}
	b.ollamaBackendPort.Store(managedOllamaBackendStart)
	if got := b.ollamaBackendSourcePort(); got != managedOllamaBackendStart {
		t.Fatalf("configured source = %d, want %d", got, managedOllamaBackendStart)
	}
}

func TestDuplicateOllamaReadyDoesNotOpenGateDuringMove(t *testing.T) {
	b := &Broker{ollamaPortReady: make(chan struct{})}
	b.managedOllamaFacade.Store(true)
	b.managedOllamaBackend.Store(managedOllamaBackendStart + 1)
	b.setProxy(&proxyProcess{})
	if got := b.takePendingManagedOllamaBackend(managedOllamaFacadePort); got != managedOllamaBackendStart+1 {
		t.Fatalf("first reconciler got %d", got)
	}

	b.reconcileProxyPortOnReady(managedOllamaFacadePort)
	select {
	case <-b.ollamaPortReady:
		t.Fatal("duplicate ready opened the gate while the backend move was in flight")
	default:
	}
}

func TestOwningOllamaReadyOpensGateAfterMove(t *testing.T) {
	engineClient, engineServer := net.Pipe()
	defer engineClient.Close()
	defer engineServer.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go worker.peer.Serve(nil, nil)

	b := &Broker{ollamaPortReady: make(chan struct{})}
	b.managedOllamaFacade.Store(true)
	b.managedOllamaBackend.Store(managedOllamaBackendStart + 1)
	b.ollamaBackendPort.Store(managedOllamaBackendStart)
	b.setEngineMgr(worker)
	b.setProxy(&proxyProcess{})

	go func() {
		codec := NewCodec(engineServer)
		msg, err := codec.Read()
		if err == nil {
			_ = codec.Respond(msg.ID, ollamaPortStatus{Port: managedOllamaBackendStart + 1})
		}
	}()

	b.reconcileProxyPortOnReady(managedOllamaFacadePort)
	select {
	case <-b.ollamaPortReady:
	default:
		t.Fatal("owning reconciler did not open the gate after a successful move")
	}
}

func TestEnginePortAssignmentRequest(t *testing.T) {
	engine, port, ok := enginePortAssignmentRequest("engine:set-port", []byte(`{"engine":"ollama","port":11434}`))
	if !ok || engine != "ollama" || port != 11434 {
		t.Fatalf("valid request = (%q, %d, %v), want (ollama, 11434, true)", engine, port, ok)
	}
	for _, tc := range []struct {
		method string
		params string
	}{
		{"engine:status", `{"engine":"ollama","port":11434}`},
		{"engine:set-port", `{"engine":"","port":11434}`},
		{"engine:set-port", `{"engine":"ollama","port":0}`},
		{"engine:install", `{"engine":"ollama","port":11433}`},
		{"engine:restart", `{"engine":"ollama","port":11433}`},
		{"engine:set-port", `{`},
	} {
		if _, _, ok := enginePortAssignmentRequest(tc.method, []byte(tc.params)); ok {
			t.Fatalf("unexpected engine set-port match for %s %s", tc.method, tc.params)
		}
	}
}

func TestLMStudioSetPortRequest(t *testing.T) {
	port, ok := lmstudioSetPortRequest("engine:set-port", []byte(`{"engine":"lmstudio","port":1234}`))
	if !ok || port != managedLMStudioFacadePort {
		t.Fatalf("valid LM Studio request = (%d, %v), want (%d, true)", port, ok, managedLMStudioFacadePort)
	}
	for _, tc := range []struct {
		method string
		params string
	}{
		{"engine:status", `{"engine":"lmstudio","port":1234}`},
		{"engine:set-port", `{"engine":"ollama","port":1234}`},
		{"engine:set-port", `{"engine":"lmstudio","port":0}`},
		{"engine:set-port", `{`},
	} {
		if _, ok := lmstudioSetPortRequest(tc.method, []byte(tc.params)); ok {
			t.Fatalf("unexpected LM Studio set-port match for %s %s", tc.method, tc.params)
		}
	}
}

func TestManagedLMStudioRejectsFacadeBackendPort(t *testing.T) {
	brokerClient, brokerServer := net.Pipe()
	defer brokerClient.Close()
	defer brokerServer.Close()
	b := &Broker{codec: NewCodec(brokerClient)}
	b.managedLMStudioFacade.Store(true)

	response := make(chan *Message, 1)
	go func() {
		msg, err := NewCodec(brokerServer).Read()
		if err == nil {
			response <- msg
		}
	}()
	id := json.RawMessage(`11`)
	b.relayToEngine(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "engine:set-port",
		Params:  json.RawMessage(`{"engine":"lmstudio","port":1234}`),
	})

	select {
	case got := <-response:
		if got.Error == nil || !strings.Contains(got.Error.Message, "reserved by the managed LM Studio proxy") {
			t.Fatalf("reservation response = %+v", got.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing managed LM Studio reservation response")
	}
}

func TestLMStudioSetPortUpdatesBackendCache(t *testing.T) {
	engineClient, engineServer := net.Pipe()
	brokerClient, brokerServer := net.Pipe()
	defer engineClient.Close()
	defer engineServer.Close()
	defer brokerClient.Close()
	defer brokerServer.Close()

	engine := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go engine.peer.Serve(nil, nil)
	b := &Broker{codec: NewCodec(brokerClient)}
	b.setEngineMgr(engine)

	request := make(chan *Message, 1)
	go func() {
		codec := NewCodec(engineServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		request <- msg
		_ = codec.Respond(msg.ID, ollamaPortStatus{Running: true, Port: 12400})
	}()
	response := make(chan *Message, 1)
	go func() {
		msg, err := NewCodec(brokerServer).Read()
		if err == nil {
			response <- msg
		}
	}()

	id := json.RawMessage(`12`)
	b.relayToEngine(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "engine:set-port",
		Params:  json.RawMessage(`{"engine":"lmstudio","port":12400}`),
	})
	select {
	case got := <-request:
		if got.Method != "engine:set-port" {
			t.Fatalf("relayed method = %q", got.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LM Studio set-port was not relayed")
	}
	select {
	case got := <-response:
		if got.Error != nil {
			t.Fatalf("LM Studio set-port response error: %v", got.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing LM Studio set-port response")
	}
	if got := b.lmstudioBackendPort.Load(); got != 12400 {
		t.Fatalf("cached LM Studio backend = %d, want 12400", got)
	}
}

func TestRelayRejectsEnginePortAssignmentToActiveOllamaHostAlias(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		params string
	}{
		{name: "set Ollama port", method: "engine:set-port", params: `{"engine":"ollama","port":11433}`},
		{name: "set LM Studio port", method: "engine:set-port", params: `{"engine":"lmstudio","port":11433}`},
		{name: "start custom engine override", method: "engine:start", params: `{"engine":"custom","port":11433}`},
		{name: "install and start custom engine override", method: "engine:install", params: `{"engine":"custom","port":11433,"start":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brokerConn, clientConn := net.Pipe()
			defer brokerConn.Close()
			defer clientConn.Close()
			b := &Broker{
				codec:           NewCodec(brokerConn),
				ollamaPortReady: make(chan struct{}),
			}
			b.setOllamaHostAlias(ollamaHostAlias{Port: 11433})
			close(b.ollamaPortReady)
			id := json.RawMessage(`7`)
			response := make(chan *Message, 1)
			readErr := make(chan error, 1)
			go func() {
				msg, err := NewCodec(clientConn).Read()
				if err != nil {
					readErr <- err
					return
				}
				response <- msg
			}()

			b.relayToEngine(&Message{
				JSONRPC: "2.0",
				ID:      &id,
				Method:  tc.method,
				Params:  json.RawMessage(tc.params),
			})

			select {
			case err := <-readErr:
				t.Fatal(err)
			case msg := <-response:
				if msg.Error == nil || msg.Error.Code != -32000 || !strings.Contains(msg.Error.Message, "OLLAMA_HOST proxy alias") {
					t.Fatalf("response = %+v, want alias-port rejection", msg)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for alias-port rejection")
			}
		})
	}
}

func TestBrokerRejectsLMStudioProxyPortAssignmentToActiveOllamaHostAlias(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()
	b := &Broker{codec: NewCodec(brokerConn)}
	b.setOllamaHostAlias(ollamaHostAlias{Port: 11433})
	id := json.RawMessage(`8`)
	response := make(chan *Message, 1)
	readErr := make(chan error, 1)
	go func() {
		msg, err := NewCodec(clientConn).Read()
		if err != nil {
			readErr <- err
			return
		}
		response <- msg
	}()

	b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "lmstudio-proxy:set-port",
		Params:  json.RawMessage(`{"port":11433}`),
	})

	select {
	case err := <-readErr:
		t.Fatal(err)
	case msg := <-response:
		if msg.Error == nil || msg.Error.Code != -32000 || !strings.Contains(msg.Error.Message, "OLLAMA_HOST proxy alias") {
			t.Fatalf("response = %+v, want alias-port rejection", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for LM Studio alias-port rejection")
	}
}

func TestRelayCachesActualOllamaPortFromResponse(t *testing.T) {
	engineClient, engineServer := net.Pipe()
	defer engineClient.Close()
	defer engineServer.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go worker.peer.Serve(nil, nil)

	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()
	b := &Broker{codec: NewCodec(brokerConn)}
	b.setEngineMgr(worker)

	go func() {
		codec := NewCodec(engineServer)
		request, err := codec.Read()
		if err == nil {
			_ = codec.Respond(request.ID, map[string]any{"engine": "ollama", "port": 11435})
		}
	}()
	response := make(chan *Message, 1)
	go func() {
		msg, _ := NewCodec(clientConn).Read()
		response <- msg
	}()

	id := json.RawMessage(`9`)
	b.relayToEngine(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "engine:start",
		Params:  json.RawMessage(`{"engine":"ollama","port":12000}`),
	})

	select {
	case msg := <-response:
		if msg == nil || msg.Error != nil {
			t.Fatalf("response = %+v, want success", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for engine:start response")
	}
	if got := b.ollamaBackendPort.Load(); got != 11435 {
		t.Fatalf("cached Ollama backend port = %d, want returned port 11435", got)
	}
}

func TestBrokerDoesNotExposeInternalReservationSetter(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()
	b := &Broker{codec: NewCodec(brokerConn)}
	id := json.RawMessage(`10`)
	response := make(chan *Message, 1)
	go func() {
		msg, _ := NewCodec(clientConn).Read()
		response <- msg
	}()

	b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "engine:set-reserved-port",
		Params:  json.RawMessage(`{"port":0}`),
	})

	select {
	case msg := <-response:
		if msg == nil || msg.Error == nil || msg.Error.Code != -32601 {
			t.Fatalf("response = %+v, want method-not-found", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for private-method rejection")
	}
}

func TestNeedsOllamaPortGate(t *testing.T) {
	for _, tc := range []struct {
		method string
		params string
		want   bool
	}{
		{"engine:get-installed", `{}`, true},
		{"engine:status", `{"engine":"ollama"}`, true},
		{"engine:install", `{"engine":"ollama"}`, true},
		{"engine:start", `{"engine":"ollama"}`, true},
		{"engine:restart", `{"engine":"ollama"}`, true},
		{"engine:status", `{"engine":"lmstudio"}`, false},
		{"engine:models", `{"engine":"ollama"}`, false},
		{"engine:status", `{`, false},
	} {
		if got := needsOllamaPortGate(tc.method, []byte(tc.params)); got != tc.want {
			t.Errorf("needsOllamaPortGate(%q, %s) = %v, want %v", tc.method, tc.params, got, tc.want)
		}
	}
}

func TestOllamaPresenceRequestWaitsForPortGate(t *testing.T) {
	workerClient, workerServer := net.Pipe()
	defer workerClient.Close()
	defer workerServer.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(workerClient))}
	go worker.peer.Serve(nil, nil)

	brokerClient, brokerServer := net.Pipe()
	defer brokerClient.Close()
	defer brokerServer.Close()
	b := &Broker{codec: NewCodec(brokerClient), ollamaPortReady: make(chan struct{})}
	b.setEngineMgr(worker)
	b.managedOllamaFacade.Store(true)
	b.ollamaBackendPort.Store(managedOllamaFacadePort)

	method := make(chan string, 1)
	workerErr := make(chan error, 1)
	go func() {
		codec := NewCodec(workerServer)
		request, err := codec.Read()
		if err != nil {
			workerErr <- err
			return
		}
		method <- request.Method
		workerErr <- codec.Respond(request.ID, map[string]any{"engines": []any{}})
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
		Method:  "engine:get-installed",
	})

	select {
	case got := <-method:
		t.Fatalf("%q was relayed before the Ollama port gate opened", got)
	case err := <-workerErr:
		t.Fatalf("worker failed before the Ollama port gate opened: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Model the successful backend move before releasing the existing gate.
	b.ollamaBackendPort.Store(managedOllamaBackendStart)
	close(b.ollamaPortReady)
	select {
	case got := <-method:
		if got != "engine:get-installed" {
			t.Fatalf("method = %q, want engine:get-installed", got)
		}
	case err := <-workerErr:
		t.Fatalf("worker failed after the Ollama port gate opened: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("engine:get-installed was not relayed after the Ollama port gate opened")
	}
	if err := <-workerErr; err != nil {
		t.Fatalf("respond to relayed request: %v", err)
	}
	select {
	case err := <-response:
		if err != nil {
			t.Fatalf("read broker response: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("broker did not return the relayed response")
	}
}

func TestNextFreeProxyPort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  int
		req   int
		taken []int
	}{
		{"free returns requested", 11435, 11435, nil},
		{"free with others taken", 11435, 11435, []int{11434, 1234}},
		{"single collision bumps by one", 11435, 11434, []int{11434}},
		{"consecutive collisions skip", 11436, 11434, []int{11434, 11435}},
		{"gap above collision", 11435, 11434, []int{11434, 11436}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taken := map[int]bool{}
			for _, p := range tc.taken {
				taken[p] = true
			}
			if got := nextFreeProxyPort(tc.req, taken); got != tc.want {
				t.Errorf("nextFreeProxyPort(%d, %v) = %d, want %d", tc.req, tc.taken, got, tc.want)
			}
		})
	}
}
