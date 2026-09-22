// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// llama.cpp's managed-port behaviour: which port the facade claims, where the
// engine is moved to, and how a user's own configuration overrides both.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// The facade claims llama.cpp's own default and the engine is relocated
// directly above it, the same relationship Ollama and LM Studio have. 8080 is
// upstream's default (common/common.h), so a llama.cpp client that was already
// pointed at this machine keeps working through the proxy.
func TestLlamaCppClaimsItsStockPortAndMovesTheEngineAbove(t *testing.T) {
	profile := llamacppEffectiveProfile()
	if profile.FacadePort != 8080 {
		t.Errorf("facade port = %d, want llama.cpp's own default 8080", profile.FacadePort)
	}
	if profile.EnginePortBase != profile.FacadePort+1 {
		t.Errorf("engine port base = %d, want one above the facade (%d)",
			profile.EnginePortBase, profile.FacadePort+1)
	}
}

// 11434 and 1234 belong to Ollama and LM Studio. One process hosts all three
// facades, so a shared port would not be a config mistake to discover at
// runtime — it would be one facade losing the bind and landing somewhere
// arbitrary, with the OLLAMA_HOST reservation built around 11434 being
// Ollama's alone.
func TestLlamaCppPortsDoNotCollideWithSiblingEngines(t *testing.T) {
	llamacpp := llamacppEffectiveProfile()
	for _, sibling := range []engineProxyProfile{ollamaProxyProfile, lmstudioProxyProfile} {
		for _, taken := range []int{sibling.FacadePort, sibling.EnginePortBase} {
			if llamacpp.FacadePort == taken || llamacpp.EnginePortBase == taken {
				t.Errorf("llama.cpp (%d/%d) collides with %s port %d",
					llamacpp.FacadePort, llamacpp.EnginePortBase, sibling.Name, taken)
			}
		}
	}
}

// A user who set LLAMA_ARG_PORT has told us where their llama.cpp listens, so
// that is the port their clients use and the one the facade must claim. The
// backend follows it rather than staying on the table's default, which would
// otherwise strand the engine away from its own facade.
func TestLlamaArgPortOverridesTheStockPort(t *testing.T) {
	t.Setenv("LLAMA_ARG_PORT", "9000")
	profile := llamacppEffectiveProfile()
	if profile.FacadePort != 9000 {
		t.Errorf("facade port = %d, want the configured 9000", profile.FacadePort)
	}
	if profile.EnginePortBase != 9001 {
		t.Errorf("engine port base = %d, want 9001", profile.EnginePortBase)
	}
}

// Whatever the environment says, the pair it produces has to be two real
// ports. The backend sits one above the facade, so the facade can never be the
// last port.
func TestEffectiveLlamaCppPortsAreAlwaysValid(t *testing.T) {
	for _, value := range []string{"", "1", "8080", "65534", "65535", "99999"} {
		t.Run("LLAMA_ARG_PORT="+value, func(t *testing.T) {
			t.Setenv("LLAMA_ARG_PORT", value)
			profile := llamacppEffectiveProfile()
			for name, port := range map[string]int{
				"facade": profile.FacadePort,
				"engine": profile.EnginePortBase,
			} {
				if port < 1 || port > 65535 {
					t.Errorf("%s port = %d, which is not a usable TCP port", name, port)
				}
			}
		})
	}
}

// An unusable value is not a reason to plan against a nonsense port; the
// documented default stands.
//
// 65535 is unusable here even though it is a valid port: the backend goes one
// above the facade, so honouring it would plan a move onto 65536.
func TestUnusableLlamaArgPortFallsBackToTheDefault(t *testing.T) {
	for _, value := range []string{"", "not-a-port", "0", "-1", "70000", "65535"} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("LLAMA_ARG_PORT", value)
			if got := llamacppEffectiveProfile().FacadePort; got != 8080 {
				t.Errorf("facade port = %d with LLAMA_ARG_PORT=%q, want 8080", got, value)
			}
		})
	}
}

