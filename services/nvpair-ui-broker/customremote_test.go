// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Broker-side contract for the remote custom-endpoint carrier (Task 11).
//
// Two halves live in the broker and each gets pinned here:
//
//   - Origin half: a custom/* request naming a *different* nodeId relays to
//     the local engine-manager (which owns peer trust) with params verbatim;
//     a self-addressed request runs locally; a peer-addressed request with no
//     engine-manager fails with the unified unavailable message rather than
//     masquerading as an empty local list.
//   - Owner half: a custom:relay-request from our engine-manager (already
//     past its pin gate) executes through the same custom plane a local edit
//     uses — persistence decides the reply, the self-forward guard reads
//     *this node's* facades, the in-flight cap answers busy — and the API key
//     stays in the request body and the 0600 store, never in a reply, an
//     error string, or a local path.
//
// The fake engine-manager mirrors the real wiring: notifications from the
// fake end are dispatched through forwardEngineNotification, the same hook
// the supervised process feeds, so the dispatch branch is under test too.

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// customRelayReply is one custom:relay-reply frame body as the target's
// engine-manager would decode it.
type customRelayReply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// customOwnerEmFaker: an rpcWorker over one end of a net.Pipe whose read
// pump dispatches into Broker.forwardEngineNotification, plus the fake-side
// Codec the test injects relay frames through and reads replies from.
type customOwnerEmFaker struct {
	worker *rpcWorker
	em     *Codec
}

func newCustomOwnerEmFaker(t *testing.T, b *Broker) *customOwnerEmFaker {
	t.Helper()
	brokerSide, emSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = emSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	go worker.peer.Serve(nil, b.forwardEngineNotification)
	return &customOwnerEmFaker{worker: worker, em: NewCodec(emSide)}
}

func (f *customOwnerEmFaker) inject(t *testing.T, frame string) {
	t.Helper()
	sent := make(chan error, 1)
	go func() { sent <- f.em.Notify("custom:relay-request", json.RawMessage(frame)) }()
	select {
	case err := <-sent:
		require.NoError(t, err, "injecting custom:relay-request")
	case <-time.After(3 * time.Second):
		require.FailNow(t, "custom:relay-request never consumed")
	}
}

// reply returns the decoded relay reply plus the raw params string, so a test
// can assert the key is absent from the whole frame body, not just the
// decoded fields.
func (f *customOwnerEmFaker) reply(t *testing.T) (customRelayReply, string) {
	t.Helper()
	replied := make(chan *Message, 1)
	go func() {
		msg, err := f.em.Read()
		if err != nil {
			replied <- nil
			return
		}
		replied <- msg
	}()
	var msg *Message
	select {
	case msg = <-replied:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "no custom:relay-reply within 3s")
	}
	require.NotNil(t, msg, "relay reply stream closed")
	require.Equal(t, "custom:relay-reply", msg.Method)
	var r customRelayReply
	require.NoError(t, json.Unmarshal(msg.Params, &r), "relay reply body")
	return r, string(msg.Params)
}

func (f *customOwnerEmFaker) attach(t *testing.T, b *Broker) {
	t.Helper()
	b.setEngineMgr(f.worker)
}

// --- Owner half -------------------------------------------------------------

func TestCustomRelayAdd_LandsInStoreAndAnswersKeySetOnly(t *testing.T) {
	b, _, path := customTestBroker(t, nil)
	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	fake.inject(t, `{"id":"op1","op":"add","request":{"url":"http://10.9.8.7","port":8000,"apiKey":"`+testEndpointSecret+`"},"caller":"urn:nvpair:node:peer-a"}`)

	reply, raw := fake.reply(t)
	require.Empty(t, reply.Error, "relay add reply")
	assert.Contains(t, string(reply.Result), `"keySet":true`)
	assert.NotContains(t, raw, testEndpointSecret, "relay reply leaked the endpoint key")

	data, err := os.ReadFile(path)
	require.NoError(t, err, "a peer-driven add must persist like a local one")
	assert.Contains(t, string(data), testEndpointSecret, "the config file is the one place a key may persist")
}

func TestCustomRelayAdd_SelfForwardGuardReadsOwnerFacades(t *testing.T) {
	b, _, path := customTestBroker(t, nil)
	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	// 127.0.0.1:8002 is this node's own Custom facade port: the origin's
	// view of its machine is irrelevant, the guard is evaluated against the
	// target's facades.
	fake.inject(t, `{"id":"op1","op":"add","request":{"url":"http://127.0.0.1","port":8002},"caller":"urn:nvpair:node:peer-a"}`)

	reply, _ := fake.reply(t)
	require.NotEmpty(t, reply.Error, "loopback aimed at the owner's facade port must be refused")
	assert.Contains(t, reply.Error, "may not forward to its own proxy")
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "a refused add must not write the store")
}

func TestCustomRelayList_AnswersCurrentViewsWithoutKeys(t *testing.T) {
	b, _, path := customTestBroker(t, nil)
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{ID: "lab", Name: "lab", BaseURL: "http://10.9.8.7:8000", APIKey: testEndpointSecret},
	}))
	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	fake.inject(t, `{"id":"op1","op":"list","caller":"urn:nvpair:node:peer-a"}`)

	reply, raw := fake.reply(t)
	require.Empty(t, reply.Error, "relay list reply")
	assert.Contains(t, raw, `"keySet":true`)
	assert.NotContains(t, raw, testEndpointSecret, "the key must not ride the reply path")
}

