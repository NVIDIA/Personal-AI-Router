// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
)

// TestManualToEnrichedIncludesOpenAI: an endpoint's models merge into the
// node's model list and attribute under the "openai" engine key (the node is
// not LM Studio), alongside the historical ollama/lmstudio attribution.
func TestManualToEnrichedIncludesOpenAI(t *testing.T) {
	s := manualNodeStatus{
		ID:             "stub",
		Address:        "10.0.1.9",
		OpenAIUp:       true,
		OpenAIBaseURL:  "http://10.0.1.9:8888/v1",
		OpenAIHost:     "10.0.1.9",
		OpenAIPort:     8888,
		OpenAIBasePath: "/v1",
		OpenAIModels:   []string{"m1", "m2"},
		NodeInfoPort:   14318,
	}
	en := manualToEnriched(s)
	if len(en.Models) != 2 || en.Models[0] != "m1" || en.Models[1] != "m2" {
		t.Fatalf("Models = %#v, want [m1 m2]", en.Models)
	}
	if got := en.ModelsByEngine["openai"]; len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("ModelsByEngine[openai] = %#v, want [m1 m2]", got)
	}
	if _, hasOllama := en.ModelsByEngine["ollama"]; hasOllama {
		t.Fatalf("no ollama key expected: %#v", en.ModelsByEngine)
	}

	// An address-based node with only ollama models is unchanged: no "openai" key.
	plain := manualNodeStatus{ID: "p", Address: "10.0.1.10", OllamaModels: []string{"o1"}, NodeInfoPort: 14318}
	en = manualToEnriched(plain)
	if _, has := en.ModelsByEngine["openai"]; has {
		t.Fatalf("unexpected openai key for address entry: %#v", en.ModelsByEngine)
	}
	if got := en.ModelsByEngine["ollama"]; len(got) != 1 || got[0] != "o1" {
		t.Fatalf("ModelsByEngine[ollama] = %#v, want [o1]", got)
	}
}

// TestManualModelsByEngineOpenAIOnly: an endpoint with no other engines
// reports exactly the "openai" attribution.
func TestManualModelsByEngineOpenAIOnly(t *testing.T) {
	byEngine := manualModelsByEngine(manualNodeStatus{OpenAIModels: []string{"m1"}})
	if len(byEngine) != 1 {
		t.Fatalf("byEngine = %#v, want exactly one key", byEngine)
	}
	if got := byEngine["openai"]; len(got) != 1 || got[0] != "m1" {
		t.Fatalf("openai attribution = %#v, want [m1]", got)
	}
}

// TestManualAliasForStoreKey: the node/remove translation maps a store key
// (hostUuid) back to the alias id the manual-nodes service tracks, choosing
// the lexicographically smallest alias when several share a key.
func TestManualAliasForStoreKey(t *testing.T) {
	b := newManualTestBroker()
	b.upsertManualNode(manualStatus("alias-b", "10.0.0.2", "uuid-1"))
	b.upsertManualNode(manualStatus("alias-a", "10.0.0.1", "uuid-1"))
	b.upsertManualNode(manualStatus("alias-c", "10.0.0.3", "uuid-2"))

	if got, ok := b.manualAliasForStoreKey("uuid-1"); !ok || got != "alias-a" {
		t.Fatalf("manualAliasForStoreKey(uuid-1) = %q, %v; want alias-a true", got, ok)
	}
	if got, ok := b.manualAliasForStoreKey("uuid-2"); !ok || got != "alias-c" {
		t.Fatalf("manualAliasForStoreKey(uuid-2) = %q, %v; want alias-c true", got, ok)
	}
	if got, ok := b.manualAliasForStoreKey("unknown"); ok || got != "" {
		t.Fatalf("manualAliasForStoreKey(unknown) = %q, %v; want empty false", got, ok)
	}
}

