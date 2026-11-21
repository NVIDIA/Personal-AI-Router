// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// A model alias write travels as far as a broker will send it: to this node's
// settings service when the document is this node's own, and to another machine
// over the workload manager when it is not. These tests are each one hop of that
// trip, seen from the broker, with the machine on the far end of the link played
// by the test.
//
// What they hold fixed is the part a broker can get wrong on its own: sending a
// peer's document to the local settings service, answering a Save that has no
// verdict yet, answering one twice, or turning "nobody answered" into "that node
// said no".

const (
	// aliasTestNodeID is this broker's own cluster id, so a write aimed at it is
	// the local case and anything else is a peer's.
	aliasTestNodeID = "uuid-this-node"
	aliasTestPeerID = "uuid-that-node"

	// hopWindow is how long a test waits for a frame that should arrive. It only
	// bounds a pipe round-trip, so it is long enough for a loaded CI machine and
	// short enough that a missing frame reads as a failure rather than a hang.
	hopWindow = 2 * time.Second

	// quietWindow is how long a link must stay silent for a test to call it
	// silent. The waits it rules out are all on other goroutines, so without this
	// window a test would pass on a race it did not win.
	quietWindow = 100 * time.Millisecond
)

// wireAlias is one alias as the wire spells it. Every payload in these tests is
// decoded through a shape declared here rather than through the production type,
// so a renamed field fails a test instead of silently decoding to nothing.
type wireAlias struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

type wireAliasDoc struct {
	Aliases []wireAlias `json:"aliases"`
}

// wireReplacePeer is the frame the broker asks the workload manager to carry to
// the node that owns the document.
type wireReplacePeer struct {
	RequestID string      `json:"requestId"`
	NodeUUID  string      `json:"nodeUuid"`
	Aliases   []wireAlias `json:"aliases"`
}

// wireVerdict is what the owning node says about a write it was handed. The same
// shape travels in both directions, so one struct covers both hops.
type wireVerdict struct {
	RequestID string `json:"requestId"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error"`
}

const (
	aliasDocJSON       = `[{"name":"fast","model":"qwen/qwen3.6-35b-a3b"}]`
	aliasDocErasedJSON = `[]`
)

// fastAlias is the one alias these tests move around, as both sides spell it.
var fastAlias = wireAlias{Name: "fast", Model: "qwen/qwen3.6-35b-a3b"}

// peerAliasHarness wires a broker to everything one alias write touches: the
// client whose Save it answers, the cluster worker that carries a peer's
// document, and this node's own settings service. Each is a pipe the test drives
// from the far end, so a test can be the machine on the other side of any hop it
// is checking — including one that refuses, answers late, or never answers.
type peerAliasHarness struct {
	broker *Broker

	clientConn   net.Conn
	clientCodec  *Codec
	settingsConn net.Conn
	settings     *Codec

	// toManager carries every frame the broker writes to the cluster worker's
	// stdin. A goroutine drains the pipe into it, so a write is never held up by
	// a test that has not looked yet, and reading it out of order is visible.
	toManager chan *Message
}

func newPeerAliasHarness(t *testing.T) *peerAliasHarness {
	t.Helper()

	clientConn, brokerConn := net.Pipe()
	managerConn, testManagerConn := net.Pipe()
	settingsConn, brokerSettingsConn := net.Pipe()

	// The settings link is wired by hand rather than with the shared worker-pipe
	// fixture because one test needs to sever it mid-write, and a fixture that
	// closes both of its own ends at cleanup would hide the difference between a
	// service that refused one and a service that never spoke.
	settingsWorker := &rpcWorker{peer: NewPeer(NewCodec(brokerSettingsConn))}
	go settingsWorker.peer.Serve(nil, nil)

	b := &Broker{
		nodeID:           aliasTestNodeID,
		peerAliasTimeout: hopWindow,
	}
	b.codec = NewCodec(brokerConn)
	b.setWorkloadMgr(&workloadManagerProcess{stdin: managerConn, done: make(chan struct{})})
	b.setSettings(settingsWorker)

	h := &peerAliasHarness{
		broker:       b,
		clientConn:   clientConn,
		clientCodec:  NewCodec(clientConn),
		settingsConn: settingsConn,
		settings:     NewCodec(settingsConn),
		toManager:    make(chan *Message, 8),
	}

	go func() {
		codec := NewCodec(testManagerConn)
		for {
			frame, err := codec.Read()
			if !assertRPCRead(t, err) {
				return
			}
			h.toManager <- frame
		}
	}()

	t.Cleanup(func() {
		// Closing the manager's end unblocks the reader goroutine above; the
		// others simply end their pumps.
		_ = managerConn.Close()
		_ = testManagerConn.Close()
		_ = clientConn.Close()
		_ = brokerConn.Close()
		_ = settingsConn.Close()
		_ = brokerSettingsConn.Close()
	})
	return h
}