func TestCustomRelayRemove_DropsEntryAndStaysIdempotent(t *testing.T) {
	b, _, path := customTestBroker(t, nil)
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{ID: "lab", BaseURL: "http://10.9.8.7:8000"},
	}))
	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	fake.inject(t, `{"id":"op1","op":"remove","request":{"id":"lab"},"caller":"urn:nvpair:node:peer-a"}`)
	first, _ := fake.reply(t)
	require.Empty(t, first.Error, "first remove")
	assert.Contains(t, string(first.Result), `"removed":true`)

	fake.inject(t, `{"id":"op2","op":"remove","request":{"id":"lab"},"caller":"urn:nvpair:node:peer-a"}`)
	second, _ := fake.reply(t)
	require.Empty(t, second.Error, "retry of the same remove must stay a no-op, not an error")
	assert.Contains(t, string(second.Result), `"removed":false`)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "10.9.8.7", "the entry must be gone from the store")
}

func TestCustomRelayPersistenceFailure_CrossesNetworkScrubbed(t *testing.T) {
	b, _, _ := customTestBroker(t, nil)
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("not-a-directory"), 0o600))
	b.customEndpointsPath = filepath.Join(blocked, "custom-endpoints.json")
	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	fake.inject(t, `{"id":"op1","op":"add","request":{"url":"http://10.9.8.7","port":8000,"apiKey":"`+testEndpointSecret+`"},"caller":"urn:nvpair:node:peer-a"}`)

	reply, _ := fake.reply(t)
	require.NotEmpty(t, reply.Error, "a persistence failure must answer as an error")
	assert.Equal(t, "custom-endpoints persistence failed on this node", reply.Error)
	assert.NotContains(t, reply.Error, blocked, "local paths must not cross the network")
	assert.NotContains(t, reply.Error, testEndpointSecret, "the key must not ride the error path either")
}

func TestCustomRelayBeyondCap_AnswersBusyWithoutExecuting(t *testing.T) {
	b, _, _ := customTestBroker(t, nil)
	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)
	b.customRelayInFlight = customRelayLimit

	fake.inject(t, `{"id":"op1","op":"list","caller":"urn:nvpair:node:peer-a"}`)

	reply, _ := fake.reply(t)
	assert.Equal(t, "too many custom endpoint requests", reply.Error)
}

func TestCustomRelayUnknownOp_AnswersErrorAndStops(t *testing.T) {
	b, _, _ := customTestBroker(t, nil)
	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	fake.inject(t, `{"id":"op1","op":"teleport","caller":"urn:nvpair:node:peer-a"}`)

	reply, _ := fake.reply(t)
	assert.Contains(t, reply.Error, "unknown custom operation: teleport")
}

// --- Origin half ------------------------------------------------------------

func TestCustomRemotePeerAddressedList_RelayedThroughEngineManagerVerbatim(t *testing.T) {
	seen := make(chan proberCall, 4)
	b, client, path := customTestBroker(t, nil)
	b.setEngineMgr(newCustomProberFaker(t, seen))

	resp := customCall(t, b, client, "custom/list", `{"nodeId":"peer-uuid"}`)
	require.Nil(t, resp.Error, "peer-addressed list")
	assert.Equal(t, `{"ok":true}`, string(resp.Result), "the peer's answer passes through, not a local snapshot")

	select {
	case call := <-seen:
		assert.Equal(t, "custom:remote-list", call.method)
		assert.Contains(t, call.params, `"nodeId":"peer-uuid"`, "params ride verbatim; engine-manager picks the peer from them")
	case <-time.After(3 * time.Second):
		require.FailNow(t, "engine-manager never received custom:remote-list")
	}
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "a peer-targeted read must not touch this node's store")
}

func TestCustomRemotePeerAddressedAdd_KeepsTheKeyOutOfTheLocalReply(t *testing.T) {
	seen := make(chan proberCall, 4)
	b, client, path := customTestBroker(t, nil)
	b.setEngineMgr(newCustomProberFaker(t, seen))

	resp := customCall(t, b, client, "custom/add", `{"nodeId":"peer-uuid","url":"http://10.9.8.7","port":8000,"apiKey":"`+testEndpointSecret+`"}`)
	require.Nil(t, resp.Error, "peer-addressed add")

	select {
	case call := <-seen:
		require.Equal(t, "custom:remote-add", call.method)
		assert.Contains(t, call.params, `"nodeId":"peer-uuid"`)
		assert.Contains(t, call.params, testEndpointSecret, "the add body carries the key across the local stdio link — sanctioned plumbing, and it ends there")
	case <-time.After(3 * time.Second):
		require.FailNow(t, "engine-manager never received custom:remote-add")
	}
	assert.NotContains(t, string(resp.Result), testEndpointSecret, "the reply carries no key")
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "the origin must not persist an endpoint addressed to another node")
}

func TestCustomRemoteSelfAddressedEdit_RunsLocallyWithoutDialing(t *testing.T) {
	b, client, path := customTestBroker(t, nil)
	b.nodeID = "this-node"

	// No engine-manager is attached: if the self-addressed request forked
	// onto the relay it would fail with the unavailable error, so a clean
	// reply also proves the local path was taken.
	resp := customCall(t, b, client, "custom/add", `{"nodeId":"this-node","name":"lab","url":"http://10.9.8.7","port":8000}`)
	require.Nil(t, resp.Error, "a self-addressed edit runs the local path")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "http://10.9.8.7:8000")
}

func TestCustomRemotePeerEditWithoutEngineManager_FailsFastWithUnavailableMessage(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)

	resp := customCall(t, b, client, "custom/list", `{"nodeId":"peer-uuid"}`)
	require.NotNil(t, resp.Error, "a peer-addressed read with no engine-manager must fail, not answer an empty list")
	assert.Contains(t, resp.Error.Message, "custom endpoints unavailable")
}
