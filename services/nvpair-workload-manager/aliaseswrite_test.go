// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

// The wire shapes below mirror the JSON this service sends and receives, rather
// than reusing the Go structs that produce it: the field names are the contract
// with the broker and with the peer, and a typo in a tag must fail a test.

// peerResult is what this node reports to its own broker about a write it asked
// another node to make.
type peerResult struct {
	RequestID string `json:"requestId"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error"`
}

// wireAlias is one synonym as it crosses the wire.
type wireAlias struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// writtenDocument is what a node's own settings service says it was asked to
// write, including who asked.
type writtenDocument struct {
	RequestID  string       `json:"requestId"`
	CallerUUID string       `json:"callerUuid"`
	Aliases    *[]wireAlias `json:"aliases"`
}

// localSettings stands in for the owning node's nvpair-node-settings, reached
// through that node's broker: it records each document as the JSON it was
// handed and answers with a verdict — or, when silent, with nothing at all.
type localSettings struct {
	mu      sync.Mutex
	handled [][]byte
	verdict applyModelAliasesResult
	silent  bool
	broken  error
	// reply carries the verdict back to the write waiting on it; tests point it
	// at the noteResult of whichever aliasApplier is serving.
	reply func(*Message)
}

func newLocalSettings() *localSettings {
	return &localSettings{verdict: applyModelAliasesResult{Applied: true}}
}

func (s *localSettings) emit(method string, params any) error {
	if method != MethodApplyAliases {
		return errors.New("settings service was handed an unexpected method: " + method)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		return s.broken
	}

	data, err := json.Marshal(params)
	if err != nil {
		return errors.New("encode the document handed to the settings service: " + err.Error())
	}
	s.handled = append(s.handled, data)
	if s.silent {
		return nil
	}

	// A real settings service answers asynchronously; the verdict still has to
	// name the request it belongs to, so read the id back out of the frame.
	var asked applyModelAliasesParams
	if err := json.Unmarshal(data, &asked); err != nil {
		return errors.New("decode the document handed to the settings service: " + err.Error())
	}
	verdict := s.verdict
	verdict.RequestID = asked.RequestID
	s.reply(&Message{Method: MethodApplyAliasesResult, Params: marshalParams(verdict)})
	return nil
}

func (s *localSettings) documents(t *testing.T) []writtenDocument {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	docs := make([]writtenDocument, 0, len(s.handled))
	for _, data := range s.handled {
		var doc writtenDocument
		require.NoError(t, json.Unmarshal(data, &doc), "decode document %s", data)
		docs = append(docs, doc)
	}
	return docs
}

// newStubApplier serves the owning node's route without a mesh: any caller is
// accepted as callerUUID, and the local settings service is the stand-in. The
// short wait keeps the no-verdict case honest without making the suite slow.
func newStubApplier(settings *localSettings, callerUUID string) *aliasApplier {
	applier := &aliasApplier{
		emit: settings.emit,
		wait: 25 * time.Millisecond,
		verify: func(*http.Request) (string, bool) {
			return callerUUID, callerUUID != ""
		},
		pending: make(map[string]chan applyModelAliasesResult),
	}
	settings.reply = applier.noteResult
	return applier
}

func postAliasWrite(t *testing.T, handler http.HandlerFunc, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, aliasWritePath, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec.Code, rec.Body.String()
}

// aliasListener puts the owning node's alias route behind cluster mTLS, the way
// its always-on inter-node listener does, and returns a func that posts a body
// to it as a given mesh. The serving mesh must be one built by
// newPinnedPeerMeshes or setupNode with the identity uuid-self.
func aliasListener(t *testing.T, server *clustertrust.Mesh, handler http.HandlerFunc) func(caller *clustertrust.Mesh, body string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(aliasWritePath, handler)
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = server.ServerTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	return func(caller *clustertrust.Mesh, body string) (int, string) {
		cfg, ok := caller.ClientTLSConfig("uuid-self")
		require.True(t, ok, "the caller must be able to build a client for the node it is writing to")
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}

		resp, err := client.Post(ts.URL+aliasWritePath, "application/json", bytes.NewBufferString(body))
		require.NoError(t, err, "post alias write over cluster mTLS")
		defer func() { require.NoError(t, resp.Body.Close()) }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "read the owning node's reply")
		return resp.StatusCode, string(data)
	}
}

// brokerLog records the notification frames this node would emit to its broker,
// holding them as the JSON the broker receives.
type brokerLog struct {
	mu      sync.Mutex
	methods []string
	payload []byte
}

