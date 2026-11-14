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
// peer, and a pull sent after a cancel until that cancel has ended.
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
//
// The order holds the other way too. The peer's cancel stops every request for
// the download it holds when it lands, so a retry that overtook it there — the
// cancel still held here, or slower on its own connection — would be stopped
// along with the download it was sent to replace. A pull therefore waits for
// every cancel of its download sent before it to end, which also leaves the
// peer to finish stopping and cleaning up before the retry arrives.
type remotePullGate struct {
	mu      sync.Mutex
	pulls   map[remotePullKey][]*remotePullRequest
	cancels map[remotePullKey][]*remoteCancelRequest
	// generations counts the cancels placed for each download while any
	// request for it is outstanding; see remotePullRequest.generation.
	generations map[remotePullKey]uint64
}

// remotePullRequest is one engine:remote-pull-model on its way to the peer.
type remotePullRequest struct {
	// cancelsAhead are the cancels of its download sent before it, which it
	// lets finish before it sets off for the peer.
	cancelsAhead []*remoteCancelRequest
	// landed closes when the peer has taken the request, or when the request
	// ended without getting that far, so a cancel waiting on it proceeds either
	// way rather than outliving the download it was chasing.
	landed chan struct{}
	settle sync.Once
	// generation counts the cancels of its download placed before it. Pulls
	// with the same generation were sent with no cancel between them.
	generation uint64
	// download is the peer's ID for the download the request runs as there,
	// once the request's stream has carried it. Guarded by the gate's mu.
	download uint64
}

func (r *remotePullRequest) land() { r.settle.Do(func() { close(r.landed) }) }

// sharesDownload reports whether two pulls are for one download on the peer.
// Once the peer has named the download each runs as, that settles it. Until
// then the generations decide: a duplicate sent with no cancel between joins
// the download in flight, and a pull sent after a cancel is a retry, which the
// peer holds until the cancelled download has stopped and then runs as a
// download of its own. Called with the gate's mu held.
func (r *remotePullRequest) sharesDownload(other *remotePullRequest) bool {
	if r.download != 0 && other.download != 0 {
		return r.download == other.download
	}
	return r.generation == other.generation
}

// awaitCancelsAhead waits until every cancel the request was sent after has
// ended, or the request has landed or ended without them, reporting false if
// ctx ended first. It has no window of its own: each cancel ends by itself,
// once the peer answers or its transport gives up, and letting the request go
// sooner would hand that cancel the retry to stop.
func (r *remotePullRequest) awaitCancelsAhead(ctx context.Context) bool {
	for _, cancel := range r.cancelsAhead {
		select {
		case <-cancel.ended:
		case <-r.landed:
			return true
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// remoteCancelRequest is one engine:remote-cancel-pull that has not ended.
type remoteCancelRequest struct {
	// ended closes once the cancel has its answer from the peer, or has given
	// up on getting one.
	ended chan struct{}
}

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
// cancel arrives rather than when the pull did — and for a pull held behind an
// earlier cancel, only once that cancel has ended and the pull has set off.
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
	// cancel is a remote cancel's own entry.
	cancel *remoteCancelRequest
	// ahead are, for a cancel, the pulls of its download sent before it.
	ahead []*remotePullRequest
}

// place puts a request in the gate: a remote pull joins the pulls of its
// download and notes the cancels already there, and a cancel joins the cancels
// and notes the pulls. It runs on the read loop, where the client's order
// still holds.
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
		order.cancel = &remoteCancelRequest{ended: make(chan struct{})}
		if g.cancels == nil {
			g.cancels = make(map[remotePullKey][]*remoteCancelRequest)
		}
		g.cancels[key] = append(g.cancels[key], order.cancel)
		if g.generations == nil {
			g.generations = make(map[remotePullKey]uint64)
		}
		g.generations[key]++
		return order
	}
	order.pull = &remotePullRequest{
		cancelsAhead: slices.Clone(g.cancels[key]),
		landed:       make(chan struct{}),
		generation:   g.generations[key],
	}
	if g.pulls == nil {
		g.pulls = make(map[remotePullKey][]*remotePullRequest)
	}
	g.pulls[key] = append(g.pulls[key], order.pull)
	return order
}

