// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Engine-manager-side contract for the remote custom-endpoint carrier.
//
// The carrier's shape (correlated relay, pin-gated routes, a unified
// offline-or-unsupported answer) only earns its keep if the pieces actually
// meet, so the main harness is the whole chain: a pinned peer POSTs to the
// real mux, the route hands the op to the real customRelay, the reply comes
// back over a pipe through the real read loop and handleMessage. A broken id
// correlation or a swallowed notification would surface here as a timeout,
// not hide behind a direct reply() call.
//
// The key policy is pinned too: the add body carries it across both links by
// design (request plumbing, not an event), so the assertions require it in
// the relay frame and absent from every response body.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

// customTestKey stands in for a real endpoint key. It must appear exactly
// twice across this whole file's assertions: once allowed (the outbound
// request body), once forbidden (every response body).
const customTestKey = "sk-custom-secret-not-for-wires"

// customCarrier is one node's full custom carrier: Manager + control server +
// a fake broker on the far end of the stdio pipe that answers every
// custom:relay-request through the real read loop.
type customCarrier struct {
	server   *httptest.Server
	mgr      *Manager
	exec     *Executor
	pinned   *http.Client // peer "b", pinned here and pinning us
	stranger *http.Client // mesh-valid cert this node does not pin
	frames   chan customRelayFrame
}

func newCustomCarrier(t *testing.T, answer func(op, id string) (result string, errText string)) *customCarrier {
	t.Helper()
	aDir, certA := settingsMesh(t, "a")
	bDir, certB := settingsMesh(t, "b")
	sDir, _ := settingsMesh(t, "stranger")
	settingsPin(t, aDir, "b", certB)
	settingsPin(t, bDir, "a", certA)
	settingsPin(t, sDir, "a", certA)

	mesh := clustertrust.Open(aDir)
	emEnd, brokerEnd := net.Pipe()
	t.Cleanup(func() {
		_ = emEnd.Close()
		_ = brokerEnd.Close()
	})
	exec := &Executor{progress: newProgressHub()}
	mgr := NewManager(NewCodec(emEnd), exec, mesh)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The em half of the read loop, mirroring Manager.readLoop so dispatch
	// through handleMessage (settings hook first, custom hook second) is
	// under test, not just the handlers.
	go func() {
		for {
			msg, err := mgr.codec.Read()
			if err != nil {
				return
			}
			mgr.handleMessage(ctx, msg)
		}
	}()

	fakeBroker := NewCodec(brokerEnd)
	frames := make(chan customRelayFrame, 8)
	go func() {
		for {
			msg, err := fakeBroker.Read()
			if err != nil {
				return
			}
			if msg.Method != "custom:relay-request" {
				continue
			}
			var frame customRelayFrame
			if json.Unmarshal(msg.Params, &frame) != nil {
				continue
			}
			frames <- frame
			result, errText := answer(frame.Op, frame.ID)
			body := map[string]any{"id": frame.ID}
			if errText != "" {
				body["error"] = errText
			} else {
				body["result"] = json.RawMessage(result)
			}
			_ = fakeBroker.Notify("custom:relay-reply", body)
		}
	}()

	server := httptest.NewUnstartedServer((&controlServer{exec: exec, mesh: mesh}).mux())
	server.TLS = mesh.ServerTLSConfig()
	server.StartTLS()
	t.Cleanup(server.Close)

	clientTo := func(dir string) *http.Client {
		config, ok := clustertrust.Open(dir).ClientTLSConfig("a")
		require.True(t, ok, "pin unavailable for client dial")
		tr := &http.Transport{TLSClientConfig: config}
		t.Cleanup(tr.CloseIdleConnections)
		return &http.Client{Transport: tr, Timeout: 15 * time.Second}
	}
	return &customCarrier{
		server:   server,
		mgr:      mgr,
		exec:     exec,
		pinned:   clientTo(bDir),
		stranger: clientTo(sDir),
		frames:   frames,
	}
}

