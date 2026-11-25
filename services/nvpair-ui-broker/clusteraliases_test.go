// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"nvpair-shared/noderec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The broker is a courier: it reads what the settings worker stored and hands
// the same list to the proxy's router and to engine-manager's announcement.
// Both ends spell an alias the same two names, so there is nothing to
// translate — and that is exactly why the courier still checks the payload.
// A document written for another shape decodes cleanly in Go, and a list that
// decodes into nameless-target rows is not a broken alias list but a silently
// inert one: the cluster keeps answering /v1/models and stops routing.

// TestAliasesToForwardedWire_CarriesEveryAliasItWasGiven: the two names arrive
// as written, in order, with the "aliases" key both consumers parse.
func TestAliasesToForwardedWire_CarriesEveryAliasItWasGiven(t *testing.T) {
	stored := `{"aliases":[
		{"name":"quick-chat","model":"llama3.2:1b"},
		{"name":"embeddings","model":"text-embedding-nomic-embed-text-v1.5"}
	]}`

	out, err := aliasesToForwardedWire(json.RawMessage(stored))
	require.NoError(t, err, "forward a configured list")

	assert.JSONEq(t, stored, string(out), "the delivered payload")

	var got modelAliasesPayload
	require.NoError(t, json.Unmarshal(out, &got), "decode the delivered payload")
	assert.Equal(t, []noderec.ModelAlias{
		{Name: "quick-chat", Model: "llama3.2:1b"},
		{Name: "embeddings", Model: "text-embedding-nomic-embed-text-v1.5"},
	}, got.Aliases, "a synonym lost a name on the way through")
}

// TestAliasesToForwardedWire_RefusesAPayloadThatCitesProviders is the reason a
// copy step still decodes: a provider-citing document — one written before an
// alias became a synonym — decodes as a list of aliases with no model, which
// would be delivered and then route nothing. Refusing it costs one sync and
// leaves a warning in the log; delivering it loses both.
func TestAliasesToForwardedWire_RefusesAPayloadThatCitesProviders(t *testing.T) {
	legacy := `{"aliases":[{"name":"quick-chat","providers":[
		{"node_id":"2b6564aa-0b0e-47af-83bd-6a6f19d99780","engine":"ollama","model":"llama3.2:1b"}
	]}]}`

	_, err := aliasesToForwardedWire(json.RawMessage(legacy))
	require.Error(t, err, "a provider-citing payload was accepted for delivery")
	assert.Contains(t, err.Error(), "model must name the model this alias stands for")
}

// TestAliasesToForwardedWire_RefusesAnAliasWithNoModelToStandFor covers the
// same silent-inert failure from the other direction: a well-shaped document
// whose target is missing or blank. Names only the courier will not pass on,
// because a synonym without a model routes nothing wherever it lands.
func TestAliasesToForwardedWire_RefusesAnAliasWithNoModelToStandFor(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{"no model field", `{"aliases":[{"name":"quick-chat"}]}`, "model must name the model"},
		{"blank model", `{"aliases":[{"name":"quick-chat","model":"   "}]}`, "model must name the model"},
		{"blank name", `{"aliases":[{"name":"   ","model":"llama3.2:1b"}]}`, "name must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := aliasesToForwardedWire(json.RawMessage(tc.payload))
			require.Error(t, err, "an unusable alias was accepted for delivery")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestAliasesToForwardedWire_EmptyListStaysAnEmptyList: clearing aliases must
// arrive as an empty list, not as an absent field the consumers might read as
// "no news" and keep a stale set on.
func TestAliasesToForwardedWire_EmptyListStaysAnEmptyList(t *testing.T) {
	out, err := aliasesToForwardedWire(json.RawMessage(`{"aliases":[]}`))
	require.NoError(t, err, "forward an empty list")
	assert.JSONEq(t, `{"aliases":[]}`, string(out))
}

func TestAliasesToForwardedWire_RejectsUndecodablePayload(t *testing.T) {
	_, err := aliasesToForwardedWire(json.RawMessage(`{"aliases":"not-a-list"}`))
	assert.Error(t, err, "a malformed settings payload was accepted for delivery")
}

// TestAliasCourier_DeliversToAProxyStartedWithoutTheClusterRouter pins that
// the courier is a courier, not a gatekeeper. This broker's spawn-time
// cluster-router preference is off — the one setting that used to stop every
// alias delivery — yet the document still has to reach the child, whether
// that process started with the endpoint bound or had it lifted later at
// runtime. The settings worker is the list's real source, wired the way a
// live settings service is: what the courier carries is what was stored, not
// what a test planted.
func TestAliasCourier_DeliversToAProxyStartedWithoutTheClusterRouter(t *testing.T) {
	const stored = `{"aliases":[{"name":"quick-chat","model":"llama3.2:1b"}]}`

	b := &Broker{}
	b.setSettings(newAnsweringSettingsWorker(t, stored))
	frames := addAnsweringProxyWithFrames(t, b, ollamaProxyProfile, map[string]any{"ok": true})
	proxy := b.engineProxyHandle(ollamaProxyProfile)
	require.NotNil(t, proxy, "the courier needs a child to deliver to")

	b.syncClusterModelAliasesToProxy(proxy)

	frame, ok := nextFrameWithin(t, frames, hopWindow)
	require.True(t, ok, "the courier stopped before the child")
	assert.Equal(t, "cluster/set-aliases", frame.Method)
	assert.JSONEq(t, stored, string(frame.Params), "the delivered document was not the stored one")
}
