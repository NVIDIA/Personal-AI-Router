// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/errors"
)

func TestInheritedOllamaHostAlias(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		want   ollamaHostAlias
		wantOK bool
	}{
		{name: "unset"},
		{name: "bare default", raw: "localhost"},
		{name: "explicit default", raw: "http://127.0.0.1:11434"},
		{name: "bare localhost custom", raw: "localhost:11433", want: ollamaHostAlias{Address: "127.0.0.1:11433", AlternateAddress: "[::1]:11433", Port: 11433}, wantOK: true},
		{name: "quoted local custom", raw: " 'http://127.0.0.1:11433' ", want: ollamaHostAlias{Address: "127.0.0.1:11433", Port: 11433}, wantOK: true},
		{name: "http default port", raw: "http://localhost", want: ollamaHostAlias{Address: "127.0.0.1:80", AlternateAddress: "[::1]:80", Port: 80}, wantOK: true},
		{name: "ipv4 wildcard normalizes", raw: "0.0.0.0:11433", want: ollamaHostAlias{Address: "127.0.0.1:11433", AlternateAddress: "[::1]:11433", Port: 11433}, wantOK: true},
		{name: "schemed ipv4 wildcard normalizes", raw: "http://0.0.0.0:11433", want: ollamaHostAlias{Address: "127.0.0.1:11433", AlternateAddress: "[::1]:11433", Port: 11433}, wantOK: true},
		{name: "ipv6 wildcard normalizes", raw: "[::]:11433", want: ollamaHostAlias{Address: "127.0.0.1:11433", AlternateAddress: "[::1]:11433", Port: 11433}, wantOK: true},
		{name: "schemed ipv6 wildcard normalizes", raw: "http://[::]:11433", want: ollamaHostAlias{Address: "127.0.0.1:11433", AlternateAddress: "[::1]:11433", Port: 11433}, wantOK: true},
		{name: "ipv6 loopback", raw: "http://[::1]:11433/", want: ollamaHostAlias{Address: "[::1]:11433", Port: 11433}, wantOK: true},
		{name: "empty host is local", raw: ":11433", want: ollamaHostAlias{Address: "127.0.0.1:11433", AlternateAddress: "[::1]:11433", Port: 11433}, wantOK: true},
		{name: "https never intercepted", raw: "https://localhost:11433"},
		{name: "remote hostname never resolved", raw: "http://example.test:11433"},
		{name: "private LAN address is remote", raw: "192.168.1.20:11433"},
		{name: "userinfo rejected", raw: "http://user@localhost:11433"},
		{name: "base path rejected", raw: "http://localhost:11433/base"},
		{name: "query rejected", raw: "http://localhost:11433?x=1"},
		{name: "invalid port falls back to default", raw: "localhost:not-a-port"},
		{name: "schemed invalid port falls back to HTTP default", raw: "http://localhost:not-a-port", want: ollamaHostAlias{Address: "127.0.0.1:80", AlternateAddress: "[::1]:80", Port: 80}, wantOK: true},
		{name: "zero port rejected", raw: "localhost:0"},
		{name: "ollama cloud special case", raw: "ollama.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := inheritedOllamaHostAlias(tc.raw)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("inheritedOllamaHostAlias(%q) = (%+v, %v), want (%+v, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPrepareOllamaHostAliasReservesBackendPort(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11435")
	b := brokerWithEngineStatus(t, "lmstudio", 1234)
	b.prepareOllamaHostAlias(true, managedOllamaFacadePort)
	alias := b.currentOllamaHostAlias()
	if alias != (ollamaHostAlias{Address: "127.0.0.1:11435", Port: 11435}) {
		t.Fatalf("prepared alias = %+v", alias)
	}
	available := func(port int) bool {
		return port != alias.Port && (port == managedOllamaFacadePort || port == 11435 || port == 11436)
	}
	plan := planManagedOllamaPorts(true, ollamaPortStatus{Port: managedOllamaFacadePort}, available)
	if plan.BackendPort != 11436 {
		t.Fatalf("backend port = %d, want 11436 because 11435 is reserved for OLLAMA_HOST", plan.BackendPort)
	}
}

func TestPrepareOllamaHostAliasRejectsAnyConfiguredEnginePort(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:15555")
	b := brokerWithEngineInventory(t, map[string]int{
		"ollama":   managedOllamaFacadePort,
		"lmstudio": 1234,
		"custom":   15555,
	})
	b.prepareOllamaHostAlias(true, managedOllamaFacadePort)
	if alias := b.currentOllamaHostAlias(); alias != (ollamaHostAlias{}) {
		t.Fatalf("alias claimed configured custom-engine port: %+v", alias)
	}
}

func TestReservedOllamaHostAliasPort(t *testing.T) {
	for _, tc := range []struct {
		name             string
		port             int
		lmstudioBackend  int
		lmstudioProxy    int
		wantReasonSubstr string
	}{
		{name: "free", port: 15555, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort},
		{name: "lmstudio backend", port: managedLMStudioBackendStart, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "backend"},
		{name: "lmstudio proxy", port: managedLMStudioFacadePort, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "proxy"},
		// The managed LM Studio facade is prepared after the alias, so both of
		// its well-known ports are reserved even before the backend moves.
		{name: "managed lmstudio backend target", port: managedLMStudioBackendStart, lmstudioBackend: defaultLMStudioPort, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "managed LM Studio backend"},
		{name: "lmstudio compatibility proxy", port: managedLMStudioFacadePort, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: 1300, wantReasonSubstr: "LM Studio compatibility proxy"},
		{name: "node info", port: 14318, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "node-info"},
		{name: "errors", port: 14319, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "errors"},
		{name: "workloads", port: 14320, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "workload"},
		{name: "cluster manager", port: 14321, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "cluster manager"},
		{name: "engine models", port: 14322, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "engine model"},
		{name: "engine control", port: 14323, lmstudioBackend: managedLMStudioBackendStart, lmstudioProxy: managedLMStudioFacadePort, wantReasonSubstr: "engine control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enginePorts := map[int]string{}
			if tc.lmstudioBackend > 0 {
				enginePorts[tc.lmstudioBackend] = "lmstudio"
			}
			reason := (&Broker{servicePorts: resolveServicePorts(func(string) string { return "" })}).reservedOllamaHostAliasPort(tc.port, enginePorts, tc.lmstudioProxy)
			if tc.wantReasonSubstr == "" && reason != "" {
				t.Fatalf("reason = %q, want none", reason)
			}
			if tc.wantReasonSubstr != "" && !strings.Contains(reason, tc.wantReasonSubstr) {
				t.Fatalf("reason = %q, want substring %q", reason, tc.wantReasonSubstr)
			}
		})
	}
}

func TestConfiguredLMStudioProxyPort(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	if got := configuredLMStudioProxyPort(); got != managedLMStudioFacadePort {
		t.Fatalf("missing persisted port = %d, want default %d", got, managedLMStudioFacadePort)
	}
	path, err := appdir.Path(lmstudioProxyPortFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"port":1240}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := configuredLMStudioProxyPort(); got != 1240 {
		t.Fatalf("persisted port = %d, want 1240", got)
	}
}

func TestPrepareOllamaHostAliasHonorsOptOutAndBackendOwnership(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11433")
	for _, tc := range []struct {
		name        string
		enabled     bool
		backendPort int
	}{
		{name: "force ports opt out", enabled: false, backendPort: 11434},
		{name: "custom backend owns alias port", enabled: true, backendPort: 11433},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Broker{}
			b.prepareOllamaHostAlias(tc.enabled, tc.backendPort)
			if alias := b.currentOllamaHostAlias(); alias != (ollamaHostAlias{}) {
				t.Fatalf("unsafe alias prepared: %+v", alias)
			}
		})
	}
}

