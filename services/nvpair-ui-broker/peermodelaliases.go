// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/noderec"
)

// Peer model alias writes: editing the alias list of another machine from this
// one.
//
// One node's alias document belongs to that node: only its own
// nvpair-node-settings may write it, and it is the only thing that decides
// whether an alias is valid. So this is not a shared-registry edit — it is a
// document handed to the owning node, which validates it, applies it locally,
// and reports whether it did. The broker is a courier on both ends: it names the
// target and the caller and never interprets an alias itself.
//
// The workload manager carries the document because the cluster-mTLS listener it
// binds is the one port on a node a pinned peer can always reach. A proxy's
// facade port would have been a tidier home for a settings route, but that
// listener follows an engine and is closed whenever the engine is stopped —
// which is exactly when an operator may need to fix the alias that was pointing
// at it.
//
// One file serves two roles, depending on which machine owns the document:
//
//	editing a peer   settings/set-node-model-aliases   (desktop) ->
//	               settings/replace-peer-model-aliases  (manager stdin)
//	               settings/replace-model-aliases       (node -> node, mTLS)
//	               <- settings/peer-model-aliases-result  correlated by requestId
//	owning it        settings/apply-model-aliases       (manager stdout) ->
//	               settings/set-cluster-model-aliases   (node-settings)
//	               -> settings/apply-model-aliases-result  back down the stdin
//
// The manager's interface to this broker is notification-only in both directions
// (broker spec §7.4 projection table; the manager's spec states it as
// "JSON-RPC 2.0 notifications only"), so nothing but the requestId ties a verdict
// to the Save that asked for it. That is why every path here either answers the
// waiting client or logs why nobody could be answered.

const (
	// defaultPeerAliasWriteTimeout bounds how long this broker holds a client's
	// write open for the workload manager's verdict.
	//
	// It stays above the manager's own budget deliberately: the manager dials the
	// owning node under its own deadline, and that node holds the request while
	// its settings service applies it, so a verdict exists before this clock
	// speaks. Answering first would report "no answer" about a change whose
	// outcome was already known, which is both wrong and slower to say.
	defaultPeerAliasWriteTimeout = 12 * time.Second
)

// setNodeModelAliasesParams is the desktop's write: which node's document, and
// what it should say.
//
// Aliases is a pointer because the whole document is replaced, so an empty list
// and no list are different requests: the first erases the node's aliases, the
// second is a frame that names no document and is refused rather than guessed at.
type setNodeModelAliasesParams struct {
	NodeID  string                `json:"nodeId"`
	Aliases *[]noderec.ModelAlias `json:"aliases"`
}

// applyPeerAliasesParams is what the workload manager hands over when another
// node has asked this one to write its document. CallerUUID is the authenticated
// cluster id of the machine that asked, taken from its certificate: it is logged,
// because a change an operator did not make has to be traceable to a machine.
type applyPeerAliasesParams struct {
	RequestID  string                `json:"requestId"`
	CallerUUID string                `json:"callerUuid"`
	Aliases    *[]noderec.ModelAlias `json:"aliases"`
}

// replacePeerAliasesParams is this broker's request to the workload manager:
// carry this document to the node with this cluster id. The id is the same value
// the desktop's node cards are keyed by, and the manager resolves it to a pinned
// peer the same way it resolves one for a workload event.
type replacePeerAliasesParams struct {
	RequestID string               `json:"requestId"`
	NodeUUID  string               `json:"nodeUuid"`
	Aliases   []noderec.ModelAlias `json:"aliases"`
}

// aliasWriteVerdict is the answer in both directions on the manager link: what
// the owning node says about a write it was handed, and what the manager says
// about a write it was asked to carry.
type aliasWriteVerdict struct {
	RequestID string `json:"requestId"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error,omitempty"`
}

// peerAliasWrite is one client write this broker has handed to the workload
// manager and is still waiting to answer.
type peerAliasWrite struct {
	// clientID is the JSON-RPC id of the client whose Save this write answers.
	clientID *json.RawMessage
	nodeID   string
	timer    *time.Timer
}