// The policy that decides whether to move the engine or leave it alone is the
// shared one; these pin that llama.cpp is routed through it as a managed engine
// rather than being special-cased.
func TestLlamaCppPortPlanMovesTheEngineOffTheFacade(t *testing.T) {
	free := func(int) bool { return true }
	profile := llamacppEffectiveProfile()

	// Engine sitting on the port the facade wants: it is managed, so it moves.
	plan := planManagedLlamaCppPorts(true, ollamaPortStatus{Running: true, Port: profile.FacadePort}, free)
	if plan.Blocked != "" {
		t.Fatalf("a managed engine on the facade port was blocked: %q", plan.Blocked)
	}
	if !plan.Enabled || plan.BackendPort != profile.EnginePortBase {
		t.Fatalf("plan = %+v, want the backend moved to %d", plan, profile.EnginePortBase)
	}

	// Already clear of the facade: nothing to move.
	plan = planManagedLlamaCppPorts(true, ollamaPortStatus{Running: true, Port: profile.EnginePortBase}, free)
	if !plan.Enabled || plan.BackendPort != 0 {
		t.Fatalf("plan = %+v, want the facade claimed with no backend move", plan)
	}

	// Policy off: nothing is claimed and nothing is moved.
	if plan = planManagedLlamaCppPorts(false, ollamaPortStatus{}, free); plan.Enabled || plan.BackendPort != 0 {
		t.Fatalf("plan = %+v, want an untouched engine when managed ports are off", plan)
	}
}

// A busy facade port is refused rather than displacing whatever holds it.
func TestLlamaCppPortPlanRefusesAnOccupiedFacade(t *testing.T) {
	profile := llamacppEffectiveProfile()
	occupied := func(port int) bool { return port != profile.FacadePort }
	plan := planManagedLlamaCppPorts(true, ollamaPortStatus{Running: true, Port: profile.EnginePortBase}, occupied)
	if plan.Blocked == "" {
		t.Fatalf("plan = %+v, want a block while the compatibility port is in use", plan)
	}
}

// The gate and the set-port guard are scoped to this engine: another engine's
// request must not be held behind llama.cpp's port transition, and must not be
// rejected by llama.cpp's reservation.
func TestLlamaCppPortGuardsAreScopedToThisEngine(t *testing.T) {
	mine, _ := json.Marshal(map[string]any{"engine": "llamacpp", "port": 8080})
	theirs, _ := json.Marshal(map[string]any{"engine": "lmstudio", "port": 1234})

	if !needsLlamaCppPortGate("engine:status", mine) {
		t.Error("llama.cpp engine:status is not gated")
	}
	if needsLlamaCppPortGate("engine:status", theirs) {
		t.Error("another engine's engine:status is gated behind llama.cpp")
	}
	if _, ok := llamacppSetPortRequest("engine:set-port", mine); !ok {
		t.Error("llama.cpp engine:set-port was not recognised")
	}
	if _, ok := llamacppSetPortRequest("engine:set-port", theirs); ok {
		t.Error("another engine's engine:set-port was claimed by llama.cpp")
	}
	if _, ok := llamacppSetPortRequest("engine:start", mine); ok {
		t.Error("a non set-port method was treated as one")
	}
}

// The ownership gate is what holds engine requests while the port changes
// hands, so a facade that never comes up has to release it or those requests
// wait out their call timeout for the life of the process.
func TestLlamaCppTerminalFacadeReleasesItsGate(t *testing.T) {
	b := &Broker{llamacppPortReady: make(chan struct{})}
	b.llamacppState().managedFacade.Store(true)

	if !b.llamacppPortOwnershipPending() {
		t.Fatal("a fresh gate is not pending")
	}
	b.finishLlamaCppProxyTerminal()
	if b.llamacppPortOwnershipPending() {
		t.Error("the gate is still pending after the facade went terminal")
	}
	if b.llamacppState().managedFacade.Load() {
		t.Error("the managed claim survived a terminal facade")
	}
	// Idempotent: the supervisor and the spawn path can both reach this.
	b.finishLlamaCppProxyTerminal()
}

