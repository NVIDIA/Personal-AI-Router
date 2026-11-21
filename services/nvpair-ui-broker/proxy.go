// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/engines"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// proxyCallTimeout bounds how long the broker waits for the proxy to answer a
// relayed control-plane request before giving up.
//
// The router/set-enabled relay is the one broker call that consumes this
// budget twice sequentially: once for the apply call into nvpair-proxy, then
// once for the deliberate courier-before-answer of the node's alias document.
// Its worst case is therefore ~2x this constant. That equals the desktop
// caller's default 10 s JSON-RPC budget (the timeoutMs default in
// desktop/src/electron/service-bridge/json-rpc-subprocess.ts): a slow-but-alive
// proxy can make the desktop give up at the same moment the apply succeeds.
// The desktop recovers from card truth, but the budgets touching is a
// deliberate relationship — do not lower one number without re-reading the
// other.
const proxyCallTimeout = 5 * time.Second

// proxyProcess is the broker's handle on its child nvpair-proxy: a full
// bidirectional JSON-RPC peer hosting a facade per enabled engine, each of
// which emits an engine-addressed "ready" notification carrying its bound port
// and accepts id-bearing control-plane requests. The id-correlation + read pump
// live in the shared jsonrpc.Peer; this handle owns the OS process and the
// per-engine readiness/port state.
type proxyProcess struct {
	// name identifies the process in shutdown diagnostics. It is
	// engines.ProxyComponent: one process covers every engine, so there is no
	// per-engine handle to name.
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	peer  *Peer
	done  chan struct{}

	// readyMu guards facadeState, written by the read pump when a facade's
	// addressed "ready" notification arrives and read by the broker to answer
	// <engine>-proxy:get-status / replay the baseline on a fresh subscribe.
	//
	// Keyed by engine because one process hosts several facades on different
	// ports. A single port for the process would be whichever facade readied
	// last, so get-status would hand clients another engine's port.
	readyMu     sync.Mutex
	facadeState map[string]proxyFacadeState

	// onNotify, if non-nil, is invoked for every notification the proxy emits
	// (including "ready") so the broker can forward its event stream.
	onNotify func(method string, params json.RawMessage)

	// relayDir is the broker's LAN directory. When a facade sends a
	// discovery:subscribe, the broker subscribes it here and pushes
	// discovery:nodes snapshots back down for that facade's routing set. nil
	// when the relay isn't wired (older call paths / tests).
	//
	// subIDs is keyed by engine because each facade subscribes for its own
	// engine's discovery service: one id per process would let a second
	// facade's subscribe replace the first's. Nil until the first subscribe.
	// The process-scoped alias feed joins that map under proxyAliasFeedKey.
	relayDir *relay.Directory
	subMu    sync.Mutex
	subIDs   map[string]int
	// pendingNodes retains only the latest complete snapshot per subscription
	// key while one sender writes to stdin. Both fields are protected by subMu.
	pendingNodes map[string][]noderec.DirectoryNode
	sendingNodes bool
}

// proxyAliasFeedKey is the subIDs/pendingNodes key of the proxy process's
// process-scoped alias feed (cluster:subscribe-aliases). It rides the same
// machinery as the per-facade subscriptions — one registration, one latest-
// snapshot-pending slot, the same sender goroutine — because the delivery
// shape is identical; only the notify method differs (sendNodeSnapshots).
//
// It lives in the engine-keyed map because "cluster" is not an engine id:
// SplitAddressedMethod only strips known engine names, so no facade-scoped
// message can ever be keyed this way, and the router's engine tag ("cluster")
// is likewise not routable to a facade.
const proxyAliasFeedKey = "cluster"

// proxyReadyParams mirrors the proxy's "ready" notification payload: a facade
// binds its port synchronously and echoes it here, the authoritative source of
// that facade's listen port.
type proxyReadyParams struct {
	Version string `json:"version"`
	Port    int    `json:"port"`
}

// proxyFacadeState is what one facade has told the broker about itself.
type proxyFacadeState struct {
	ready bool
	port  int
	// params is the verbatim ready payload, replayed to a client that
	// subscribes after the fact so it does not have to wait for the next one.
	params json.RawMessage
}