// handleSetNodeModelAliases answers settings/set-node-model-aliases, the one
// write method the desktop uses for any node's aliases. Which machine's document
// it is stays the broker's business: it holds this node's cluster id, so it is
// the place that can tell "mine" from "a peer's", and a client that had to decide
// would eventually get it wrong.
func (b *Broker) handleSetNodeModelAliases(msg *Message) {
	// The verdict on this write can arrive long after the frame that asked for it,
	// so the id is kept as a copy the reader cannot reuse. A write sent as a
	// notification has no id to answer with, and there is nothing worth doing
	// here that anybody could then be told about.
	if msg.ID == nil {
		slog.Warn("model alias write sent as a notification cannot be answered")
		return
	}
	idBytes := append(json.RawMessage(nil), *msg.ID...)
	id := &idBytes

	var p setNodeModelAliasesParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		b.rejectModelAliasWrite(id, -32602, `invalid params: expected {"nodeId": ..., "aliases": [{"name": ..., "model": ...}]}`)
		return
	}
	if p.NodeID == "" {
		b.rejectModelAliasWrite(id, -32602, "invalid params: nodeId is required, and it is the cluster id of the node whose aliases are being written")
		return
	}
	// Sent as an explicit null: the whole document is replaced, so guessing here
	// would erase a node's aliases on a malformed frame.
	if p.Aliases == nil {
		b.rejectModelAliasWrite(id, -32602, `invalid params: "aliases" is required, and an absent list is not an empty list`)
		return
	}

	if p.NodeID == b.nodeID {
		b.setLocalModelAliases(id, *p.Aliases)
		return
	}
	b.setPeerModelAliases(id, p.NodeID, *p.Aliases)
}

// setLocalModelAliases writes this node's own document with the settings call the
// Cluster settings page has always used, so one document has one set of rules no
// matter which machine its editor was opened on.
func (b *Broker) setLocalModelAliases(id *json.RawMessage, aliases []noderec.ModelAlias) {
	params, err := json.Marshal(modelAliasesPayload{Aliases: aliases})
	if err != nil {
		b.rejectModelAliasWrite(id, -32602, "could not encode the alias document: "+err.Error())
		return
	}
	b.callSettings(id, "settings/set-cluster-model-aliases", params)
}

// setPeerModelAliases hands the document to the workload manager to carry to the
// owning node, and waits to answer the client until that node has said whether it
// wrote it.
func (b *Broker) setPeerModelAliases(id *json.RawMessage, nodeID string, aliases []noderec.ModelAlias) {
	wm := b.getWorkloadMgr()
	if wm == nil {
		b.rejectModelAliasWrite(id, -32000, "the cluster worker on this computer is not running, so no other node can be asked to change its settings")
		return
	}

	requestID := b.nextPeerAliasID()
	// A Broker assembled by hand (every test that exercises one branch of this
	// file) names no budget of its own, so the default is the fallback rather than
	// a clock that expires the instant a write is registered.
	timeout := b.peerAliasTimeout
	if timeout <= 0 {
		timeout = defaultPeerAliasWriteTimeout
	}
	write := &peerAliasWrite{clientID: id, nodeID: nodeID}
	write.timer = time.AfterFunc(timeout, func() {
		// The honest sentence matters more here than anywhere else: the request may
		// well have arrived, so "it failed" would invite an operator to re-apply a
		// change that is already in place.
		b.forfeitModelAliasWrite(requestID, "that node did not answer in time, so this change is not confirmed")
	})
	b.registerPeerAlias(requestID, write)

	params, err := json.Marshal(replacePeerAliasesParams{RequestID: requestID, NodeUUID: nodeID, Aliases: aliases})
	if err != nil {
		b.forfeitModelAliasWrite(requestID, "could not encode the alias document: "+err.Error())
		return
	}
	if err := wm.Forward(noderec.MethodReplacePeerAliases, params); err != nil {
		b.forfeitModelAliasWrite(requestID, "could not hand the change to this computer's cluster worker")
		return
	}
	slog.Info("asking a peer to replace its model aliases",
		"nodeId", nodeID, "requestId", requestID, "aliases", len(aliases))
}

