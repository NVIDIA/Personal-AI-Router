// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Coverage for automatic facade preparation, bind recovery, and ownership gates.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"nvpair-shared/engines"
)

// brokerWithRunningEngines answers one engine:get-installed with the given
// ports marked running, which is what runningEnginePorts treats as taken.
// brokerWithEngineInventory reports no running flag, so it cannot drive these
// paths.
func brokerWithRunningEngines(t *testing.T, ports ...int) *Broker {
	t.Helper()
	client, server := net.Pipe()
	worker := &rpcWorker{peer: NewPeer(NewCodec(client))}
	go worker.peer.Serve(nil, nil)
	b := &Broker{nodeID: "local-node"}
	b.setEngineMgr(worker)
	go func() {
		codec := NewCodec(server)
		request, err := codec.Read()
		if err != nil {
			return
		}
		engines := make([]map[string]any, 0, len(ports))
		for _, port := range ports {
			engines = append(engines, map[string]any{"running": true, "port": port})
		}
		_ = codec.Respond(request.ID, map[string]any{"engines": engines})
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return b
}

// observeErrors attaches an errors process and returns a channel of the
// notifications the broker pushes into it.
func observeErrors(t *testing.T, b *Broker) <-chan *Message {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	b.setErrors(&errorsProcess{peer: NewPeer(NewCodec(client))})

	seen := make(chan *Message, 4)
	go func() {
		codec := NewCodec(server)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			select {
			case seen <- msg:
			default:
			}
		}
	}()
	return seen
}

func awaitErrorsNotification(t *testing.T, seen <-chan *Message, method string) *Message {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-seen:
			if msg != nil && msg.Method == method {
				return msg
			}
		case <-deadline:
			t.Fatalf("no %s notification arrived", method)
			return nil
		}
	}
}

