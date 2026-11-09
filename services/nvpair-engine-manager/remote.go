// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// remote.go implements the engine:remote-* methods: the local engine-manager
// acting as a client that drives a peer's ec surface on behalf of a UI. The
// broker relays engine:* verbatim, so these arrive here directly. Each resolves
// the target node in the ec peer directory, dials it over pinned mTLS, and (for
// the streaming ops) relays progress up as engine:remote-progress before
// settling the request with the terminal result.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

// remoteParam is the shared input for the engine:remote-* methods. Fields not
// relevant to a given method are ignored.
type remoteParam struct {
	Node   string          `json:"node"`
	Engine string          `json:"engine,omitempty"`
	Start  bool            `json:"start,omitempty"`
	Port   int             `json:"port,omitempty"`
	Model  string          `json:"model,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// remoteProgress is the engine:remote-progress notification payload the broker
// forwards to a subscribed UI.
type remoteProgress struct {
	Model   string `json:"model,omitempty"`
	OpID    string `json:"opId"`
	Node    string `json:"node"`
	Engine  string `json:"engine,omitempty"`
	Op      string `json:"op,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Percent int    `json:"percent,omitempty"`
	Message string `json:"message,omitempty"`
}

// newOpID mints a random correlation id for a remote operation.
func newOpID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// remotePullGate holds an engine:remote-cancel-pull until every
// engine:remote-pull-model of the same download sent before it has reached the
// peer.
//
// Both methods dispatch their own goroutine from the same read loop, so nothing
// downstream preserves the order the UI sent them in. A cancel that wins that
// race arrives at the peer with no download registered and nothing claimed, is
// answered as a cancel for nothing, and leaves the transfer running under a row
// stuck on "Canceling". Executor.claimPull is the local cure for exactly this,
// and it cannot help here: the claim that matters belongs to the peer.
//
// handlePull claims the pull before streamOp writes the stream's response
// header, so that header arriving is proof the peer will recognize the cancel.
// Placing both requests on the read loop, where the client's order still holds,
// is what gives the cancel something to wait for, and keeps a pull sent after
// it — a retry — out of what it waits for.
//
// Each request is tracked on its own. The peer joins a duplicate onto the
// download already in flight, but only once the duplicate gets there: a cancel
// let go when the first request landed can overtake a second still on its way,
// stop the download, and leave the second to start it over.
type remotePullGate struct {
	mu    sync.Mutex
	pulls map[remotePullKey][]*remotePullRequest
}

// remotePullRequest is one engine:remote-pull-model on its way to the peer.
type remotePullRequest struct {
	// landed closes when the peer has taken the request, or when the request
	// ended without getting that far, so a cancel waiting on it proceeds either
	// way rather than outliving the download it was chasing.
	landed chan struct{}
	settle sync.Once
}

func (r *remotePullRequest) land() { r.settle.Do(func() { close(r.landed) }) }

// remotePullGateWindow is the backstop under a cancel waiting for its pulls to
// reach the peer, not the mechanism. The two signals that actually free it
// both land a pull the cancel waits on: the peer taking it, or the request
// ending. One of them always arrives, because the pull's own transport
// gives up by itself — 30s to dial and 30s for the handshake, clustertrust's
// fallback when no PeerClientOptions.Timeout is set, then
// remoteResponseHeaderTimeout for the header, which is the point stream
// reports acceptance.
//
// Those three phases chain only on success, so the sum is a ceiling the pull
// cannot actually reach: getting as far as the header wait means the dial and
// the handshake each finished inside their own 30s, putting the failure
// strictly under this. The cancel's timer then starts later still, when the
// cancel arrives rather than when the pull did.
//
// A timer that fires first is the bug rather than the safety net: it frees the
// cancel while the pull is still legitimately in flight, and a cancel arriving
// at a peer that has nothing registered is answered as a cancel for nothing,
// leaving the transfer running under a row stuck on "Canceling". Waiting is
// the cheaper error, because a pull that never lands releases this itself.
const remotePullGateWindow = 90 * time.Second

// remotePullKey identifies a download on a peer: one node's engine fetching one
// model.
type remotePullKey struct {
	node   string
	engine string
	model  string
}