// registerPeerAlias claims a correlation id for a write and starts the clock that
// answers the client if no verdict ever arrives. The write is recorded before its
// caller is allowed to send anything, so a verdict that comes back immediately
// still finds the client it belongs to.
func (b *Broker) registerPeerAlias(requestID string, w *peerAliasWrite) {
	b.peerAliasMu.Lock()
	defer b.peerAliasMu.Unlock()
	if b.peerAliasWrites == nil {
		b.peerAliasWrites = make(map[string]*peerAliasWrite)
	}
	b.peerAliasWrites[requestID] = w
}

// takePeerAlias retires a write, nil when somebody has already answered it — the
// clock or the verdict, never both for one request id.
func (b *Broker) takePeerAlias(requestID string) *peerAliasWrite {
	b.peerAliasMu.Lock()
	w := b.peerAliasWrites[requestID]
	delete(b.peerAliasWrites, requestID)
	b.peerAliasMu.Unlock()
	if w == nil {
		return nil
	}
	w.timer.Stop()
	return w
}

// nextPeerAliasID mints a correlation id. Uniqueness does not stop at this
// broker's own in-flight writes: the node that owns the document keys the writes
// it is applying by this id alone, so two machines whose counters happen to agree
// would look like one write repeating to it, and the second operator is refused a
// write of their own. Carrying this node's cluster id is what separates them.
// resolveLocalNodeID fixed that id at construction, before any peer link exists
// to write over, so an id is never minted without one.
func (b *Broker) nextPeerAliasID() string {
	b.peerAliasMu.Lock()
	defer b.peerAliasMu.Unlock()
	b.peerAliasSeq++
	return "aliases-" + b.nodeID + "-" + strconv.FormatUint(b.peerAliasSeq, 10)
}

// answerPeerAlias answers the client behind a write and reports whether this
// caller won it, so the verdict and the clock cannot both speak for one request.
func (b *Broker) answerPeerAlias(requestID string, respond func(w *peerAliasWrite)) bool {
	w := b.takePeerAlias(requestID)
	if w == nil {
		return false
	}
	respond(w)
	return true
}

// forfeitModelAliasWrite answers a client whose write will produce no verdict.
func (b *Broker) forfeitModelAliasWrite(requestID, reason string) {
	if !b.answerPeerAlias(requestID, func(w *peerAliasWrite) {
		slog.Warn("model alias write for a peer produced no verdict",
			"nodeId", w.nodeID, "requestId", requestID, "reason", reason)
		b.rejectModelAliasWrite(w.clientID, -32000, reason)
	}) {
		slog.Debug("model alias write already answered", "requestId", requestID)
	}
}

// rejectModelAliasWrite answers a client whose write is not going to happen.
func (b *Broker) rejectModelAliasWrite(id *json.RawMessage, code int, reason string) {
	if err := b.codec.RespondError(id, code, reason); err != nil {
		slog.Warn("failed to answer a model alias write", "err", err)
	}
}

// onPeerAliasVerdict handles settings/peer-model-aliases-result, the owning
// node's answer to a write this node asked for.
func (b *Broker) onPeerAliasVerdict(params json.RawMessage) {
	var v aliasWriteVerdict
	if err := json.Unmarshal(params, &v); err != nil || v.RequestID == "" {
		slog.Warn("workload-manager sent an invalid peer model alias result", "err", err)
		return
	}
	if !b.answerPeerAlias(v.RequestID, func(w *peerAliasWrite) {
		if !v.Applied {
			slog.Warn("a peer refused to change its model aliases",
				"nodeId", w.nodeID, "requestId", v.RequestID, "reason", v.Error)
			b.rejectModelAliasWrite(w.clientID, -32000, aliasWriteFailure(v.Error))
			return
		}
		slog.Info("a peer replaced its model aliases", "nodeId", w.nodeID, "requestId", v.RequestID)
		if err := b.codec.Respond(w.clientID, map[string]bool{"ok": true}); err != nil {
			slog.Warn("failed to relay a peer model alias write result", "err", err)
		}
	}) {
		slog.Debug("peer model alias result with no waiting client", "requestId", v.RequestID)
	}
}