// The unmanaged reconcile branch: a proxy that came up on a port a running
// engine holds is corrected onto the next free port, and the correction is
// announced. Both existing reconcile tests set managedOllamaFacade first and
// return before reaching this code.
func TestReconcileUnmanagedProxyPortBumpsOffRunningEngine(t *testing.T) {
	b := brokerWithRunningEngines(t, 11435)
	seen := observeErrors(t, b)

	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	b.setProxy(proxy)

	setPort := make(chan int, 1)
	go func() {
		codec := NewCodec(proxyServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var params struct {
			Port int `json:"port"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		select {
		case setPort <- params.Port:
		default:
		}
		_ = codec.Respond(msg.ID, map[string]any{"port": params.Port})
	}()

	b.reconcileProxyPortOnReady(11435)

	select {
	case got := <-setPort:
		if got != 11436 {
			t.Fatalf("corrective set-port asked for %d, want 11436", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no corrective set-port reached the proxy")
	}

	msg := awaitErrorsNotification(t, seen, methodErrorsReport)
	if !strings.Contains(string(msg.Params), proxyPortBumpedID) {
		t.Fatalf("bump was not announced: %s", msg.Params)
	}
}

// A proxy on a port nothing holds is left alone — no corrective call, so the
// reconcile must not disturb a healthy unmanaged proxy.
func TestReconcileUnmanagedProxyPortLeavesFreePortAlone(t *testing.T) {
	b := brokerWithRunningEngines(t)

	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)
	b.setProxy(proxy)

	called := make(chan struct{}, 1)
	go func() {
		codec := NewCodec(proxyServer)
		if _, err := codec.Read(); err == nil {
			select {
			case called <- struct{}{}:
			default:
			}
		}
	}()

	b.reconcileProxyPortOnReady(11435)

	select {
	case <-called:
		t.Fatal("reconcile issued a corrective set-port for a port no engine holds")
	case <-time.After(250 * time.Millisecond):
	}
}

// The facade/enable request the broker hands an Ollama proxy. This is where the
// port decision and the inherited OLLAMA_HOST alias travel now; the child's
// argv carries only process-scoped flags, because a single-valued flag cannot
// express a different port plan per engine.
func TestOllamaFacadeSpec(t *testing.T) {
	for _, tc := range []struct {
		name        string
		startupPort int
		alias       ollamaHostAlias
		want        enableFacadeRequest
	}{
		{
			name:        "managed facade",
			startupPort: managedOllamaFacadePort,
			want: enableFacadeRequest{
				Engine:              "ollama",
				Port:                managedOllamaFacadePort,
				IgnorePersistedPort: true,
			},
		},
		{
			// No port named, so the child keeps its persisted one. Naming a port
			// here would override whatever the user last chose via set-port.
			name:        "no startup port leaves the proxy on its persisted or default port",
			startupPort: 0,
			want:        enableFacadeRequest{Engine: "ollama"},
		},
		{
			name:        "inherited OLLAMA_HOST alias is threaded through",
			startupPort: managedOllamaFacadePort,
			alias:       ollamaHostAlias{Address: "127.0.0.1:11433", AlternateAddress: "[::1]:11433"},
			want: enableFacadeRequest{
				Engine:              "ollama",
				Port:                managedOllamaFacadePort,
				IgnorePersistedPort: true,
				AliasAddresses:      []string{"127.0.0.1:11433", "[::1]:11433"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Broker{}
			b.ollamaState().startupPort.Store(int32(tc.startupPort))

			if got := b.ollamaFacadeSpec(tc.alias); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("facade spec = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// serveFacadeEnable answers facade/enable frames on a pipe, recording the port
// each attempt asked for and rejecting the ones in reject as bind races.
//
// It replies with codeFacadeBindFailed rather than a generic error because that
// code is the entire basis on which the broker decides a retry is worthwhile.
func serveFacadeEnable(t *testing.T, conn net.Conn, reject map[int]bool, attempts chan<- enableFacadeRequest) {
	t.Helper()
	go func() {
		codec := NewCodec(conn)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.Method != "facade/enable" {
				continue
			}
			var spec enableFacadeRequest
			if err := json.Unmarshal(msg.Params, &spec); err != nil {
				return
			}
			attempts <- spec
			if reject[spec.Port] {
				_ = codec.RespondError(msg.ID, codeFacadeBindFailed,
					fmt.Sprintf("facade bind failed: port %d: address already in use", spec.Port))
				continue
			}
			_ = codec.Respond(msg.ID, enableFacadeReply{Engine: spec.Engine, Port: spec.Port})
		}
	}()
}

// A facade that loses a bind race is retried on a fallback port inside the same
// process. It used to be a process exit: the child log.Fatalf'd and the
// supervisor respawned it on a corrected port. That is only acceptable while a
// process hosts one engine — once it hosts several, one engine losing a race
// would take working listeners down with it.
func TestFacadeBindRaceRetriesInProcess(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	attempts := make(chan enableFacadeRequest, 4)
	serveFacadeEnable(t, proxyServer, map[int]bool{managedOllamaFacadePort: true}, attempts)

	b := &Broker{}
	b.ollamaState().managedFacade.Store(true)
	b.ollamaState().backendPort.Store(managedOllamaBackendStart)

	const fallback = 12000
	err := b.enableProxyFacadeWithFallback(context.Background(), proxy,
		enableFacadeRequest{
			Engine:              ollamaProxyProfile.Name,
			Port:                managedOllamaFacadePort,
			IgnorePersistedPort: true,
		},
		func(failed int) int {
			if failed != managedOllamaFacadePort {
				t.Errorf("fallback asked for port %d, want the port that failed (%d)", failed, managedOllamaFacadePort)
			}
			return fallback
		})
	if err != nil {
		t.Fatalf("enable with fallback = %v, want success on the fallback port", err)
	}

	first := <-attempts
	if first.Port != managedOllamaFacadePort {
		t.Fatalf("first attempt port = %d, want %d", first.Port, managedOllamaFacadePort)
	}
	second := <-attempts
	if second.Port != fallback {
		t.Fatalf("retry port = %d, want the fallback %d", second.Port, fallback)
	}
	// The persisted port is the one that just failed to bind, so a retry that
	// let the child restore it would land straight back on the taken port.
	if !second.IgnorePersistedPort {
		t.Error("retry did not set ignorePersistedPort, so the child could restore the port that just failed")
	}
	if second.Engine != first.Engine {
		t.Errorf("retry engine = %q, want %q", second.Engine, first.Engine)
	}
}

// A rejection no other port would fix must not be retried. Retrying an
// unknown-engine or unsupported-alias enable would burn a port change and hide
// the real cause behind a second identical failure.
func TestFacadeEnableRejectionIsNotRetried(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	attempts := make(chan enableFacadeRequest, 4)
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			var spec enableFacadeRequest
			if json.Unmarshal(msg.Params, &spec) != nil {
				return
			}
			attempts <- spec
			// -32602 is what an unknown engine or an unsupported alias returns.
			_ = codec.RespondError(msg.ID, -32602, "unknown engine \"nope\"")
		}
	}()

	b := &Broker{}
	fallbackCalls := 0
	err := b.enableProxyFacadeWithFallback(context.Background(), proxy,
		enableFacadeRequest{Engine: "nope", Port: 11434},
		func(int) int {
			fallbackCalls++
			return 12000
		})
	if err == nil {
		t.Fatal("enable with fallback = nil, want the rejection surfaced")
	}
	if fallbackCalls != 0 {
		t.Errorf("fallback consulted %d times, want 0: only a bind race is retryable", fallbackCalls)
	}
	<-attempts
	select {
	case extra := <-attempts:
		t.Fatalf("a second enable was sent for %+v; a rejection must not be retried", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// The proxy's explicit fallback port must never be one the engine is going to
// want. When the configured backend is unknown, a free port is not evidence the
// engine will not claim it, and the stock backend port is exactly where a
// managed Ollama gets placed — so both it and the facade are excluded until an
// authoritative port is known.
func TestOllamaProxyFallbackAvoidsTheEnginesPorts(t *testing.T) {
	t.Run("backend unknown excludes both stock ports", func(t *testing.T) {
		b := &Broker{}
		fallback := b.setOllamaProxyFallback()

		if fallback == managedOllamaFacadePort || fallback == managedOllamaBackendStart {
			t.Fatalf("fallback = %d, want neither the facade (%d) nor the stock backend (%d) while the backend is unknown",
				fallback, managedOllamaFacadePort, managedOllamaBackendStart)
		}
		if got := int(b.ollamaState().startupPort.Load()); got != fallback {
			t.Fatalf("startup port = %d, want the chosen fallback %d", got, fallback)
		}
	})

	t.Run("known backend is excluded", func(t *testing.T) {
		b := &Broker{}
		b.ollamaState().backendPort.Store(managedOllamaBackendStart)

		if fallback := b.setOllamaProxyFallback(); fallback == managedOllamaBackendStart {
			t.Fatalf("fallback = %d, want the configured backend port excluded", fallback)
		}
	})

	t.Run("callers need not pass the backend", func(t *testing.T) {
		// blockManagedOllamaFacade used to append the backend itself. The
		// exclusion moved into setOllamaProxyFallback so no call site can omit
		// it; this pins that the blocked path still avoids the engine's port.
		b := &Broker{nodeID: "local-node", ollamaPortReady: make(chan struct{})}
		b.ollamaState().backendPort.Store(managedOllamaBackendStart)

		if fallback := b.blockManagedOllamaFacade("test"); fallback == managedOllamaBackendStart {
			t.Fatalf("blocked fallback = %d, want the configured backend port excluded", fallback)
		}
	})
}

// The two ways an engine ends up without a proxy need different treatment. An
// unresolved binary is a packaging or --proxy-path problem the user should see;
// an engine deliberately left out of --proxy-engines is not, and reporting it
// would be noise for something they asked for.
//
// Gating preparation on proxyEnabled made this worth pinning: the disabled
// branches used to reach the blocked-ownership report through managedFacade,
// which is now necessarily false there, so the report had to move.
func TestUnavailableProxyIsReportedButDeselectionIsNot(t *testing.T) {
	reason, report := proxyDisabledReason("")
	if !report {
		t.Error("an unresolved proxy binary is not reported; a packaging problem would be log-only")
	}
	if !strings.Contains(reason, "not resolved") {
		t.Errorf("reason for a missing binary = %q", reason)
	}

	reason, report = proxyDisabledReason("/path/to/nvpair-proxy")
	if report {
		t.Error("a deliberately deselected engine is reported as an error; that is noise for a requested state")
	}
	if !strings.Contains(reason, "--proxy-engines") {
		t.Errorf("reason for a deselected engine = %q", reason)
	}
}

// A bind failure has to move the next spawn somewhere else. The supervisor
// restarts the proxy with whatever startup port is currently stored, so leaving
// it unchanged means respawning onto the port that just rejected the bind, over
// and over until the restart budget runs out. LM Studio handled this; Ollama
// only handled the managed-facade case and ignored every other bind failure.
func TestBindFailureMovesTheNextSpawnOffTheContestedPort(t *testing.T) {
	t.Run("ollama", func(t *testing.T) {
		b := &Broker{nodeID: "local-node", ollamaPortReady: make(chan struct{})}
		// Unmanaged: no facade claim, so this is the path that used to be
		// ignored entirely.
		b.ollamaState().backendPort.Store(managedOllamaBackendStart)
		contested := managedOllamaBackendStart + 7
		b.ollamaState().startupPort.Store(int32(contested))

		b.forwardProxyNotification("error", json.RawMessage(
			`{"code":"bind-failed","port":`+strconv.Itoa(contested)+`}`))

		if got := int(b.ollamaState().startupPort.Load()); got == contested {
			t.Fatalf("startup port still %d; the next spawn would hit the same bind failure", got)
		}
	})

	t.Run("lmstudio", func(t *testing.T) {
		b := &Broker{nodeID: "local-node", lmstudioPortReady: make(chan struct{})}
		b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)
		contested := managedLMStudioBackendStart + 7
		b.lmstudioState().startupPort.Store(int32(contested))

		b.forwardLMStudioProxyNotification("error", json.RawMessage(
			`{"code":"bind-failed","port":`+strconv.Itoa(contested)+`}`))

		if got := int(b.lmstudioState().startupPort.Load()); got == contested {
			t.Fatalf("startup port still %d; the next spawn would hit the same bind failure", got)
		}
	})
}

// A "ready" from a proxy generation that has already been replaced must not
// drive the current generation's reconciliation. The dead process announced a
// port it no longer holds, and acting on it opens the ownership gate — and so
// releases every request waiting behind it — while the replacement is still
// deciding where to bind.
//
// LM Studio pinned this; Ollama's only stale-generation test covered the alias
// release, leaving the readiness path uncovered on the engine whose gate also
// gates engine:status and engine:get-installed.
func TestStaleProxyReadyDoesNotOpenTheOwnershipGate(t *testing.T) {
	newBroker := func(t *testing.T) *Broker {
		t.Helper()
		b := &Broker{nodeID: "local-node", ollamaPortReady: make(chan struct{})}
		observeErrors(t, b)
		// Managed with nothing pending is the state where a ready would
		// otherwise settle ownership immediately, so the guard is the only
		// thing keeping the gate shut.
		b.ollamaState().managedFacade.Store(true)
		// Reconciliation returns early without a live proxy, which would make
		// the stale case below pass for the wrong reason.
		client, server := net.Pipe()
		t.Cleanup(func() {
			_ = client.Close()
			_ = server.Close()
		})
		proxy := &proxyProcess{peer: NewPeer(NewCodec(client))}
		go proxy.peer.Serve(nil, nil)
		b.setProxy(proxy)
		return b
	}
	ready := json.RawMessage(fmt.Sprintf(`{"port":%d}`, managedOllamaFacadePort))

	t.Run("stale generation is ignored", func(t *testing.T) {
		b := newBroker(t)
		stale, _ := b.beginOllamaProxyGeneration()
		b.beginOllamaProxyGeneration() // the replacement

		b.forwardProxyNotificationForGeneration(stale, "ready", ready)

		requireGateStaysShut(t, b.ollamaPortReady, "a replaced proxy's ready")
	})

	// Without this the test above would pass even if the guard swallowed every
	// ready, stale or not.
	t.Run("current generation still settles ownership", func(t *testing.T) {
		b := newBroker(t)
		current, _ := b.beginOllamaProxyGeneration()

		b.forwardProxyNotificationForGeneration(current, "ready", ready)

		requireGateOpens(t, b.ollamaPortReady, "the current proxy's ready")
	})
}

// A proxy the supervisor has given up on must release its ownership gate.
// Otherwise every gated engine request waits out rpcWorkerCallTimeout and
// answers "retry" for the life of the broker, because nothing else ever closes
// the channel. LM Studio wired onExhausted to do this; Ollama did not.
func TestTerminalProxyReleasesOwnershipGate(t *testing.T) {
	t.Run("ollama", func(t *testing.T) {
		b := &Broker{
			nodeID:            "local-node",
			ollamaPortReady:   make(chan struct{}),
			lmstudioPortReady: make(chan struct{}),
		}
		// Mid-move state left behind by the proxy that died: the facade is
		// claimed and a backend move is pending.
		b.ollamaState().managedFacade.Store(true)
		b.managedOllamaBackend.Store(managedOllamaBackendStart)
		b.ollamaMoveInFlight.Store(true)

		sup := newSupervisor(engines.ProxyComponent, defaultRestartPolicy(), nil)
		b.configureProxySupervisorCallbacks(sup)
		if sup.onExhausted == nil {
			t.Fatal("proxy supervisor has no onExhausted; a terminal proxy would strand the gate")
		}
		sup.onExhausted(3)

		requireGateOpenNow(t, b.ollamaPortReady, "the proxy going terminal")
		// Closing the channel is not enough for Ollama: relayToEngine re-checks
		// this predicate after the wait and rejects while it holds.
		if b.ollamaFacadeIsPendingBackend() {
			t.Fatal("gate reopened but the pending-move re-check still rejects; requests would keep failing")
		}
	})

	t.Run("lmstudio", func(t *testing.T) {
		b := &Broker{
			nodeID:            "local-node",
			ollamaPortReady:   make(chan struct{}),
			lmstudioPortReady: make(chan struct{}),
		}
		b.lmstudioState().managedFacade.Store(true)

		sup := newSupervisor(engines.ProxyComponent, defaultRestartPolicy(), nil)
		b.configureProxySupervisorCallbacks(sup)
		if sup.onExhausted == nil {
			t.Fatal("proxy supervisor has no onExhausted")
		}
		sup.onExhausted(3)

		if b.lmstudioPortOwnershipPending() {
			t.Fatal("ownership gate still pending after the proxy went terminal")
		}
		if b.lmstudioState().managedFacade.Load() {
			t.Fatal("managed facade still claimed by a proxy that will not return")
		}
	})
}
