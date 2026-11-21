// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The broker's half of the cluster endpoint. It keeps none of the endpoint's
// state: it relays process-scoped reads — and, since the endpoint became
// liftable at runtime, the live write — to the proxy child and reports what
// comes back, because a port the proxy never bound has to be absent from the
// Endpoints list rather than listed. These tests therefore pin
// what a config-echoing implementation would get wrong — where the answer
// comes from when a proxy does answer, what happens when no proxy exists to
// ask, how the relay finds the process when Ollama, whose handle getProxy
// resolves, is not fronted, and for the write what else rides to the child
// behind it.
//
// The fake child answers over an in-memory pipe, which is what makes the
// relay's own shape visible: a channel carries each frame the broker sends
// downstream, so a test can see whether a call reached the child at all,
// under what name, and carrying what.

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newClientFacingBroker returns a broker whose JSON-RPC peer is the test, plus
// the codec for that client end. The five-second read deadline is what turns a
// missing reply into a failure instead of a hung suite.
func newClientFacingBroker(t *testing.T) (*Broker, *Codec) {
	t.Helper()
	brokerEnd, clientEnd := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, brokerEnd.Close())
		assert.NoError(t, clientEnd.Close())
	})
	require.NoError(t, clientEnd.SetReadDeadline(time.Now().Add(5*time.Second)), "set client read deadline")
	return &Broker{codec: NewCodec(brokerEnd)}, NewCodec(clientEnd)
}

// addAnsweringProxy publishes a fake proxy child under one engine's slot —
// which is how a test controls which handle the broker can find, since one
// process is reached through every enabled engine's slot at once. Each frame
// the broker sends downstream reaches the methods channel, so a test can tell
// "asked the child" from "answered locally". A test that must also read what
// a frame carried takes addAnsweringProxyWithFrames instead.
func addAnsweringProxy(t *testing.T, b *Broker, owner engineProxyProfile, answer any) chan string {
	t.Helper()
	methods := make(chan string, 8)
	frames := addAnsweringProxyWithFrames(t, b, owner, answer)
	go func() {
		for request := range frames {
			methods <- request.Method
		}
	}()
	return methods
}

// addAnsweringProxyWithFrames installs the same answering child but streams
// each whole request — method and params — to the test. A relay test that
// must pin not just which method reached the child but what it carried (the
// {enabled,port} an endpoint write sent, or the alias document that rode
// behind it) reads frames instead of method names.
func addAnsweringProxyWithFrames(t *testing.T, b *Broker, owner engineProxyProfile, answer any) chan *Message {
	t.Helper()
	proxySide, childSide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, proxySide.Close())
		assert.NoError(t, childSide.Close())
	})

	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxySide))}
	go proxy.peer.Serve(nil, nil)

	frames := make(chan *Message, 8)
	go func() {
		defer close(frames)
		codec := NewCodec(childSide)
		for {
			request, err := codec.Read()
			if !assertRPCRead(t, err) {
				return
			}
			frames <- request
			if rpcErr, refused := answer.(*RPCError); refused {
				assert.NoError(t, codec.RespondError(request.ID, rpcErr.Code, rpcErr.Message))
				continue
			}
			assert.NoError(t, codec.Respond(request.ID, answer))
		}
	}()

	b.setEngineProxyHandle(owner, proxy)
	return frames
}

// callRouterStatus asks the broker for the endpoint's bound state and waits for
// its reply.
func callRouterStatus(t *testing.T, b *Broker, client *Codec) *Message {
	t.Helper()
	id := json.RawMessage(`1`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "router/status"})

	response, err := client.Read()
	require.NoError(t, err, "the broker never answered router/status")
	require.NotNil(t, response.ID, "the reply is not correlated with the request")
	require.Equal(t, string(id), string(*response.ID), "the reply carries another request's id")
	return response
}

// decodedRouterStatus reads a reply as the state it carries. A reply that came
// back as an error fails here rather than being decoded as a down endpoint, so
// "we do not know" and "there is none" stay distinguishable in the assertions.
func decodedRouterStatus(t *testing.T, reply *Message) RouterStatusResult {
	t.Helper()
	require.Nil(t, reply.Error, "the broker reported a failure instead of a state")
	var status RouterStatusResult
	require.NoError(t, json.Unmarshal(reply.Result, &status))
	return status
}

