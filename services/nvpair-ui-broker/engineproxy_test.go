// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The broker's table must cover exactly the shared engine set, in the same
// order. Preparation order is load-bearing: Ollama goes first because its
// preparation reserves any inherited OLLAMA_HOST alias that later engines have
// to route around.
func TestEngineProxyTableMatchesSharedEngines(t *testing.T) {
	require.NotEmpty(t, engineProxyProfiles, "no engine proxy profiles")
	require.Equal(t, "ollama", engineProxyProfiles[0].Name, "first profile")
	for _, p := range engineProxyProfiles {
		assert.NotEqual(t, p.EnginePortBase, p.FacadePort)
		assert.NotEmpty(t, p.ComponentName())
		assert.NotEmpty(t, p.DisplayName)
		// An empty probe path would silently become a GET of the root, which
		// is Ollama's convention and wrong for anything OpenAI-compatible —
		// the engine would read as down whenever it is actually up.
		assert.NotEmpty(t, p.HealthProbePath)
	}
}

// The probe path is the one advertiser value that is per-engine, and getting it
// wrong makes a healthy engine look permanently down rather than failing loudly.
func TestEngineHealthProbePaths(t *testing.T) {
	test := func(engine, want string) {
		t.Run(engine, func(t *testing.T) {
			p, ok := engineProxyProfileFor(engine)
			require.True(t, ok, "no profile for engine")
			assert.Equal(t, want, p.HealthProbePath)
		})
	}
	test("ollama", "/")
	test("lmstudio", "/v1/models")
	test("llamacpp", "/health")
}

// proxyport.go and lmstudioport.go restate ports and error-ID prefixes the
// engine table already carries, because they feed int32 atomics throughout the
// package. They are deleted when those files collapse; until then this is what
// stops them drifting from the table.
func TestBrokerConstantsMatchTheEngineTable(t *testing.T) {
	test := func(engine string, facade, backendStart int, blockedID string) {
		t.Run(engine, func(t *testing.T) {
			p, ok := engineProxyProfileFor(engine)
			require.True(t, ok, "no profile for engine")
			assert.Equal(t, facade, p.FacadePort, "facade constant")
			assert.Equal(t, backendStart, p.EnginePortBase, "backend-start constant")
			assert.Equal(t, p.ComponentName()+":port-ownership-blocked", blockedID, "blocked error id")
		})
	}
	test("ollama", managedOllamaFacadePort, managedOllamaBackendStart, portOwnershipBlockedID)
	test("lmstudio", managedLMStudioFacadePort, managedLMStudioBackendStart, lmstudioPortOwnershipBlockedID)
	assert.Equal(t, ollamaProxyProfile.ComponentName()+":port-bumped", proxyPortBumpedID, "bumped error id")
}

// Every engine's relocation authority is pinned explicitly. Getting these
// backwards does not fail
// to compile — it silently changes which engine the broker believes it may stop.
func TestEngineOwnershipAssignments(t *testing.T) {
	test := func(engine string, want engineOwnership) {
		t.Run(engine, func(t *testing.T) {
			p, ok := engineProxyProfileFor(engine)
			require.True(t, ok, "no profile for engine")
			assert.Equal(t, want, p.Ownership)
		})
	}
	test("ollama", adoptedEngine)
	test("lmstudio", managedEngine)
	test("llamacpp", managedEngine)
}

// The two engines' port choreography diverges on exactly one input, and this is
// it: the engine is running on its own facade port, so the port is unavailable.
//
// An adopted engine must block — the broker cannot tell its own engine from a
// stranger there, and has authority over neither. A managed engine must plan
// the move, because engine-manager can stop and reposition it; blocking instead
// would break the most common LM Studio install, where it is already running on
// 1234.
//
// This is the regression guard for collapsing the two planners into one. A
// change that makes both engines agree here has broken one of them.
func TestOwnershipDecidesTheOccupiedFacadeOutcome(t *testing.T) {
	test := func(engine string, wantMove bool, wantBlock string) {
		t.Run(engine, func(t *testing.T) {
			p, ok := engineProxyProfileFor(engine)
			require.True(t, ok, "no profile for engine")
			// Running on the facade, which is therefore taken; the backend base
			// is free.
			status := ollamaPortStatus{Running: true, Port: p.FacadePort}
			available := func(port int) bool { return port == p.EnginePortBase }

			got := planManagedEnginePorts(p, true, status, available)

			if wantMove {
				require.Equal(t, managedPortPlan{Enabled: true, BackendPort: p.EnginePortBase}, got, "a managed engine must be moved, not refused")
				return
			}
			require.Equal(t, wantBlock, got.Blocked, "plan (%v)", got)
		})
	}
	test("ollama", false, "Ollama is already running on the compatibility port")
	test("lmstudio", true, "")
}

