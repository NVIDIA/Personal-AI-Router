// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"
)

// The broker's table must cover exactly the shared engine set, in the same
// order. Preparation order is load-bearing: Ollama goes first because its
// preparation reserves any inherited OLLAMA_HOST alias that later engines have
// to route around.
func TestEngineProxyTableMatchesSharedEngines(t *testing.T) {
	if len(engineProxyProfiles) == 0 {
		t.Fatal("no engine proxy profiles")
	}
	if got := engineProxyProfiles[0].Name; got != "ollama" {
		t.Fatalf("first profile = %q, want ollama prepared first", got)
	}
	for _, p := range engineProxyProfiles {
		if p.FacadePort == p.EnginePortBase {
			t.Errorf("%s: facade and backend base are both %d; the proxy and engine would collide",
				p.Name, p.FacadePort)
		}
		if p.ComponentName() == "" || p.DisplayName == "" {
			t.Errorf("%s: incomplete identity %+v", p.Name, p.Engine)
		}
		// An empty probe path would silently become a GET of the root, which
		// is Ollama's convention and wrong for anything OpenAI-compatible —
		// the engine would read as down whenever it is actually up.
		if p.HealthProbePath == "" {
			t.Errorf("%s: no health probe path", p.Name)
		}
	}
}

// The probe path is the one advertiser value that is per-engine, and getting it
// wrong makes a healthy engine look permanently down rather than failing loudly.
func TestEngineHealthProbePaths(t *testing.T) {
	for _, tc := range []struct {
		engine string
		want   string
	}{
		{"ollama", "/"},
		{"lmstudio", "/v1/models"},
	} {
		p, ok := engineProxyProfileFor(tc.engine)
		if !ok {
			t.Fatalf("no profile for %s", tc.engine)
		}
		if p.HealthProbePath != tc.want {
			t.Errorf("%s health probe = %q, want %q", tc.engine, p.HealthProbePath, tc.want)
		}
	}
}

// proxyport.go and lmstudioport.go restate ports and error-ID prefixes the
// engine table already carries, because they feed int32 atomics throughout the
// package. They are deleted when those files collapse; until then this is what
// stops them drifting from the table.
func TestBrokerConstantsMatchTheEngineTable(t *testing.T) {
	for _, tc := range []struct {
		engine       string
		facade       int
		backendStart int
		blockedID    string
	}{
		{"ollama", managedOllamaFacadePort, managedOllamaBackendStart, portOwnershipBlockedID},
		{"lmstudio", managedLMStudioFacadePort, managedLMStudioBackendStart, lmstudioPortOwnershipBlockedID},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			p, ok := engineProxyProfileFor(tc.engine)
			if !ok {
				t.Fatalf("no profile for %s", tc.engine)
			}
			if tc.facade != p.FacadePort {
				t.Errorf("facade constant = %d, table says %d", tc.facade, p.FacadePort)
			}
			if tc.backendStart != p.EnginePortBase {
				t.Errorf("backend-start constant = %d, table says %d", tc.backendStart, p.EnginePortBase)
			}
			if want := p.ComponentName() + ":port-ownership-blocked"; tc.blockedID != want {
				t.Errorf("blocked error id = %q, want %q", tc.blockedID, want)
			}
		})
	}
	if want := ollamaProxyProfile.ComponentName() + ":port-bumped"; proxyPortBumpedID != want {
		t.Errorf("bumped error id = %q, want %q", proxyPortBumpedID, want)
	}
}

