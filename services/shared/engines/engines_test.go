// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engines

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// TestOllamaIsPreparedFirst pins the ordering the broker's managed-port
// preparation depends on: Ollama's preparation reserves any inherited
// OLLAMA_HOST alias, and every later engine's port planning must route around
// it. Reordering the table silently breaks that on roughly half of all runs.
func TestOllama_IsPreparedFirst(t *testing.T) {
	got := Names()
	require.NotEmpty(t, got)
	assert.Equal(t, "ollama", got[0])
}

func TestEngineNames_ReturnsCanonicalOrder(t *testing.T) {
	assert.Equal(t, []string{"ollama", "lmstudio", "llamacpp", "custom"}, Names())
}

// "custom" fronts user-added external endpoints: PAIR never installs,
// launches, or relocates anything behind it. Unmanaged is what tells every
// launcher-shaped consumer — broker port preparation, engine-manager
// manifests — to skip it, and EnginePortBase 0 encodes "there is no
// relocated engine process whose port must be freed". Custom joins the set
// last because everything written for the managed three predates it.
func TestCustom_IsAnUnmanagedEngineKind(t *testing.T) {
	custom, ok := ByName("custom")
	require.True(t, ok, "custom must be a declared engine")
	assert.True(t, custom.Unmanaged, "custom is routing-only: no manifest, no launch lifecycle")
	assert.Zero(t, custom.EnginePortBase, "no managed engine process sits behind the custom facade")
	assert.Equal(t, 8000, custom.FacadePort)
	assert.Equal(t, "custom-proxy-port.json", custom.PortFile)
	assert.Equal(t, noderec.ServiceCustom, custom.DiscoveryService)
}

// There are two proxy identities: ComponentName per facade, ProxyComponent per
// process. Ollama's relay prefix was once the bare "proxy" while its error IDs
// were already "ollama-proxy" — two near-identical values that coincided for LM
// Studio, which made them easy to mistake for one field. This pins the shape of
// both so a new engine cannot reintroduce that ambiguity.
func TestEngineProfiles_DeclareProxyIdentities(t *testing.T) {
	for _, e := range All() {
		assert.Equal(t, e.Name+"-proxy", e.ComponentName())
		// The bare prefix Ollama used to relay under must not come back.
		assert.NotEqual(t, "proxy", e.ComponentName())
		// A facade identity that equals the process identity would make the
		// relay prefix and the process name indistinguishable.
		assert.NotEqual(t, ProxyComponent, e.ComponentName())
	}

	// The process identity must match the binary name, so a log prefix, a
	// support bundle, and a process listing agree. services/versions.json and
	// the desktop binary inventory both spell it this way.
	assert.Equal(t, "nvpair-proxy", ProxyComponent)
}

// TestFacadeAndEnginePortsDiffer guards the invariant that makes managed mode
// coherent: the proxy claims the engine's stock port, so the engine has to move
// somewhere else. An unmanaged engine has no process of its own to relocate,
// so its EnginePortBase is 0 — the facade port is the only port in play, and
// the two still may not coincide.
func TestFacadeAndEnginePorts_Differ(t *testing.T) {
	for _, e := range All() {
		assert.NotEqual(t, e.EnginePortBase, e.FacadePort)
		assert.Positive(t, e.FacadePort)
		if e.Unmanaged {
			assert.Zero(t, e.EnginePortBase, "unmanaged %s declares no relocated engine port", e.Name)
		} else {
			assert.Positive(t, e.EnginePortBase, "managed %s must relocate its engine off the facade port", e.Name)
		}
	}
}

// PortFile is declared, not derived. Ollama's is the bare "proxy-port.json"
// from before there was more than one engine; deriving it from Name would
// rename the file under every existing install and orphan whatever port the
// user had chosen. This test exists so a future tidy-up cannot quietly make
// that trade.
func TestPortFiles_AreDeclaredNotDerived(t *testing.T) {
	ollama, ok := ByName("ollama")
	require.True(t, ok, "no ollama engine")
	assert.Equal(t, "proxy-port.json", ollama.PortFile, "ollama PortFile")
	assert.NotEqual(t, ollama.ComponentName()+"-port.json", ollama.PortFile, "ollama PortFile now matches the derived name")
	for _, e := range All() {
		assert.NotEqual(t, "", e.PortFile)
	}
}

