// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
			require.Equal(t, tc.wantOK, ok, "inheritedOllamaHostAlias")
			require.Equal(t, tc.want, got, "inheritedOllamaHostAlias")
		})
	}
}

func TestPrepareOllamaHostAliasReservesBackendPort(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11435")
	b := brokerWithEngineStatus(t, "lmstudio", 1234)
	b.prepareOllamaHostAlias(true, managedOllamaFacadePort)
	alias := b.currentOllamaHostAlias()
	require.Equal(t, ollamaHostAlias{Address: "127.0.0.1:11435", Port: 11435}, alias, "prepared alias")
	available := func(port int) bool {
		return port != alias.Port && (port == managedOllamaFacadePort || port == 11435 || port == 11436)
	}
	plan := planManagedOllamaPorts(true, ollamaPortStatus{Port: managedOllamaFacadePort}, available)
	require.Equal(t, 11436, plan.BackendPort, "backend port")
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
	require.Equal(t, ollamaHostAlias{}, b.currentOllamaHostAlias(), "alias claimed configured custom-engine port")
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
			// The sibling-proxy set is built from the engine table in
			// production; here it is spelled out so a case can vary the
			// persisted port independently.
			siblings := map[int]string{
				managedLMStudioFacadePort:   fmt.Sprintf("the LM Studio compatibility proxy uses port %d", managedLMStudioFacadePort),
				managedLMStudioBackendStart: fmt.Sprintf("the managed LM Studio backend uses port %d", managedLMStudioBackendStart),
			}
			if tc.lmstudioProxy > 0 {
				siblings[tc.lmstudioProxy] = fmt.Sprintf("the LM Studio proxy is configured on port %d", tc.lmstudioProxy)
			}
			reason := reservedOllamaHostAliasPort(tc.port, enginePorts, siblings)
			if tc.wantReasonSubstr == "" {
				require.Empty(t, reason)
			} else {
				require.Contains(t, reason, tc.wantReasonSubstr)
			}
		})
	}
}

// The alias reads each sibling proxy's persisted port from its own file, so a
// third engine is covered by adding a table entry rather than another reader.
func TestConfiguredEngineProxyPort(t *testing.T) {
	isolateOllamaHostTestConfig(t)

	for _, p := range engineProxyProfiles {
		require.Equal(t, p.FacadePort, configuredEngineProxyPort(p), "%s: missing persisted port must use its facade", p.Name)
	}

	// Persisting one engine's port must not move another's.
	stored := lmstudioProxyProfile.FacadePort + 6
	path, err := appdir.Path(lmstudioProxyProfile.PortFile)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, `{"port":%d}`, stored), 0o600))
	require.Equal(t, stored, configuredEngineProxyPort(lmstudioProxyProfile), "persisted port")
	require.Equal(t, ollamaProxyProfile.FacadePort, configuredEngineProxyPort(ollamaProxyProfile), "Ollama must not read LM Studio's port file")

	// And the reserved set the alias checks picks that persisted port up.
	b := &Broker{}
	require.NotEqual(t, "", b.siblingEngineProxyPorts(ollamaProxyProfile)[stored], "persisted sibling port (%v)", stored)
}

// A proxy that bind-fails picks a fallback port, and that search must not land
// on a port a *different* engine is configured to use. Nothing enforced this
// before: the fallback excluded the alias and its own backend, so a stopped
// sibling configured just above the search base was taken, and that engine then
// silently failed to start on the next desired-state restore.
//
// Both directions are checked because the two engines have separate fallback
// functions and fixing one is not evidence about the other.
func TestProxyFallbackSkipsASiblingEnginesConfiguredPort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		self     engineProxyProfile
		sibling  engineProxyProfile
		fallback func(*Broker, ...int) int
	}{
		{
			name:     "ollama fallback avoids LM Studio",
			self:     ollamaProxyProfile,
			sibling:  lmstudioProxyProfile,
			fallback: (*Broker).setOllamaProxyFallback,
		},
		{
			name:     "lmstudio fallback avoids Ollama",
			self:     lmstudioProxyProfile,
			sibling:  ollamaProxyProfile,
			fallback: (*Broker).setLMStudioProxyFallback,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateOllamaHostTestConfig(t)

			// Put the sibling's persisted port inside the search range so a
			// fallback that ignores siblings would hand it out.
			stored := tc.self.EnginePortBase + 1
			path, err := appdir.Path(tc.sibling.PortFile)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, `{"port":%d}`, stored), 0o600))

			b := &Broker{}
			got := tc.fallback(b, tc.self.EnginePortBase)
			require.NotEqual(t, stored, got, "fallback must skip %s's configured port", tc.sibling.DisplayName)
			require.NotEqual(t, tc.sibling.FacadePort, got, "fallback must skip %s's default ports", tc.sibling.DisplayName)
			require.NotEqual(t, tc.sibling.EnginePortBase, got, "fallback must skip %s's default ports", tc.sibling.DisplayName)
		})
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
			require.Equal(t, ollamaHostAlias{}, b.currentOllamaHostAlias(), "unsafe alias prepared")
		})
	}
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
		require.NotNil(t, msg, "replayed warning")
		require.Equal(t, methodErrorsReport, msg.Method, "replayed warning (%v)", msg)
		require.Contains(t, string(msg.Params), ollamaHostAliasBlockedID, "replayed warning (%v)", msg)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for recovered errors process warning replay")
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
			require.Equal(t, want, got.BackendPort, "the alias must be skipped")
		})
	}

	// Without an alias the wrapper must be transparent.
	plain := (&Broker{}).availableOffOllamaHostAlias(func(port int) bool { return port == managedOllamaBackendStart })
	require.True(t, plain(managedOllamaBackendStart), "no alias configured, but the port probe was still filtered")
}

