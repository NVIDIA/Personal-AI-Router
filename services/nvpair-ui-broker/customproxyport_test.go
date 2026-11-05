// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Contract for the Custom facade's configurable port (customproxyport.go).
//
// The plane is deliberately narrow — two methods, one number — so everything
// interesting here is about whose number it is and what a refusal costs:
//
//   - The proxy child owns the port. A read answers the live listen port, and
//     when nothing is listening it answers the port the next bring-up would use,
//     labelled as such. A write only takes effect as a rebind; the broker keeps
//     no port of its own, which is why a set with no live facade refuses rather
//     than filing a plan away — a stored plan would outrank the proxy's own
//     persisted port on the next start, the one asymmetry that could let a good
//     choice lose to a bad one.
//   - A taken port is refused before the bind, checked against this node's whole
//     picture: a sibling engine's proxy or managed-backend port, a port a running
//     engine holds right now, and a port one of this node's own Custom endpoints
//     forwards to. That last one is custom/add's self-forward loop seen from the
//     other end.
//   - Both methods answer for the addressed node, and the peer path is transport:
//     the same checks run on the owner's machine, with refusal text kept verbatim
//     because the person across the network needs to know which port to type
//     instead.

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/appdir"
	"nvpair-shared/engines"
)

// --- fixtures -----------------------------------------------------------------

// customFacadePair builds a fake unified proxy hosting one Custom facade.
// listening is the port that facade reports ready on (0 keeps the process up
// with no facade, which is how a facade that never announced itself arrives).
// bind decides what a set-port request does: nil binds the requested port,
// otherwise the returned port is what the facade reports taking and a non-empty
// failure is the RPC error it answers with instead. Frames the proxy receives
// are recorded, so a test can tell "rebounded" from "refused before the bind".
func customFacadePair(t *testing.T, listening int, bind func(want int) (bound int, failure string)) (*proxyProcess, chan bridgeFrame) {
	t.Helper()
	brokerSide, serverSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = serverSide.Close()
	})
	proc := &proxyProcess{peer: NewPeer(NewCodec(brokerSide)), facadeState: map[string]proxyFacadeState{}}
	if listening > 0 {
		proc.facadeState[customProxyProfile.Name] = proxyFacadeState{ready: true, port: listening}
	}
	go proc.peer.Serve(nil, nil)

	seen := make(chan bridgeFrame, 8)
	go func() {
		codec := NewCodec(serverSide)
		for {
			msg, err := codec.Read()
			if !assertRPCRead(t, err) {
				return
			}
			if !msg.IsRequest() {
				continue
			}
			select {
			case seen <- bridgeFrame{method: msg.Method, params: string(msg.Params)}:
			case <-time.After(2 * time.Second):
			}
			engine, method := engines.SplitAddressedMethod(msg.Method)
			if engine != customProxyProfile.Name || method != "set-port" {
				_ = codec.RespondError(msg.ID, -32601, "unknown method "+msg.Method)
				continue
			}
			var p struct {
				Port int `json:"port"`
			}
			if json.Unmarshal(msg.Params, &p) != nil {
				_ = codec.RespondError(msg.ID, -32602, "port must be between 1 and 65535")
				continue
			}
			bound, failure := p.Port, ""
			if bind != nil {
				bound, failure = bind(p.Port)
			}
			if failure != "" {
				_ = codec.RespondError(msg.ID, codeFacadeBindFailed, failure)
				continue
			}
			if bound > 0 {
				proc.readyMu.Lock()
				proc.facadeState[engine] = proxyFacadeState{ready: true, port: bound}
				proc.readyMu.Unlock()
			}
			// The child's real answer is its ready payload: the port it
			// actually took, which is what the broker's confirm check reads.
			_ = codec.Respond(msg.ID, proxyReadyParams{Version: "test", Port: bound})
		}
	}()
	return proc, seen
}

// putCustomFacadeOn points the broker's Custom facade handle at that fake.
func putCustomFacadeOn(t *testing.T, b *Broker, listening int, bind func(int) (int, string)) chan bridgeFrame {
	t.Helper()
	proc, seen := customFacadePair(t, listening, bind)
	b.setEngineProxyHandle(customProxyProfile, proc)
	return seen
}

// customPortTestBroker is the config-plane fixture with the per-user config dir
// pointed at a temp dir: the Custom facade's persisted port lives in that dir,
// so a test that plants or checks one must not read a developer's real file.
func customPortTestBroker(t *testing.T) (*Broker, *Codec, string) {
	t.Helper()
	isolateOllamaHostTestConfig(t)
	return customTestBroker(t, nil)
}