func brokerWithEngineStatus(t *testing.T, engine string, port int) *Broker {
	return brokerWithEngineInventory(t, map[string]int{engine: port})
}

func brokerWithEngineInventory(t *testing.T, ports map[string]int) *Broker {
	t.Helper()
	client, server := net.Pipe()
	worker := &rpcWorker{peer: NewPeer(NewCodec(client))}
	go worker.peer.Serve(nil, nil)
	b := &Broker{}
	b.setEngineMgr(worker)
	go func() {
		codec := NewCodec(server)
		request, err := codec.Read()
		if err != nil {
			return
		}
		switch request.Method {
		case "engine:status":
			var params struct {
				Engine string `json:"engine"`
			}
			if json.Unmarshal(request.Params, &params) != nil {
				_ = codec.RespondError(request.ID, -32602, "invalid engine status request")
				return
			}
			_ = codec.Respond(request.ID, map[string]any{"port": ports[params.Engine]})
		case "engine:get-installed":
			engines := make([]map[string]any, 0, len(ports))
			for engine, port := range ports {
				engines = append(engines, map[string]any{"engine": engine, "port": port})
			}
			_ = codec.Respond(request.ID, map[string]any{"engines": engines})
		default:
			_ = codec.RespondError(request.ID, -32602, "unexpected engine status request")
		}
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return b
}

func isolateOllamaHostTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
}