// --proxy-engines selects which engines the one binary is started for.
func TestParseProxyEngines(t *testing.T) {
	test := func(name, csv string, want []string, wantErr bool) {
		t.Run(name, func(t *testing.T) {
			got, err := parseProxyEngines(csv)
			if wantErr {
				require.Error(t, err, "parseProxyEngines (%v)", got)
				return
			}
			require.NoError(t, err, "parseProxyEngines")
			require.Equal(t, append([]string{}, want...), append([]string{}, got...))
		})
	}
	test("default set", "ollama,lmstudio,llamacpp", []string{"ollama", "lmstudio", "llamacpp"}, false)
	test("single engine", "lmstudio", []string{"lmstudio"}, false)
	test("whitespace and blanks are tolerated", " ollama , , lmstudio ", []string{"ollama", "lmstudio"}, false)
	test("duplicates collapse", "ollama,ollama", []string{"ollama"}, false)
	test("empty selects nothing", "", nil, false)
	// Silently fronting the engines it did recognize would look like the flag worked.
	test("unknown engine fails", "ollama,vllm", nil, true)
}

// An engine left out of --proxy-engines is not started, and neither is any
// engine when the binary could not be resolved.
func TestProxyEnabledHonorsSelectionAndBinary(t *testing.T) {
	b := &Broker{proxyPath: "/path/to/nvpair-proxy", proxyEngines: []string{"ollama"}}
	assert.True(t, b.proxyEnabled(ollamaProxyProfile), "ollama was selected but is not enabled")
	assert.False(t, b.proxyEnabled(lmstudioProxyProfile), "lmstudio was not selected but is enabled")

	b = &Broker{proxyEngines: []string{"ollama", "lmstudio"}}
	assert.False(t, b.proxyEnabled(ollamaProxyProfile), "no proxy binary resolved, but the engine is enabled")
}

// Preparing a facade is not read-only. For a managed engine the backend move
// runs inside preparation, so preparing an engine whose proxy is then never
// started relocates the engine off its own stock port and leaves nothing
// serving it — every existing client on that port breaks.
//
// Ollama cannot show the symptom, because its move is deferred until its proxy
// proves it holds the facade. That asymmetry is exactly why the guard has to be
// at the preparation call rather than relied on further down.
func TestDeselectedEngineIsNotPrepared(t *testing.T) {
	settings, settingsCodec := newTestRPCWorkerPipe(t)
	engine, engineCodec := newTestRPCWorkerPipe(t)

	// Every preparation begins by reading the managed-port policy, so counting
	// those reads is what proves how many engines were prepared — one for
	// Ollama and no more. Asserting on engine:set-port instead would depend on
	// whether 1234/1235 happen to be free on this machine, and would pass for
	// the wrong reason whenever they are not.
	policyReads := make(chan struct{}, 4)
	go func() {
		for {
			msg, err := settingsCodec.Read()
			if !assertRPCRead(t, err) {
				return
			}
			if msg.Method == "settings/get-force-ports" {
				policyReads <- struct{}{}
			}
			assert.NoError(t, settingsCodec.Respond(msg.ID, map[string]bool{"value": true}))
		}
	}()
	go func() {
		for {
			msg, err := engineCodec.Read()
			if !assertRPCRead(t, err) {
				return
			}
			assert.NoError(t, engineCodec.Respond(msg.ID, json.RawMessage(`{}`)))
		}
	}()

	b := &Broker{
		nodeID:            "local-node",
		proxyPath:         "/path/to/nvpair-proxy",
		proxyEngines:      []string{"ollama"},
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	b.setSettings(settings)
	b.setEngineMgr(engine)

	b.prepareEnabledFacades()

	select {
	case <-policyReads:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the selected engine was never prepared; the fixture is not exercising preparation")
	}
	select {
	case <-policyReads:
		require.FailNow(t, "a deselected engine was prepared; its backend would be relocated with no proxy to claim the port")
	case <-time.After(300 * time.Millisecond):
	}
}

// A stranger on the facade blocks both engines. This is the companion to the
// test above: the divergence is about who the occupant is, not about whether an
// occupied facade matters.
func TestAStrangerOnTheFacadeBlocksEveryEngine(t *testing.T) {
	for _, p := range engineProxyProfiles {
		t.Run(p.Name, func(t *testing.T) {
			// The engine is stopped somewhere else entirely, so whoever holds
			// the facade is not it.
			status := ollamaPortStatus{Port: p.EnginePortBase + 100}
			available := func(port int) bool { return port != p.FacadePort }

			got := planManagedEnginePorts(p, true, status, available)

			require.Equal(t, "the compatibility port is already in use", got.Blocked, "plan (%v)", got)
		})
	}
}