// runsAs records the peer's ID for the download a remote pull runs as there,
// from a frame of its stream. A frame without one changes nothing.
func (o remoteOrder) runsAs(download uint64) {
	if o.pull == nil || download == 0 {
		return
	}
	o.gate.mu.Lock()
	defer o.gate.mu.Unlock()
	o.pull.download = download
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
	// A pull still held behind an earlier cancel has not set off for the peer,
	// so the window starts once every pull ahead is free to go.
	for _, pull := range o.ahead {
		if !pull.awaitCancelsAhead(ctx) {
			return
		}
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

// awaitCancelsAhead holds a pull until every cancel of its download sent before
// it has ended.
func (o remoteOrder) awaitCancelsAhead(ctx context.Context) {
	if o.pull != nil {
		o.pull.awaitCancelsAhead(ctx)
	}
}

// end takes a request out of the gate. A pull that ended frees any cancel
// still waiting on it — it has nothing left for one to chase — and end reports
// whether it was the last pull of its download outstanding here; see
// sharesDownload. A cancel that ended frees the pulls sent after it. Only the
// first call for a request does anything.
func (o remoteOrder) end() (last bool) {
	if o.cancel != nil {
		o.gate.endCancel(o.key, o.cancel)
		return false
	}
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
	last = !slices.ContainsFunc(pulls, o.pull.sharesDownload)
	if len(pulls) == 0 {
		delete(g.pulls, o.key)
		g.forgetGenerations(o.key)
	} else {
		g.pulls[o.key] = pulls
	}
	g.mu.Unlock()
	o.pull.land()
	return last
}

// endCancel takes a cancel out of the gate and frees the pulls waiting on it.
func (g *remotePullGate) endCancel(key remotePullKey, cancel *remoteCancelRequest) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cancels := g.cancels[key]
	i := slices.Index(cancels, cancel)
	if i < 0 {
		return
	}
	cancels = slices.Delete(cancels, i, i+1)
	if len(cancels) == 0 {
		delete(g.cancels, key)
		g.forgetGenerations(key)
	} else {
		g.cancels[key] = cancels
	}
	close(cancel.ended)
}

// forgetGenerations drops a download's count of cancels once no request for
// it is outstanding, leaving no pull for a later one to be compared with.
// Called with mu held.
func (g *remotePullGate) forgetGenerations(key remotePullKey) {
	if len(g.pulls[key]) == 0 && len(g.cancels[key]) == 0 {
		delete(g.generations, key)
	}
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
// for the reply has to go on. The UIs know a download's frames by node, engine
// and model alone, so while another pull of the same download is outstanding
// here its frames speak for this one too, and only the last of them to end
// sends that frame. Every pull's stream carries the peer's copy, so each holds
// its copy until it knows whether it is the last. A retry sent after a cancel
// runs as a download of its own, so the cancelled pull does not wait for it;
// see remotePullRequest.sharesDownload. The peer names the download each frame
// is from, and the stream records it for that.
//
// The last sends the copy only if it agrees with its own result. The peer's
// progress hub drops what a slow stream has no room for, the outcome included,
// and the stream can end before its result arrives. The result is the
// authority, so otherwise the frame is built from it, the way emitPullOutcome
// builds a local pull's.
func (m *Manager) pullRemote(ctx context.Context, msg *Message, client *remoteClient, node string, p remoteParam, order remoteOrder) {
	// Let the cancels this pull was sent after finish first; see
	// remotePullGate.
	order.awaitCancelsAhead(ctx)
	opID := newOpID()
	body := pullRequest{OpID: opID, Engine: p.Engine, Model: p.Model, Params: p.Params}
	relay := m.remoteProgressFn(opID, node)
	var outcome *streamFrame
	terminal, err := client.stream(ctx, controlPullPath, body, func(f streamFrame) {
		order.runsAs(f.Download)
		if f.Op == "pull" && isPullOutcome(f.Stage) {
			outcome = &f
			return
		}
		relay(f)
	}, order.accepted())
	order.runsAs(terminal.Download)
	if order.end() {
		if outcome != nil && outcome.Stage == remotePullOutcomeStage(terminal, err) {
			relay(*outcome)
		} else {
			m.notifyRemotePullOutcome(opID, node, p.Engine, order.key.model, terminal, err)
		}
	}
	if err != nil {
		m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	m.codec.Respond(msg.ID, map[string]any{"opId": opID, "result": terminal.Result})
}

// remotePullOutcomeStage is the stage of the frame a remote pull ends on, from
// how its stream settled: "error" when it failed, "cancelled" when the peer's
// result says so, "success" otherwise.
func remotePullOutcomeStage(terminal streamFrame, err error) string {
	switch {
	case err != nil:
		return "error"
	case pullResultCancelled(terminal.Result):
		return "cancelled"
	default:
		return "success"
	}
}

// notifyRemotePullOutcome sends the engine:remote-progress frame a remote pull
// ends on, built from how its stream settled; a failure carries its reason.
func (m *Manager) notifyRemotePullOutcome(opID, node, engine, model string, terminal streamFrame, err error) {
	p := remoteProgress{Model: model, OpID: opID, Node: node, Engine: engine, Op: "pull", Message: model,
		Stage: remotePullOutcomeStage(terminal, err)}
	switch p.Stage {
	case "error":
		p.Percent, p.Message = -1, err.Error()
	case "success":
		p.Percent = 100
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