func TestAliasWarningReplaysAfterErrorsProcessRecovery(t *testing.T) {
	b := &Broker{nodeID: "local-node"}
	b.forwardErrorsReport(errors.ServiceError{
		ID:       ollamaHostAliasBlockedID,
		Message:  "alias still blocked",
		Severity: "warning",
		Action:   "none",
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	b.setErrors(&errorsProcess{peer: NewPeer(NewCodec(client))})

	received := make(chan *Message, 1)
	go func() {
		msg, _ := NewCodec(server).Read()
		received <- msg
	}()
	b.replayOllamaHostAliasError()
	select {
	case msg := <-received:
		if msg == nil || msg.Method != methodErrorsReport || !strings.Contains(string(msg.Params), ollamaHostAliasBlockedID) {
			t.Fatalf("replayed warning = %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovered errors process warning replay")
	}
}

// A managed backend whose configured port is occupied advances to the next
// free port. The alias is bound by the proxy only after both facades are
// prepared, so a plain TCP probe still reports it free — it must be treated as
// taken or the advancing backend would land on it and orphan the alias.
func TestManagedBackendMovesSkipTheOllamaHostAlias(t *testing.T) {
	for _, tc := range []struct {
		name         string
		facadePort   int
		backendStart int
		plan         func(status ollamaPortStatus, available func(int) bool) managedPortPlan
	}{
		{
			name:         "ollama",
			facadePort:   managedOllamaFacadePort,
			backendStart: managedOllamaBackendStart,
			plan: func(status ollamaPortStatus, available func(int) bool) managedPortPlan {
				return planManagedOllamaPorts(true, status, available)
			},
		},
		{
			name:         "lmstudio",
			facadePort:   managedLMStudioFacadePort,
			backendStart: managedLMStudioBackendStart,
			plan: func(status ollamaPortStatus, available func(int) bool) managedPortPlan {
				return planManagedLMStudioPorts(true, status, available)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			aliasPort := tc.backendStart + 1
			want := aliasPort + 1
			b := &Broker{}
			b.setOllamaHostAlias(ollamaHostAlias{Port: aliasPort})
			// The configured backend port is occupied, so the plan advances. Only
			// the alias and the port after it are free above it, and the proxy has
			// not bound the alias yet, so a plain probe still reports it free.
			available := b.availableOffOllamaHostAlias(func(port int) bool {
				return port == tc.facadePort || port == aliasPort || port == want
			})
			got := tc.plan(ollamaPortStatus{Port: tc.backendStart}, available)
			if got.BackendPort != want {
				t.Fatalf("backend = %d, want %d (the alias must be skipped)", got.BackendPort, want)
			}
		})
	}

	// Without an alias the wrapper must be transparent.
	plain := (&Broker{}).availableOffOllamaHostAlias(func(port int) bool { return port == managedOllamaBackendStart })
	if !plain(managedOllamaBackendStart) {
		t.Fatal("no alias configured, but the port probe was still filtered")
	}
}

func TestOllamaProxyFallbackSkipsTheOllamaHostAlias(t *testing.T) {
	b := &Broker{}
	b.setOllamaHostAlias(ollamaHostAlias{Port: managedOllamaBackendStart})
	if got := b.setOllamaProxyFallback(); got == managedOllamaBackendStart {
		t.Fatalf("proxy fallback = %d, want a port other than the alias", got)
	}
	lm := &Broker{}
	lm.setOllamaHostAlias(ollamaHostAlias{Port: managedLMStudioBackendStart})
	if got := lm.setLMStudioProxyFallback(); got == managedLMStudioBackendStart {
		t.Fatalf("LM Studio proxy fallback = %d, want a port other than the alias", got)
	}
}

func TestDisableAliasClearsEngineReservation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(client))}
	go worker.peer.Serve(nil, nil)
	b := &Broker{}
	b.setOllamaHostAlias(ollamaHostAlias{
		Address:          "127.0.0.1:11433",
		AlternateAddress: "[::1]:11433",
		Port:             11433,
	})
	b.setEngineMgr(worker)

	reserved := make(chan int, 1)
	go func() {
		codec := NewCodec(server)
		request, err := codec.Read()
		if err != nil {
			return
		}
		var params struct {
			Port int `json:"port"`
		}
		if request.Method == "internal:set-reserved-port" && json.Unmarshal(request.Params, &params) == nil {
			reserved <- params.Port
		}
		_ = codec.Respond(request.ID, map[string]int{"port": params.Port})
	}()

	b.disableOllamaHostAliasReservation()
	if alias := b.currentOllamaHostAlias(); alias != (ollamaHostAlias{}) {
		t.Fatalf("alias reservation not cleared: %+v", alias)
	}
	select {
	case port := <-reserved:
		if port != 0 {
			t.Fatalf("engine-manager reservation = %d, want 0", port)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for engine-manager reservation clear")
	}
}

func TestManagedAliasReservationSyncUsesReplacementEngineManager(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:15555")

	settings, settingsCodec := newTestRPCWorkerPipe(t)
	oldEngine, oldEngineCodec := newTestRPCWorkerPipe(t)
	replacement, replacementCodec := newTestRPCWorkerPipe(t)
	b := &Broker{ollamaPortReady: make(chan struct{})}
	b.setSettings(settings)
	b.setEngineMgr(oldEngine)

	oldReservation := make(chan int, 1)
	go func() {
		msg, err := oldEngineCodec.Read()
		if err != nil {
			return
		}
		var request struct {
			Port int `json:"port"`
		}
		_ = json.Unmarshal(msg.Params, &request)
		oldReservation <- request.Port
		_ = oldEngineCodec.Respond(msg.ID, map[string]int{"port": request.Port})
	}()

	replacementReservation := make(chan int, 1)
	go func() {
		for range 3 {
			msg, err := replacementCodec.Read()
			if err != nil {
				return
			}
			switch msg.Method {
			case "engine:status":
				_ = replacementCodec.Respond(msg.ID, ollamaPortStatus{Port: 16000})
			case "engine:get-installed":
				_ = replacementCodec.Respond(msg.ID, map[string]any{
					"engines": []map[string]any{{"engine": "ollama", "port": 16000}},
				})
			case "internal:set-reserved-port":
				var request struct {
					Port int `json:"port"`
				}
				_ = json.Unmarshal(msg.Params, &request)
				replacementReservation <- request.Port
				_ = replacementCodec.Respond(msg.ID, map[string]int{"port": request.Port})
			default:
				_ = replacementCodec.RespondError(msg.ID, -32601, "unexpected method")
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		b.prepareManagedOllamaFacadeWithPortCheck(func(int) bool { return true })
		close(done)
	}()

	// Hold preparation at its first blocking RPC, replace engine-manager, then
	// let it commit the alias. The deferred final sync must resolve this new
	// generation at execution time rather than use the entry-time handle.
	settingsRequest, err := settingsCodec.Read()
	if err != nil {
		t.Fatal(err)
	}
	b.setEngineMgr(replacement)
	if err := settingsCodec.Respond(settingsRequest.ID, map[string]bool{"value": true}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for managed alias preparation")
	}
	select {
	case port := <-replacementReservation:
		if port != 15555 {
			t.Fatalf("replacement engine-manager reservation = %d, want 15555", port)
		}
	default:
		t.Fatal("replacement engine-manager did not receive the committed alias reservation")
	}
	select {
	case port := <-oldReservation:
		t.Fatalf("stale engine-manager received final reservation %d", port)
	default:
	}
}

func TestAliasReservationSyncsSerializeStateOrder(t *testing.T) {
	engine, engineCodec := newTestRPCWorkerPipe(t)
	b := &Broker{}
	b.setEngineMgr(engine)
	b.setOllamaHostAlias(ollamaHostAlias{Port: 15555})

	firstDone := make(chan struct{})
	go func() {
		b.syncCurrentEngineOllamaHostAliasReservation()
		close(firstDone)
	}()
	first, err := engineCodec.Read()
	if err != nil {
		t.Fatal(err)
	}
	var firstRequest struct {
		Port int `json:"port"`
	}
	if first.Method != "internal:set-reserved-port" || json.Unmarshal(first.Params, &firstRequest) != nil || firstRequest.Port != 15555 {
		t.Fatalf("first reservation request = %s %s", first.Method, first.Params)
	}

	// Clear the alias while the older request is still waiting for its reply.
	// The newer sync must not reach the worker until the older call completes;
	// it then reads and applies the current zero state last.
	b.setOllamaHostAlias(ollamaHostAlias{})
	secondDone := make(chan struct{})
	go func() {
		b.syncCurrentEngineOllamaHostAliasReservation()
		close(secondDone)
	}()
	type readResult struct {
		msg *Message
		err error
	}
	secondRead := make(chan readResult, 1)
	go func() {
		msg, err := engineCodec.Read()
		secondRead <- readResult{msg: msg, err: err}
	}()
	select {
	case result := <-secondRead:
		t.Fatalf("new reservation overtook the in-flight update: msg=%+v err=%v", result.msg, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := engineCodec.Respond(first.ID, map[string]int{"port": 15555}); err != nil {
		t.Fatal(err)
	}

	var second *Message
	select {
	case result := <-secondRead:
		if result.err != nil {
			t.Fatal(result.err)
		}
		second = result.msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for current reservation update")
	}
	var secondRequest struct {
		Port int `json:"port"`
	}
	if second.Method != "internal:set-reserved-port" || json.Unmarshal(second.Params, &secondRequest) != nil || secondRequest.Port != 0 {
		t.Fatalf("second reservation request = %s %s", second.Method, second.Params)
	}
	if err := engineCodec.Respond(second.ID, map[string]int{"port": 0}); err != nil {
		t.Fatal(err)
	}
	for name, done := range map[string]<-chan struct{}{"first": firstDone, "second": secondDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s reservation sync did not finish", name)
		}
	}
}

func TestAliasBindFailureReleasesReservationButKeepsWarning(t *testing.T) {
	engine, engineCodec := newTestRPCWorkerPipe(t)
	b := &Broker{nodeID: "local-node"}
	b.setEngineMgr(engine)
	b.setOllamaHostAlias(ollamaHostAlias{
		Address:          "127.0.0.1:15555",
		AlternateAddress: "[::1]:15555",
		Port:             15555,
	})

	reservation := make(chan int, 1)
	go func() {
		msg, err := engineCodec.Read()
		if err != nil {
			return
		}
		var request struct {
			Port int `json:"port"`
		}
		_ = json.Unmarshal(msg.Params, &request)
		reservation <- request.Port
		_ = engineCodec.Respond(msg.ID, map[string]int{"port": request.Port})
	}()

	params, err := json.Marshal(errors.ServiceError{
		ID:       ollamaHostAliasBlockedID,
		Message:  "alias bind failed",
		Severity: "warning",
		Action:   "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	b.forwardProxyNotification(methodErrorsReport, params)

	if alias := b.currentOllamaHostAlias(); alias != (ollamaHostAlias{}) {
		t.Fatalf("failed alias still reserved in broker: %+v", alias)
	}
	select {
	case port := <-reservation:
		if port != 0 {
			t.Fatalf("engine-manager reservation = %d, want 0", port)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed-alias reservation release")
	}
	b.ollamaHostAliasErrorMu.Lock()
	warning := b.ollamaHostAliasError
	b.ollamaHostAliasErrorMu.Unlock()
	if warning == nil || warning.ID != ollamaHostAliasBlockedID {
		t.Fatal("bind warning was cleared while releasing the reservation")
	}
	if b.rejectOllamaHostAliasPort(&Message{}, 15555, "Ollama") {
		t.Fatal("released alias still rejects assignment to the existing owner's port")
	}
	if !b.availableOffOllamaHostAlias(func(port int) bool { return port == 15555 })(15555) {
		t.Fatal("released alias still excludes the existing owner's port from planning")
	}
}

func TestStaleAliasBindFailureDoesNotReleaseReplacementReservation(t *testing.T) {
	b := &Broker{nodeID: "local-node"}
	b.setOllamaHostAlias(ollamaHostAlias{Address: "127.0.0.1:15555", Port: 15555})
	staleGeneration, _ := b.beginOllamaProxyGeneration()
	b.setOllamaHostAlias(ollamaHostAlias{Address: "127.0.0.1:16666", Port: 16666})
	currentGeneration, _ := b.beginOllamaProxyGeneration()

	params, err := json.Marshal(errors.ServiceError{
		ID:       ollamaHostAliasBlockedID,
		Message:  "stale alias bind failed",
		Severity: "warning",
		Action:   "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	b.forwardProxyNotificationForGeneration(staleGeneration, methodErrorsReport, params)

	if got := b.currentOllamaProxyGeneration(); got != currentGeneration {
		t.Fatalf("current proxy generation = %d, want %d", got, currentGeneration)
	}
	if alias := b.currentOllamaHostAlias(); alias.Port != 16666 {
		t.Fatalf("stale failure changed replacement alias: %+v", alias)
	}
	b.ollamaHostAliasErrorMu.Lock()
	warning := b.ollamaHostAliasError
	b.ollamaHostAliasErrorMu.Unlock()
	if warning != nil {
		t.Fatal("stale failure published a warning for the replacement generation")
	}
}

// TestResolveServicePorts covers the env-override contract: spec defaults when
// nothing is set, per-service overrides when valid, and out-of-range or
// non-numeric values ignored with the default kept.
func TestResolveServicePorts(t *testing.T) {
	t.Run("defaults with no overrides", func(t *testing.T) {
		ports := resolveServicePorts(func(string) string { return "" })
		if ports.NodeInfo != nodeInfoHTTPPort || ports.Errors != errorsHTTPPort ||
			ports.Workload != workloadHTTPPort || ports.ClusterManager != clusterManagerHTTPPort ||
			ports.EngineHTTP != engineManagerHTTPPort || ports.EngineControl != engineControlPort ||
			ports.OllamaProxy != 0 || ports.LMStudioProxy != 0 {
			t.Fatalf("defaults = %+v, want spec defaults and zero proxy ports", ports)
		}
	})

	t.Run("valid overrides applied", func(t *testing.T) {
		env := map[string]string{
			"NVPAIR_SERVICE_NODE_INFO_PORT":       "24318",
			"NVPAIR_SERVICE_ERRORS_PORT":          "24319",
			"NVPAIR_SERVICE_WORKLOAD_PORT":        "24320",
			"NVPAIR_SERVICE_CLUSTER_MANAGER_PORT": "24321",
			"NVPAIR_SERVICE_ENGINE_HTTP_PORT":     "24322",
			"NVPAIR_SERVICE_ENGINE_CONTROL_PORT":  "24323",
			"NVPAIR_SERVICE_OLLAMA_PROXY_PORT":    "24324",
			"NVPAIR_SERVICE_LMSTUDIO_PROXY_PORT":  "24325",
		}
		ports := resolveServicePorts(func(k string) string { return env[k] })
		if ports.NodeInfo != 24318 || ports.Errors != 24319 || ports.Workload != 24320 ||
			ports.ClusterManager != 24321 || ports.EngineHTTP != 24322 ||
			ports.EngineControl != 24323 || ports.OllamaProxy != 24324 || ports.LMStudioProxy != 24325 {
			t.Fatalf("overrides = %+v, want every valid override applied", ports)
		}
	})

	t.Run("invalid overrides ignored", func(t *testing.T) {
		env := map[string]string{
			"NVPAIR_SERVICE_NODE_INFO_PORT":       "not-a-port",
			"NVPAIR_SERVICE_ERRORS_PORT":          "0",
			"NVPAIR_SERVICE_WORKLOAD_PORT":        "65536",
			"NVPAIR_SERVICE_CLUSTER_MANAGER_PORT": "-1",
		}
		ports := resolveServicePorts(func(k string) string { return env[k] })
		if ports.NodeInfo != nodeInfoHTTPPort || ports.Errors != errorsHTTPPort ||
			ports.Workload != workloadHTTPPort || ports.ClusterManager != clusterManagerHTTPPort {
			t.Fatalf("invalid overrides = %+v, want defaults kept", ports)
		}
	})
}