func (h *customCarrier) post(t *testing.T, client *http.Client, op, body string) (int, string) {
	t.Helper()
	resp, err := client.Post(h.server.URL+customPath+op, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

// --- Target half: route + relay ---------------------------------------------

func TestCustomCarrierPinnedAdd_SettlesThroughBrokerAndKeepsTheKeyOffTheResponse(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{"id":"lab:8000","keySet":true}`, ""
	})

	status, body := h.post(t, h.pinned, "add", `{"url":"http://10.9.8.7","port":8000,"apiKey":"`+customTestKey+`"}`)

	require.Equal(t, 200, status, "pinned add refused: %s", body)
	assert.Contains(t, body, `"keySet":true`)
	assert.NotContains(t, body, customTestKey, "the key must never ride the response path")

	select {
	case frame := <-h.frames:
		require.Equal(t, "add", frame.Op)
		assert.Equal(t, "b", frame.Caller, "caller identity must reach the audit trail")
		// The request carrying the key is the one sanctioned hop; pin it so
		// a future refactor cannot drop it silently.
		assert.Contains(t, string(frame.Request), customTestKey, "relay frame lost the add payload")
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no custom:relay-request reached the fake broker")
	}

	h.mgr.customRelay.mu.Lock()
	assert.Empty(t, h.mgr.customRelay.pending, "relay leaked a waiter")
	h.mgr.customRelay.mu.Unlock()
}

func TestCustomCarrierPersistenceFailure_AnswersScrubbedTextOnly(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		// Exactly what the broker sends: the scrubbed cross-network form.
		return "", "custom-endpoints persistence failed on this node"
	})

	status, body := h.post(t, h.pinned, "add", `{"url":"http://10.9.8.7","port":8000,"apiKey":"`+customTestKey+`"}`)

	require.Equal(t, 409, status, "failure answered %d: %s", status, body)
	assert.Contains(t, body, "custom-endpoints persistence failed on this node")
	assert.NotContains(t, body, customTestKey)
	assert.NotContains(t, body, t.TempDir(), "local paths must not cross the network")
}

func TestCustomCarrierUnpinnedCaller_TurnedAwayBeforeAnyWork(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{}`, ""
	})

	status, _ := h.post(t, h.stranger, "add", `{"url":"http://10.9.8.7","port":8000}`)

	require.Equal(t, 403, status, "unpinned caller reached the custom plane")
	select {
	case frame := <-h.frames:
		require.FailNow(t, "unpinned request still relayed: %v", frame)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCustomCarrierRequestShape_GuardsFireBeforeTheRelay(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{}`, ""
	})

	t.Run("smuggled nodeId rejected", func(t *testing.T) {
		status, body := h.post(t, h.pinned, "remove", `{"nodeId":"another-node","id":"lab:8000"}`)
		require.Equal(t, 400, status, "addressing field accepted: %s", body)
	})
	t.Run("trailing second document rejected", func(t *testing.T) {
		status, _ := h.post(t, h.pinned, "list", `{} {}`)
		require.Equal(t, 400, status)
	})
	t.Run("unknown verb rejected", func(t *testing.T) {
		status, _ := h.post(t, h.pinned, "list", `[1,2]`)
		require.Equal(t, 400, status)
	})
	select {
	case frame := <-h.frames:
		require.FailNow(t, "rejected shapes still relayed: %v", frame)
	default:
	}
}

func TestCustomCarrierGuards_MethodAndMissingCoordinator(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{}`, ""
	})

	resp, err := h.pinned.Get(h.server.URL + customPath + "list")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 405, resp.StatusCode, "GET reached the custom surface")

	// A control server whose executor never met a Manager (no broker link)
	// must say so instead of pretending the peer's config is empty.
	h.exec.customParent = nil
	status, body := h.post(t, h.pinned, "list", `{}`)
	require.Equal(t, 503, status)
	assert.Contains(t, body, "coordinator unavailable")
}