// TestRouterStatus_WithoutAProxyAnswersNoEndpoint covers the shape a desktop
// sees on a machine with no proxy at all: an absent endpoint is a normal
// configuration, so it is answered as a result. An error here would render as
// a broken endpoint on a correctly configured machine.
func TestRouterStatus_WithoutAProxyAnswersNoEndpoint(t *testing.T) {
	b, client := newClientFacingBroker(t)

	status := decodedRouterStatus(t, callRouterStatus(t, b, client))

	assert.Equal(t, RouterStatusResult{Running: false, Port: 0}, status)
}

// TestRouterStatus_ReportsTheEndpointTheProxyAnswersOn is the trust property
// in one test: the reported port is the one the child says it
// bound, not the port the desktop was configured with. clusterRouterPort is
// planted at a different number so an implementation that answered from config
// fails here — which is exactly how somebody else's server on the configured
// port would come to be listed as the cluster endpoint.
func TestRouterStatus_ReportsTheEndpointTheProxyAnswersOn(t *testing.T) {
	b, client := newClientFacingBroker(t)
	b.clusterRouter = true
	b.clusterRouterPort = 9999
	methods := addAnsweringProxy(t, b, ollamaProxyProfile, RouterStatusResult{Running: true, Port: 4321})

	status := decodedRouterStatus(t, callRouterStatus(t, b, client))

	assert.Equal(t, RouterStatusResult{Running: true, Port: 4321}, status)
	assert.Equal(t, "router/status", <-methods,
		"the read was not sent to the child as a process-scoped method")
}

// TestRouterStatus_ReachesTheProxyWhenOllamaIsNotFronted pins why the read
// cannot use getProxy. That helper resolves Ollama's handle, so on a machine
// with Ollama switched off it answers nil while the same proxy process is
// serving three other engines — and the cluster endpoint belongs to the
// process, not to a facade.
func TestRouterStatus_ReachesTheProxyWhenOllamaIsNotFronted(t *testing.T) {
	b, client := newClientFacingBroker(t)
	methods := addAnsweringProxy(t, b, mustEngineProxyProfile("vllm"), RouterStatusResult{Running: true, Port: 4567})
	require.Nil(t, b.getProxy(), "this test needs Ollama's slot empty, which is what makes it discriminate")

	status := decodedRouterStatus(t, callRouterStatus(t, b, client))

	assert.Equal(t, RouterStatusResult{Running: true, Port: 4567}, status)
	assert.Equal(t, "router/status", <-methods)
}

// TestRouterStatus_ReportsAFailureWhenTheChildRefuses keeps an unknown state
// unknown. A refused read is not evidence of no endpoint: were it reported as
// one, the Endpoints list would go quiet on a machine whose proxy is merely
// unreachable, which is the same lie in the other direction.
func TestRouterStatus_ReportsAFailureWhenTheChildRefuses(t *testing.T) {
	b, client := newClientFacingBroker(t)
	addAnsweringProxy(t, b, ollamaProxyProfile, &RPCError{Code: -32601, Message: "method not found: router/status"})

	reply := callRouterStatus(t, b, client)

	require.NotNil(t, reply.Error, "a refused read was answered as a state")
	assert.Equal(t, -32601, reply.Error.Code)
}

// --- router/get-aliases -------------------------------------------------------

// decodedRouterAliases reads an alias-view reply as data. As with the status
// decode, an error reply fails here, so "the view could not be read" cannot be
// silently drawn as "the cluster has no aliases".
func decodedRouterAliases(t *testing.T, reply *Message) []struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	OriginNode string `json:"originNode"`
	OriginHost string `json:"originHost"`
} {
	t.Helper()
	require.Nil(t, reply.Error, "the broker reported a failure instead of a view")
	var result struct {
		Aliases []struct {
			Name       string `json:"name"`
			Model      string `json:"model"`
			OriginNode string `json:"originNode"`
			OriginHost string `json:"originHost"`
		} `json:"aliases"`
	}
	require.NoError(t, json.Unmarshal(reply.Result, &result))
	return result.Aliases
}

func callRouterGetAliases(t *testing.T, b *Broker, client *Codec) *Message {
	t.Helper()
	id := json.RawMessage(`2`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "router/get-aliases"})

	response, err := client.Read()
	require.NoError(t, err, "the broker never answered router/get-aliases")
	require.NotNil(t, response.ID, "the reply is not correlated with the request")
	require.Equal(t, string(id), string(*response.ID), "the reply carries another request's id")
	return response
}