// SetLogLevel forwards an already-validated log level as a log/set-level
// notification (through the peer's codec).
func (p *proxyProcess) SetLogLevel(level string) error {
	return p.peer.Notify(applog.SetLevelMethod, applog.SetLevelParams{Level: level})
}

// Done implements supervisedHandle: the returned channel closes once the proxy
// process has exited (cmd.Wait returned).
func (p *proxyProcess) Done() <-chan struct{} { return p.done }

// Status reports whether one engine's facade has announced itself ready and, if
// so, the HTTP port it bound (0 until its "ready" arrives).
func (p *proxyProcess) Status(engine string) (bool, int) {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	state := p.facadeState[engine]
	return state.ready, state.port
}

// ReadyParams returns the verbatim payload of one facade's "ready"
// notification, or nil if that engine hasn't announced itself yet.
func (p *proxyProcess) ReadyParams(engine string) json.RawMessage {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	return p.facadeState[engine].params
}

// startProxy spawns nvpair-proxy with the broker's current log level, hides the
// console window on Windows, and runs the peer read pump to capture each
// facade's "ready" notification (and thus its bound port). The process starts
// with no facade; the caller enables them. onNotify (may be nil) is invoked for
// every notification the proxy emits so the broker can forward its event
// stream. Proxy stderr goes to the broker's non-blocking sink (see
// stderrsink.go), so a stalled reader cannot block the proxy's exit.
func startProxy(name, binaryPath, logLevel string, relayDir *relay.Directory, onNotify func(method string, params json.RawMessage), extraArgs ...string) (*proxyProcess, error) {
	args := append([]string{"--log-level", logLevel}, extraArgs...)
	cmd := exec.Command(binaryPath, args...)
	configureSubprocess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = stderrOut

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start %s: %w", binaryPath, err)
	}

	pp := &proxyProcess{
		name:     name,
		cmd:      cmd,
		stdin:    stdin,
		peer:     NewPeer(NewCodec(readWriter{stdout, stdin})),
		done:     make(chan struct{}),
		onNotify: onNotify,
		relayDir: relayDir,
	}

	go pp.peer.Serve(nil, pp.handleNotify)
	go func() {
		_ = cmd.Wait()
		pp.peer.Close()
		// Every subscription goes, not just one: the process is gone, so a
		// surviving registration — facade or process-scoped alias feed — would
		// push snapshots at a closed peer.
		pp.subMu.Lock()
		for engine, id := range pp.subIDs {
			if id != 0 && pp.relayDir != nil {
				pp.relayDir.Unsubscribe(id)
			}
			delete(pp.subIDs, engine)
		}
		pp.pendingNodes = nil
		pp.subMu.Unlock()
		close(pp.done)
	}()

	return pp, nil
}

// handleNotify records the proxy's "ready" port and forwards every notification
// to the broker. A malformed "ready" is logged and not forwarded (matching the
// pre-refactor behavior).
//
// Facade-scoped notifications arrive addressed to their engine, so the address
// is split off before dispatching on the method. The addressed form is what
// goes to onNotify: each consumer re-splits and checks the engine itself,
// rather than trusting a bare method to have come from the engine it expected.
func (p *proxyProcess) handleNotify(method string, params json.RawMessage) {
	engine, bare := engines.SplitAddressedMethod(method)
	if bare == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err != nil {
			slog.Warn("proxy emitted invalid ready payload", "engine", engine, "err", err)
			return
		}
		p.readyMu.Lock()
		if p.facadeState == nil {
			p.facadeState = make(map[string]proxyFacadeState, len(engines.Names()))
		}
		p.facadeState[engine] = proxyFacadeState{
			ready:  true,
			port:   rp.Port,
			params: append(json.RawMessage(nil), params...),
		}
		p.readyMu.Unlock()
		slog.Info("proxy reported ready", "engine", engine, "version", rp.Version, "port", rp.Port)
	}
	// The proxy subscribes upward for its routing targets: wire it to
	// the relay directory and push events down, rather than forwarding this as a
	// client-facing event.
	if bare == noderec.MethodSubscribe {
		p.handleSubscribe(engine, params)
		return
	}
	// The process-scoped counterpart: the alias feed is one per process, not
	// one per facade, so it has no engine address to key on. An engine-service
	// filter would hide every alias announced by a peer that fronts no engine,
	// which is exactly what the feed exists to carry.
	if method == noderec.MethodSubscribePeerAliases {
		p.handleSubscribe(proxyAliasFeedKey, params)
		return
	}
	if p.onNotify != nil {
		p.onNotify(method, params)
	}
}