func TestCustomCarrierBeyondCap_AnswersBusyWithoutRelaying(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{}`, ""
	})
	// Fill the map the same lazy way call() would: an empty relay starts
	// with a nil pending map, and a nil map at the cap means "zero in
	// flight", not "blocked".
	h.mgr.customRelay.mu.Lock()
	h.mgr.customRelay.pending = make(map[string]chan customReply, customRelayPendingLimit)
	for i := 0; i < customRelayPendingLimit; i++ {
		h.mgr.customRelay.pending["fill-"+strconv.Itoa(i)] = make(chan customReply, 1)
	}
	h.mgr.customRelay.mu.Unlock()

	status, body := h.post(t, h.pinned, "add", `{"url":"http://10.9.8.7","port":8000,"apiKey":"`+customTestKey+`"}`)

	require.Equal(t, 409, status)
	assert.Contains(t, body, "too many custom endpoint requests")
	assert.NotContains(t, body, customTestKey)
	select {
	case frame := <-h.frames:
		require.FailNow(t, "capped request still relayed: %v", frame)
	default:
	}
}

// --- Relay unit shape ---------------------------------------------------------

func TestCustomRelay_CorrelatesRepliesAndIgnoresStrayOnes(t *testing.T) {
	sent := make(chan customRelayFrame, 4)
	relay := &customRelay{send: func(method string, value any) error {
		require.Equal(t, "custom:relay-request", method)
		data, err := json.Marshal(value)
		require.NoError(t, err)
		var frame customRelayFrame
		require.NoError(t, json.Unmarshal(data, &frame))
		sent <- frame
		return nil
	}}

	results := make(chan string, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) {
			result, err := relay.call(context.Background(), "list", json.RawMessage(`{}`), "")
			if err != nil {
				results <- "error"
				return
			}
			results <- string(result)
		}(name)
	}
	first, second := <-sent, <-sent
	// Answer in reverse order to expose accidental FIFO matching.
	for _, frame := range []customRelayFrame{second, first} {
		data, err := json.Marshal(customReply{ID: frame.ID, Result: json.RawMessage(`"` + frame.Op + "-" + frame.ID + `"`)})
		require.NoError(t, err)
		relay.reply(data)
	}
	got := map[string]bool{<-results: true, <-results: true}
	require.Len(t, got, 2, "relay collapsed two results into one")

	// A reply for a settled or unknown id must return quietly — it arrives
	// on the read loop, and a block there stops every other method.
	relay.reply([]byte(`{"id":"nobody","result":123}`))
	relay.reply([]byte(`not json`))
	relay.mu.Lock()
	assert.Empty(t, relay.pending, "relay leaked waiters")
	relay.mu.Unlock()
}

// --- Origin half --------------------------------------------------------------

func TestRunCustomRemote_UnknownPeer_AnswersUnifiedUnavailable(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage(`"1"`)
	m.runCustomRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "custom:remote-list", Params: json.RawMessage(`{"nodeId":"ghost"}`)})
	mustContain(t, out.String(), "does not support custom endpoint management")
}

func TestRunCustomRemote_Unclustered_KeepsPairingRefusal(t *testing.T) {
	var out bytes.Buffer
	// The peer is discovered, but this node holds no cluster identity, so
	// the dial cannot be built. That refusal keeps its own words: "pair
	// with that device first" is a different user action than "update it".
	mesh := clustertrust.Open(t.TempDir())
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, mesh)
	m.peers.set([]noderec.DirectoryNode{{
		HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42", ClusterUUID: "cuuid-b",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}})
	id := json.RawMessage(`"1"`)
	m.runCustomRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "custom:remote-add",
		Params: json.RawMessage(`{"nodeId":"uuid-b","url":"http://10.9.8.7","port":8000,"apiKey":"` + customTestKey + `"}`)})
	mustContain(t, out.String(), "not clustered")
	assert.NotContains(t, out.String(), customTestKey, "the stdio error reply must not repeat the key")
}

func TestRunCustomRemote_AddWithoutUsableTarget_FailsLocallyWithoutDialing(t *testing.T) {
	var out bytes.Buffer
	mesh := clustertrust.Open(t.TempDir())
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, mesh)
	m.peers.set([]noderec.DirectoryNode{{
		HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42", ClusterUUID: "cuuid-b",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}})

	// Port is mandatory everywhere (D2), so a portless remote add is a
	// local shape error, not a dial attempt that would strand the origin's
	// proxy pointing at an unspecified port.
	id := json.RawMessage(`"1"`)
	m.runCustomRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "custom:remote-add", Params: json.RawMessage(`{"nodeId":"uuid-b","url":"http://10.9.8.7"}`)})
	mustContain(t, out.String(), "requires a url and an explicit port")

	id2 := json.RawMessage(`"2"`)
	m.runCustomRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id2,
		Method: "custom:remote-remove", Params: json.RawMessage(`{"nodeId":"uuid-b"}`)})
	mustContain(t, out.String(), "remove requires an id")
}

func TestPostCustom_OfflineOrOlderPeer_FoldsIntoUnifiedUnavailable(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	t.Cleanup(notFound.Close)
	client := &remoteClient{http: notFound.Client(), base: notFound.URL}
	_, err := client.postCustom(context.Background(), customPath+"list", struct{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), customUnavailable, "a peer without the routes must read as unsupported")

	// The facade-port pair folds the same way. A peer predating this release
	// answers 404 for get-port, and the section on this node has to say
	// "update the other device" rather than show a port that only exists
	// here — which is the whole reason the port read is not a local default.
	_, err = client.postCustom(context.Background(), customPath+"get-port", struct{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), customUnavailable, "a peer without the port routes must read as unsupported")

	// Offline reads the same way: connection refused, no peer to ask.
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	goneURL := gone.URL
	gone.Close()
	dialer := &http.Client{Timeout: 3 * time.Second}
	client2 := &remoteClient{http: dialer, base: goneURL}
	_, err = client2.postCustom(context.Background(), customPath+"add", customAddBody{URL: "http://10.9.8.7", Port: 8000})
	require.Error(t, err)
	assert.Contains(t, err.Error(), customUnavailable)
}

func TestPostCustom_PeerAnswersError_KeepsTheDetail(t *testing.T) {
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "custom-endpoints persistence failed on this node", http.StatusConflict)
	}))
	t.Cleanup(refused.Close)
	client := &remoteClient{http: refused.Client(), base: refused.URL}
	_, err := client.postCustom(context.Background(), customPath+"add", customAddBody{URL: "http://10.9.8.7", Port: 8000, APIKey: customTestKey})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 409")
	assert.Contains(t, err.Error(), "custom-endpoints persistence failed on this node")
	assert.NotContains(t, err.Error(), customTestKey)
}

func TestHandleCustomMessage_ClaimsRelayReplyAndLeavesModelPushAlone(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)

	ch := make(chan customReply, 1)
	m.customRelay.mu.Lock()
	m.customRelay.pending = make(map[string]chan customReply)
	m.customRelay.pending["op-1"] = ch
	m.customRelay.mu.Unlock()

	reply := &Message{JSONRPC: "2.0", Method: "custom:relay-reply",
		Params: json.RawMessage(`{"id":"op-1","result":{"removed":true}}`)}
	require.True(t, m.handleCustomMessage(context.Background(), reply), "relay reply must be claimed")
	settled, ok := <-ch
	require.True(t, ok, "reply never reached its waiter")
	assert.Contains(t, string(settled.Result), "removed")

	// custom:set-models is the model plane's notification; the carrier hook
	// must fall through or every broker push would vanish here.
	models := &Message{JSONRPC: "2.0", Method: "custom:set-models",
		Params: json.RawMessage(`{"models":["lab/x"]}`)}
	require.False(t, m.handleCustomMessage(context.Background(), models), "model push belongs to the model path")
}

// --- Two-node chain: origin rebuild → pinned dial → target routes ------------

// customChain is one whole round trip: node "self" runs a real Manager as
// the carrier's origin, node "peer" serves the real custom routes over
// pinned mTLS, and a recording customParent stands in for the peer's broker.
// The one-sided harnesses above can check a rebuilt body or a decoded route,
// but only this shape catches an origin that rebuilds fields the target then
// refuses — the drift Task 5's wire widening exists to prevent.
type customChain struct {
	mgr    *Manager
	client *Codec // test-UI side of the origin's stdio link
	calls  chan customChainCall
}

// customChainCall is one operation as the peer's custom surface received it:
// the relay op name and the body that survived strict decode.
type customChainCall struct {
	op   string
	body string
}

// answer decides what the peer's custom surface returns for one operation: a
// result body, or a failure, which the route answers as HTTP 409 exactly as a
// real broker refusal would. The second shape needs to be reachable from this
// harness because "the origin did not swallow the peer's refusal" is a
// whole-chain property, not a postCustom one.
func newCustomChain(t *testing.T, answer func(op, body string) (result string, failure string)) *customChain {
	t.Helper()
	selfDir := t.TempDir()
	peerDir := t.TempDir()
	clustertrusttest.WriteKeypair(t, selfDir, "uuid-self")
	clustertrusttest.WriteKeypair(t, peerDir, "uuid-peer")
	clustertrusttest.WriteAdmission(t, selfDir, "cluster-1", 1)
	clustertrusttest.WriteAdmission(t, peerDir, "cluster-1", 1)
	writePinFromCert(t, selfDir, "uuid-peer", filepath.Join(peerDir, "node.crt"))
	writePinFromCert(t, peerDir, "uuid-self", filepath.Join(selfDir, "node.crt"))
	selfMesh := clustertrust.Open(selfDir)
	peerMesh := clustertrust.Open(peerDir)
	require.True(t, selfMesh.Clustered(), "origin must read as clustered before it can dial out")

	calls := make(chan customChainCall, 8)
	peerExec := &Executor{progress: newProgressHub()}
	peerExec.customParent = func(_ context.Context, op string, body json.RawMessage, caller string) (json.RawMessage, error) {
		calls <- customChainCall{op: op, body: string(body)}
		result, failure := answer(op, string(body))
		if failure != "" {
			return nil, errors.New(failure)
		}
		return json.RawMessage(result), nil
	}
	srv := httptest.NewUnstartedServer((&controlServer{exec: peerExec, mesh: peerMesh}).mux())
	srv.TLS = peerMesh.ServerTLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	originEnd, uiEnd := net.Pipe()
	t.Cleanup(func() {
		_ = originEnd.Close()
		_ = uiEnd.Close()
	})
	mgr := NewManager(NewCodec(originEnd), &Executor{progress: newProgressHub()}, selfMesh)
	t.Cleanup(func() {
		mgr.remoteHTTP.CloseIdle()
		mgr.readyHTTP.CloseIdle()
	})
	mgr.peers.set([]noderec.DirectoryNode{{
		HostUUID: "uuid-peer", Name: "peerB", IP: "127.0.0.1", IPs: []string{host},
		ClusterUUID: "uuid-peer",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: port},
		},
	}})
	return &customChain{mgr: mgr, client: NewCodec(uiEnd), calls: calls}
}

// call drives one custom:remote-* request through handleMessage (so the
// dispatch table is under test, not just the runner) and waits for the stdio
// reply. nil means no answer ever came back.
func (h *customChain) call(t *testing.T, method string, params string) *Message {
	t.Helper()
	id := json.RawMessage(`"chain-1"`)
	replies := make(chan *Message, 1)
	go func() {
		msg, err := h.client.Read()
		if err != nil {
			replies <- nil
			return
		}
		replies <- msg
	}()
	h.mgr.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
	select {
	case msg := <-replies:
		return msg
	case <-time.After(10 * time.Second):
		return nil
	}
}

func (h *customChain) awaitCall(t *testing.T) customChainCall {
	t.Helper()
	select {
	case call := <-h.calls:
		return call
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no custom operation reached the peer's custom surface")
		return customChainCall{}
	}
}

func TestRunCustomRemote_AddEdit_RebuildCarriesReplaceFields(t *testing.T) {
	h := newCustomChain(t, func(op, body string) (string, string) {
		return `{"id":"lab:8000","keySet":false}`, ""
	})

	// A peer editing this node's endpoint: the edit fields must survive the
	// origin's body rebuild. Everything not rebuilt (nodeId above all) is
	// deliberately dropped — that field list is the whole carrier contract.
	reply := h.call(t, "custom:remote-add", `{"nodeId":"uuid-peer","url":"http://10.9.8.7:8000","port":8000,"apiKey":"`+customTestKey+`","replaceId":"custom:http://10.9.8.7:8000","keyAction":"clear"}`)

	peerCall := h.awaitCall(t)
	require.Equal(t, "add", peerCall.op)
	assert.Contains(t, peerCall.body, `"replaceId":"custom:http://10.9.8.7:8000"`, "peer rebuild lost the replace id")
	assert.Contains(t, peerCall.body, `"keyAction":"clear"`, "peer rebuild lost the key action")
	assert.Contains(t, peerCall.body, customTestKey, "relay lost the add payload")
	assert.NotContains(t, peerCall.body, "nodeId", "addressing must never ride into the HTTP body")

	require.NotNil(t, reply, "the peer edit never answered")
	require.Nil(t, reply.Error, "peer edit reply (%v)", reply)
	assert.Contains(t, string(reply.Result), `"keySet":false`)
	assert.NotContains(t, string(reply.Result), customTestKey, "the key must never ride the response path")
}

func TestRunCustomRemote_Refresh_ReachesTargetRefreshRoute(t *testing.T) {
	h := newCustomChain(t, func(op, body string) (string, string) {
		return `{"endpoints":[{"id":"lab:8000","healthy":true,"models":["model-x"]}]}`, ""
	})

	// Scoped refresh: the id addresses one entry on the owner's config and
	// must arrive intact; the owner's fresh snapshot must come back verbatim
	// (the desktop renders it straight).
	reply := h.call(t, "custom:remote-refresh", `{"nodeId":"uuid-peer","id":"custom:http://10.9.8.7:8000"}`)
	peerCall := h.awaitCall(t)
	require.Equal(t, "refresh", peerCall.op, "a scoped refresh never reached the peer's custom surface")
	assert.Contains(t, peerCall.body, `"id":"custom:http://10.9.8.7:8000"`, "refresh lost its scope")
	assert.NotContains(t, peerCall.body, "nodeId")
	require.NotNil(t, reply, "the peer refresh never answered")
	require.Nil(t, reply.Error, "peer refresh reply (%v)", reply)
	assert.Contains(t, string(reply.Result), "endpoints", "the owner's snapshot must reach the origin reply")

	// Unscoped refresh is legal (the page-level Refresh button) and sends a
	// body with no fields at all.
	reply = h.call(t, "custom:remote-refresh", `{"nodeId":"uuid-peer"}`)
	peerCall = h.awaitCall(t)
	require.Equal(t, "refresh", peerCall.op)
	assert.Equal(t, `{}`, peerCall.body, "an id-less refresh sends no fields")
	require.NotNil(t, reply)
	require.Nil(t, reply.Error, "unscoped refresh (%v)", reply)
}

func TestRunCustomRemote_Refresh_UnknownPeer_AnswersUnifiedUnavailable(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage(`"1"`)
	m.runCustomRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "custom:remote-refresh", Params: json.RawMessage(`{"nodeId":"ghost"}`)})
	// A peer without the refresh routes (or no peer at all) reads as "update
	// the other device", never as an empty endpoint list.
	mustContain(t, out.String(), "does not support custom endpoint management")
}

// --- Target half: refresh route and widened add decode ------------------------

func TestCustomCarrierRefresh_RoutesAsProbeCycle(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{"endpoints":[{"id":"lab:8000","healthy":true}]}`, ""
	})

	status, body := h.post(t, h.pinned, "refresh", `{}`)
	require.Equal(t, 200, status, "refresh route missing or refused: %s", body)
	assert.Contains(t, body, `"endpoints"`, "the broker's snapshot must reach the response verbatim")

	status, _ = h.post(t, h.pinned, "refresh", `{"id":"custom:http://10.9.8.7:8000"}`)
	require.Equal(t, 200, status, "scoped refresh refused: %s", body)

	// Empty body is the whole-scope refresh (same normalization list uses),
	// not a shape error.
	status, _ = h.post(t, h.pinned, "refresh", ``)
	require.Equal(t, 200, status, "empty refresh body refused")

	var relayed []string
	for i := 0; i < 3; i++ {
		select {
		case frame := <-h.frames:
			relayed = append(relayed, frame.Op+" "+string(frame.Request))
		case <-time.After(10 * time.Second):
			require.FailNow(t, "missing refresh relay frame %d", i)
		}
	}
	assert.Equal(t, []string{
		"refresh {}",
		`refresh {"id":"custom:http://10.9.8.7:8000"}`,
		"refresh {}",
	}, relayed, "refresh must relay op refresh with the scope preserved")
}