// remotePullKeyFrom reports the download an engine:remote-pull-model or
// engine:remote-cancel-pull names. A pull can name its model inside params, as
// the peer reads it; a cancel names it at the top level, as runRemote sends it.
// Malformed or incomplete params name none and are left to runRemote, which
// owns the error response.
func remotePullKeyFrom(method string, params json.RawMessage) (remotePullKey, bool) {
	if method != "engine:remote-pull-model" && method != "engine:remote-cancel-pull" {
		return remotePullKey{}, false
	}
	var p remoteParam
	if err := json.Unmarshal(params, &p); err != nil {
		return remotePullKey{}, false
	}
	model := p.Model
	if method == "engine:remote-pull-model" {
		if fromParams := modelFromParams(p.Params); fromParams != "" {
			model = fromParams
		}
	}
	if p.Node == "" || p.Engine == "" || model == "" {
		return remotePullKey{}, false
	}
	return remotePullKey{node: p.Node, engine: p.Engine, model: model}, true
}

// remoteOrder is an engine:remote-* request's place in remotePullGate. Only a
// remote pull and a remote cancel take one; any other request's does nothing.
type remoteOrder struct {
	gate *remotePullGate
	key  remotePullKey
	// pull is a remote pull's own entry.
	pull *remotePullRequest
	// ahead are, for a cancel, the pulls of its download sent before it.
	ahead []*remotePullRequest
}