// Ownership is the one judgment call in adding an engine, so the two values in
// the table today are pinned explicitly. Getting these backwards does not fail
// to compile — it silently changes which engine the broker believes it may stop.
func TestEngineOwnershipAssignments(t *testing.T) {
	for _, tc := range []struct {
		engine string
		want   engineOwnership
	}{
		{"ollama", adoptedEngine},
		{"lmstudio", managedEngine},
	} {
		p, ok := engineProxyProfileFor(tc.engine)
		if !ok {
			t.Fatalf("no profile for %s", tc.engine)
		}
		if p.Ownership != tc.want {
			t.Errorf("%s ownership = %v, want %v", tc.engine, p.Ownership, tc.want)
		}
	}
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
	for _, tc := range []struct {
		engine    string
		wantMove  bool
		wantBlock string
	}{
		{engine: "ollama", wantBlock: "Ollama is already running on the compatibility port"},
		{engine: "lmstudio", wantMove: true},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			p, ok := engineProxyProfileFor(tc.engine)
			if !ok {
				t.Fatalf("no profile for %s", tc.engine)
			}
			// Running on the facade, which is therefore taken; the backend base
			// is free.
			status := ollamaPortStatus{Running: true, Port: p.FacadePort}
			available := func(port int) bool { return port == p.EnginePortBase }

			got := planManagedEnginePorts(p, true, status, available)

			if tc.wantMove {
				want := managedPortPlan{Enabled: true, BackendPort: p.EnginePortBase}
				if got != want {
					t.Fatalf("plan = %+v, want %+v (a managed engine must be moved, not refused)", got, want)
				}
				return
			}
			if got.Blocked != tc.wantBlock {
				t.Fatalf("plan = %+v, want blocked with %q (an adopted engine must not be displaced)", got, tc.wantBlock)
			}
		})
	}
}

// --proxy-engines selects which engines the one binary is started for.
func TestParseProxyEngines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		csv     string
		want    []string
		wantErr bool
	}{
		{name: "default is every engine", csv: "ollama,lmstudio", want: []string{"ollama", "lmstudio"}},
		{name: "single engine", csv: "lmstudio", want: []string{"lmstudio"}},
		{name: "whitespace and blanks are tolerated", csv: " ollama , , lmstudio ", want: []string{"ollama", "lmstudio"}},
		{name: "duplicates collapse", csv: "ollama,ollama", want: []string{"ollama"}},
		{name: "empty selects nothing", csv: "", want: nil},
		// Silently fronting the engines it did recognize would look like the
		// flag worked.
		{name: "unknown engine fails", csv: "ollama,vllm", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProxyEngines(tc.csv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseProxyEngines(%q) = %v, want an error", tc.csv, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProxyEngines(%q): %v", tc.csv, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseProxyEngines(%q) = %v, want %v", tc.csv, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("parseProxyEngines(%q) = %v, want %v", tc.csv, got, tc.want)
				}
			}
		})
	}
}

// An engine left out of --proxy-engines is not started, and neither is any
// engine when the binary could not be resolved.
func TestProxyEnabledHonorsSelectionAndBinary(t *testing.T) {
	b := &Broker{proxyPath: "/path/to/nvpair-proxy", proxyEngines: []string{"ollama"}}
	if !b.proxyEnabled(ollamaProxyProfile) {
		t.Error("ollama was selected but is not enabled")
	}
	if b.proxyEnabled(lmstudioProxyProfile) {
		t.Error("lmstudio was not selected but is enabled")
	}

	b = &Broker{proxyEngines: []string{"ollama", "lmstudio"}}
	if b.proxyEnabled(ollamaProxyProfile) {
		t.Error("no proxy binary resolved, but the engine is enabled")
	}
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
			if err != nil {
				return
			}
			if msg.Method == "settings/get-force-ports" {
				policyReads <- struct{}{}
			}
			_ = settingsCodec.Respond(msg.ID, map[string]bool{"value": true})
		}
	}()
	go func() {
		for {
			msg, err := engineCodec.Read()
			if err != nil {
				return
			}
			_ = engineCodec.Respond(msg.ID, json.RawMessage(`{}`))
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
		t.Fatal("the selected engine was never prepared; the fixture is not exercising preparation")
	}
	select {
	case <-policyReads:
		t.Fatal("a deselected engine was prepared; its backend would be relocated with no proxy to claim the port")
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

			if got.Blocked != "the compatibility port is already in use" {
				t.Fatalf("plan = %+v, want blocked on the occupied facade", got)
			}
		})
	}
}