func (l *brokerLog) emit(method string, params any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.methods = append(l.methods, method)
	l.payload = data
	return nil
}

// onlyResult fails unless exactly one frame was emitted and it is the answer the
// broker is waiting for, then decodes it.
func (l *brokerLog) onlyResult(t *testing.T) peerResult {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	require.Equal(t, []string{MethodPeerAliasesResult}, l.methods, "the broker must get exactly one answer")
	var res peerResult
	require.NoError(t, json.Unmarshal(l.payload, &res), "decode %s", l.payload)
	return res
}

const aliasWriteOneAlias = `{"jsonrpc":"2.0","method":"settings/replace-model-aliases",` +
	`"params":{"requestId":"req-1","aliases":[{"name":"quick","model":"google/gemma-4-12b"}]}}`

// TestAliasWrite_ReplacesTheLocalDocumentAndConfirms: a pinned peer's write
// reaches this node's settings service and the caller is told it landed.
func TestAliasWrite_ReplacesTheLocalDocumentAndConfirms(t *testing.T) {
	settings := newLocalSettings()
	code, body := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite, aliasWriteOneAlias)

	assert.Equal(t, http.StatusOK, code, body)
	var got peerResult
	require.NoError(t, json.Unmarshal([]byte(body), &got), "decode reply %s", body)
	assert.Equal(t, peerResult{RequestID: "req-1", Applied: true}, got)

	docs := settings.documents(t)
	require.Len(t, docs, 1, "the settings service must be asked exactly once")
	assert.Equal(t, "req-1", docs[0].RequestID)
	require.NotNil(t, docs[0].Aliases, "the document must arrive, not be dropped")
	require.Len(t, *docs[0].Aliases, 1)
	assert.Equal(t, wireAlias{Name: "quick", Model: "google/gemma-4-12b"}, (*docs[0].Aliases)[0])
}

// TestAliasWrite_TreatsAnEmptyDocumentAsAnErase: an empty list is a real edit —
// "this node has no aliases now" — and must survive the trip as an empty list
// rather than becoming no document at all.
func TestAliasWrite_TreatsAnEmptyDocumentAsAnErase(t *testing.T) {
	settings := newLocalSettings()
	code, body := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite,
		`{"jsonrpc":"2.0","method":"settings/replace-model-aliases","params":{"requestId":"req-2","aliases":[]}}`)

	assert.Equal(t, http.StatusOK, code, body)
	docs := settings.documents(t)
	require.Len(t, docs, 1)
	require.NotNil(t, docs[0].Aliases, `an empty list must stay a list, so "aliases" is absent differs from "aliases":[]`)
	assert.Empty(t, *docs[0].Aliases)
}

func TestAliasWrite_RejectsAFrameThatNamesNoAliasList(t *testing.T) {
	test := func(name, frame string) {
		t.Run(name, func(t *testing.T) {
			settings := newLocalSettings()
			code, body := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite, frame)

			assert.Equal(t, http.StatusBadRequest, code, body)
			assert.Empty(t, settings.documents(t), "a frame with no document must not reach the settings service")
		})
	}

	test("aliases absent", `{"jsonrpc":"2.0","method":"settings/replace-model-aliases","params":{"requestId":"req-3"}}`)
	test("aliases null", `{"jsonrpc":"2.0","method":"settings/replace-model-aliases","params":{"requestId":"req-3","aliases":null}}`)
	test("params absent", `{"jsonrpc":"2.0","method":"settings/replace-model-aliases"}`)
}

// TestAliasWrite_RejectsAWriteNobodyCanAskAbout: a write without a request id
// could not be confirmed to anyone, so it is refused rather than applied.
func TestAliasWrite_RejectsAWriteNobodyCanAskAbout(t *testing.T) {
	settings := newLocalSettings()
	code, _ := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite,
		`{"jsonrpc":"2.0","method":"settings/replace-model-aliases","params":{"aliases":[]}}`)

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Empty(t, settings.documents(t))
}

// TestAliasWrite_RejectsAFrameForAnotherMethod: the port serves more than one
// route, so a frame that arrives at the wrong door is refused rather than served.
func TestAliasWrite_RejectsAFrameForAnotherMethod(t *testing.T) {
	settings := newLocalSettings()
	code, _ := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite,
		`{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"wl-1"}}}`)

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Empty(t, settings.documents(t))
}