// stopClusterWorker takes this node's cluster worker away, the way a crash does.
func (h *peerAliasHarness) stopClusterWorker() {
	h.broker.setWorkloadMgr(nil)
}

// withoutSettings takes this node's settings service away.
func (h *peerAliasHarness) withoutSettings() {
	h.broker.setSettings(nil)
}

// save sends the desktop's write for one node's alias document to the broker.
// It is a real request frame through the broker's own dispatch, because which
// hop a write takes is decided below the method's prefix.
func (h *peerAliasHarness) save(t *testing.T, params string) {
	t.Helper()
	id := json.RawMessage(`"save-1"`)
	go h.broker.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  noderec.MethodSetNodeModelAliases,
		Params:  json.RawMessage(params),
	})
}

// peerWrite is the desktop's Save for a peer's document, spelled once because
// most of these tests start from it.
func peerWrite(aliases string) string {
	return fmt.Sprintf(`{"nodeId":%q,"aliases":%s}`, aliasTestPeerID, aliases)
}

// nextManagerFrame reads the next frame the broker wrote to the cluster worker.
func (h *peerAliasHarness) nextManagerFrame(t *testing.T) *Message {
	t.Helper()
	select {
	case frame := <-h.toManager:
		return frame
	case <-time.After(hopWindow):
		require.FailNow(t, "the cluster worker received no frame from the broker")
		return nil
	}
}

// nextReplacePeer reads the carry request the broker asked the cluster worker to
// deliver, and returns the correlation id it will wait for.
func (h *peerAliasHarness) nextReplacePeer(t *testing.T) wireReplacePeer {
	t.Helper()
	frame := h.nextManagerFrame(t)
	require.Equalf(t, noderec.MethodReplacePeerAliases, frame.Method, "want the carry request, got %s", frameLine(frame))
	var asked wireReplacePeer
	require.NoError(t, json.Unmarshal(frame.Params, &asked))
	return asked
}

// nextVerdict reads the answer the broker sends back down the cluster worker's
// stdin when this node is the one that owns the document.
func (h *peerAliasHarness) nextVerdict(t *testing.T) wireVerdict {
	t.Helper()
	frame := h.nextManagerFrame(t)
	require.Equalf(t, noderec.MethodApplyAliasesResult, frame.Method, "want the local verdict, got %s", frameLine(frame))
	var verdict wireVerdict
	require.NoError(t, json.Unmarshal(frame.Params, &verdict))
	return verdict
}

// requireNoManagerFrame fails if the cluster worker is asked to do anything.
func (h *peerAliasHarness) requireNoManagerFrame(t *testing.T, why string) {
	t.Helper()
	select {
	case frame := <-h.toManager:
		require.FailNowf(t, "the cluster worker was asked to carry something", "%s: it got %s", why, frameLine(frame))
	case <-time.After(quietWindow):
	}
}

// settingsCall is one request the broker made of this node's settings service,
// with the answer still owed.
type settingsCall struct {
	Method  string
	Aliases []wireAlias

	id    *json.RawMessage
	codec *Codec
}

// accept writes what a settings service that stored the document answers.
func (c settingsCall) accept(t *testing.T) {
	t.Helper()
	require.NoError(t, c.codec.Respond(c.id, map[string]bool{"ok": true}))
}

