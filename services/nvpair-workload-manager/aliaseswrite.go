// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// Peer model-alias writes.
//
// One node's cluster model alias document belongs to that node: only its own
// nvpair-node-settings may write it, and it is the only thing that validates
// what an alias may be. So a write wanted from another machine is not a
// shared-registry edit — it is a request handed to the owning node, which
// validates and applies it locally and says whether it did.
//
// The workload manager carries it because this port is the one thing on a node
// that is always reachable from a pinned peer: it binds its cluster-mTLS
// listener for the life of the process and every peer already knows its address
// from the node record. A proxy facade port would have been a tidier home for a
// settings route, but that listener follows an engine and is closed whenever the
// engine is stopped — which is exactly when an operator may need to fix the
// alias that was pointing at it.
//
// The courier is not the authority. This package never interprets an alias: it
// decodes the document enough to know it is a document, and passes it to the
// broker, which asks node-settings. A peer's own certificate names the caller;
// nothing in a request body is trusted for that.
//
// The five frames:
//
//	broker  -> this, origin role   settings/replace-peer-model-aliases  {requestId, nodeUuid, aliases}
//	this    -> owning node        settings/replace-model-aliases       {requestId, aliases}   (HTTPS POST, aliasWritePath)
//	this    -> owning broker      settings/apply-model-aliases         {requestId, callerUuid, aliases}
//	owning broker -> this         settings/apply-model-aliases-result  {requestId, applied, error}
//	this    -> origin broker      settings/peer-model-aliases-result   {requestId, applied, error}
//
// Those five names are the shared wire vocabulary in nvpair-shared/noderec, which
// the broker relay on the other end of this pipe names too: a name that changes on
// one side only is a write nobody ever answers. The aliases below keep this file's
// call sites short.
const (
	// MethodReplacePeerAliases is the broker's request to replace one peer's
	// alias document. It is a notification: the answer comes back as
	// MethodPeerAliasesResult, because the manager's interface to its broker is
	// notification-only in both directions (spec §7.1).
	MethodReplacePeerAliases = noderec.MethodReplacePeerAliases
	// MethodPeerAliasesResult is the answer to MethodReplacePeerAliases,
	// correlated by requestId.
	MethodPeerAliasesResult = noderec.MethodPeerAliasesResult
	// MethodAliasWrite is the inter-node method on aliasWritePath.
	MethodAliasWrite = noderec.MethodAliasWrite
	// MethodApplyAliases asks the owning node's broker to write its local
	// settings document.
	MethodApplyAliases = noderec.MethodApplyAliases
	// MethodApplyAliasesResult is the owning broker's verdict, correlated by
	// requestId. It arrives as a notification for the same reason the request
	// did.
	MethodApplyAliasesResult = noderec.MethodApplyAliasesResult

	// aliasWritePath is the inter-node route for MethodAliasWrite, served on the
	// same always-on cluster-mTLS listener as eventsPath.
	aliasWritePath = "/v1/settings/model-aliases"

	// peerAliasApplyTimeout bounds how long the owning node waits for its own
	// settings service to answer before telling the caller it has no verdict.
	//
	// It stays under perPeerTimeout on purpose: the pooled peer client that
	// carries the request gives up at 3s, so a longer wait here would make the
	// caller's timeout the one that speaks, reporting an unreachable node for a
	// write that may well have landed.
	peerAliasApplyTimeout = 2 * time.Second

	// peerAliasTripTimeout bounds the calling side's whole trip: picking an
	// address for the peer, one request there, and its answer.
	//
	// Picking matters because it is the part with no other clock. Choosing among
	// a multi-homed node's candidates dials each one that does not answer, and the
	// request itself is bounded only by the pooled client's perPeerTimeout, so a
	// peer that publishes several dead addresses could otherwise hold the write
	// open long after anyone is waiting for it. The operator waiting on this clock
	// gets "could not reach that node", which is the honest answer for a peer that
	// did not answer in time, and their Save stops spinning.
	peerAliasTripTimeout = 10 * time.Second

	// maxAliasReplyBytes bounds how much of the owning node's answer we read.
	// Verdicts are a status line and a validation sentence; a peer streaming
	// something larger forfeits the rest rather than our memory.
	maxAliasReplyBytes = 4 << 10
)