// handleSubscribe wires one upward subscription from the proxy into the relay
// directory: it registers a subscriber whose Send pushes a discovery:nodes
// snapshot down the proxy's peer, then sends the initial snapshot so the
// facade's routing set is populated immediately. A re-subscribe (e.g. the
// facade resends) drops that subscription's prior registration so it isn't
// double-fed.
//
// key is the subscription's slot in subIDs/pendingNodes: the engine for a
// facade-scoped discovery:subscribe, or proxyAliasFeedKey for the process-
// scoped alias feed. Subscriptions are tracked per key, not per process. Each
// facade filters on its own engine's discovery service, so a single id per
// process would make a second facade's subscribe silently replace the first's
// — leaving that engine with a routing set that never updates again.
//
// Snapshots are pushed back under the method that belongs to the key
// (sendNodeSnapshots): addressed to the requesting engine for a facade, bare
// for the process-scoped feed, so the child hands each one where it belongs.
func (p *proxyProcess) handleSubscribe(key string, params json.RawMessage) {
	if p.relayDir == nil {
		return
	}
	var id int // read and assigned under subMu, including in the callback
	send := func(nodes []noderec.DirectoryNode) {
		p.subMu.Lock()
		defer p.subMu.Unlock()
		if p.subIDs[key] != id {
			return // a delivery from a replaced subscription must not overwrite it
		}
		if p.pendingNodes == nil {
			p.pendingNodes = make(map[string][]noderec.DirectoryNode)
		}
		p.pendingNodes[key] = nodes
		if !p.sendingNodes {
			p.sendingNodes = true
			go p.sendNodeSnapshots()
		}
	}
	p.subMu.Lock()
	if p.subIDs == nil {
		p.subIDs = make(map[string]int, len(engines.Names()))
	}
	if prior := p.subIDs[key]; prior != 0 {
		p.relayDir.Unsubscribe(prior)
	}
	delete(p.pendingNodes, key)
	var sub *relay.Subscriber
	var err error
	id, sub, err = subscribeRelay(p.relayDir, params, send)
	p.subIDs[key] = id
	p.subMu.Unlock()
	if err != nil {
		slog.Warn("proxy sent invalid discovery:subscribe", "key", key, "err", err)
		return
	}
	p.relayDir.Deliver(sub)
}

// The relay serializes initial and live deliveries before they are queued. A
// blocked pipe retains one pending snapshot per subscription, including empty
// sets, while the writer runs outside discovery, relay, and subscription locks.
func (p *proxyProcess) sendNodeSnapshots() {
	for {
		p.subMu.Lock()
		key := ""
		var nodes []noderec.DirectoryNode
		for candidate, snapshot := range p.pendingNodes {
			key, nodes = candidate, snapshot
			delete(p.pendingNodes, candidate)
			break
		}
		if key == "" {
			p.sendingNodes = false
			p.subMu.Unlock()
			return
		}
		p.subMu.Unlock()
		method := engines.AddressMethod(key, noderec.NotifyNodes)
		if key == proxyAliasFeedKey {
			method = noderec.NotifyPeerAliases
		}
		if err := p.peer.Notify(method, noderec.GetNodesResult{Nodes: nodes}); err != nil {
			slog.Debug("failed to push node snapshot to proxy", "subscription", key, "err", err)
		}
	}
}

// Call issues an id-bearing control-plane request to the proxy and blocks until
// the response arrives, ctx is cancelled, the proxy exits, or proxyCallTimeout
// elapses.
func (p *proxyProcess) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	if p == nil || p.peer == nil {
		return nil, nil, fmt.Errorf("proxy peer not available")
	}
	ctx, cancel := context.WithTimeout(ctx, proxyCallTimeout)
	defer cancel()
	return p.peer.Call(ctx, method, params)
}

// Stop signals the proxy to exit by closing its stdin (observed as EOF; the
// proxy drains its HTTP server) and waits for it to exit, escalating if it does
// not — see waitForStdinClose. Safe to call multiple times.
func (p *proxyProcess) Stop() {
	waitForStdinClose(p.name, p.cmd, p.stdin, p.done)
}