// refuse writes what a settings service that rejected the document answers, in
// its own words: those words are what the operator across the cluster reads.
func (c settingsCall) refuse(t *testing.T, sentence string) {
	t.Helper()
	require.NoError(t, c.codec.RespondError(c.id, -32602, sentence))
}

// nextSettingsCall reads the request the broker made of this node's settings
// service.
func (h *peerAliasHarness) nextSettingsCall(t *testing.T) settingsCall {
	t.Helper()
	require.NoError(t, h.settingsConn.SetReadDeadline(time.Now().Add(hopWindow)), "set settings read deadline")
	frame, err := h.settings.Read()
	require.NoErrorf(t, err, "this node's settings service received no request")
	require.Truef(t, frame.IsRequest(), "want a request, got %s", frameLine(frame))

	var doc wireAliasDoc
	require.NoError(t, json.Unmarshal(frame.Params, &doc))
	return settingsCall{Method: frame.Method, Aliases: doc.Aliases, id: frame.ID, codec: h.settings}
}

// requireNoSettingsCall fails if this node's settings service is asked for
// anything, which for a peer's document is the whole bug.
func (h *peerAliasHarness) requireNoSettingsCall(t *testing.T, why string) {
	t.Helper()
	require.NoError(t, h.settingsConn.SetReadDeadline(time.Now().Add(quietWindow)), "set settings read deadline")
	frame, err := h.settings.Read()
	if err == nil {
		require.FailNowf(t, "this node's settings service was asked to write something", "%s: it got %s", why, frameLine(frame))
	}
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "settings service link broke early")
}

// nextClientAnswer reads the one frame the broker sends the waiting Save.
func (h *peerAliasHarness) nextClientAnswer(t *testing.T) *Message {
	t.Helper()
	require.NoError(t, h.clientConn.SetReadDeadline(time.Now().Add(hopWindow)), "set client read deadline")
	frame, err := h.clientCodec.Read()
	require.NoErrorf(t, err, "the client's Save was never answered")
	require.NotNilf(t, frame.ID, "want an answer to the Save, got %s", frameLine(frame))
	return frame
}

// requireClientApplied reads the answer a Save gets when the document was stored.
func (h *peerAliasHarness) requireClientApplied(t *testing.T) {
	t.Helper()
	answer := h.nextClientAnswer(t)
	require.Nilf(t, answer.Error, "want success, got %s", frameLine(answer))
	var ok struct {
		OK bool `json:"ok"`
	}
	require.NoError(t, json.Unmarshal(answer.Result, &ok))
	assert.Truef(t, ok.OK, "want an applied answer, got %s", frameLine(answer))
}

// requireClientRefusal reads the answer a Save gets when it is not happening.
func (h *peerAliasHarness) requireClientRefusal(t *testing.T, wantCode int, wantPhrase string) {
	t.Helper()
	answer := h.nextClientAnswer(t)
	require.NotNilf(t, answer.Error, "want a refusal, got %s", frameLine(answer))
	assert.Equal(t, wantCode, answer.Error.Code)
	assert.Contains(t, answer.Error.Message, wantPhrase)
}

// requireSaveStillOpen fails if the broker answers a Save before the node that
// owns the document has had its say.
func (h *peerAliasHarness) requireSaveStillOpen(t *testing.T, why string) {
	t.Helper()
	require.NoError(t, h.clientConn.SetReadDeadline(time.Now().Add(quietWindow)), "set client read deadline")
	frame, err := h.clientCodec.Read()
	if err == nil {
		require.FailNowf(t, "the client's Save was answered early", "%s: it got %s", why, frameLine(frame))
	}
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the client link broke while its Save was open")
}

// fromManager hands the broker a notification the cluster worker emitted, on a
// goroutine of its own, the way the manager's stdout reader does. A verdict
// answers a client, which is a write: the test goes on to read it, so waiting
// for the handler here would be a test waiting on itself.
func (h *peerAliasHarness) fromManager(t *testing.T, method, params string) {
	t.Helper()
	go h.broker.forwardWorkloadManagerNotification(method, json.RawMessage(params))
}