// place puts a request in the gate: a remote pull joins the pulls of its
// download, and a cancel notes the ones already there. It runs on the read
// loop, where the client's order still holds.
func (g *remotePullGate) place(method string, params json.RawMessage) remoteOrder {
	key, ok := remotePullKeyFrom(method, params)
	if !ok {
		return remoteOrder{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	order := remoteOrder{gate: g, key: key}
	if method == "engine:remote-cancel-pull" {
		order.ahead = slices.Clone(g.pulls[key])
		return order
	}
	order.pull = &remotePullRequest{landed: make(chan struct{})}
	if g.pulls == nil {
		g.pulls = make(map[remotePullKey][]*remotePullRequest)
	}
	g.pulls[key] = append(g.pulls[key], order.pull)
	return order
}

// accepted is the callback remoteClient.stream runs once the peer has taken a
// remote pull, freeing the cancels waiting on it. Nothing else has one.
func (o remoteOrder) accepted() func() {
	if o.pull == nil {
		return nil
	}
	return o.pull.land
}

// awaitAhead holds a cancel until every pull it was sent after has reached the
// peer or ended, or the window elapses. A cancel for a download nobody
// requested has nothing ahead of it and proceeds at once.
func (o remoteOrder) awaitAhead(ctx context.Context) {
	if len(o.ahead) == 0 {
		return
	}
	timer := time.NewTimer(remotePullGateWindow)
	defer timer.Stop()
	for _, pull := range o.ahead {
		select {
		case <-pull.landed:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// end takes a remote pull out of the gate, freeing any cancel still waiting on
// it — a pull that ended has nothing left for one to chase — and reports
// whether it was the last pull of its download outstanding here. Only the
// first call for a pull does anything.
func (o remoteOrder) end() (last bool) {
	if o.pull == nil {
		return false
	}
	g := o.gate
	g.mu.Lock()
	pulls := g.pulls[o.key]
	i := slices.Index(pulls, o.pull)
	if i < 0 {
		g.mu.Unlock()
		return false
	}
	pulls = slices.Delete(pulls, i, i+1)
	last = len(pulls) == 0
	if last {
		delete(g.pulls, o.key)
	} else {
		g.pulls[o.key] = pulls
	}
	g.mu.Unlock()
	o.pull.land()
	return last
}

// runRemote dispatches an engine:remote-* request. It runs on its own goroutine
// (like the other long ops) so the read loop stays responsive during a
// multi-minute remote install/pull. order is the place the read loop gave the
// request in remotePullGate.
func (m *Manager) runRemote(ctx context.Context, msg *Message, order remoteOrder) {
	var p remoteParam
	if !m.parse(msg, &p) {
		return
	}
	if p.Node == "" {
		m.codec.RespondError(msg.ID, -32602, "node is required")
		return
	}
	peer, ok := m.peers.lookup(p.Node)
	if !ok {
		m.codec.RespondError(msg.ID, -32000, "node "+p.Node+" is not a discovered ec peer")
		return
	}
	client, err := m.remoteClient(ctx, peer)
	if err != nil {
		m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}

	switch msg.Method {
	case "engine:remote-get-installed":
		res, err := client.getEngines(ctx)
		m.respondOrErr(msg, res, err)

	case "engine:remote-install":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		opID := newOpID()
		body := installRequest{OpID: opID, Engine: p.Engine, Start: p.Start}
		terminal, err := client.stream(ctx, controlInstallPath, body, m.remoteProgressFn(opID, peer.nodeID), nil)
		if err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		m.codec.Respond(msg.ID, map[string]any{"opId": opID, "status": terminal.Status})

	case "engine:remote-pull-model":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		if p.Model == "" && len(p.Params) == 0 {
			m.codec.RespondError(msg.ID, -32602, "model or params is required")
			return
		}
		m.pullRemote(ctx, msg, client, peer.nodeID, p, order)

	case "engine:remote-load-model", "engine:remote-unload-model", "engine:remote-delete-model", "engine:remote-cancel-pull":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		if p.Model == "" {
			m.codec.RespondError(msg.ID, -32602, "model is required")
			return
		}
		path := controlLoadPath
		switch msg.Method {
		case "engine:remote-cancel-pull":
			path = controlCancelPullPath
			// Let the pulls this cancel was sent after reach the peer first;
			// see remotePullGate.
			order.awaitAhead(ctx)
		case "engine:remote-unload-model":
			path = controlUnloadPath
		case "engine:remote-delete-model":
			path = controlDeletePath
		}
		res, err := client.postJSON(ctx, path, p.Engine, modelActionRequest{Engine: p.Engine, Model: p.Model})
		m.respondOrErr(msg, res, err)

	case "engine:remote-start":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		res, err := client.postJSON(ctx, controlStartPath, p.Engine, startRequest{Engine: p.Engine, Port: p.Port})
		m.respondOrErr(msg, res, err)

	case "engine:remote-stop":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		res, err := client.postJSON(ctx, controlStopPath, p.Engine, stopRequest{Engine: p.Engine})
		m.respondOrErr(msg, res, err)

	default:
		m.codec.RespondError(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

// pullRemote streams a model pull from the peer, relaying its progress, and
// settles the request with the peer's result.
//
// A download ends on a frame saying how, which is all a UI that stopped waiting
// for the reply has to go on. The peer sends that frame through its progress
// hub, which drops what a slow stream has no room for; the result travels
// separately and always arrives. So a stream that carried no such frame gets
// one built from the result, the way emitPullOutcome builds a local pull's.
// The UIs know a download's frames by node, engine and model alone, so while
// another pull of the same download is outstanding here its frames speak for
// this one too, and only the last of them to end sends it.
func (m *Manager) pullRemote(ctx context.Context, msg *Message, client *remoteClient, node string, p remoteParam, order remoteOrder) {
	opID := newOpID()
	body := pullRequest{OpID: opID, Engine: p.Engine, Model: p.Model, Params: p.Params}
	relay := m.remoteProgressFn(opID, node)
	reported := false
	terminal, err := client.stream(ctx, controlPullPath, body, func(f streamFrame) {
		reported = reported || (f.Op == "pull" && isPullOutcome(f.Stage))
		relay(f)
	}, order.accepted())
	if order.end() && !reported {
		m.notifyRemotePullOutcome(opID, node, p.Engine, order.key.model, terminal, err)
	}
	if err != nil {
		m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	m.codec.Respond(msg.ID, map[string]any{"opId": opID, "result": terminal.Result})
}

// notifyRemotePullOutcome sends the engine:remote-progress frame a remote pull
// ends on, from how its stream settled: "error" with the reason when it
// failed, "cancelled" when the peer's result says so, "success" otherwise.
func (m *Manager) notifyRemotePullOutcome(opID, node, engine, model string, terminal streamFrame, err error) {
	p := remoteProgress{Model: model, OpID: opID, Node: node, Engine: engine, Op: "pull", Message: model}
	switch {
	case err != nil:
		p.Stage, p.Percent, p.Message = "error", -1, err.Error()
	case pullResultCancelled(terminal.Result):
		p.Stage = "cancelled"
	default:
		p.Stage, p.Percent = "success", 100
	}
	_ = m.codec.Notify("engine:remote-progress", p)
}

// remoteProgressFn returns an onProgress callback that relays a peer's stream
// frames up as engine:remote-progress notifications, stamped with our opId and
// the target node id.
func (m *Manager) remoteProgressFn(opID, node string) func(streamFrame) {
	return func(f streamFrame) {
		p := remoteProgress{
			Model: f.Model,
			OpID:  opID, Node: node, Engine: f.Engine, Op: f.Op,
			Stage: f.Stage, Message: f.Message,
		}
		if wirePercentIncluded(f.Percent) {
			p.Percent = f.Percent
		}
		_ = m.codec.Notify("engine:remote-progress", p)
	}
}