// ReplacePeerAliasesParams is the broker's request to replace a peer node's
// alias document. Aliases is a pointer so that "replace the document with an
// empty one" is distinguishable from "I sent no document", which is the
// difference between erasing a peer's aliases and asking to erase them.
type ReplacePeerAliasesParams struct {
	RequestID string                `json:"requestId"`
	NodeUUID  string                `json:"nodeUuid"`
	Aliases   *[]noderec.ModelAlias `json:"aliases"`
}

// peerAliasesResult is the origin-side verdict: did the owning node write it.
type peerAliasesResult struct {
	RequestID string `json:"requestId"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error,omitempty"`
}

// applyModelAliasesParams asks this node's broker to write its own settings
// document. CallerUUID is the authenticated cluster id of the peer that asked,
// taken from its certificate: the broker logs it, because a write that surprises
// an operator has to be traceable to a machine.
type applyModelAliasesParams struct {
	RequestID  string               `json:"requestId"`
	CallerUUID string               `json:"callerUuid"`
	Aliases    []noderec.ModelAlias `json:"aliases"`
}

// applyModelAliasesResult is the owning broker's verdict.
type applyModelAliasesResult struct {
	RequestID string `json:"requestId"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error,omitempty"`
}

// aliasWriteFrame is the body carried on aliasWritePath. It names no caller:
// identity comes from the client certificate the route already requires.
type aliasWriteFrame struct {
	RequestID string                `json:"requestId"`
	Aliases   *[]noderec.ModelAlias `json:"aliases"`
}

// aliasApplier is this node's side of an incoming write: hand the document to
// the broker that owns it, then wait for that broker's verdict so the caller
// gets an answer instead of a receipt.
type aliasApplier struct {
	emit func(method string, params any) error
	// verify is the cluster gate plus the caller's authenticated id: it refreshes
	// membership and returns the cluster id of a pinned peer, false for anyone
	// else. Held as a function so the wait-and-answer behavior can be tested
	// without a mesh.
	verify func(r *http.Request) (string, bool)
	// wait is how long to hold the caller for that verdict.
	wait time.Duration

	mu      sync.Mutex
	pending map[string]chan applyModelAliasesResult
}

func newAliasApplier(mesh *clustertrust.Mesh, emit func(method string, params any) error) *aliasApplier {
	if emit == nil {
		// A manager that never wired its broker pipe refuses a write instead of
		// panicking on a nil call: the caller gets "this node could not take it",
		// which is true, and the node stays up to serve its workloads.
		emit = func(string, any) error { return errors.New("this node has no local settings relay wired") }
	}
	return &aliasApplier{
		emit: emit,
		wait: peerAliasApplyTimeout,
		verify: func(r *http.Request) (string, bool) {
			// Re-derive membership and pins per request, the way the events route
			// does, so a peer removed from the cluster mid-write stops being served
			// without a restart. It also yields the caller's authenticated id.
			mesh.Refresh()
			return mesh.VerifyClientPin(r)
		},
		pending: make(map[string]chan applyModelAliasesResult),
	}
}

// register claims a request id, false when one write with that id is already in
// flight here. The channel is buffered so a verdict can be delivered even if the
// waiting handler has just given up on the clock.
func (a *aliasApplier) register(requestID string) (chan applyModelAliasesResult, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, busy := a.pending[requestID]; busy {
		return nil, false
	}
	ch := make(chan applyModelAliasesResult, 1)
	a.pending[requestID] = ch
	return ch, true
}

func (a *aliasApplier) unregister(requestID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pending, requestID)
}