// plantPersistedCustomPort writes the facade's persisted-port file, the shape
// the proxy restores from when it starts.
func plantPersistedCustomPort(t *testing.T, port int) {
	t.Helper()
	path, err := appdir.Path(customProxyProfile.PortFile)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"port":`+strconv.Itoa(port)+`}`), 0o600))
}

// runningEnginesAnswer is the engine-manager reply that says which engines are
// up and on which ports right now.
func runningEnginesAnswer(ports map[string]int) map[string]any {
	running := make([]map[string]any, 0, len(ports))
	for engine, port := range ports {
		running = append(running, map[string]any{"engine": engine, "running": true, "port": port})
	}
	return map[string]any{"engines": running}
}

// putRunningEngines wires an engine-manager that reports each named engine as
// running on its port — the inventory runningEnginePorts asks for, so a test can
// put a live engine on a port the static engine table knows nothing about.
func putRunningEngines(t *testing.T, b *Broker, ports map[string]int) {
	t.Helper()
	worker, codec := newTestRPCWorkerPipe(t)
	b.setEngineMgr(worker)
	go func() {
		for {
			msg, err := codec.Read()
			if !assertRPCRead(t, err) {
				return
			}
			if !msg.IsRequest() {
				continue
			}
			_ = codec.Respond(msg.ID, runningEnginesAnswer(ports))
		}
	}()
}

// customPortRelayLink is the owner-half fixture: one fake engine-manager that
// both carries custom:relay-request frames and answers engine:get-installed.
// Those share a link for real, and they share one here on purpose — a port
// check answered inline would wait out its timeout, because its answer can only
// arrive on the pump the check is blocking.
type customPortRelayLink struct {
	em      *Codec
	replies chan customRelayReply
}

func newCustomPortRelayLink(t *testing.T, b *Broker, runningPorts map[string]int) *customPortRelayLink {
	t.Helper()
	brokerSide, emSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = emSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	link := &customPortRelayLink{em: NewCodec(emSide), replies: make(chan customRelayReply, 4)}
	// One reader for the whole link: the port check's answer and the relay
	// reply both land here, and two readers would race for them.
	go func() {
		for {
			msg, err := link.em.Read()
			if !assertRPCRead(t, err) {
				return
			}
			if msg.IsRequest() && msg.Method == "engine:get-installed" {
				_ = link.em.Respond(msg.ID, runningEnginesAnswer(runningPorts))
				continue
			}
			if msg.Method != "custom:relay-reply" {
				continue
			}
			var reply customRelayReply
			if json.Unmarshal(msg.Params, &reply) != nil {
				continue
			}
			select {
			case link.replies <- reply:
			case <-time.After(3 * time.Second):
				return
			}
		}
	}()
	b.setEngineMgr(worker)
	go worker.peer.Serve(nil, b.forwardEngineNotification)
	return link
}

// inject drives one peer request into this node's broker the way its own
// engine-manager does after a pinned peer POSTs to the custom surface.
func (l *customPortRelayLink) inject(t *testing.T, frame string) {
	t.Helper()
	sent := make(chan error, 1)
	go func() { sent <- l.em.Notify("custom:relay-request", json.RawMessage(frame)) }()
	select {
	case err := <-sent:
		require.NoError(t, err, "injecting custom:relay-request")
	case <-time.After(3 * time.Second):
		require.FailNow(t, "custom:relay-request never consumed")
	}
}

func (l *customPortRelayLink) reply(t *testing.T) customRelayReply {
	t.Helper()
	select {
	case reply := <-l.replies:
		return reply
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no custom:relay-reply arrived")
		return customRelayReply{}
	}
}

func takeRebindFrame(t *testing.T, seen chan bridgeFrame) bridgeFrame {
	t.Helper()
	select {
	case frame := <-seen:
		return frame
	case <-time.After(3 * time.Second):
		require.FailNow(t, "the facade was never asked to move")
		return bridgeFrame{}
	}
}

func assertNoRebind(t *testing.T, seen chan bridgeFrame) {
	t.Helper()
	select {
	case frame := <-seen:
		require.FailNow(t, "a refused port still reached the proxy: %v", frame)
	case <-time.After(200 * time.Millisecond):
	}
}

// --- read: whose number is it -------------------------------------------------

func TestCustomGetPort_LiveFacadeAnswersItsListenPort(t *testing.T) {
	b, client, _ := customPortTestBroker(t)
	putCustomFacadeOn(t, b, 8123, nil)

	resp := customCall(t, b, client, "custom/get-port", `{}`)
	require.Nil(t, resp.Error, "port read")
	assert.JSONEq(t, `{"port":8123,"running":true}`, string(resp.Result))
}

func TestCustomGetPort_NoFacadeAnswersStockPortAsNotListening(t *testing.T) {
	b, client, _ := customPortTestBroker(t)

	// No proxy process at all. The answer is still a port — the one a facade
	// would be planned onto — because the settings section has to say something,
	// but running:false keeps it from advertising a listener that answers
	// nothing.
	resp := customCall(t, b, client, "custom/get-port", `{}`)
	require.Nil(t, resp.Error, "port read without a proxy")
	assert.JSONEq(t, `{"port":8000,"running":false}`, string(resp.Result))
}

func TestCustomGetPort_PersistedPortAnswersAfterARestart(t *testing.T) {
	b, client, _ := customPortTestBroker(t)
	plantPersistedCustomPort(t, 8137)

	resp := customCall(t, b, client, "custom/get-port", `{}`)
	require.Nil(t, resp.Error, "port read")
	assert.JSONEq(t, `{"port":8137,"running":false}`, string(resp.Result),
		"a port the operator chose comes back where they left it")
}

func TestCustomGetPort_LostBindRaceOutranksThePersistedPort(t *testing.T) {
	b, client, _ := customPortTestBroker(t)
	plantPersistedCustomPort(t, 8137)
	// The persisted file is exactly what a fallback is chosen to replace: the
	// bind there lost, so the next bring-up plans elsewhere.
	b.engineProxy(customProxyProfile).startupPort.Store(8141)
	putCustomFacadeOn(t, b, 0, nil)

	resp := customCall(t, b, client, "custom/get-port", `{}`)
	require.Nil(t, resp.Error, "port read")
	assert.JSONEq(t, `{"port":8141,"running":false}`, string(resp.Result),
		"a planned fallback outranks the file it was chosen to replace")
}

// --- write: the rebind --------------------------------------------------------

func TestCustomSetPort_TypedPortMovesTheFacadeAndConfirms(t *testing.T) {
	b, client, _ := customPortTestBroker(t)
	seen := putCustomFacadeOn(t, b, 8123, nil)

	resp := customCall(t, b, client, "custom/set-port", `{"port":8137}`)
	require.Nil(t, resp.Error, "port move")
	assert.JSONEq(t, `{"port":8137,"running":true}`, string(resp.Result))

	frame := takeRebindFrame(t, seen)
	require.Equal(t, "custom:set-port", frame.method, "the rebind must be addressed to the Custom facade")
	assert.JSONEq(t, `{"port":8137}`, frame.params)

	after := customCall(t, b, client, "custom/get-port", `{}`)
	assert.JSONEq(t, `{"port":8137,"running":true}`, string(after.Result),
		"a later read reports the moved port, not the one typed")
}

func TestCustomSetPort_AlreadyOnThatPortSucceedsWithoutRebinding(t *testing.T) {
	b, client, _ := customPortTestBroker(t)
	seen := putCustomFacadeOn(t, b, 8123, nil)

	resp := customCall(t, b, client, "custom/set-port", `{"port":8123}`)
	require.Nil(t, resp.Error, "applying the port already in use")
	assertNoRebind(t, seen)
}

func TestCustomSetPort_WithoutAFacadeRefusesInsteadOfStoringAPlan(t *testing.T) {
	b, client, _ := customPortTestBroker(t)

	resp := customCall(t, b, client, "custom/set-port", `{"port":8137}`)
	require.NotNil(t, resp.Error, "a move with nothing to rebind must not read as applied")
	assert.Contains(t, resp.Error.Message, "not running on this node")

	path, err := appdir.Path(customProxyProfile.PortFile)
	require.NoError(t, err)
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr),
		"the proxy's persisted port stays the only stored authority for this number")
}

func TestCustomSetPort_RefusesAPortAnotherEngineOwns(t *testing.T) {
	t.Run("a sibling engine's proxy port stays reserved", func(t *testing.T) {
		b, client, _ := customPortTestBroker(t)
		seen := putCustomFacadeOn(t, b, 8123, nil)

		// 1234 is LM Studio's client-facing port. Its proxy is not running
		// here, and that is the point: the sibling's saved configuration
		// outlives this process, so a facade parked on it meets a conflict on
		// the next start with nothing pointing at the cause.
		resp := customCall(t, b, client, "custom/set-port", `{"port":1234}`)
		require.NotNil(t, resp.Error, "sibling proxy port taken")
		assert.Contains(t, resp.Error.Message, "already in use on this node")
		assert.Contains(t, resp.Error.Message, "LM Studio")
		assertNoRebind(t, seen)
	})

	t.Run("a running engine on an unlisted port is found live", func(t *testing.T) {
		b, client, _ := customPortTestBroker(t)
		seen := putCustomFacadeOn(t, b, 8123, nil)
		// 9292 appears in no engine table: a relocated or externally installed
		// llama.cpp is only visible by asking what is running.
		putRunningEngines(t, b, map[string]int{"llamacpp": 9292})

		resp := customCall(t, b, client, "custom/set-port", `{"port":9292}`)
		require.NotNil(t, resp.Error, "a port a live engine answers on must be refused")
		assert.Contains(t, resp.Error.Message, "inference engine is running on that port")
		assertNoRebind(t, seen)
	})
}

func TestCustomSetPort_RefusesThePortItsOwnEndpointForwardsTo(t *testing.T) {
	b, client, path := customPortTestBroker(t)
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{ID: "lab", Name: "lab", BaseURL: "http://127.0.0.1:8137"},
	}))
	seen := putCustomFacadeOn(t, b, 8123, nil)

	// This is custom/add's self-forward refusal seen from the other end: an
	// endpoint already configured on the port the facade is about to take would
	// leave the proxy forwarding to itself.
	resp := customCall(t, b, client, "custom/set-port", `{"port":8137}`)
	require.NotNil(t, resp.Error, "the facade must not be moved onto its own endpoint")
	assert.Contains(t, resp.Error.Message, "forwards to that port")
	assert.Contains(t, resp.Error.Message, "8137", "the refusal has to name the port it refused")
	assertNoRebind(t, seen)

	// A non-loopback endpoint on the same port number is somebody else's
	// machine: no loop, so nothing to refuse.
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{ID: "remote", Name: "remote", BaseURL: "http://10.9.8.7:8137"},
	}))
	resp = customCall(t, b, client, "custom/set-port", `{"port":8137}`)
	require.Nil(t, resp.Error, "an endpoint across the network is not a self-forward")
}

func TestCustomSetPort_BindProblemKeepsTheProxyDetail(t *testing.T) {
	t.Run("bind lost", func(t *testing.T) {
		b, client, _ := customPortTestBroker(t)
		putCustomFacadeOn(t, b, 8123, func(int) (int, string) {
			return 0, "address already in use"
		})

		resp := customCall(t, b, client, "custom/set-port", `{"port":8137}`)
		require.NotNil(t, resp.Error, "a lost bind is not a port change")
		assert.Contains(t, resp.Error.Message, "could not listen on port 8137")
		assert.Contains(t, resp.Error.Message, "address already in use",
			"the child's own reason is the part the operator can act on")

		after := customCall(t, b, client, "custom/get-port", `{}`)
		assert.JSONEq(t, `{"port":8123,"running":true}`, string(after.Result),
			"a failed move leaves the working listener where it was")
	})

	t.Run("answered with a different port", func(t *testing.T) {
		b, client, _ := customPortTestBroker(t)
		// The facade replies, just not on 8137. The broker must not record a
		// port nothing confirmed: that is how a half-moved facade ends up
		// advertised.
		putCustomFacadeOn(t, b, 8123, func(int) (int, string) { return 8123, "" })

		resp := customCall(t, b, client, "custom/set-port", `{"port":8137}`)
		require.NotNil(t, resp.Error, "an unconfirmed port must not read as applied")
		assert.Contains(t, resp.Error.Message, "did not confirm port 8137")
	})
}

func TestCustomSetPort_RejectsPortsThatWereNeverTyped(t *testing.T) {
	b, client, _ := customPortTestBroker(t)
	seen := putCustomFacadeOn(t, b, 8123, nil)

	// Two refusals, because two things can go wrong before a port is a port. A
	// number outside the range reaches moveCustomFacadePort and is refused as a
	// port; a body that cannot even yield a number is refused as bad input. Both
	// have to refuse: a silently zeroed port would read as "applied" and move
	// the facade to whatever the next free port happened to be.
	for _, input := range []struct{ params, want string }{
		{`{}`, "between 1 and 65535"},
		{`{"port":0}`, "between 1 and 65535"},
		{`{"port":70000}`, "between 1 and 65535"},
		{`{"port":"8137"}`, "expected {\"port\": <int>}"},
	} {
		resp := customCall(t, b, client, "custom/set-port", input.params)
		require.NotNil(t, resp.Error, "refused input: "+input.params)
		assert.Contains(t, resp.Error.Message, input.want, "input: "+input.params)
	}
	assertNoRebind(t, seen)
}

// --- peer path ----------------------------------------------------------------

func TestCustomPortPeerAddressed_RelayedVerbatimAndFailsWithoutEngineManager(t *testing.T) {
	seen := make(chan proberCall, 4)
	b, client, _ := customTestBroker(t, nil)
	b.setEngineMgr(newCustomProberFaker(t, seen))

	for _, pair := range [][2]string{
		{"custom/get-port", "custom:remote-get-port"},
		{"custom/set-port", "custom:remote-set-port"},
	} {
		resp := customCall(t, b, client, pair[0], `{"nodeId":"peer-uuid","port":8137}`)
		require.Nil(t, resp.Error, pair[0]+" relayed")
		assert.JSONEq(t, `{"ok":true}`, string(resp.Result), "the peer's answer passes through")

		select {
		case call := <-seen:
			assert.Equal(t, pair[1], call.method, "remote carrier method for "+pair[0])
			assert.Contains(t, call.params, `"nodeId":"peer-uuid"`, "params ride verbatim")
		case <-time.After(3 * time.Second):
			require.FailNow(t, "%s never reached the engine-manager", pair[0])
		}
	}

	// A peer-addressed read with no carrier must fail rather than answer this
	// node's port: the section would render a stranger's proxy as its own.
	b2, client2, _ := customTestBroker(t, nil)
	resp := customCall(t, b2, client2, "custom/get-port", `{"nodeId":"peer-uuid"}`)
	require.NotNil(t, resp.Error, "a peer-addressed read with no engine-manager")
	assert.Contains(t, resp.Error.Message, "custom endpoints unavailable")
}

func TestCustomPortRelay_OwnerAnswersTheSamePlaneALocalEditUses(t *testing.T) {
	t.Run("read reports this node's facade", func(t *testing.T) {
		b, _, _ := customPortTestBroker(t)
		putCustomFacadeOn(t, b, 8123, nil)
		link := newCustomPortRelayLink(t, b, nil)

		link.inject(t, `{"id":"op1","op":"get-port","request":{},"caller":"urn:nvpair:node:peer-a"}`)

		reply := link.reply(t)
		require.Empty(t, reply.Error, "relay port read")
		assert.JSONEq(t, `{"port":8123,"running":true}`, string(reply.Result),
			"a peer reads the port it would have to route to, not ours")
	})

	t.Run("move rebinds here", func(t *testing.T) {
		b, _, _ := customPortTestBroker(t)
		seen := putCustomFacadeOn(t, b, 8123, nil)
		link := newCustomPortRelayLink(t, b, nil)

		link.inject(t, `{"id":"op1","op":"set-port","request":{"port":8137},"caller":"urn:nvpair:node:peer-a"}`)

		reply := link.reply(t)
		require.Empty(t, reply.Error, "relay port move")
		assert.JSONEq(t, `{"port":8137,"running":true}`, string(reply.Result))

		frame := takeRebindFrame(t, seen)
		assert.Equal(t, "custom:set-port", frame.method,
			"a peer-driven move still runs against this node's proxy")
	})

	t.Run("refusals keep their detail", func(t *testing.T) {
		b, _, _ := customPortTestBroker(t)
		link := newCustomPortRelayLink(t, b, map[string]int{"llamacpp": 9292})

		// No facade on this node, then an engine on the typed port: both
		// refusals name what is in the way. Unlike a persistence failure they
		// are not scrubbed — they quote no local path, and a port plus who
		// holds it is what the person across the network needs.
		link.inject(t, `{"id":"op1","op":"set-port","request":{"port":8137},"caller":"urn:nvpair:node:peer-a"}`)
		reply := link.reply(t)
		require.NotEmpty(t, reply.Error, "a move with nothing to rebind must refuse")
		assert.Contains(t, reply.Error, "not running on this node")

		putCustomFacadeOn(t, b, 8123, nil)
		link.inject(t, `{"id":"op2","op":"set-port","request":{"port":9292},"caller":"urn:nvpair:node:peer-a"}`)
		reply = link.reply(t)
		require.NotEmpty(t, reply.Error, "a port this node's engine holds must be refused")
		assert.Contains(t, reply.Error, "inference engine is running on that port")

		link.inject(t, `{"id":"op3","op":"set-port","request":{},"caller":"urn:nvpair:node:peer-a"}`)
		reply = link.reply(t)
		require.NotEmpty(t, reply.Error, "a portless move is a local mistake wherever it arrives")
		assert.Contains(t, reply.Error, "explicit port between 1 and 65535")
	})
}
