// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Edit semantics for Custom endpoints (cards plan, Task 4).
//
// Editing an endpoint is one in-place replace carrying the editor's key
// intent: a typed apiKey sets a credential, keyAction:"clear" deletes one,
// and a blank key field means "keep what is stored" — the desktop form never
// round-trips the secret, so an editor that left the field empty must not
// quietly disarm the endpoint. replaceId names the entry being edited; an
// id that matches no configured endpoint is refused (-32602) rather than
// forked, so a stale editor cannot orphan a second entry. When the edit
// changes the endpoint's address, the old key's candidate is retracted and
// the prober loses it before gaining the new one. The peer relay runs the
// same pipeline on the owner half.

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const altSecret = "sk-custom-alt-secret"

// seedBridgedEndpoint makes the endpoint look routable — cached healthy
// verdict plus the store claim the routing bridge would have made — so edit
// tests can assert what survives the edit.
func seedBridgedEndpoint(t *testing.T, b *Broker, key, url string) {
	t.Helper()
	b.manualNotificationHandler(b.manualGeneration)("custom/updated", updatedParams(key, url, true, "model-x"))
}

func readStore(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "config file")
	return string(data)
}

func TestCustomAdd_BlankKeyKeepsStoredKey(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"`+testEndpointSecret+`"}`)
	seedBridgedEndpoint(t, b, stateTestKey(), stateTestURL)

	// The editor was opened on this endpoint: replaceId names it and the
	// key field is blank because the form does not display the secret.
	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.9:8123","port":8123,"replaceId":"`+stateTestKey()+`"}`)
	require.Nil(t, resp.Error, "edit with blank key")
	body := string(resp.Result)
	assert.Contains(t, body, `"keySet":true`, "a blank key field means keep, not drop")
	assert.NotContains(t, body, testEndpointSecret, "keeping a key still does not display it")
	assert.NotContains(t, body, `"healthy":true`,
		"an edit answers unprobed; the old verdict belongs to the old configuration")

	store := readStore(t, path)
	assert.Contains(t, store, testEndpointSecret, "the kept key stays in the 0600 store")
}

func TestCustomAdd_ClearDropsKey(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"`+testEndpointSecret+`"}`)

	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.9:8123","port":8123,"replaceId":"`+stateTestKey()+`","keyAction":"clear"}`)
	require.Nil(t, resp.Error, "edit with clear")
	assert.Contains(t, string(resp.Result), `"keySet":false`, "clear removes the credential, not just the display")

	store := readStore(t, path)
	assert.NotContains(t, store, testEndpointSecret, "a cleared key leaves the store too")
	assert.Contains(t, store, `"baseUrl": "http://10.0.0.9:8123"`, "the endpoint itself stays configured")
}

func TestCustomAdd_TypedKeyReplaces(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"`+testEndpointSecret+`"}`)

	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.9:8123","port":8123,"replaceId":"`+stateTestKey()+`","apiKey":"`+altSecret+`"}`)
	require.Nil(t, resp.Error, "edit with rotated key")
	body := string(resp.Result)
	assert.Contains(t, body, `"keySet":true`)
	assert.NotContains(t, body, altSecret, "the reply never carries the key, new or old")

	store := readStore(t, path)
	assert.Contains(t, store, altSecret, "the rotated key replaces the old one in the store")
	assert.NotContains(t, store, testEndpointSecret, "the replaced credential does not linger")
}

func TestCustomAdd_UnknownKeyActionRejected(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"`+testEndpointSecret+`"}`)

	// "clear" is the only keyAction. A misspelling must fail loudly: quietly
	// reading "rotate" as "keep" (or "clear") would silently change what
	// credential the endpoint routes with.
	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.9:8123","port":8123,"replaceId":"`+stateTestKey()+`","keyAction":"rotate"}`)
	require.NotNil(t, resp.Error, "unknown keyAction must be refused, not guessed")
	assert.Equal(t, -32602, resp.Error.Code, "response (%v)", resp)

	store := readStore(t, path)
	assert.Contains(t, store, testEndpointSecret, "a refused edit writes nothing")
}

func TestCustomAdd_ReplaceIdChangesUrlForksNothing(t *testing.T) {
	seen := make(chan proberCall, 8)
	b, client, path := customTestBroker(t, newCustomProberFaker(t, seen))
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)
	setup := drainProberCall(t, seen)
	require.Equal(t, "custom/add", setup.method, "setup add forwarded first")

	seedBridgedEndpoint(t, b, stateTestKey(), stateTestURL)
	require.True(t, b.store.hasClaim(stateTestKey(), sourceCustom),
		"setup: the endpoint reads as a bridged candidate")

	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.9:8124","port":8124,"replaceId":"`+stateTestKey()+`"}`)
	require.Nil(t, resp.Error, "cross-address edit")

	assert.False(t, b.store.hasClaim(stateTestKey(), sourceCustom),
		"the old address's candidate is retracted: nothing may still route to the endpoint the user replaced")
	_, entries := customListReply(t, b, client)
	require.Len(t, entries, 1, "an edit replaces, it never forks")
	require.Len(t, entries[0].Models, 0, "the replaced entry starts unprobed")
	assert.Equal(t, "http://10.0.0.9:8124", entries[0].BaseURL)
	assert.Equal(t, "custom:http://10.0.0.9:8124", entries[0].ID,
		"the probe identity follows the new address")

	assert.Equal(t, 1, strings.Count(readStore(t, path), `"baseUrl"`), "exactly one entry in the store")

	// The prober loses the old key before it gains the new one: replayed in
	// order, a respawned prober cannot briefly track both addresses.
	remove := drainProberCall(t, seen)
	assert.Equal(t, "custom/remove", remove.method, "cross-address edit releases the old key first")
	assert.Contains(t, remove.params, "10.0.0.9:8123")
	add := drainProberCall(t, seen)
	assert.Equal(t, "custom/add", add.method, "then the new address is offered for probing")
	assert.Contains(t, add.params, "10.0.0.9:8124")
}

