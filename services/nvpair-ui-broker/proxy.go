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
	relayDir *relay.Directory
	subMu    sync.Mutex
	subIDs   map[string]int
}

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
		// Every facade's subscription goes, not just one: the process is gone,
		// so a surviving registration would push snapshots at a closed peer.
		pp.subMu.Lock()
		for engine, id := range pp.subIDs {
			if id != 0 && pp.relayDir != nil {
				pp.relayDir.Unsubscribe(id)
			}
			delete(pp.subIDs, engine)
		}
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
	if p.onNotify != nil {
		p.onNotify(method, params)
	}
}

// handleSubscribe wires one facade's discovery:subscribe into the relay
// directory: it registers a subscriber whose Send pushes a discovery:nodes
// snapshot down the proxy's peer, then sends the initial snapshot so the
// facade's routing set is populated immediately. A re-subscribe (e.g. the
// facade resends) drops that facade's prior registration so it isn't
// double-fed.
//
// Subscriptions are tracked per engine, not per process. Each facade filters on
// its own engine's discovery service, so a single id per process would make a
// second facade's subscribe silently replace the first's — leaving that engine
// with a routing set that never updates again.
//
// Snapshots are pushed back addressed to the requesting engine, so the child
// hands each one to the facade that asked for it.
func (p *proxyProcess) handleSubscribe(engine string, params json.RawMessage) {
	if p.relayDir == nil {
		return
	}
	send := func(nodes []noderec.DirectoryNode) {
		method := engines.AddressMethod(engine, noderec.NotifyNodes)
		if err := p.peer.Notify(method, noderec.GetNodesResult{Nodes: nodes}); err != nil {
			slog.Debug("failed to push node snapshot to proxy", "engine", engine, "err", err)
		}
	}
	p.subMu.Lock()
	if p.subIDs == nil {
		p.subIDs = make(map[string]int, len(engines.Names()))
	}
	if prior := p.subIDs[engine]; prior != 0 {
		p.relayDir.Unsubscribe(prior)
	}
	id, sub, err := subscribeRelay(p.relayDir, params, send)
	p.subIDs[engine] = id
	p.subMu.Unlock()
	if err != nil {
		slog.Warn("proxy sent invalid discovery:subscribe", "engine", engine, "err", err)
		return
	}
	p.relayDir.Deliver(sub)
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