// fromManagerReturns is fromManager for a frame that answers no client, and so
// can be waited for. It is the shape that asserts the apply path hands the
// write to its own goroutine: this node's workload events all come down the
// same reader, and a settings service that took its time must not hold them.
func (h *peerAliasHarness) fromManagerReturns(t *testing.T, method, params string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.broker.forwardWorkloadManagerNotification(method, json.RawMessage(params))
	}()
	select {
	case <-done:
	case <-time.After(hopWindow):
		require.FailNowf(t, "the broker kept the cluster worker's reader waiting", "%s", method)
	}
}

// frameLine renders a frame for a failure message, including the read that
// returned nothing.
func frameLine(frame *Message) string {
	if frame == nil {
		return "<nothing>"
	}
	return fmt.Sprintf("method=%s params=%s result=%s error=%s", frame.Method, frame.Params, frame.Result, errorLine(frame.Error))
}

func errorLine(rpcErr *RPCError) string {
	if rpcErr == nil {
		return ""
	}
	return fmt.Sprintf("%d %s", rpcErr.Code, rpcErr.Message)
}

func TestSetNodeModelAliases_WritesThisNodesOwnDocumentThroughItsSettingsService(t *testing.T) {
	h := newPeerAliasHarness(t)

	h.save(t, fmt.Sprintf(`{"nodeId":%q,"aliases":%s}`, aliasTestNodeID, aliasDocJSON))

	asked := h.nextSettingsCall(t)
	assert.Equal(t, "settings/set-cluster-model-aliases", asked.Method, "one document has one set of rules, whichever machine edited it")
	assert.Equal(t, []wireAlias{fastAlias}, asked.Aliases)
	asked.accept(t)

	h.requireClientApplied(t)
	h.requireNoManagerFrame(t, "this node owns the document, so nobody has to carry it anywhere")
}

func TestSetNodeModelAliases_HandsAPeerDocumentToTheClusterWorker(t *testing.T) {
	h := newPeerAliasHarness(t)

	h.save(t, peerWrite(aliasDocJSON))

	asked := h.nextReplacePeer(t)
	assert.Equal(t, aliasTestPeerID, asked.NodeUUID, "the write is aimed at the node the Save named")
	assert.Equal(t, []wireAlias{fastAlias}, asked.Aliases)
	require.NotEmpty(t, asked.RequestID, "the verdict is correlated by this id, so it has to exist")

	h.requireSaveStillOpen(t, "the owning node has not said whether it wrote the document")
	h.requireNoSettingsCall(t, "a peer's alias document is not this node's to write")
}