// TestRelayRemovesTranslateStoreKey: a node/remove arriving keyed by a store
// key (the desktop removes by UUID) is rewritten to the alias id before
// relaying; ids the broker doesn't recognize pass through untouched.
func TestRelayRemovesTranslateStoreKey(t *testing.T) {
	cases := []struct {
		name          string
		existing      string // alias known to own store key "uuid-1" ("" = none)
		in            string
		wantRewrite   bool
		want          string
	}{
		{name: "store-key-translated", existing: "alias-a", in: "uuid-1", wantRewrite: true, want: "alias-a"},
		{name: "alias-id-passthrough", existing: "alias-a", in: "alias-a"},
		{name: "unknown-id-passthrough", existing: "alias-a", in: "someone-else"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := translateManualRemoveID(tc.in, func(storeKey string) (string, bool) {
				if tc.existing != "" && storeKey == "uuid-1" {
					return tc.existing, true
				}
				return "", false
			})
			if tc.wantRewrite {
				if !ok {
					t.Fatalf("translateManualRemoveID(%q): expected rewrite", tc.in)
				}
				var out struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(got, &out); err != nil {
					t.Fatalf("unmarshal rewritten params %q: %v", got, err)
				}
				if out.ID != tc.want {
					t.Fatalf("rewritten id = %q, want %q", out.ID, tc.want)
				}
				return
			}
			// Passthrough: no rewrite signal, so the relay keeps the original
			// params (id unchanged).
			if ok {
				t.Fatalf("translateManualRemoveID(%q): unexpected rewrite to %s", tc.in, got)
			}
		})
	}
}

// TestManualToEnrichedEndpointKeepsManualKey pins the endpoint identity rule:
// a declared endpoint's operational key is the manual id (the declared URL's
// identity), never the host's learned node-info UUID. Endpoints commonly sit on
// the same box as the PAIR installation (or on a peer that also runs
// node-info); adopting the host's UUID would fold the endpoint into that
// machine's own node, and two endpoints on one host would collapse into a
// single slot that clobbers itself on every probe cycle.
func TestManualToEnrichedEndpointKeepsManualKey(t *testing.T) {
	s := manualNodeStatus{
		ID:             "manual:127.0.0.1:8888",
		Address:        "127.0.0.1",
		OpenAIUp:       true,
		OpenAIBaseURL:  "http://127.0.0.1:8888/v1",
		OpenAIHost:     "127.0.0.1",
		OpenAIPort:     8888,
		OpenAIBasePath: "/v1",
		OpenAIModels:   []string{"m1"},
		NodeInfoPort:   14318,
		HostUUID:       "learned-host-uuid",
	}
	if got := manualToEnriched(s).storeKey(); got != s.ID {
		t.Fatalf("endpoint storeKey = %q, want the manual id %q (an endpoint never adopts the host's learned UUID)", got, s.ID)
	}

	// Regression: a classic address entry still re-keys to the learned UUID so
	// a manually-added PAIR peer collapses onto its mDNS entry.
	plain := manualNodeStatus{
		ID:           "manual:10.0.1.10",
		Address:      "10.0.1.10",
		OllamaUp:     true,
		OllamaPort:   11434,
		NodeInfoPort: 14318,
		HostUUID:     "learned-host-uuid",
	}
	if got := manualToEnriched(plain).storeKey(); got != "learned-host-uuid" {
		t.Fatalf("address-entry storeKey = %q, want the learned uuid", got)
	}
}

// TestBridgeIntentEquality pins the comparison that keeps the manual→proxy
// bridge idempotent: an identical intent (same add payload, or a remove) is
// equal, while any routing-relevant difference (model list, base path,
// add↔remove) is not. The bridge skips re-issuing the RPC for an equal
// intent, so a repeated telemetry-only probe can't churn the proxy.
func TestBridgeIntentEquality(t *testing.T) {
	add := bridgeIntent{node: proxyManualNode{
		ID: "k", Host: "h", Port: 8888, Addresses: []string{"h"},
		Models: []string{"m1"}, BasePath: "/v1",
	}}
	rem := bridgeIntent{removed: true}

	if !bridgeIntentsEqual(add, bridgeIntent{node: proxyManualNode{
		ID: "k", Host: "h", Port: 8888, Addresses: []string{"h"},
		Models: []string{"m1"}, BasePath: "/v1",
	}}) {
		t.Fatal("identical add intents must be equal")
	}
	if !bridgeIntentsEqual(rem, bridgeIntent{removed: true}) {
		t.Fatal("identical remove intents must be equal")
	}

	changedModels := add
	changedModels.node.Models = []string{"m1", "m2"}
	if bridgeIntentsEqual(add, changedModels) {
		t.Fatal("a model-list change must not be equal")
	}
	changedPath := add
	changedPath.node.BasePath = ""
	if bridgeIntentsEqual(add, changedPath) {
		t.Fatal("a base-path change must not be equal")
	}
	if bridgeIntentsEqual(add, rem) {
		t.Fatal("add and remove intents must not be equal")
	}
}
