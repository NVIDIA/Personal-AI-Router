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

// remotePullGate holds an engine:remote-cancel-pull until the
// engine:remote-pull-model it targets has reached the peer.
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
// Registering the pull on the read loop, where the client's order still holds,
// is what gives the cancel something to wait for.
type remotePullGate struct {
	mu    sync.Mutex
	pulls map[string]*remotePullAttempt
}

type remotePullAttempt struct {
	// accepted closes when the peer has taken the pull, or when the attempt
	// ended without ever getting that far, so a waiting cancel proceeds either
	// way rather than outliving the download it was chasing.
	accepted chan struct{}
	settle   sync.Once
	// refs counts the requests sharing this attempt. The peer joins a duplicate
	// pull onto the download already in flight, so two can be outstanding.
	refs int
}

// remotePullGateWindow is the backstop under a cancel waiting for its pull to
// reach the peer, not the mechanism. The two signals that actually free it
// both close attempt.accepted: the peer taking the pull, or every request for
// it ending. One of them always arrives, because the pull's own transport
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

func remotePullKey(node, engine, model string) string {
	return node + "\x00" + engine + "\x00" + model
}

// remotePullClaimFrom reports the gate key of an engine:remote-pull-model
// request. Malformed or incomplete params are left to runRemote, which owns the
// error response.
func remotePullClaimFrom(method string, params json.RawMessage) (string, bool) {
	if method != "engine:remote-pull-model" {
		return "", false
	}
	var p remoteParam
	if err := json.Unmarshal(params, &p); err != nil {
		return "", false
	}
	model := p.Model
	if fromParams := modelFromParams(p.Params); fromParams != "" {
		model = fromParams
	}
	if p.Node == "" || p.Engine == "" || model == "" {
		return "", false
	}
	return remotePullKey(p.Node, p.Engine, model), true
}

// register records a remote pull for the cancel that may be chasing it, and
// returns the release to run when the attempt ends. Registering anything that
// is not a remote pull is a no-op.
func (g *remotePullGate) register(key string, isPull bool) func() {
	if !isPull {
		return func() {}
	}
	g.mu.Lock()
	if g.pulls == nil {
		g.pulls = make(map[string]*remotePullAttempt)
	}
	attempt := g.pulls[key]
	if attempt == nil {
		attempt = &remotePullAttempt{accepted: make(chan struct{})}
		g.pulls[key] = attempt
	}
	attempt.refs++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			attempt.refs--
			last := attempt.refs == 0
			if last {
				delete(g.pulls, key)
			}
			g.mu.Unlock()
			// An attempt that ended without the peer accepting it has nothing
			// left for a cancel to chase — but only once every request sharing
			// it has ended. One duplicate failing while another is still on
			// its way to the peer is not the attempt ending, and freeing the
			// cancel there sends it ahead of a pull that may still be
			// accepted. Acceptance has its own closer and does not wait on
			// this count.
			if last {
				attempt.settle.Do(func() { close(attempt.accepted) })
			}
		})
	}
}

// accepted marks the peer as holding this pull, releasing any cancel waiting on
// it. Returns a callback so the caller can hand it straight to remoteClient.stream.
func (g *remotePullGate) accepted(key string) func() {
	return func() {
		g.mu.Lock()
		attempt := g.pulls[key]
		g.mu.Unlock()
		if attempt != nil {
			attempt.settle.Do(func() { close(attempt.accepted) })
		}
	}
}

// awaitAccepted blocks until the pull this cancel targets has reached the peer,
// the attempt ended, or the window elapses. A cancel for a download nobody
// requested finds no attempt and proceeds immediately.
func (g *remotePullGate) awaitAccepted(ctx context.Context, key string) {
	g.mu.Lock()
	attempt := g.pulls[key]
	g.mu.Unlock()
	if attempt == nil {
		return
	}
	timer := time.NewTimer(remotePullGateWindow)
	defer timer.Stop()
	select {
	case <-attempt.accepted:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// runRemote dispatches an engine:remote-* request. It runs on its own goroutine
// (like the other long ops) so the read loop stays responsive during a
// multi-minute remote install/pull.
func (m *Manager) runRemote(ctx context.Context, msg *Message) {
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
		opID := newOpID()
		body := pullRequest{OpID: opID, Engine: p.Engine, Model: p.Model, Params: p.Params}
		key, isPull := remotePullClaimFrom(msg.Method, msg.Params)
		var onAccepted func()
		if isPull {
			onAccepted = m.remotePulls.accepted(key)
		}
		terminal, err := client.stream(ctx, controlPullPath, body, m.remoteProgressFn(opID, peer.nodeID), onAccepted)
		if err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		m.codec.Respond(msg.ID, map[string]any{"opId": opID, "result": terminal.Result})

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
			// Let the download this cancel names reach the peer first; see
			// remotePullGate.
			m.remotePulls.awaitAccepted(ctx, remotePullKey(p.Node, p.Engine, p.Model))
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