// TestRouterGetAliases_WithoutAProxyAnswersAnEmptyView: same contract as the
// status read — nothing to ask is a state, not a failure, and the aliases card
// renders "no aliases" rather than an error on a machine whose proxy is down.
func TestRouterGetAliases_WithoutAProxyAnswersAnEmptyView(t *testing.T) {
	b, client := newClientFacingBroker(t)

	assert.Empty(t, decodedRouterAliases(t, callRouterGetAliases(t, b, client)))
}

// TestRouterGetAliases_RelaysTheChildsViewVerbatim pins both halves of the
// relay: the read goes to the child as a process-scoped method, and the
// origins come back untouched. They are the payload — a relay that dropped or
// reshaped them would leave the card unable to mark which names belong to
// another machine, which is the entire point of the view.
func TestRouterGetAliases_RelaysTheChildsViewVerbatim(t *testing.T) {
	b, client := newClientFacingBroker(t)
	childView := json.RawMessage(`{"aliases":[
		{"name":"mine","model":"llama3.2:1b"},
		{"name":"theirs","model":"qwen3:4b","originNode":"peer-uuid","originHost":"peer-one"}
	]}`)
	methods := addAnsweringProxy(t, b, ollamaProxyProfile, childView)

	aliases := decodedRouterAliases(t, callRouterGetAliases(t, b, client))

	require.Len(t, aliases, 2)
	assert.Equal(t, "mine", aliases[0].Name)
	assert.Empty(t, aliases[0].OriginNode, "a local entry gained an origin")
	assert.Equal(t, "theirs", aliases[1].Name)
	assert.Equal(t, "peer-uuid", aliases[1].OriginNode)
	assert.Equal(t, "peer-one", aliases[1].OriginHost)
	assert.Equal(t, "router/get-aliases", <-methods,
		"the read was not sent to the child as a process-scoped method")
}

// --- router/set-enabled --------------------------------------------------------

// callRouterSetEnabled drives the endpoint write through the broker's
// dispatch and waits for its reply. The params are the desktop's payload;
// the broker forwards them verbatim, so a test spells what the child should
// receive.
func callRouterSetEnabled(t *testing.T, b *Broker, client *Codec, params string) *Message {
	t.Helper()
	id := json.RawMessage(`3`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "router/set-enabled", Params: json.RawMessage(params)})

	response, err := client.Read()
	require.NoError(t, err, "the broker never answered router/set-enabled")
	require.NotNil(t, response.ID, "the reply is not correlated with the request")
	require.Equal(t, string(id), string(*response.ID), "the reply carries another request's id")
	return response
}

// TestRouterSetEnabled_RelaysToTheProxyAndReportsWhatEndsUpListening covers
// both halves of the write's relay: the desktop's {enabled,port} reaches the
// child unchanged, and the answer is the child's bind report rather than a
// restatement of the request. Only the child knows whether the port came up.
func TestRouterSetEnabled_RelaysToTheProxyAndReportsWhatEndsUpListening(t *testing.T) {
	b, client := newClientFacingBroker(t)
	frames := addAnsweringProxyWithFrames(t, b, ollamaProxyProfile, RouterStatusResult{Running: true, Port: 6111})

	reply := callRouterSetEnabled(t, b, client, `{"enabled":true,"port":6111}`)

	status := decodedRouterStatus(t, reply)
	assert.Equal(t, RouterStatusResult{Running: true, Port: 6111}, status,
		"the reply must be what the child says it bound")

	frame, ok := nextFrameWithin(t, frames, hopWindow)
	require.True(t, ok, "the write never reached the child")
	assert.Equal(t, "router/set-enabled", frame.Method,
		"the write was not sent to the child as a process-scoped method")
	assert.JSONEq(t, `{"enabled":true,"port":6111}`, string(frame.Params),
		"the broker reshaped the desktop's payload instead of forwarding it")
}

// TestRouterSetEnabled_KeepsTheChildsBindRefusalAsAnError pins the error
// relay. A requested port may already belong to someone else's server; the
// child says so with its bind-refusal code and its own reason, and the
// desktop's endpoint card prints exactly that. Answering a refusal with a
// success shape — or with broker prose — would hide from the operator that
// the port is occupied.
func TestRouterSetEnabled_KeepsTheChildsBindRefusalAsAnError(t *testing.T) {
	b, client := newClientFacingBroker(t)
	addAnsweringProxy(t, b, ollamaProxyProfile, &RPCError{Code: -32011, Message: "cannot bind 127.0.0.1:6111: address already in use"})

	reply := callRouterSetEnabled(t, b, client, `{"enabled":true,"port":6111}`)

	require.NotNil(t, reply.Error, "a refused bind was answered as a state")
	assert.Equal(t, -32011, reply.Error.Code, "the child's bind-refusal code did not survive the relay")
	assert.Contains(t, reply.Error.Message, "address already in use",
		"the child's reason was replaced on the way through")
}