// aliasWriteFailure turns the manager's report into a sentence for the operator.
// The manager carries the owning node's own words when that node refused, so the
// only time a fallback is needed is when there were no words to carry.
func aliasWriteFailure(reason string) string {
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		return trimmed
	}
	return "that node did not confirm the change"
}

// onPeerModelAliasesApply handles settings/apply-model-aliases: a pinned peer has
// asked this node to write its own document, and this node's settings service is
// the only thing allowed to decide whether it can.
//
// The settings call runs on its own goroutine. This is the manager's stdout
// reader, so a settings service that took its time here would also stall this
// node's workload events, which have nothing to do with aliases.
func (b *Broker) onPeerModelAliasesApply(params json.RawMessage) {
	var asked applyPeerAliasesParams
	if err := json.Unmarshal(params, &asked); err != nil {
		slog.Warn("workload-manager sent an invalid model alias apply request", "err", err)
		return
	}
	if asked.RequestID == "" || asked.Aliases == nil {
		slog.Warn("model alias apply request names no request or document")
		return
	}
	go b.applyPeerModelAliases(asked)
}

func (b *Broker) applyPeerModelAliases(asked applyPeerAliasesParams) {
	verdict := aliasWriteVerdict{RequestID: asked.RequestID}

	st := b.getSettings()
	switch {
	case st == nil:
		verdict.Error = "this node's settings service is not running"
	default:
		body, err := json.Marshal(modelAliasesPayload{Aliases: *asked.Aliases})
		if err != nil {
			verdict.Error = "could not encode the alias document: " + err.Error()
			break
		}
		_, rpcErr, err := st.Call(context.Background(), "settings/set-cluster-model-aliases", body)
		switch {
		case err != nil:
			// The settings service is up and did not answer, which is not the same
			// claim as "it refused": the caller must not be told to fix an alias
			// document that may well have been written.
			verdict.Error = "this node's settings service did not answer"
		case rpcErr != nil:
			// The authority on this node's aliases has spoken, and its sentence is
			// what the person across the cluster should read.
			verdict.Error = rpcErr.Message
		default:
			verdict.Applied = true
		}
	}

	if verdict.Applied {
		slog.Info("wrote this node's model aliases for a cluster peer",
			"requestId", asked.RequestID, "callerUuid", asked.CallerUUID, "aliases", len(*asked.Aliases))
	} else {
		slog.Warn("refused to write this node's model aliases for a cluster peer",
			"requestId", asked.RequestID, "callerUuid", asked.CallerUUID, "reason", verdict.Error)
	}
	b.reportAliasVerdict(verdict)
}

// reportAliasVerdict sends the local answer back down the manager's stdin, to the
// machine that asked.
func (b *Broker) reportAliasVerdict(v aliasWriteVerdict) {
	wm := b.getWorkloadMgr()
	if wm == nil {
		slog.Warn("no cluster worker to carry a model alias verdict back to", "requestId", v.RequestID)
		return
	}
	body, err := json.Marshal(v)
	if err != nil {
		slog.Warn("failed to encode a model alias verdict", "requestId", v.RequestID, "err", err)
		return
	}
	if err := wm.Forward(noderec.MethodApplyAliasesResult, body); err != nil {
		slog.Warn("failed to send a model alias verdict to the cluster worker", "requestId", v.RequestID, "err", err)
	}
}

// failPeerAliasWrites answers every write still waiting on the workload manager,
// for when that manager is gone: a client whose Save can never be answered
// deserves to hear that now rather than after its own timeout.
func (b *Broker) failPeerAliasWrites(reason string) {
	b.peerAliasMu.Lock()
	waiting := make([]string, 0, len(b.peerAliasWrites))
	for requestID := range b.peerAliasWrites {
		waiting = append(waiting, requestID)
	}
	b.peerAliasMu.Unlock()
	for _, requestID := range waiting {
		b.forfeitModelAliasWrite(requestID, reason)
	}
}
