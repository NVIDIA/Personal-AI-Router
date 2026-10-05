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
func TestOllamaIsPreparedFirst(t *testing.T) {
	got := Names()
	require.NotEmpty(t, got)
	assert.Equal(t, "ollama", got[0])
}

func TestNames(t *testing.T) {
	got := Names()
	want := []string{"ollama", "lmstudio", "llamacpp"}
	require.Len(t, got, len(want))
	for i := range want {
		assert.Equal(t, want[i], got[i])
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
		assert.Equal(t, want, e.ComponentName())
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
// somewhere else.
func TestFacadeAndEnginePortsDiffer(t *testing.T) {
	for _, e := range All() {
		assert.NotEqual(t, e.EnginePortBase, e.FacadePort)
		assert.Greater(t, e.FacadePort, 0)
		assert.Greater(t, e.EnginePortBase, 0)
	}
}

// PortFile is declared, not derived. Ollama's is the bare "proxy-port.json"
// from before there was more than one engine; deriving it from Name would
// rename the file under every existing install and orphan whatever port the
// user had chosen. This test exists so a future tidy-up cannot quietly make
// that trade.
func TestPortFilesAreDeclaredNotDerived(t *testing.T) {
	ollama, ok := ByName("ollama")
	require.True(t, ok, "no ollama engine")
	assert.Equal(t, "proxy-port.json", ollama.PortFile, "ollama PortFile")
	assert.NotEqual(t, ollama.ComponentName()+"-port.json", ollama.PortFile, "ollama PortFile now matches the derived name")
	for _, e := range All() {
		assert.NotEqual(t, "", e.PortFile)
	}
}

func TestIdentitiesAreUnique(t *testing.T) {
	names := map[string]bool{}
	components := map[string]bool{}
	services := map[noderec.ServiceKey]bool{}
	facades := map[int]bool{}

	for _, e := range All() {
		assert.NotEqual(t, "", e.Name)
		assert.NotEqual(t, "", e.DisplayName)
		assert.NotEqual(t, "", e.PortFile)
		assert.NotEqual(t, "", e.DiscoveryService)
		assert.False(t, names[e.Name], "duplicate Name")
		assert.False(t, components[e.ComponentName()], "duplicate ComponentName")
		assert.False(t, services[e.DiscoveryService], "duplicate DiscoveryService")
		assert.False(t, facades[e.FacadePort], "duplicate FacadePort")
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
	require.GreaterOrEqual(t, len(first), 2, "expected at least two engines,")
	first[0], first[1] = first[1], first[0]

	assert.Equal(t, "ollama", Names()[0], "mutating the result of All() reordered the shared table")
}

func TestLookups(t *testing.T) {
	for _, e := range All() {
		byName, ok := ByName(e.Name)
		assert.True(t, ok, "ByName")
		assert.Equal(t, e.ComponentName(), byName.ComponentName())
	}

	{
		_, ok := ByName("vllm")
		assert.False(t, ok, "ByName should report ok=false for an unknown engine")
	}
}

func TestAddressedMethodRoundTrip(t *testing.T) {
	for _, e := range All() {
		for _, method := range []string{"ready", "nodes/list", "errors:report"} {
			addressed := AddressMethod(e.Name, method)
			engine, bare := SplitAddressedMethod(addressed)
			assert.Equal(t, e.Name, engine, "round trip of")
			assert.Equal(t, method, bare, "round trip of")
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
		assert.Equal(t, "", engine)
		assert.Equal(t, method, bare)
	}
}

// An engine id that is a prefix of another would make addresses ambiguous:
// splitting on the first colon cannot tell "lm:x" from "lmstudio:x" if one id
// is a prefix of the other and the separator is ever omitted.
func TestNoEngineIDContainsTheAddressSeparator(t *testing.T) {
	for _, e := range All() {
		assert.NotContains(t, e.Name, ":", "engine id")
	}
}