// deliver hands a verdict to its waiter, false when nobody is waiting — the
// waiter timed out, or the frame belongs to a peer we no longer talk to.
func (a *aliasApplier) deliver(res applyModelAliasesResult) bool {
	a.mu.Lock()
	ch, ok := a.pending[res.RequestID]
	delete(a.pending, res.RequestID)
	a.mu.Unlock()
	if !ok {
		return false
	}
	ch <- res
	return true
}

// handleAliasWrite serves aliasWritePath. The caller is already gated: the
// listener is cluster mTLS and VerifyClientPin has accepted a pinned peer.
func (a *aliasApplier) handleAliasWrite(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.verify(r)
	if !ok {
		http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The route serves one operation, and the frame must still say which: this
	// port carries more than one method, and a caller that dialed the wrong door
	// should be told, not served.
	if msg.Method != MethodAliasWrite {
		http.Error(w, "unexpected method: "+msg.Method, http.StatusBadRequest)
		return
	}
	var frame aliasWriteFrame
	if err := json.Unmarshal(msg.Params, &frame); err != nil {
		http.Error(w, "invalid params: expected {\"requestId\": ..., \"aliases\": [...]} ", http.StatusBadRequest)
		return
	}
	if frame.RequestID == "" {
		http.Error(w, "requestId is required: a write nobody can ask about is a write nobody can confirm", http.StatusBadRequest)
		return
	}
	// An absent list is refused rather than read as an empty one: the whole
	// document is replaced, so guessing here would erase a peer's aliases on a
	// malformed frame.
	if frame.Aliases == nil {
		http.Error(w, `invalid params: "aliases" is required, and an absent list is not an empty list`, http.StatusBadRequest)
		return
	}

	waiter, claimed := a.register(frame.RequestID)
	if !claimed {
		http.Error(w, "another write with this request id is already in flight", http.StatusConflict)
		return
	}
	defer a.unregister(frame.RequestID)

	params := applyModelAliasesParams{RequestID: frame.RequestID, CallerUUID: caller, Aliases: *frame.Aliases}
	if err := a.emit(MethodApplyAliases, params); err != nil {
		slog.Warn("failed to hand a peer alias write to the local settings owner",
			"requestId", frame.RequestID, "callerUuid", caller, "err", err)
		http.Error(w, "this node's settings service is not reachable", http.StatusBadGateway)
		return
	}

	select {
	case res := <-waiter:
		if !res.Applied {
			reason := strings.TrimSpace(res.Error)
			if reason == "" {
				reason = "this node's settings service refused the document"
			}
			// 422: the request reached the authority and the authority said the
			// document is bad. Nothing about retrying will help until it changes.
			http.Error(w, reason, http.StatusUnprocessableEntity)
			return
		}
		slog.Info("applied a peer model alias write", "requestId", frame.RequestID, "callerUuid", caller, "aliases", len(params.Aliases))
		writeJSON(w, peerAliasesResult{RequestID: frame.RequestID, Applied: true})
	case <-time.After(a.wait):
		// The document is with the settings service; we simply have no verdict.
		// Saying "failed" here would be a lie the operator could act on.
		slog.Warn("peer alias write unconfirmed after the local wait",
			"requestId", frame.RequestID, "callerUuid", caller, "waited", a.wait.String())
		http.Error(w, "the change was handed to this node's settings service and not confirmed in time", http.StatusGatewayTimeout)
	case <-r.Context().Done():
	}
}

// noteResult routes the broker's verdict from the stdin read loop to its waiter.
func (a *aliasApplier) noteResult(msg *Message) {
	var res applyModelAliasesResult
	if err := json.Unmarshal(msg.Params, &res); err != nil || res.RequestID == "" {
		slog.Warn("invalid settings/apply-model-aliases-result frame", "err", err)
		return
	}
	if !a.deliver(res) {
		slog.Debug("alias apply result with no waiter", "requestId", res.RequestID)
	}
}

// aliasForwarder is the calling node's side: take the broker's request, dial the
// owning node, report the verdict back.
type aliasForwarder struct {
	broadcaster *Broadcaster
	emit        func(method string, params any) error
	// verify lets the tests and the caller share one membership gate.
	verify func() bool
	// timeout is how long one trip to the owning node may take.
	timeout time.Duration
}

func newAliasForwarder(b *Broadcaster, emit func(method string, params any) error) *aliasForwarder {
	return &aliasForwarder{broadcaster: b, emit: emit, verify: b.mesh.Clustered, timeout: peerAliasTripTimeout}
}

// replacePeer answers MethodReplacePeerAliases. It always reports a verdict for
// a well-formed request, including a failure to reach the node: a request that
// goes unanswered would leave the operator's Save spinning until the broker
// gives up, which is a slower and less specific answer than the one we can give.
func (f *aliasForwarder) replacePeer(ctx context.Context, msg *Message) {
	var p ReplacePeerAliasesParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		slog.Warn("invalid settings/replace-peer-model-aliases frame", "err", err)
		return
	}
	// Without an id there is nothing to correlate a verdict to, and without a
	// target there is nobody to ask; a malformed frame is a broker bug, logged
	// rather than answered.
	if p.RequestID == "" || p.NodeUUID == "" {
		slog.Warn("settings/replace-peer-model-aliases frame names no request or node")
		return
	}
	if p.Aliases == nil {
		f.report(ctx, peerAliasesResult{RequestID: p.RequestID, Error: `the request named no alias document`})
		return
	}
	if !f.verify() {
		f.report(ctx, peerAliasesResult{RequestID: p.RequestID, Error: "this computer is not in a cluster"})
		return
	}

	frame, err := json.Marshal(&Message{
		JSONRPC: "2.0",
		Method:  MethodAliasWrite,
		Params:  marshalParams(aliasWriteFrame{RequestID: p.RequestID, Aliases: p.Aliases}),
	})
	if err != nil {
		f.report(ctx, peerAliasesResult{RequestID: p.RequestID, Error: "could not encode the request: " + err.Error()})
		return
	}

	trip, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	status, body, err := f.broadcaster.PostToPeer(trip, p.NodeUUID, aliasWritePath, append(frame, '\n'))
	switch {
	case err != nil:
		f.report(ctx, peerAliasesResult{RequestID: p.RequestID, Error: "could not reach that node: " + err.Error()})
	case status == http.StatusOK:
		slog.Info("peer model alias write confirmed", "nodeUuid", p.NodeUUID, "requestId", p.RequestID, "aliases", len(*p.Aliases))
		f.report(ctx, peerAliasesResult{RequestID: p.RequestID, Applied: true})
	default:
		reason := strings.TrimSpace(string(body))
		if reason == "" {
			reason = http.StatusText(status)
		}
		slog.Warn("peer model alias write refused",
			"nodeUuid", p.NodeUUID, "requestId", p.RequestID, "status", status, "reason", reason)
		f.report(ctx, peerAliasesResult{RequestID: p.RequestID, Error: truncate(reason, maxAliasReplyBytes)})
	}
}

func (f *aliasForwarder) report(ctx context.Context, res peerAliasesResult) {
	if err := f.emit(MethodPeerAliasesResult, res); err != nil {
		slog.Warn("failed to report a peer alias write to the broker",
			"requestId", res.RequestID, "err", err, "ctxErr", ctx.Err())
	}
}

// truncate keeps a peer's refusal inside a size an error banner can show.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// writeJSON answers an inter-node request with a JSON body. The caller is
// machine-readable by construction, so there is no text fallback.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("failed to write inter-node reply", "err", err)
	}
}

// marshalParams encodes a params object, or nil when it cannot be encoded so
// the frame stays well-formed rather than carrying a broken blob.
func marshalParams(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