func TestAliasWrite_RejectsARequestThatIsNotAPost(t *testing.T) {
	settings := newLocalSettings()
	applier := newStubApplier(settings, "uuid-peer")

	rec := httptest.NewRecorder()
	applier.handleAliasWrite(rec, httptest.NewRequest(http.MethodGet, aliasWritePath, nil))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Empty(t, settings.documents(t))
}

// TestAliasWrite_RejectsASecondWriteWithTheSameRequestId: two in-flight writes
// claiming one id cannot both be answered by it, so the second is turned away
// while the first is still being applied.
func TestAliasWrite_RejectsASecondWriteWithTheSameRequestId(t *testing.T) {
	applier := newStubApplier(newLocalSettings(), "uuid-peer")
	_, held := applier.register("req-4")
	require.True(t, held, "the first write must be able to claim its request id")
	defer applier.unregister("req-4")

	code, body := postAliasWrite(t, applier.handleAliasWrite,
		`{"jsonrpc":"2.0","method":"settings/replace-model-aliases","params":{"requestId":"req-4","aliases":[]}}`)

	assert.Equal(t, http.StatusConflict, code, body)
}

// TestAliasWrite_ReportsUnconfirmedWhenTheLocalSettingsServiceIsSilent: the
// document is gone, and the honest answer is that there is no verdict — not the
// "failed" the operator would otherwise read and re-apply on top of.
func TestAliasWrite_ReportsUnconfirmedWhenTheLocalSettingsServiceIsSilent(t *testing.T) {
	settings := newLocalSettings()
	settings.silent = true

	code, body := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite, aliasWriteOneAlias)

	assert.Equal(t, http.StatusGatewayTimeout, code)
	assert.Contains(t, body, "not confirmed")
	assert.Len(t, settings.documents(t), 1, "the document must still have been handed over")
}

func TestAliasWrite_AnswersARefusalWithTheSettingsServicesReason(t *testing.T) {
	test := func(name, verdictError, wantReason string) {
		t.Run(name, func(t *testing.T) {
			settings := newLocalSettings()
			settings.verdict = applyModelAliasesResult{Applied: false, Error: verdictError}

			code, body := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite, aliasWriteOneAlias)

			// 422: the document reached the authority that owns it and that
			// authority says the document is wrong, so retrying changes nothing.
			assert.Equal(t, http.StatusUnprocessableEntity, code)
			assert.Contains(t, body, wantReason)
			assert.Len(t, settings.documents(t), 1)
		})
	}

	test("passes the reason through", "an alias name may not contain a space", "an alias name may not contain a space")
	test("fills in a blank verdict", "", "settings service refused")
}

func TestAliasWrite_ReportsWhenTheDocumentCannotBeHandedOver(t *testing.T) {
	settings := newLocalSettings()
	settings.broken = errors.New("broker pipe closed")

	code, body := postAliasWrite(t, newStubApplier(settings, "uuid-peer").handleAliasWrite, aliasWriteOneAlias)

	assert.Equal(t, http.StatusBadGateway, code)
	assert.Contains(t, body, "settings service")
}

// TestAliasWrite_RequiresAPinnedClusterPeer: this route writes a node's
// configuration, so the gate that guards workload events has to guard it too — a
// node that completes the handshake but holds no standing in the cluster is
// refused, and its document is never read.
func TestAliasWrite_RequiresAPinnedClusterPeer(t *testing.T) {
	selfCert, selfKey := genLeaf(t, "uuid-self")
	peerCert, peerKey := genLeaf(t, "uuid-peer")
	outsiderCert, outsiderKey := genLeaf(t, "uuid-outsider")

	// The outsider pins the serving node, so it reaches the handler; the serving
	// node holds no pin for it, which is what makes it a non-member.
	selfDir := setupNode(t, selfCert, selfKey, map[string][]byte{"uuid-peer": peerCert})
	peerDir := setupNode(t, peerCert, peerKey, map[string][]byte{"uuid-self": selfCert})
	outsiderDir := setupNode(t, outsiderCert, outsiderKey, map[string][]byte{"uuid-self": selfCert})

	server, caller := clustertrust.Open(selfDir), clustertrust.Open(peerDir)
	outsider := clustertrust.Open(outsiderDir)
	require.True(t, server.Clustered(), "the serving node must belong to the cluster")
	require.True(t, caller.Clustered(), "the calling node must belong to the cluster")
	require.True(t, outsider.Clustered(), "the outsider must at least have a cluster identity")

	settings := newLocalSettings()
	receiver := NewServer(0, newDedupIndex(8), server,
		func(*Workload) error { return nil },
		func(string, string) error { return nil },
		settings.emit)
	settings.reply = receiver.aliases().noteResult
	post := aliasListener(t, server, receiver.aliases().handleAliasWrite)

	code, body := post(outsider, aliasWriteOneAlias)
	assert.Equal(t, http.StatusForbidden, code, "a node the server does not pin must be refused: %s", body)
	assert.Empty(t, settings.documents(t), "a refused node's document must not reach the settings service")

	// The same route serves a real member, so the refusal above is the gate and
	// not a route that was never mounted.
	code, body = post(caller, aliasWriteOneAlias)
	assert.Equal(t, http.StatusOK, code, "a pinned member's write must be accepted: %s", body)
}

