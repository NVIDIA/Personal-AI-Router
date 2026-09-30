// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		if assert.NoError(t, json.NewDecoder(server).Decode(&msg)) {
			method <- msg.Method
		}
	}()
	done := make(chan bool, 1)
	go func() { done <- broker.restoreEnabledEnginesAfterPortGate(context.Background()) }()

	select {
	case got := <-method:
		require.FailNowf(t, "restore was sent before either port gate opened", "method %q", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(broker.ollamaPortReady)
	select {
	case got := <-method:
		require.FailNowf(t, "restore was sent before the LM Studio port gate opened", "method %q", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(broker.lmstudioPortReady)
	select {
	case got := <-method:
		require.Equal(t, restoreEnabledEnginesMethod, got, "restore method")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "restore was not sent after both port gates opened")
	}
	require.True(t, <-done, "restore wait reported cancellation")
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
			require.Equal(t, tc.want, planManagedOllamaPorts(tc.enabled, tc.status, tc.available), "plan")
		})
	}
}

func TestNextAvailablePortExcludingCustomBackend(t *testing.T) {
	available := func(port int) bool { return port == 11435 || port == 11436 }
	require.Equal(t, 11436, nextAvailablePortExcluding(11435, []int{11435}, available), "fallback must skip the configured backend")
}

func TestTakePendingManagedOllamaBackendOnce(t *testing.T) {
	b := &Broker{}
	b.ollamaState().managedFacade.Store(true)
	b.managedOllamaBackend.Store(11435)

	require.Equal(t, 0, b.takePendingManagedOllamaBackend(11436), "wrong bound port consumed pending backend")
	require.Equal(t, 11435, b.takePendingManagedOllamaBackend(managedOllamaFacadePort), "first facade reconciliation")
	require.Equal(t, 0, b.takePendingManagedOllamaBackend(managedOllamaFacadePort), "duplicate reconciliation consumed backend twice")
}

func TestOllamaBackendSourcePort(t *testing.T) {
	b := &Broker{}
	require.Equal(t, managedOllamaFacadePort, b.ollamaBackendSourcePort(), "unset source")
	b.ollamaState().backendPort.Store(managedOllamaBackendStart)
	require.Equal(t, managedOllamaBackendStart, b.ollamaBackendSourcePort(), "configured source")
}

func TestDuplicateOllamaReadyDoesNotOpenGateDuringMove(t *testing.T) {
	b := &Broker{ollamaPortReady: make(chan struct{})}
	b.ollamaState().managedFacade.Store(true)
	b.managedOllamaBackend.Store(managedOllamaBackendStart + 1)
	b.setProxy(&proxyProcess{})
	require.Equal(t, managedOllamaBackendStart+1, b.takePendingManagedOllamaBackend(managedOllamaFacadePort), "first reconciler")

	b.reconcileProxyPortOnReady(managedOllamaFacadePort)
	requireGateShutNow(t, b.ollamaPortReady, "a duplicate ready while the backend move was in flight")
}

func TestOwningOllamaReadyOpensGateAfterMove(t *testing.T) {
	engineClient, engineServer := net.Pipe()
	defer engineClient.Close()
	defer engineServer.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go worker.peer.Serve(nil, nil)

	b := &Broker{ollamaPortReady: make(chan struct{})}
	b.ollamaState().managedFacade.Store(true)
	b.managedOllamaBackend.Store(managedOllamaBackendStart + 1)
	b.ollamaState().backendPort.Store(managedOllamaBackendStart)
	b.setEngineMgr(worker)
	b.setProxy(&proxyProcess{})

	go func() {
		codec := NewCodec(engineServer)
		msg, err := codec.Read()
		if assert.NoError(t, err) {
			assert.NoError(t, codec.Respond(msg.ID, ollamaPortStatus{Port: managedOllamaBackendStart + 1}))
		}
	}()

	b.reconcileProxyPortOnReady(managedOllamaFacadePort)
	requireGateOpenNow(t, b.ollamaPortReady, "a successful backend move")
}

func TestEnginePortAssignmentRequest(t *testing.T) {
	engine, port, ok := enginePortAssignmentRequest("engine:set-port", []byte(`{"engine":"ollama","port":11434}`))
	require.True(t, ok, "valid engine set-port request")
	require.Equal(t, "ollama", engine, "valid engine set-port request")
	require.Equal(t, 11434, port, "valid engine set-port request")
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
		_, _, ok := enginePortAssignmentRequest(tc.method, []byte(tc.params))
		require.False(t, ok, "unexpected engine set-port match for %s %s", tc.method, tc.params)
	}
}