// A llama.cpp facade can announce ready before engine-manager is up. Nothing
// can finish reconciling then — the configured backend port is unknown, so the
// gate deliberately stays closed — and it is engine-manager's own ready
// notification that has to replay the reconcile for the published generation,
// exactly as it does for LM Studio. Without that hook every gated llama.cpp
// request waits out its call timeout for the life of the process.
func TestLlamaCppGateOpensWhenEngineManagerReadiesAfterTheFacade(t *testing.T) {
	t.Setenv("LLAMA_ARG_PORT", "")
	proxy, localBackend := llamaCppProxyPipe(t, defaultLlamaCppProxyPort)
	b := &Broker{
		clusterDir:        filepath.Join(t.TempDir(), "cluster"),
		llamacppPortReady: make(chan struct{}),
	}
	b.setLlamaCppProxy(proxy)
	// spawnProxy publishes the generation it brought the facade up under; the
	// facade's own ready ran before any engine-manager existed.
	generation := b.llamaCppProxyGeneration.Add(1)
	b.llamaCppProxyPublishedGeneration.Store(generation)
	if !b.llamacppPortOwnershipPending() {
		t.Fatal("a fresh gate is not pending")
	}

	b.setEngineMgr(serveEngineStatus(t, defaultLlamaCppPort))
	b.forwardEngineNotification("engine:ready", nil)

	select {
	case <-b.llamacppPortReady:
	case <-time.After(5 * time.Second):
		t.Fatal("llama.cpp gate stayed closed after engine-manager readied")
	}
	select {
	case backend := <-localBackend:
		if backend.Engine != llamacppProxyProfile.Name || backend.Port != defaultLlamaCppPort || !backend.Healthy {
			t.Fatalf("local backend = %+v, want healthy %s:%d", backend, llamacppProxyProfile.Name, defaultLlamaCppPort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the replayed reconcile did not hand the facade its engine backend")
	}
	if got := int(b.llamacppState().backendPort.Load()); got != defaultLlamaCppPort {
		t.Fatalf("cached backend port = %d, want %d", got, defaultLlamaCppPort)
	}
	if b.llamacppPortOwnershipPending() {
		t.Fatal("the gate reports pending after it was released")
	}
}

// Every engine needs a startup-gate finisher, because the gate is what holds
// client requests while a port changes hands. An engine missing from
// finishEngineProxyStartup keeps its gate shut for the life of the process, so
// every gated request — engine:get-installed among them — waits out its call
// timeout and answers "retry" forever. That is what a broker running without a
// resolvable proxy binary does, and it is not a test-only condition.
func TestEveryEngineHasAStartupGateFinisher(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			b := &Broker{
				ollamaPortReady:   make(chan struct{}),
				lmstudioPortReady: make(chan struct{}),
				llamacppPortReady: make(chan struct{}),
			}
			// Only this engine's gate, not managedPortOwnershipReady: that
			// wants every gate open, and finishing one engine is not supposed
			// to release the others.
			gates := map[string]chan struct{}{
				ollamaProxyProfile.Name:   b.ollamaPortReady,
				lmstudioProxyProfile.Name: b.lmstudioPortReady,
				llamacppProxyProfile.Name: b.llamacppPortReady,
			}
			gate, ok := gates[profile.Name]
			if !ok {
				t.Fatalf("%s has no ownership gate in this test's table", profile.Name)
			}

			b.finishEngineProxyStartup(profile)

			select {
			case <-gate:
			default:
				t.Fatalf("%s has no startup-gate finisher, so its gated requests would time out",
					profile.Name)
			}
		})
	}
}

// A nil gate means "not configured", which is how a bare &Broker{} drives one
// behaviour in isolation. Waiting on it would block that caller forever.
func TestUnconfiguredPortGatesAreNotWaitedOn(t *testing.T) {
	if got := len((&Broker{}).managedPortGates()); got != 0 {
		t.Fatalf("managedPortGates on a bare broker = %d, want none", got)
	}
	b := &Broker{llamacppPortReady: make(chan struct{})}
	if got := len(b.managedPortGates()); got != 1 {
		t.Fatalf("managedPortGates = %d, want just the configured one", got)
	}
	if b.managedPortOwnershipReady() {
		t.Error("ownership reported ready while the one configured gate is shut")
	}
	close(b.llamacppPortReady)
	if !b.managedPortOwnershipReady() {
		t.Error("ownership not ready after the only configured gate opened")
	}
}
