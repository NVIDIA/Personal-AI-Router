// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Config-plane contract for Custom endpoints (Task 4).
//
// Unlike the manual-node trio it mirrors, the broker is the source of truth
// for this one: the prober is a cache of probe results, not the owner of the
// configuration, so persistence decides the response and the worker forward is
// advisory (it exists to kick a re-probe, and a respawn replays the file).
//
// Two properties the rest of the design leans on are pinned here rather than
// assumed:
//
//   - The API key persists exactly where policy allows — the 0600 config
//     file and the local broker↔prober stdio link — and appears in no
//     client-facing payload. That is why one test asserts the key IS in the
//     file (deleting it there would break probing) while the client-side
//     assertions require it absent from every response.
//   - The port is mandatory and never inferred: not defaulted from the
//     scheme, not lifted out of the URL when the field is missing, and a
//     URL port that disagrees with the explicit field is refused rather
//     than silently resolved. A loopback endpoint aimed at one of this
//     node's own facade ports is refused at the door: that is the
//     self-forward loop, and the proxy-side self-guard behind it is
//     defense-in-depth, not permission.

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEndpointSecret = "sk-endpoint-secret"

type proberCall struct {
	method string
	params string
}

// newCustomProberFaker builds an rpcWorker over one end of a net.Pipe whose
// other end answers every request immediately and records what arrived, the
// same fake shape manualnodes_persistence_test.go uses.
func newCustomProberFaker(t *testing.T, seen chan proberCall) *rpcWorker {
	t.Helper()
	brokerSide, workerSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = workerSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	go worker.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(workerSide)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.ID == nil || !msg.IsRequest() {
				continue
			}
			select {
			case seen <- proberCall{method: msg.Method, params: string(msg.Params)}:
			case <-time.After(2 * time.Second):
			}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()
	return worker
}

// customTestBroker drives handleMessage through a real Codec pair against a
// throwaway config path, with or without a fake prober attached.
func customTestBroker(t *testing.T, worker *rpcWorker) (*Broker, *Codec, string) {
	t.Helper()
	clientBroker, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientBroker.Close()
		_ = clientSide.Close()
	})
	path := filepath.Join(t.TempDir(), "configs", "custom-endpoints.json")
	b := &Broker{codec: NewCodec(clientBroker), store: newDiscoveryStore(), customEndpointsPath: path}
	if worker != nil {
		b.setManualNodes(worker)
	}
	return b, NewCodec(clientSide), path
}

// customCall runs one request through handleMessage and waits for its
// response. Going through handleMessage (not the handler function) keeps the
// dispatch-table registration itself under test.
func customCall(t *testing.T, b *Broker, client *Codec, method string, params string) *Message {
	t.Helper()
	id := json.RawMessage(`"c1"`)
	response := make(chan *Message, 1)
	go func() {
		msg, err := client.Read()
		if err != nil {
			response <- nil
			return
		}
		response <- msg
	}()
	b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
	select {
	case msg := <-response:
		require.NotNil(t, msg, "no response to "+method)
		return msg
	case <-time.After(3 * time.Second):
		require.FailNow(t, "no response to "+method)
		return nil
	}
}

func TestCustomAdd_PersistsEndpointAndMasksTheKeyEverywhere(t *testing.T) {
	b, client, path := customTestBroker(t, nil)

	resp := customCall(t, b, client, "custom/add", `{"name":"lab","url":"http://10.9.8.7","port":8000,"apiKey":"`+testEndpointSecret+`"}`)
	require.Nil(t, resp.Error, "add response")
	body := string(resp.Result)
	assert.Contains(t, body, `"http://10.9.8.7:8000"`, "composed baseUrl")
	assert.Contains(t, body, `"keySet":true`, "key presence reported without the key")
	assert.NotContains(t, body, testEndpointSecret, "add response leaked the endpoint key")

	data, err := os.ReadFile(path)
	require.NoError(t, err, "endpoint config was not written")
	assert.Contains(t, string(data), testEndpointSecret, "the config file is the one place a key may persist")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "config file mode")

	// A keyless second endpoint must report keySet false, not omit the field:
	// the UI's "key set ✓" indicator reads the presence, not emptiness.
	resp = customCall(t, b, client, "custom/add", `{"url":"http://10.9.8.9","port":8000}`)
	require.Nil(t, resp.Error, "second add")

	resp = customCall(t, b, client, "custom/list", `{}`)
	require.Nil(t, resp.Error, "list response")
	list := string(resp.Result)
	assert.NotContains(t, list, testEndpointSecret, "list payload leaked the endpoint key")
	assert.Contains(t, list, `"keySet":true`)
	assert.Contains(t, list, `"keySet":false`)
	assert.Contains(t, list, `"id":"custom:http://10.9.8.7:8000"`, "derived stable endpoint id")
}