func TestLMStudioSetPortRequest(t *testing.T) {
	port, ok := lmstudioSetPortRequest("engine:set-port", []byte(`{"engine":"lmstudio","port":1234}`))
	require.True(t, ok, "valid LM Studio request")
	require.Equal(t, managedLMStudioFacadePort, port, "valid LM Studio request")
	for _, tc := range []struct {
		method string
		params string
	}{
		{"engine:status", `{"engine":"lmstudio","port":1234}`},
		{"engine:set-port", `{"engine":"ollama","port":1234}`},
		{"engine:set-port", `{"engine":"lmstudio","port":0}`},
		{"engine:set-port", `{`},
	} {
		_, ok := lmstudioSetPortRequest(tc.method, []byte(tc.params))
		require.False(t, ok, "unexpected LM Studio set-port match for %s %s", tc.method, tc.params)
	}
}

func TestManagedLMStudioRejectsFacadeBackendPort(t *testing.T) {
	brokerClient, brokerServer := net.Pipe()
	defer brokerClient.Close()
	defer brokerServer.Close()
	b := &Broker{codec: NewCodec(brokerClient)}
	b.lmstudioState().managedFacade.Store(true)

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
		require.NotNil(t, got.Error, "reservation response")
		require.Contains(t, got.Error.Message, "reserved by the managed LM Studio proxy", "reservation response")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "missing managed LM Studio reservation response")
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
		if !assertRPCRead(t, err) {
			return
		}
		request <- msg
		assert.NoError(t, codec.Respond(msg.ID, ollamaPortStatus{Running: true, Port: 12400}))
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
		require.Equal(t, "engine:set-port", got.Method, "relayed method")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "LM Studio set-port was not relayed")
	}
	select {
	case got := <-response:
		require.Nil(t, got.Error, "LM Studio set-port response error")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "missing LM Studio set-port response")
	}
	require.Equal(t, int32(12400), b.lmstudioState().backendPort.Load(), "cached LM Studio backend")
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
				require.NoError(t, err)
			case msg := <-response:
				require.NotNil(t, msg.Error, "response (%v)", msg)
				require.Equal(t, -32000, msg.Error.Code, "response (%v)", msg)
				require.Contains(t, msg.Error.Message, "OLLAMA_HOST proxy alias", "response (%v)", msg)
			case <-time.After(2 * time.Second):
				require.FailNow(t, "timed out waiting for alias-port rejection")
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
		require.NoError(t, err)
	case msg := <-response:
		require.NotNil(t, msg.Error, "response (%v)", msg)
		require.Equal(t, -32000, msg.Error.Code, "response (%v)", msg)
		require.Contains(t, msg.Error.Message, "OLLAMA_HOST proxy alias", "response (%v)", msg)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for LM Studio alias-port rejection")
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
			assert.NoError(t, codec.Respond(request.ID, map[string]any{"engine": "ollama", "port": 11435}))
		}
	}()
	response := make(chan *Message, 1)
	go func() {
		msg, err := NewCodec(clientConn).Read()
		assert.NoError(t, err)
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
		require.NotNil(t, msg, "response")
		require.Nil(t, msg.Error, "response (%v)", msg)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for engine:start response")
	}
	require.Equal(t, int32(11435), b.ollamaState().backendPort.Load(), "cached Ollama backend port")
}

func TestBrokerDoesNotExposeInternalReservationSetter(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()
	b := &Broker{codec: NewCodec(brokerConn)}
	id := json.RawMessage(`10`)
	response := make(chan *Message, 1)
	go func() {
		msg, err := NewCodec(clientConn).Read()
		assert.NoError(t, err)
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
		require.NotNil(t, msg, "response")
		require.NotNil(t, msg.Error, "response (%v)", msg)
		require.Equal(t, -32601, msg.Error.Code, "response (%v)", msg)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for private-method rejection")
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
		assert.Equal(t, tc.want, needsOllamaPortGate(tc.method, []byte(tc.params)), "needsOllamaPortGate(%q, %s)", tc.method, tc.params)
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
	b.ollamaState().managedFacade.Store(true)
	b.ollamaState().backendPort.Store(managedOllamaFacadePort)

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
		require.FailNowf(t, "request was relayed before the Ollama port gate opened", "method %q", got)
	case err := <-workerErr:
		require.FailNowf(t, "worker failed before the Ollama port gate opened", "%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Model the successful backend move before releasing the existing gate.
	b.ollamaState().backendPort.Store(managedOllamaBackendStart)
	close(b.ollamaPortReady)
	select {
	case got := <-method:
		require.Equal(t, "engine:get-installed", got, "method")
	case err := <-workerErr:
		require.FailNowf(t, "worker failed after the Ollama port gate opened", "%v", err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "engine:get-installed was not relayed after the Ollama port gate opened")
	}
	require.NoError(t, <-workerErr, "respond to relayed request")
	select {
	case err := <-response:
		require.NoError(t, err, "read broker response")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "broker did not return the relayed response")
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
			assert.Equal(t, tc.want, nextFreeProxyPort(tc.req, taken), "nextFreeProxyPort")
		})
	}
}