func TestSetNodeModelAliases_RejectsAFrameThatNamesNoDocument(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{name: "malformed parameters", params: `{`},
		{name: "no target node", params: `{"aliases":[]}`},
		{name: "an explicit null alias list", params: `{"nodeId":"uuid-that-node","aliases":null}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newPeerAliasHarness(t)

			h.save(t, tc.params)

			// The whole document is replaced, so guessing that an absent list is an
			// empty one would erase a node's aliases on a frame that never asked to.
			h.requireClientRefusal(t, -32602, "invalid params")
			h.requireNoSettingsCall(t, "a malformed Save is not a write")
			h.requireNoManagerFrame(t, "a malformed Save is not a write")
		})
	}
}

func TestSetNodeModelAliases_ReportsThatNoClusterWorkerCanCarryTheWrite(t *testing.T) {
	h := newPeerAliasHarness(t)
	h.stopClusterWorker()

	h.save(t, peerWrite(aliasDocJSON))

	h.requireClientRefusal(t, -32000, "cluster worker on this computer is not running")
	h.requireNoSettingsCall(t, "a peer's document is not this node's to write, even with nowhere to send it")
}

func TestPeerAliasVerdict_AppliesWhenTheOwningNodeStoredTheDocument(t *testing.T) {
	h := newPeerAliasHarness(t)
	h.save(t, peerWrite(aliasDocJSON))
	asked := h.nextReplacePeer(t)

	h.fromManager(t, noderec.MethodPeerAliasesResult, fmt.Sprintf(`{"requestId":%q,"applied":true}`, asked.RequestID))

	h.requireClientApplied(t)
}

func TestPeerAliasVerdict_ReportsTheOwningNodesOwnWordsWhenItRefuses(t *testing.T) {
	h := newPeerAliasHarness(t)
	h.save(t, peerWrite(aliasDocJSON))
	asked := h.nextReplacePeer(t)

	const refusal = "an alias may not shadow a model id"
	h.fromManager(t, noderec.MethodPeerAliasesResult, fmt.Sprintf(`{"requestId":%q,"applied":false,"error":%q}`, asked.RequestID, refusal))

	// The refusal is the other machine's sentence, quoted rather than rewritten:
	// the person editing has to see what the owner actually objected to.
	answer := h.nextClientAnswer(t)
	require.NotNil(t, answer.Error)
	assert.Equal(t, -32000, answer.Error.Code)
	assert.Equal(t, refusal, answer.Error.Message)
}

func TestPeerAliasVerdict_WithNothingWaitingIsIgnored(t *testing.T) {
	h := newPeerAliasHarness(t)

	// A verdict whose Save this broker never asked about — a duplicate, or one
	// that arrived after its own clock ran out — must not answer whatever else is
	// in flight.
	h.fromManager(t, noderec.MethodPeerAliasesResult, `{"requestId":"aliases-42","applied":true}`)
	h.fromManager(t, noderec.MethodPeerAliasesResult, `{"applied":true}`)
	h.fromManager(t, noderec.MethodPeerAliasesResult, `{`)

	h.requireSaveStillOpen(t, "no Save is open, so nothing should be answered at all")
	h.requireNoManagerFrame(t, "a verdict is an answer, not a request")
}

func TestPeerAliasWrite_AnswersEvenWhenTheOwningNodeNeverDoes(t *testing.T) {
	h := newPeerAliasHarness(t)
	h.broker.peerAliasTimeout = 20 * time.Millisecond

	h.save(t, peerWrite(aliasDocJSON))
	h.nextReplacePeer(t)

	// The honest wording is the point of this test: the write may well have
	// landed, so the client must be told it is unconfirmed, not that it failed.
	h.requireClientRefusal(t, -32000, "not confirmed")
	h.requireNoManagerFrame(t, "this broker's clock expired; it asks the cluster worker for nothing")
}

func TestPeerAliasWrite_FailsWhenTheClusterWorkerStopsMidWrite(t *testing.T) {
	h := newPeerAliasHarness(t)
	h.save(t, peerWrite(aliasDocJSON))
	h.nextReplacePeer(t)

	// The supervisor's crash callback is what calls this in production, never the
	// client's own goroutine, so it is called the same way here: answering a
	// client is a write, and a write from the goroutine that would read it is a
	// deadlock.
	go h.broker.failPeerAliasWrites("the cluster worker on this computer stopped, so this change was not confirmed")

	h.requireClientRefusal(t, -32000, "not confirmed")

	// And only once: the verdict a restarting worker might still send finds no
	// second answer to make.
	h.fromManager(t, noderec.MethodPeerAliasesResult,
		`{"requestId":"aliases-`+aliasTestNodeID+`-1","applied":true}`)
	h.requireSaveStillOpen(t, "that Save was already answered")
}

// TestPeerAliasWrite_EachNodeMintsItsOwnFirstRequestID: the node that owns a
// document tracks the writes it is applying by request id alone and turns away a
// second one it cannot tell apart, so two machines whose counters happen to agree
// must not hand it the same id. Otherwise the second operator's Save is refused
// as a duplicate of a stranger's write, in words about request ids that tell them
// nothing about what happened.
func TestPeerAliasWrite_EachNodeMintsItsOwnFirstRequestID(t *testing.T) {
	here := &Broker{nodeID: aliasTestNodeID}
	there := &Broker{nodeID: aliasTestPeerID}

	assert.NotEqual(t, here.nextPeerAliasID(), there.nextPeerAliasID(),
		"two nodes' first peer write must not look like one write to the node that owns the document")
}

func TestPeerAliasApply_WritesThisNodesDocumentAndReportsThatItDid(t *testing.T) {
	h := newPeerAliasHarness(t)

	h.fromManagerReturns(t, noderec.MethodApplyAliases, `{"requestId":"req-1","callerUuid":"uuid-caller","aliases":`+aliasDocJSON+`}`)

	asked := h.nextSettingsCall(t)
	assert.Equal(t, "settings/set-cluster-model-aliases", asked.Method, "the owning node's own settings service is the only writer")
	assert.Equal(t, []wireAlias{fastAlias}, asked.Aliases)
	asked.accept(t)

	verdict := h.nextVerdict(t)
	assert.Equal(t, "req-1", verdict.RequestID, "the machine that asked is waiting on this id")
	assert.True(t, verdict.Applied)
	assert.Empty(t, verdict.Error)
}

func TestPeerAliasApply_ReplacesTheWholeDocumentIncludingAnEmptyOne(t *testing.T) {
	h := newPeerAliasHarness(t)

	// An empty list is a deliberate erase, and the only way to say it.
	h.fromManagerReturns(t, noderec.MethodApplyAliases, `{"requestId":"req-1","callerUuid":"uuid-caller","aliases":`+aliasDocErasedJSON+`}`)

	asked := h.nextSettingsCall(t)
	assert.NotNil(t, asked.Aliases, "an erased document still reaches the settings service as a list")
	assert.Empty(t, asked.Aliases)
	asked.accept(t)

	assert.True(t, h.nextVerdict(t).Applied, "erasing everything is a write like any other")
}

func TestPeerAliasApply_PassesOnTheSettingsServicesOwnRefusal(t *testing.T) {
	h := newPeerAliasHarness(t)

	h.fromManagerReturns(t, noderec.MethodApplyAliases, `{"requestId":"req-1","callerUuid":"uuid-caller","aliases":`+aliasDocJSON+`}`)

	const refusal = "aliases[0] (fast): model must name the model this alias stands for"
	h.nextSettingsCall(t).refuse(t, refusal)

	verdict := h.nextVerdict(t)
	assert.Equal(t, "req-1", verdict.RequestID)
	assert.False(t, verdict.Applied)
	assert.Equal(t, refusal, verdict.Error, "the document's owner objects, and its words travel untouched")
}

func TestPeerAliasApply_SaysTheSettingsServiceDidNotAnswerRatherThanCallingThatARefusal(t *testing.T) {
	h := newPeerAliasHarness(t)
	// The settings service was up and then was not: nothing here says the
	// document is bad, and the machine across the cluster must not be told
	// otherwise, or an operator will go "fix" an alias document that may already
	// have been written.
	_ = h.settingsConn.Close()

	h.fromManagerReturns(t, noderec.MethodApplyAliases, `{"requestId":"req-1","callerUuid":"uuid-caller","aliases":`+aliasDocJSON+`}`)

	verdict := h.nextVerdict(t)
	assert.False(t, verdict.Applied)
	assert.Equal(t, "this node's settings service did not answer", verdict.Error)
}

func TestPeerAliasApply_SaysSoWhenThisNodeHasNoSettingsService(t *testing.T) {
	h := newPeerAliasHarness(t)
	h.withoutSettings()

	h.fromManagerReturns(t, noderec.MethodApplyAliases, `{"requestId":"req-1","callerUuid":"uuid-caller","aliases":`+aliasDocJSON+`}`)

	verdict := h.nextVerdict(t)
	assert.False(t, verdict.Applied)
	assert.Equal(t, "this node's settings service is not running", verdict.Error)
}

func TestPeerAliasApply_RefusesBeforeWritingAnythingWithoutARequestOrADocument(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{name: "no request to answer", params: `{"callerUuid":"uuid-caller","aliases":` + aliasDocJSON + `}`},
		{name: "an explicit null alias list", params: `{"requestId":"req-1","callerUuid":"uuid-caller","aliases":null}`},
		{name: "no alias list at all", params: `{"requestId":"req-1","callerUuid":"uuid-caller"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newPeerAliasHarness(t)

			h.fromManagerReturns(t, noderec.MethodApplyAliases, tc.params)

			// A frame that names no document is not a request to erase one, and
			// there is no verdict for a machine that asked with no request id.
			h.requireNoSettingsCall(t, "the frame named no document to write")
			h.requireNoManagerFrame(t, "the frame named no request to answer")
		})
	}
}