func TestCustomAdd_RejectsMissingOrMismatchedPort(t *testing.T) {
	b, client, path := customTestBroker(t, nil)

	for _, tc := range []struct{ name, params string }{
		{"port field omitted", `{"url":"http://example.com"}`},
		{"port field zero", `{"url":"http://example.com","port":0}`},
		{"url port and missing field disagree", `{"url":"http://example.com:9999"}`},
		{"url port and port field disagree", `{"url":"http://example.com:9999","port":8000}`},
		{"no scheme", `{"url":"example.com","port":8000}`},
		{"non-http scheme", `{"url":"grpc://example.com","port":8000}`},
	} {
		resp := customCall(t, b, client, "custom/add", tc.params)
		require.NotNil(t, resp.Error, tc.name+" must be rejected")
		assert.Equal(t, -32602, resp.Error.Code, tc.name+" error code")
	}
	_, err := os.ReadFile(path)
	assert.True(t, os.IsNotExist(err), "a rejected add must leave no config behind")
}

func TestCustomAdd_RejectsLoopbackEndpointOnAFrontedFacadePort(t *testing.T) {
	b, client, path := customTestBroker(t, nil)

	// 8002 is the Custom facade default, 8000 vLLM's, and 11434 the Ollama
	// facade port: an endpoint whose URL is this node's own proxy is a
	// self-forward loop regardless of whether that facade is currently bound,
	// and the proxy's own self/alias guard behind it is defense-in-depth, not
	// permission.
	for _, params := range []string{
		`{"url":"http://127.0.0.1","port":8002}`,
		`{"url":"http://localhost","port":8002}`,
		`{"url":"http://127.0.0.1","port":8000}`,
		`{"url":"http://127.0.0.1","port":11434}`,
	} {
		resp := customCall(t, b, client, "custom/add", params)
		require.NotNil(t, resp.Error, "self-forward must be rejected: "+params)
		assert.Equal(t, -32602, resp.Error.Code, "self-forward error code")
	}

	// A loopback endpoint on a port this node fronts nothing on stays legal:
	// a locally-run mtplx-class server is a primary use case.
	resp := customCall(t, b, client, "custom/add", `{"url":"http://127.0.0.1","port":8001}`)
	require.Nil(t, resp.Error, "status")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "127.0.0.1:8001")
}

func TestCustomEndpoints_AddRemoveReadd_ForwardsToProberAndPersists(t *testing.T) {
	seen := make(chan proberCall, 8)
	worker := newCustomProberFaker(t, seen)
	b, client, path := customTestBroker(t, worker)

	resp := customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.1","port":8001,"apiKey":"`+testEndpointSecret+`"}`)
	require.Nil(t, resp.Error, "add")

	resp = customCall(t, b, client, "custom/remove", `{"id":"custom:http://10.0.0.1:8001"}`)
	require.Nil(t, resp.Error, "remove")
	assert.Contains(t, string(resp.Result), `"removed":true`)

	resp = customCall(t, b, client, "custom/add", `{"url":"http://10.0.0.1","port":8001,"apiKey":"`+testEndpointSecret+`"}`)
	require.Nil(t, resp.Error, "re-add")

	var calls []proberCall
	for i := 0; i < 3; i++ {
		select {
		case call := <-seen:
			calls = append(calls, call)
		case <-time.After(3 * time.Second):
			require.FailNow(t, "prober forward missing")
		}
	}
	require.Len(t, calls, 3)
	assert.Equal(t, "custom/add", calls[0].method, "first forward")
	assert.Contains(t, calls[0].params, testEndpointSecret, "the key rides the local broker↔prober link; policy forbids it only on network, logs, and event payloads")
	assert.Equal(t, "custom/remove", calls[1].method, "remove forward")
	assert.Contains(t, calls[1].params, "custom:http://10.0.0.1:8001", "remove forward carries the canonical id")
	assert.Equal(t, "custom/add", calls[2].method, "re-add forward")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), "10.0.0.1:8001"), "re-add replaces the removed entry, it does not duplicate it")
}

func TestReplayPersistedCustomEndpoints_ResendsEntriesToRespawnedWorker(t *testing.T) {
	seen := make(chan proberCall, 4)
	worker := newCustomProberFaker(t, seen)

	path := filepath.Join(t.TempDir(), "configs", "custom-endpoints.json")
	b := &Broker{customEndpointsPath: path}
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{ID: "custom:http://10.0.0.1:8001", Name: "home", BaseURL: "http://10.0.0.1:8001", APIKey: testEndpointSecret},
		{ID: "custom:http://10.0.0.2:8002", BaseURL: "http://10.0.0.2:8002"},
	}))

	done := make(chan struct{})
	go func() {
		b.replayPersistedCustomEndpoints(worker)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "replay did not finish")
	}

	var calls []proberCall
	for i := 0; i < 2; i++ {
		select {
		case call := <-seen:
			calls = append(calls, call)
		case <-time.After(2 * time.Second):
			require.FailNow(t, "replayed entry missing")
		}
	}
	require.Len(t, calls, 2)
	require.Equal(t, "custom/add", calls[0].method, "first replayed method")
	assert.Contains(t, calls[0].params, testEndpointSecret, "the keyed entry replays with its credential")
	assert.Equal(t, "custom/add", calls[1].method, "second replayed method")
	assert.Contains(t, calls[1].params, "http://10.0.0.2:8002", "second replayed entry")
	assert.NotContains(t, calls[1].params, testEndpointSecret, "the keyless entry carries no secret")
}

func TestCustomList_EmptyStoreAnswersEmptyArray(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	resp := customCall(t, b, client, "custom/list", `{}`)
	require.Nil(t, resp.Error, "list on an empty store")
	assert.JSONEq(t, `[]`, string(resp.Result))
}