func TestOllamaProxyFallbackSkipsTheOllamaHostAlias(t *testing.T) {
	b := &Broker{}
	b.setOllamaHostAlias(ollamaHostAlias{Port: managedOllamaBackendStart})
	require.NotEqual(t, managedOllamaBackendStart, b.setOllamaProxyFallback(), "proxy fallback must skip the alias")
	lm := &Broker{}
	lm.setOllamaHostAlias(ollamaHostAlias{Port: managedLMStudioBackendStart})
	require.NotEqual(t, managedLMStudioBackendStart, lm.setLMStudioProxyFallback(), "LM Studio proxy fallback must skip the alias")
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
	require.Equal(t, ollamaHostAlias{}, b.currentOllamaHostAlias(), "alias reservation not cleared")
	select {
	case port := <-reserved:
		require.Equal(t, 0, port, "engine-manager reservation")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for engine-manager reservation clear")
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
	require.NoError(t, err)
	b.setEngineMgr(replacement)
	require.NoError(t, settingsCodec.Respond(settingsRequest.ID, map[string]bool{"value": true}))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for managed alias preparation")
	}
	select {
	case port := <-replacementReservation:
		require.Equal(t, 15555, port, "replacement engine-manager reservation")
	default:
		require.FailNow(t, "replacement engine-manager did not receive the committed alias reservation")
	}
	select {
	case port := <-oldReservation:
		require.FailNow(t, fmt.Sprintf("stale engine-manager received final reservation %d", port))
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
	require.NoError(t, err)
	var firstRequest struct {
		Port int `json:"port"`
	}
	require.Equal(t, "internal:set-reserved-port", first.Method, "first reservation request")
	require.NoError(t, json.Unmarshal(first.Params, &firstRequest), "first reservation request")
	require.Equal(t, 15555, firstRequest.Port, "first reservation request")

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
		require.FailNow(t, fmt.Sprintf("new reservation overtook the in-flight update: msg=%+v err=%v", result.msg, result.err))
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, engineCodec.Respond(first.ID, map[string]int{"port": 15555}))

	var second *Message
	select {
	case result := <-secondRead:
		require.NoError(t, result.err)
		second = result.msg
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for current reservation update")
	}
	var secondRequest struct {
		Port int `json:"port"`
	}
	require.Equal(t, "internal:set-reserved-port", second.Method, "second reservation request")
	require.NoError(t, json.Unmarshal(second.Params, &secondRequest), "second reservation request")
	require.Equal(t, 0, secondRequest.Port, "second reservation request")
	require.NoError(t, engineCodec.Respond(second.ID, map[string]int{"port": 0}))
	for name, done := range map[string]<-chan struct{}{"first": firstDone, "second": secondDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			require.FailNow(t, fmt.Sprintf("%s reservation sync did not finish", name))
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
	require.NoError(t, err)
	b.forwardProxyNotification(methodErrorsReport, params)

	require.Equal(t, ollamaHostAlias{}, b.currentOllamaHostAlias(), "failed alias still reserved in broker")
	select {
	case port := <-reservation:
		require.Equal(t, 0, port, "engine-manager reservation")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for failed-alias reservation release")
	}
	b.ollamaHostAliasErrorMu.Lock()
	warning := b.ollamaHostAliasError
	b.ollamaHostAliasErrorMu.Unlock()
	require.NotNil(t, warning, "bind warning was cleared while releasing the reservation")
	require.Equal(t, ollamaHostAliasBlockedID, warning.ID, "bind warning was cleared while releasing the reservation")
	require.False(t, b.rejectOllamaHostAliasPort(&Message{}, 15555, "Ollama"), "released alias still rejects assignment to the existing owner's port")
	require.True(t, b.availableOffOllamaHostAlias(func(port int) bool { return port == 15555 })(15555), "released alias still excludes the existing owner's port from planning")
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
	require.NoError(t, err)
	b.forwardProxyNotificationForGeneration(staleGeneration, methodErrorsReport, params)

	require.Equal(t, currentGeneration, b.currentOllamaProxyGeneration(), "current proxy generation")
	alias := b.currentOllamaHostAlias()
	require.Equal(t, 16666, alias.Port, "stale failure changed replacement alias (%v)", alias)
	b.ollamaHostAliasErrorMu.Lock()
	warning := b.ollamaHostAliasError
	b.ollamaHostAliasErrorMu.Unlock()
	require.Nil(t, warning, "stale failure published a warning for the replacement generation")
}