// TestRouterSetEnabled_AnswersNoEndpointWhenNoProxyIsRunning keeps the write
// on the reads' contract: no proxy is a configuration, not a failure. The
// card learns "no endpoint", the same answer a machine that never had one
// gives, rather than an error that looks like a broken endpoint.
func TestRouterSetEnabled_AnswersNoEndpointWhenNoProxyIsRunning(t *testing.T) {
	b, client := newClientFacingBroker(t)

	reply := callRouterSetEnabled(t, b, client, `{"enabled":true,"port":6111}`)

	status := decodedRouterStatus(t, reply)
	assert.Equal(t, RouterStatusResult{Running: false, Port: 0}, status)
}

// TestRouterSetEnabled_ReCouriersAliasesWhenTheEndpointComesUp pins what
// rides behind a successful enable. An endpoint without the alias document is
// a nameless cluster: peers can dial it, yet none of the configured aliases
// resolve there — and the client has just been told the endpoint is on. So
// the document travels from the settings worker, its real source, to the
// child, and it arrives before the broker answers the write. The broker's
// spawn-time cluster-router preference is left off here because a runtime
// lift is exactly the case where that preference says nothing; delivery must
// not depend on it.
func TestRouterSetEnabled_ReCouriersAliasesWhenTheEndpointComesUp(t *testing.T) {
	const stored = `{"aliases":[{"name":"quick-chat","model":"llama3.2:1b"}]}`

	b, client := newClientFacingBroker(t)
	b.setSettings(newAnsweringSettingsWorker(t, stored))
	frames := addAnsweringProxyWithFrames(t, b, ollamaProxyProfile, RouterStatusResult{Running: true, Port: 6111})

	reply := callRouterSetEnabled(t, b, client, `{"enabled":true,"port":0}`)

	status := decodedRouterStatus(t, reply)
	require.True(t, status.Running,
		"the fixture must end with the endpoint up, or the courier has no trigger")

	enable, ok := nextFrameWithin(t, frames, hopWindow)
	require.True(t, ok, "the write never reached the child")
	require.Equal(t, "router/set-enabled", enable.Method)

	courier, ok := nextFrameWithin(t, frames, hopWindow)
	require.True(t, ok, "the endpoint came up without the alias document following it")
	assert.Equal(t, "cluster/set-aliases", courier.Method)
	assert.JSONEq(t, stored, string(courier.Params), "the delivered document was not the stored one")
}

// addAnsweringProxyWithAckGate installs the answering child of
// addAnsweringProxyWithFrames with one difference: it withholds the ack to
// cluster/set-aliases until the test closes the returned gate, while every
// other method — including router/set-enabled — is answered immediately with
// the same payload. The relay therefore carries exactly one delay the test
// controls, which is what lets a test tell an answer that FOLLOWS the
// courier's ack from one that merely CO-OCCURS with the courier frame.
func addAnsweringProxyWithAckGate(t *testing.T, b *Broker, owner engineProxyProfile, answer any) (chan *Message, chan struct{}) {
	t.Helper()
	proxySide, childSide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, proxySide.Close())
		assert.NoError(t, childSide.Close())
	})

	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxySide))}
	go proxy.peer.Serve(nil, nil)

	gate := make(chan struct{})
	frames := make(chan *Message, 8)
	go func() {
		defer close(frames)
		codec := NewCodec(childSide)
		for {
			request, err := codec.Read()
			if !assertRPCRead(t, err) {
				return
			}
			frames <- request
			if request.Method == "cluster/set-aliases" {
				<-gate
			}
			if rpcErr, refused := answer.(*RPCError); refused {
				assert.NoError(t, codec.RespondError(request.ID, rpcErr.Code, rpcErr.Message))
				continue
			}
			assert.NoError(t, codec.Respond(request.ID, answer))
		}
	}()

	b.setEngineProxyHandle(owner, proxy)
	return frames, gate
}