func TestCustomCarrierEditFields_DecodeAndRelayUnchangedShapes(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{"id":"lab:8000","keySet":false}`, ""
	})

	// replaceId and keyAction are now part of the sanctioned add body: the
	// strict decode accepts them and relays them to the owner's broker.
	status, body := h.post(t, h.pinned, "add", `{"url":"http://10.9.8.7:8000","port":8000,"replaceId":"custom:http://10.9.8.7:8000","keyAction":"clear"}`)
	require.Equal(t, 200, status, "edit fields refused: %s", body)

	// Widening the shape must not widen the door: anything still unknown
	// (a smuggled mode, a stray nodeId) keeps answering 400 before the relay.
	status, _ = h.post(t, h.pinned, "add", `{"url":"http://10.9.8.7:8000","port":8000,"mode":"fork"}`)
	require.Equal(t, 400, status, "unknown field accepted")

	select {
	case frame := <-h.frames:
		require.Equal(t, "add", frame.Op)
		assert.Contains(t, string(frame.Request), `"replaceId":"custom:http://10.9.8.7:8000"`, "relay frame lost the replace id")
		assert.Contains(t, string(frame.Request), `"keyAction":"clear"`, "relay frame lost the key action")
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no custom:relay-request reached the fake broker")
	}
	select {
	case frame := <-h.frames:
		require.FailNow(t, "the 400 shape still relayed: %v", frame)
	case <-time.After(200 * time.Millisecond):
	}
}

// --- Facade port: a node-level number on the same carrier ---------------------
//
// The Custom facade's listen port travels this carrier instead of the engine
// settings plane, because Custom has no launch policy to snapshot. It still has
// to earn the carrier's three guarantees: the route exists on a peer, the
// origin rebuilds a body the target's strict decode accepts (an origin that
// ships a shape the peer refuses is how a peer edit used to fail, which is what
// the wire widening in Task 5 exists to prevent), and a refusal decided on the
// owner's machine arrives in front of the person who typed the port.

func TestCustomCarrierFacadePort_RoutesRelayThePortWithoutReshapingIt(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		if op == "get-port" {
			return `{"port":8123,"running":true}`, ""
		}
		return `{"port":8137,"running":true}`, ""
	})

	// A port read carries no payload, so an empty body is the whole request —
	// the same normalization list list and refresh sit on. It has to be on
	// that list: the desktop sends `{}` for an argument-less read, and a 400
	// there would look like an unsupported peer.
	status, body := h.post(t, h.pinned, "get-port", ``)
	require.Equal(t, 200, status, "get-port route missing or refused: %s", body)
	assert.Contains(t, body, `"port":8123`, "the owner's live port must reach the caller verbatim")

	status, body = h.post(t, h.pinned, "set-port", `{"port":8137}`)
	require.Equal(t, 200, status, "set-port route refused: %s", body)
	assert.Contains(t, body, `"port":8137`)

	var relayed []string
	for i := 0; i < 2; i++ {
		select {
		case frame := <-h.frames:
			relayed = append(relayed, frame.Op+" "+string(frame.Request))
		case <-time.After(10 * time.Second):
			require.FailNow(t, "missing port relay frame %d", i)
		}
	}
	assert.Equal(t, []string{"get-port {}", `set-port {"port":8137}`}, relayed,
		"a facade port has to arrive as the owner's own port question")
}

func TestCustomCarrierFacadePort_BadShapesTurnAwayBeforeTheBroker(t *testing.T) {
	h := newCustomCarrier(t, func(op, id string) (string, string) {
		return `{"port":8123,"running":true}`, ""
	})

	t.Run("set-port with no body", func(t *testing.T) {
		// The payload is what makes a move a move. A blank body is not "move
		// to port 0", and relaying one would have the owner's proxy rebind on
		// a request that never named a port.
		status, body := h.post(t, h.pinned, "set-port", ``)
		require.Equal(t, 400, status, "an empty set-port body reached the broker: %s", body)
	})
	t.Run("set-port with a quoted port", func(t *testing.T) {
		status, body := h.post(t, h.pinned, "set-port", `{"port":"8137"}`)
		require.Equal(t, 400, status, "a non-numeric port reached the broker: %s", body)
	})
	t.Run("get-port carrying a port", func(t *testing.T) {
		// Reading the facade port takes no arguments, so any field here is
		// either an endpoint's port in the wrong place or an injected one.
		status, body := h.post(t, h.pinned, "get-port", `{"port":8137}`)
		require.Equal(t, 400, status, "get-port accepted a payload: %s", body)
	})

	select {
	case frame := <-h.frames:
		require.FailNow(t, "rejected shapes still relayed: %v", frame)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCustomCarrierFacadePort_OwnerRefusalKeepsItsDetail(t *testing.T) {
	// What the owner says is what the operator across the network reads. This
	// refusal names the port and who holds it — the part they can act on — and
	// unlike a persistence failure it quotes no local path, so nothing scrubs
	// it on the way out.
	const refusal = "port 9292 is already in use on this node: " +
		"an inference engine is running on that port"
	h := newCustomCarrier(t, func(op, id string) (string, string) { return "", refusal })

	status, body := h.post(t, h.pinned, "set-port", `{"port":9292}`)

	require.Equal(t, 409, status, "a broker refusal answered %d: %s", status, body)
	assert.Contains(t, body, refusal)
}

func TestRunCustomRemote_FacadePort_AnswersForThePeerNotHere(t *testing.T) {
	h := newCustomChain(t, func(op, body string) (string, string) {
		if op == "get-port" {
			return `{"port":8123,"running":true}`, ""
		}
		return `{"port":8137,"running":true}`, ""
	})

	reply := h.call(t, "custom:remote-get-port", `{"nodeId":"uuid-peer"}`)
	peerCall := h.awaitCall(t)
	require.Equal(t, "get-port", peerCall.op, "a peer port read never reached the peer's custom surface")
	assert.Equal(t, `{}`, peerCall.body, "a facade port belongs to the node, so the read carries nothing")
	require.NotNil(t, reply, "the peer port read never answered")
	require.Nil(t, reply.Error, "peer port read (%v)", reply)
	assert.Contains(t, string(reply.Result), `"port":8123`,
		"the peer's port must reach the desktop verbatim: this node's own 8000 would render a stranger's proxy as editable")

	reply = h.call(t, "custom:remote-set-port", `{"nodeId":"uuid-peer","port":8137}`)
	peerCall = h.awaitCall(t)
	require.Equal(t, "set-port", peerCall.op, "a peer port move never reached the peer's custom surface")
	assert.JSONEq(t, `{"port":8137}`, peerCall.body, "the typed port did not survive the origin's body rebuild")
	assert.NotContains(t, peerCall.body, "nodeId", "addressing must never ride into the HTTP body")
	require.NotNil(t, reply, "the peer port move never answered")
	require.Nil(t, reply.Error, "peer port move (%v)", reply)
	assert.Contains(t, string(reply.Result), `"port":8137`)
}

func TestRunCustomRemote_SetPort_PeerRefusal_ReachesTheCallerAsAnError(t *testing.T) {
	const refusal = "the Custom proxy is not running on this node, so there is no facade port to change"
	h := newCustomChain(t, func(op, body string) (string, string) { return "", refusal })

	reply := h.call(t, "custom:remote-set-port", `{"nodeId":"uuid-peer","port":8137}`)

	require.NotNil(t, reply, "a refused peer port edit never answered")
	require.NotNil(t, reply.Error, "a peer refusal must not read as an applied change: %v", reply)
	assert.Contains(t, reply.Error.Message, refusal,
		"the reason has to survive the two hops, or the operator cannot pick another port")
	assert.NotContains(t, reply.Error.Message, "uuid-peer", "addressing stays out of the error text")
}

func TestRunCustomRemote_SetPort_UntypedPort_FailsLocallyWithoutDialing(t *testing.T) {
	h := newCustomChain(t, func(op, body string) (string, string) {
		return `{"port":8123,"running":true}`, ""
	})

	// A port that was never typed is a local mistake. Answering it here means
	// the dial never happens, so a cleared field cannot spend the relay budget
	// to be told the same thing by the peer — and cannot move a peer's facade
	// to whatever its default happens to be.
	for _, params := range []string{`{"nodeId":"uuid-peer"}`, `{"nodeId":"uuid-peer","port":0}`,
		`{"nodeId":"uuid-peer","port":70000}`} {
		reply := h.call(t, "custom:remote-set-port", params)
		require.NotNil(t, reply, "no reply for "+params)
		require.NotNil(t, reply.Error, "accepted input: "+params)
		assert.Contains(t, reply.Error.Message, "between 1 and 65535", "input: "+params)
	}

	select {
	case call := <-h.calls:
		require.FailNow(t, "a bad shape still dialed out: %v", call)
	case <-time.After(200 * time.Millisecond):
	}
}
