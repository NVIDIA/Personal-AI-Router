// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engines

import (
	"strings"
	"testing"

	"nvpair-shared/noderec"
)

// TestOllamaIsPreparedFirst pins the ordering the broker's managed-port
// preparation depends on: Ollama's preparation reserves any inherited
// OLLAMA_HOST alias, and every later engine's port planning must route around
// it. Reordering the table silently breaks that on roughly half of all runs.
func TestOllamaIsPreparedFirst(t *testing.T) {
	got := Names()
	if len(got) == 0 || got[0] != "ollama" {
		t.Fatalf("Names()[0] = %q, want \"ollama\" — the alias-owning engine must be prepared first", got)
	}
}

func TestNames(t *testing.T) {
	got := Names()
	want := []string{"ollama", "lmstudio"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

// There are two proxy identities: ComponentName per facade, ProxyComponent per
// process. Ollama's relay prefix was once the bare "proxy" while its error IDs
// were already "ollama-proxy" — two near-identical values that coincided for LM
// Studio, which made them easy to mistake for one field. This pins the shape of
// both so a new engine cannot reintroduce that ambiguity.
func TestProxyIdentities(t *testing.T) {
	for _, e := range All() {
		want := e.Name + "-proxy"
		if e.ComponentName() != want {
			t.Errorf("%s ComponentName = %q, want %q", e.Name, e.ComponentName(), want)
		}
		// The bare prefix Ollama used to relay under must not come back.
		if e.ComponentName() == "proxy" {
			t.Errorf("%s still uses the bare %q relay prefix", e.Name, "proxy")
		}
		// A facade identity that equals the process identity would make the
		// relay prefix and the process name indistinguishable.
		if e.ComponentName() == ProxyComponent {
			t.Errorf("%s facade identity collides with the process identity %q", e.Name, ProxyComponent)
		}
	}

	// The process identity must match the binary name, so a log prefix, a
	// support bundle, and a process listing agree. services/versions.json and
	// the desktop binary inventory both spell it this way.
	if ProxyComponent != "nvpair-proxy" {
		t.Errorf("ProxyComponent = %q, want the binary name %q", ProxyComponent, "nvpair-proxy")
	}
}

// TestFacadeAndEnginePortsDiffer guards the invariant that makes managed mode
// coherent: the proxy claims the engine's stock port, so the engine has to move
// somewhere else.
func TestFacadeAndEnginePortsDiffer(t *testing.T) {
	for _, e := range All() {
		if e.FacadePort == e.EnginePortBase {
			t.Errorf("%s: FacadePort and EnginePortBase are both %d; the engine has nowhere to move",
				e.Name, e.FacadePort)
		}
		if e.FacadePort <= 0 || e.EnginePortBase <= 0 {
			t.Errorf("%s: ports must be positive, got facade=%d base=%d",
				e.Name, e.FacadePort, e.EnginePortBase)
		}
	}
}

// PortFile is declared, not derived. Ollama's is the bare "proxy-port.json"
// from before there was more than one engine; deriving it from Name would
// rename the file under every existing install and orphan whatever port the
// user had chosen. This test exists so a future tidy-up cannot quietly make
// that trade.
func TestPortFilesAreDeclaredNotDerived(t *testing.T) {
	ollama, ok := ByName("ollama")
	if !ok {
		t.Fatal("no ollama engine")
	}
	if ollama.PortFile != "proxy-port.json" {
		t.Errorf("ollama PortFile = %q, want the pre-unification proxy-port.json", ollama.PortFile)
	}
	if derived := ollama.ComponentName() + "-port.json"; ollama.PortFile == derived {
		t.Errorf("ollama PortFile now matches the derived name %q; existing installs would be orphaned", derived)
	}
	for _, e := range All() {
		if e.PortFile == "" {
			t.Errorf("%s has no PortFile; its proxy would persist nothing", e.Name)
		}
	}
}

func TestIdentitiesAreUnique(t *testing.T) {
	names := map[string]bool{}
	components := map[string]bool{}
	services := map[noderec.ServiceKey]bool{}
	facades := map[int]bool{}

	for _, e := range All() {
		if e.Name == "" || e.DisplayName == "" || e.PortFile == "" || e.DiscoveryService == "" {
			t.Errorf("%+v: every identity field must be set", e)
		}
		if names[e.Name] {
			t.Errorf("duplicate Name %q", e.Name)
		}
		if components[e.ComponentName()] {
			t.Errorf("duplicate ComponentName %q", e.ComponentName())
		}
		if services[e.DiscoveryService] {
			t.Errorf("duplicate DiscoveryService %q", e.DiscoveryService)
		}
		if facades[e.FacadePort] {
			t.Errorf("duplicate FacadePort %d", e.FacadePort)
		}
		names[e.Name] = true
		components[e.ComponentName()] = true
		services[e.DiscoveryService] = true
		facades[e.FacadePort] = true
	}
}

// TestAllReturnsACopy keeps a caller from reordering the shared table, which
// would defeat TestOllamaIsPreparedFirst at a distance.
func TestAllReturnsACopy(t *testing.T) {
	first := All()
	if len(first) < 2 {
		t.Fatalf("expected at least two engines, got %d", len(first))
	}
	first[0], first[1] = first[1], first[0]

	if Names()[0] != "ollama" {
		t.Fatal("mutating the result of All() reordered the shared table")
	}
}

func TestLookups(t *testing.T) {
	for _, e := range All() {
		byName, ok := ByName(e.Name)
		if !ok || byName.ComponentName() != e.ComponentName() {
			t.Errorf("ByName(%q) = %+v, %v", e.Name, byName, ok)
		}
	}

	if _, ok := ByName("vllm"); ok {
		t.Error("ByName should report ok=false for an unknown engine")
	}
}

func TestAddressedMethodRoundTrip(t *testing.T) {
	for _, e := range All() {
		for _, method := range []string{"ready", "nodes/list", "errors:report"} {
			addressed := AddressMethod(e.Name, method)
			engine, bare := SplitAddressedMethod(addressed)
			if engine != e.Name || bare != method {
				t.Errorf("round trip of %q gave (%q, %q), want (%q, %q)",
					addressed, engine, bare, e.Name, method)
			}
		}
	}
}

// Plenty of unaddressed methods contain a colon, so only a known engine id may
// count as an address. Treating "errors:report" as engine "errors" would strip
// a real method down to "report" and route it nowhere.
func TestUnaddressedMethodsAreNotMistakenForAddressed(t *testing.T) {
	for _, method := range []string{
		"ready",
		"errors:report",
		"errors:clear",
		"discovery:subscribe",
		"discovery:nodes",
		"discovery:node-activity",
		"workload:started",
		"proxy/request",
		"node/selection-changed",
		"log/set-level",
	} {
		engine, bare := SplitAddressedMethod(method)
		if engine != "" {
			t.Errorf("SplitAddressedMethod(%q) found engine %q; nothing but an engine id is an address", method, engine)
		}
		if bare != method {
			t.Errorf("SplitAddressedMethod(%q) rewrote the method to %q", method, bare)
		}
	}
}

// An engine id that is a prefix of another would make addresses ambiguous:
// splitting on the first colon cannot tell "lm:x" from "lmstudio:x" if one id
// is a prefix of the other and the separator is ever omitted.
func TestNoEngineIDContainsTheAddressSeparator(t *testing.T) {
	for _, e := range All() {
		if strings.Contains(e.Name, ":") {
			t.Errorf("engine id %q contains the address separator", e.Name)
		}
	}
}