// TestAliasWrite_NamesTheCallerByItsCertificate: the document the settings
// service is asked to write says which machine asked for it, and that name comes
// from the caller's certificate — the request body never gets to claim one.
func TestAliasWrite_NamesTheCallerByItsCertificate(t *testing.T) {
	server, caller := newPinnedPeerMeshes(t)

	settings := newLocalSettings()
	receiver := NewServer(0, newDedupIndex(8), server,
		func(*Workload) error { return nil },
		func(string, string) error { return nil },
		settings.emit)
	settings.reply = receiver.aliases().noteResult

	code, body := aliasListener(t, server, receiver.aliases().handleAliasWrite)(caller, aliasWriteOneAlias)
	require.Equal(t, http.StatusOK, code, body)

	docs := settings.documents(t)
	require.Len(t, docs, 1)
	assert.Equal(t, "uuid-peer", docs[0].CallerUUID, "the caller must be the machine the certificate authenticates")
}

// --- the calling node's side ---

// startOwningNode runs the route on a node whose cluster id is uuid-peer, the way
// a real node runs it on its always-on inter-node port. Pass the same
// newPinnedPeerMeshes pair the calling side uses: the caller pins that exact
// certificate, so a second mesh would be a different machine.
func startOwningNode(t *testing.T, settings *localSettings, peer *clustertrust.Mesh) *httptest.Server {
	t.Helper()
	receiver := NewServer(0, newDedupIndex(8), peer,
		func(*Workload) error { return nil },
		func(string, string) error { return nil },
		settings.emit)
	settings.reply = receiver.aliases().noteResult

	mux := http.NewServeMux()
	mux.HandleFunc(aliasWritePath, receiver.aliases().handleAliasWrite)
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = peer.ServerTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// newOriginPeerSet addresses the node served by listener as the peer whose
// cluster id is uuid-peer, from discovery's point of view.
func newOriginPeerSet(t *testing.T, listener *httptest.Server) *peerSet {
	t.Helper()
	host, portStr, err := net.SplitHostPort(listener.Listener.Addr().String())
	require.NoError(t, err, "split the owning node's address")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err, "parse the owning node's port")

	peers := newPeerSet(0)
	peers.Replace([]PeerNode{{
		ID: "peer-1", Addresses: []string{host}, Port: port,
		TXT: []string{clustertrust.ClusterUUIDTXTKey + "=uuid-peer"},
	}})
	return peers
}

func replacePeerRequest(requestID, nodeUUID string, aliases string) *Message {
	return &Message{
		JSONRPC: "2.0",
		Method:  MethodReplacePeerAliases,
		Params: json.RawMessage(`{"requestId":"` + requestID + `","nodeUuid":"` + nodeUUID +
			`","aliases":` + aliases + `}`),
	}
}

// TestReplacePeerAliases_ConfirmsOnlyWhatTheOwningNodeWrote: the whole trip —
// origin node to peer over cluster mTLS, into that node's settings service, and
// the verdict back — ends in one answer the UI can trust.
func TestReplacePeerAliases_ConfirmsOnlyWhatTheOwningNodeWrote(t *testing.T) {
	self, peer := newPinnedPeerMeshes(t)
	settings := newLocalSettings()
	listener := startOwningNode(t, settings, peer)

	b := NewBroadcaster(newOriginPeerSet(t, listener), self)
	defer b.CloseIdle()

	log := &brokerLog{}
	newAliasForwarder(b, log.emit).replacePeer(context.Background(),
		replacePeerRequest("req-5", "uuid-peer", `[{"name":"quick","model":"google/gemma-4-12b"}]`))

	got := log.onlyResult(t)
	assert.Equal(t, peerResult{RequestID: "req-5", Applied: true}, got)

	docs := settings.documents(t)
	require.Len(t, docs, 1, "the owning node must have been asked to write it once")
	require.NotNil(t, docs[0].Aliases)
	require.Len(t, *docs[0].Aliases, 1)
	assert.Equal(t, wireAlias{Name: "quick", Model: "google/gemma-4-12b"}, (*docs[0].Aliases)[0])
}