// TestRouterSetEnabled_WaitsForTheAliasCourierBeforeAnsweringTheDesktop pins
// the ORDER inside the runtime-enable relay, which the courier test beside it
// cannot: that test consumes the desktop reply before inspecting frames, so
// it passes however the two downstream legs are interleaved. Here the child
// acks the enable immediately and holds only the alias courier's ack, so
// while the gate is shut the reply's sole remaining dependency is that ack —
// a reply observed during the window can only have been answered ahead of
// the courier, and fails this test. The desktop bridge persists "endpoint
// up" from that reply, so an answer shipped before the ack would put the
// card's truth a beat ahead of what the proxy knows it routes.
func TestRouterSetEnabled_WaitsForTheAliasCourierBeforeAnsweringTheDesktop(t *testing.T) {
	const stored = `{"aliases":[{"name":"quick-chat","model":"llama3.2:1b"}]}`
	const noReplyWindow = 300 * time.Millisecond

	b, client := newClientFacingBroker(t)
	b.setSettings(newAnsweringSettingsWorker(t, stored))
	frames, gate := addAnsweringProxyWithAckGate(t, b, ollamaProxyProfile, RouterStatusResult{Running: true, Port: 6111})

	id := json.RawMessage(`3`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "router/set-enabled", Params: json.RawMessage(`{"enabled":true,"port":0}`)})

	enable, ok := nextFrameWithin(t, frames, hopWindow)
	require.True(t, ok, "the write never reached the child")
	require.Equal(t, "router/set-enabled", enable.Method)

	courier, ok := nextFrameWithin(t, frames, hopWindow)
	require.True(t, ok, "the endpoint came up without the alias document following it")
	require.Equal(t, "cluster/set-aliases", courier.Method)
	require.JSONEq(t, stored, string(courier.Params), "the delivered document was not the stored one")

	// Both frames have reached the child and the courier's ack is being
	// held. Read the reply in the background so the window can prove the
	// reply is absent, not merely late.
	type readResult struct {
		reply *Message
		err   error
	}
	replied := make(chan readResult, 1)
	go func() {
		reply, err := client.Read()
		replied <- readResult{reply, err}
	}()
	select {
	case <-replied:
		t.Fatal("the broker answered the desktop while the couriered aliases were still unacknowledged")
	case <-time.After(noReplyWindow):
	}

	close(gate)

	var reply *Message
	select {
	case res := <-replied:
		require.NoError(t, res.err, "the client read failed after the courier was acknowledged")
		reply = res.reply
	case <-time.After(hopWindow):
		t.Fatal("the broker never answered router/set-enabled after the courier was acknowledged")
	}
	require.NotNil(t, reply.ID, "the reply is not correlated with the request")
	assert.Equal(t, string(id), string(*reply.ID), "the reply carries another request's id")

	status := decodedRouterStatus(t, reply)
	require.True(t, status.Running,
		"the answer after the ack was not the child's report of an endpoint that came up")
}

// TestRouterSetEnabled_CouriersNothingWhenNoEndpointEndsUpListening is the
// other half of the courier trigger: no endpoint means nothing to route, so
// the document rides on no reply. A settings worker is wired in every case —
// delivery is possible each time, and the silence is what these cases prove.
func TestRouterSetEnabled_CouriersNothingWhenNoEndpointEndsUpListening(t *testing.T) {
	const stored = `{"aliases":[{"name":"quick-chat","model":"llama3.2:1b"}]}`

	cases := []struct {
		name    string
		request string
		answer  any
	}{
		{
			name:    "a disable is answered with the endpoint down",
			request: `{"enabled":false}`,
			answer:  RouterStatusResult{Running: false, Port: 0},
		},
		{
			name:    "an enable refused for the port never came up",
			request: `{"enabled":true,"port":6111}`,
			answer:  &RPCError{Code: -32011, Message: "cannot bind 127.0.0.1:6111: address already in use"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, client := newClientFacingBroker(t)
			b.setSettings(newAnsweringSettingsWorker(t, stored))
			frames := addAnsweringProxyWithFrames(t, b, ollamaProxyProfile, tc.answer)

			callRouterSetEnabled(t, b, client, tc.request)

			enable, ok := nextFrameWithin(t, frames, hopWindow)
			require.True(t, ok, "the write never reached the child")
			require.Equal(t, "router/set-enabled", enable.Method)
			requireStreamStaysQuiet(t, frames,
				"no endpoint came up, so no alias document should have followed")
		})
	}
}