func TestIdentities_AreUnique(t *testing.T) {
	names := map[string]bool{}
	components := map[string]bool{}
	services := map[noderec.ServiceKey]bool{}
	facades := map[int]bool{}

	for _, e := range All() {
		assert.NotEqual(t, "", e.Name)
		assert.NotEqual(t, "", e.DisplayName)
		assert.NotEqual(t, "", e.PortFile)
		assert.NotEmpty(t, e.DiscoveryService)
		assert.NotContains(t, names, e.Name, "duplicate Name")
		assert.NotContains(t, components, e.ComponentName(), "duplicate ComponentName")
		assert.NotContains(t, services, e.DiscoveryService, "duplicate DiscoveryService")
		assert.NotContains(t, facades, e.FacadePort, "duplicate FacadePort")
		names[e.Name] = true
		components[e.ComponentName()] = true
		services[e.DiscoveryService] = true
		facades[e.FacadePort] = true
	}
}

// TestAllReturnsACopy keeps a caller from reordering the shared table, which
// would defeat TestOllamaIsPreparedFirst at a distance.
func TestAll_ReturnsACopy(t *testing.T) {
	first := All()
	require.GreaterOrEqual(t, len(first), 2, "expected at least two engines,")
	first[0], first[1] = first[1], first[0]

	assert.Equal(t, "ollama", Names()[0], "mutating the result of All() reordered the shared table")
}

func TestEngineLookup_ResolvesDeclaredNamesAndKeys(t *testing.T) {
	for _, e := range All() {
		byName, ok := ByName(e.Name)
		assert.True(t, ok, "ByName")
		assert.Equal(t, e.ComponentName(), byName.ComponentName())
	}

	_, ok := ByName("vllm")
	assert.False(t, ok, "ByName should report ok=false for an unknown engine")
}

func TestAddressedMethod_RoundTripsEngineAndMethod(t *testing.T) {
	for _, e := range All() {
		for _, method := range []string{"ready", "nodes/list", "errors:report"} {
			addressed := AddressMethod(e.Name, method)
			engine, bare := SplitAddressedMethod(addressed)
			assert.Equal(t, e.Name, engine, "round trip of")
			assert.Equal(t, method, bare, "round trip of")
		}
	}
}

// The fourth engine joins facade addressing by being in the engine set, not by
// each relay learning a special case: "custom:node/add-manual" must split into
// the custom engine plus the bare method, so broker relay and proxy routing
// gain the fourth engine "for free".
func TestSplitAddressedMethod_SplitsCustomFacadeAddresses(t *testing.T) {
	engine, bare := SplitAddressedMethod("custom:node/add-manual")
	assert.Equal(t, "custom", engine)
	assert.Equal(t, "node/add-manual", bare)
}

// Plenty of unaddressed methods contain a colon, so only a known engine id may
// count as an address. Treating "errors:report" as engine "errors" would strip
// a real method down to "report" and route it nowhere.
func TestUnaddressedMethods_AreNotMistakenForAddressed(t *testing.T) {
	test := func(method string) {
		t.Run(method, func(t *testing.T) {
			engine, bare := SplitAddressedMethod(method)
			assert.Empty(t, engine)
			assert.Equal(t, method, bare)
		})
	}
	test("ready")
	test("errors:report")
	test("errors:clear")
	test("discovery:subscribe")
	test("discovery:nodes")
	test("discovery:node-activity")
	test("workload:started")
	test("proxy/request")
	test("node/selection-changed")
	test("log/set-level")
}

// An engine id that is a prefix of another would make addresses ambiguous:
// splitting on the first colon cannot tell "lm:x" from "lmstudio:x" if one id
// is a prefix of the other and the separator is ever omitted.
func TestNoEngineID_ContainsTheAddressSeparator(t *testing.T) {
	for _, e := range All() {
		assert.NotContains(t, e.Name, ":", "engine id")
	}
}