// TestReplacePeerAliases_RepeatsTheOwningNodesReasonForRefusing: the peer's own
// settings service is the authority on what a valid alias is, so its sentence is
// what the operator should see, not a generic transport failure.
func TestReplacePeerAliases_RepeatsTheOwningNodesReasonForRefusing(t *testing.T) {
	self, peer := newPinnedPeerMeshes(t)
	settings := newLocalSettings()
	settings.verdict = applyModelAliasesResult{Applied: false, Error: "an alias may not shadow a model id"}
	listener := startOwningNode(t, settings, peer)
	b := NewBroadcaster(newOriginPeerSet(t, listener), self)
	defer b.CloseIdle()

	log := &brokerLog{}
	newAliasForwarder(b, log.emit).replacePeer(context.Background(),
		replacePeerRequest("req-6", "uuid-peer", `[{"name":"gemma","model":"google/gemma-4-12b"}]`))

	got := log.onlyResult(t)
	assert.False(t, got.Applied)
	assert.Contains(t, got.Error, "an alias may not shadow a model id")
}

func TestReplacePeerAliases_ReportsWhenTheNodeIsNotDiscovered(t *testing.T) {
	self, _ := newPinnedPeerMeshes(t)
	b := NewBroadcaster(newPeerSet(0), self)
	defer b.CloseIdle()

	log := &brokerLog{}
	newAliasForwarder(b, log.emit).replacePeer(context.Background(),
		replacePeerRequest("req-7", "uuid-nowhere", `[]`))

	got := log.onlyResult(t)
	assert.False(t, got.Applied)
	assert.Contains(t, got.Error, "no node with that cluster id")
}

// TestReplacePeerAliases_AnswersEvenWhenTheOwningNodeNeverDoes: a peer that takes
// the connection and then says nothing must not leave the operator's Save waiting
// on it. The trip has its own clock, so the answer is that the node did not
// answer — the one thing this node can claim about a peer that went quiet.
func TestReplacePeerAliases_AnswersEvenWhenTheOwningNodeNeverDoes(t *testing.T) {
	self, peer := newPinnedPeerMeshes(t)

	// The handler parks until the test is done with it: an HTTP server does not
	// notice a client hanging up mid-request, so a handler that only watched the
	// request's context would keep the listener open past the end of the test —
	// which is why releasing it is deferred after closing the listener, so it
	// runs first.
	hang := make(chan struct{})
	stalled := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	stalled.TLS = peer.ServerTLSConfig()
	stalled.StartTLS()
	defer stalled.Close()
	defer close(hang)

	b := NewBroadcaster(newOriginPeerSet(t, stalled), self)
	defer b.CloseIdle()

	log := &brokerLog{}
	forwarder := newAliasForwarder(b, log.emit)
	forwarder.timeout = 100 * time.Millisecond
	forwarder.replacePeer(context.Background(), replacePeerRequest("req-9", "uuid-peer", `[]`))

	got := log.onlyResult(t)
	assert.False(t, got.Applied)
	assert.Contains(t, got.Error, "could not reach that node")
}

// TestReplacePeerAliases_SaysSoWhenThisNodeIsNotInACluster: with no cluster there
// is nobody trusted to write, and an operator who asks must be told that rather
// than left waiting on a write that was never sent.
func TestReplacePeerAliases_SaysSoWhenThisNodeIsNotInACluster(t *testing.T) {
	_, peer := newPinnedPeerMeshes(t)
	settings := newLocalSettings()
	listener := startOwningNode(t, settings, peer)

	unclustered := clustertrust.Open(t.TempDir())
	require.False(t, unclustered.Clustered(), "a node with an empty cluster dir belongs to no cluster")

	b := NewBroadcaster(newOriginPeerSet(t, listener), unclustered)
	defer b.CloseIdle()

	log := &brokerLog{}
	newAliasForwarder(b, log.emit).replacePeer(context.Background(),
		replacePeerRequest("req-8", "uuid-peer", `[]`))

	got := log.onlyResult(t)
	assert.False(t, got.Applied)
	assert.Contains(t, got.Error, "not in a cluster")
	assert.Empty(t, settings.documents(t), "an unclustered node must not try to write a peer")
}
