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
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// JSON-RPC method names on the wire for the error pipeline. Kept as constants so
// a typo fails to compile rather than silently becoming a dropped frame.
const (
	methodErrorsReport     = "errors:report"
	methodErrorsClear      = "errors:clear"
	methodErrorsUpdate     = "errors:update"
	methodErrorsGetInitial = "errors:get-initial"
)

// errorsCallTimeout bounds how long the broker waits for nvpair-errors to answer a
// relayed request (errors:get-initial) before giving up.
const errorsCallTimeout = 5 * time.Second

// errorsProcess is the broker's handle on its child nvpair-errors: a full
// bidirectional JSON-RPC peer. The broker forwards producers' errors:report /
// errors:clear notifications down its stdin, relays a client's
// errors:get-initial as an id-bearing request, and consumes the errors:update
// notifications nvpair-errors pushes (the full sorted snapshot) to fan back out.
// The id-correlation + read pump live in the shared jsonrpc.Peer; this handle
// owns the OS process lifecycle.
type errorsProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	peer  *Peer
	done  chan struct{}

	// onUpdate, if non-nil, is invoked for every errors:update notification
	// with the verbatim params (the full ServiceError[] snapshot).
	onUpdate func(params json.RawMessage)

	// relayDir is the broker's LAN directory. When nvpair-errors sends a
	// discovery:subscribe, the broker subscribes it here and pushes
	// discovery:nodes snapshots down for its peer-sync set. nil when the relay
	// isn't wired.
	relayDir *relay.Directory
	subMu    sync.Mutex
	subID    int
}

// SetLogLevel forwards an already-validated log level as a log/set-level
// notification (through the peer's codec).
func (e *errorsProcess) SetLogLevel(level string) error {
	return e.peer.Notify(applog.SetLevelMethod, applog.SetLevelParams{Level: level})
}

// Done implements supervisedHandle: the returned channel closes once the
// nvpair-errors process has exited (cmd.Wait returned).
func (e *errorsProcess) Done() <-chan struct{} { return e.done }

// Notify forwards a fire-and-forget notification (producers' errors:report /
// errors:clear) into nvpair-errors.
func (e *errorsProcess) Notify(method string, params any) error {
	return e.peer.Notify(method, params)
}

// Call issues an id-bearing request to nvpair-errors (errors:get-initial) and
// blocks until the response arrives, ctx is cancelled, the process exits, or
// errorsCallTimeout elapses.
func (e *errorsProcess) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	ctx, cancel := context.WithTimeout(ctx, errorsCallTimeout)
	defer cancel()
	return e.peer.Call(ctx, method, params)
}

// startErrors spawns nvpair-errors with --peer-sync plus the broker's current log
// level, hides the console window on Windows, and runs the peer read pump.
// onUpdate (may be nil) is invoked for every errors:update with the verbatim
// params. nvpair-errors' stderr is plumbed to the broker's stderr unmodified.
func startErrors(binaryPath, logLevel string, relayDir *relay.Directory, onUpdate func(params json.RawMessage), extraArgs ...string) (*errorsProcess, error) {
	args := append([]string{"--peer-sync", "--log-level", logLevel}, extraArgs...)
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

	ep := &errorsProcess{
		cmd:      cmd,
		stdin:    stdin,
		peer:     NewPeer(NewCodec(readWriter{stdout, stdin})),
		done:     make(chan struct{}),
		onUpdate: onUpdate,
		relayDir: relayDir,
	}

	go ep.peer.Serve(nil, ep.handleNotify)
	go func() {
		_ = cmd.Wait()
		ep.peer.Close()
		ep.subMu.Lock()
		if ep.subID != 0 && ep.relayDir != nil {
			ep.relayDir.Unsubscribe(ep.subID)
			ep.subID = 0
		}
		ep.subMu.Unlock()
		close(ep.done)
	}()

	return ep, nil
}

// handleNotify demuxes nvpair-errors' notifications: errors:update to onUpdate,
// "ready" logged, anything else dropped at debug.
func (e *errorsProcess) handleNotify(method string, params json.RawMessage) {
	switch method {
	case methodErrorsUpdate:
		if e.onUpdate != nil {
			e.onUpdate(params)
		}
	case noderec.MethodSubscribe:
		// nvpair-errors subscribes upward for its peer-sync set: wire it
		// to the relay directory and push events down its peer.
		e.handleSubscribe(params)
	case "ready":
		slog.Info("nvpair-errors reported ready", "params", string(params))
	default:
		slog.Debug("ignoring nvpair-errors notification", "method", method)
	}
}

// handleSubscribe wires nvpair-errors' discovery:subscribe into the relay directory:
// it registers a subscriber whose Send pushes a discovery:nodes snapshot down the
// peer, then sends the initial snapshot. A re-subscribe drops the prior
// registration first so it isn't double-fed.
func (e *errorsProcess) handleSubscribe(params json.RawMessage) {
	if e.relayDir == nil {
		return
	}
	send := func(nodes []noderec.DirectoryNode) {
		if err := e.peer.Notify(noderec.NotifyNodes, noderec.GetNodesResult{Nodes: nodes}); err != nil {
			slog.Debug("failed to push node snapshot to nvpair-errors", "err", err)
		}
	}
	e.subMu.Lock()
	if e.subID != 0 {
		e.relayDir.Unsubscribe(e.subID)
	}
	id, sub, err := subscribeRelay(e.relayDir, params, send)
	e.subID = id
	e.subMu.Unlock()
	if err != nil {
		slog.Warn("nvpair-errors sent invalid discovery:subscribe", "err", err)
		return
	}
	e.relayDir.Deliver(sub)
}

// Stop signals nvpair-errors to exit by closing its stdin (observed as EOF) and
// waits for it to exit, escalating if it does not — see waitForStdinClose. Safe
// to call multiple times.
func (e *errorsProcess) Stop() {
	waitForStdinClose("errors", e.cmd, e.stdin, e.done)
}