func TestCustomAdd_ReplaceIdUnknownRejected(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"`+testEndpointSecret+`"}`)

	// A stale editor (the entry was removed or renamed elsewhere) must fail
	// loudly rather than fork a second entry for the same display purpose.
	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.9:8124","port":8124,"replaceId":"custom:http://10.0.0.9:9999"}`)
	require.NotNil(t, resp.Error, "an unmatched replaceId is refused, not treated as a fresh add")
	assert.Equal(t, -32602, resp.Error.Code, "response (%v)", resp)
	assert.Contains(t, resp.Error.Message, "replaceId does not match a configured endpoint")

	store := readStore(t, path)
	assert.Equal(t, 1, strings.Count(store, `"baseUrl"`), "a refused edit writes nothing")
	assert.Contains(t, store, testEndpointSecret)
}

func TestCustomRelayAdd_ReplaceEditRunsTheOwnerPipeline(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"`+testEndpointSecret+`"}`)

	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	// A peer editing this node's endpoint: clear must land exactly as it
	// would locally — the relay is a transport, not a second implementation.
	fake.inject(t, `{"id":"op1","op":"add","request":{"url":"http://10.0.0.9:8123","port":8123,"replaceId":"custom:http://10.0.0.9:8123","keyAction":"clear"},"caller":"urn:nvpair:node:peer-a"}`)
	reply, raw := fake.reply(t)
	require.Empty(t, reply.Error, "relay edit with clear")
	assert.Contains(t, string(reply.Result), `"keySet":false`)
	assert.NotContains(t, raw, testEndpointSecret, "the relay reply carries no key, kept or cleared")

	store := readStore(t, path)
	assert.NotContains(t, store, testEndpointSecret, "clear dropped the stored key through the peer path too")

	// An unmatched replaceId over the relay answers with the same refusal
	// text as locally, and writes nothing.
	fake.inject(t, `{"id":"op2","op":"add","request":{"url":"http://10.0.0.9:8123","port":8123,"replaceId":"custom:http://ghost.test:7000"},"caller":"urn:nvpair:node:peer-a"}`)
	reply, raw = fake.reply(t)
	assert.Contains(t, reply.Error, "replaceId does not match", "an unknown replaceId is refused over the relay too")
	assert.NotContains(t, raw, testEndpointSecret)
	assert.Contains(t, readStore(t, path), `"baseUrl": "http://10.0.0.9:8123"`)
	assert.Equal(t, 1, strings.Count(readStore(t, path), `"baseUrl"`))
}

func TestCustomAdd_KeyRotationRetiresStaleCredentialCandidate(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"`+testEndpointSecret+`"}`)
	seedBridgedEndpoint(t, b, stateTestKey(), stateTestURL)
	require.True(t, b.store.hasClaim(stateTestKey(), sourceCustom),
		"setup: the endpoint reads as a bridged candidate")

	// Rotate the credential at the same address. The composed key does not
	// move, so the cross-address retraction cannot cover this, and the
	// prober's change-gate compares against the reset unprobed state: if the
	// endpoint rejects the new key, no custom/updated ever fires. A candidate
	// still dialing with the old credential must therefore be retired by the
	// edit itself.
	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.9:8123","port":8123,"replaceId":"`+stateTestKey()+`","apiKey":"`+altSecret+`"}`)
	require.Nil(t, resp.Error, "edit with rotated key")

	assert.False(t, b.store.hasClaim(stateTestKey(), sourceCustom),
		"a rotated credential retires the candidate bridged from the old one")

	// The other half of the contract: retirement is not a kill switch. A
	// successful probe over the new configuration re-bridges the endpoint.
	b.manualNotificationHandler(b.manualGeneration)("custom/updated", updatedParams(stateTestKey(), stateTestURL, true, "model-x"))
	assert.True(t, b.store.hasClaim(stateTestKey(), sourceCustom),
		"a healthy re-probe re-bridges from the new credential")

	store := readStore(t, path)
	assert.Contains(t, store, altSecret)
	assert.NotContains(t, store, testEndpointSecret)
}

func TestCustomEdit_SameIdAddressChangeRetiresStaleAddressCandidate(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"id":"garage-box","url":"http://10.0.0.9:8123","port":8123}`)
	seedBridgedEndpoint(t, b, "garage-box", "http://10.0.0.9:8123")
	require.True(t, b.store.hasClaim("garage-box", sourceCustom),
		"setup: the endpoint reads as a bridged candidate")

	// An explicit id pins the routing identity across an address change: the
	// composed key does not move, so without an edit-time retraction the
	// candidate would keep dialing the address the user replaced.
	resp := customCall(t, b, client, "custom/add", `{"id":"garage-box","url":"http://10.0.0.9:8124","port":8124,"replaceId":"garage-box"}`)
	require.Nil(t, resp.Error, "cross-address edit under one id")

	assert.False(t, b.store.hasClaim("garage-box", sourceCustom),
		"the candidate dialing the old address is retired by the edit")

	b.manualNotificationHandler(b.manualGeneration)("custom/updated", updatedParams("garage-box", "http://10.0.0.9:8124", true, "model-x"))
	assert.True(t, b.store.hasClaim("garage-box", sourceCustom),
		"a healthy probe over the new address re-bridges")
}
