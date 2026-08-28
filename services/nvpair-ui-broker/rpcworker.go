// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	"nvpair-shared/applog"
)

// rpcWorkerCallTimeout bounds how long the broker waits for a relayed request
// to be answered before giving up.
const rpcWorkerCallTimeout = 5 * time.Second

// rpcWorker is a generic handle for a supervised child that speaks
// bidirectional newline-delimited JSON-RPC over stdio: the broker can fire
// notifications and id-bearing requests down its stdin and receives the child's
// responses and notifications on its stdout. It backs the workers the broker
// relays a full control plane for (nvpair-engine-manager, nvpair-node-settings,
// nvpair-manual-nodes). The id-correlation + read pump live in the shared
// jsonrpc.Peer; this handle owns the OS process lifecycle.
type rpcWorker struct {
	name     string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	peer     *Peer
	done     chan struct{}
	onNotify func(method string, params json.RawMessage)
}

// Done implements supervisedHandle: the returned channel closes once the worker
// process has exited (cmd.Wait returned).
func (w *rpcWorker) Done() <-chan struct{} { return w.done }

// SetLogLevel forwards an already-validated log level as a log/set-level
// notification (through the peer's codec).
func (w *rpcWorker) SetLogLevel(level string) error {
	return w.peer.Notify(applog.SetLevelMethod, applog.SetLevelParams{Level: level})
}

// Notify writes a fire-and-forget JSON-RPC notification to the worker.
func (w *rpcWorker) Notify(method string, params any) error {
	return w.peer.Notify(method, params)
}

// Call issues an id-bearing request to the worker and blocks until the matching
// response arrives, ctx is cancelled, the worker exits, or rpcWorkerCallTimeout
// elapses. It returns the raw result and any JSON-RPC error the worker reported
// separately, so the broker can relay both straight back to its client.
func (w *rpcWorker) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcWorkerCallTimeout)
	defer cancel()
	return w.peer.Call(ctx, method, params)
}

// CallNoTimeout issues an id-bearing request with no broker-imposed deadline,
// for shutdown coordination where the worker's own bounded logic is the only
// clock (e.g. engine:prepare-shutdown, which returns once engine-manager's
// bounded StopAll completes). Unlike Call it does not wrap ctx in
// rpcWorkerCallTimeout; the call still returns if the worker exits (the peer
// read pump reports ErrPeerClosed) or ctx is cancelled.
func (w *rpcWorker) CallNoTimeout(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	return w.peer.Call(ctx, method, params)
}

// RelayRequest forwards a request to the worker and invokes respond with the
// eventual response, with NO broker-imposed timeout — so a long-running op
// (engine install / model pull, minutes long with progress push events) is not
// cut short. respond runs on a dedicated goroutine; if the worker exits before
// responding it is invoked with jsonrpc.ErrPeerClosed. Returns an error only if
// the request couldn't be written. Used for the transparent engine:* relay.
func (w *rpcWorker) RelayRequest(method string, params json.RawMessage, respond func(result json.RawMessage, rpcErr *RPCError, err error)) error {
	return w.peer.RelayRequest(method, params, respond)
}

// startRPCWorker spawns a child speaking bidirectional JSON-RPC over stdio with
// the given args, hides the console window on Windows, and runs the peer read
// pump. onNotify (may be nil) is invoked for every notification the worker
// emits. The worker's stderr goes to the broker's non-blocking sink (see
// stderrsink.go), so its applog prefix still interleaves with the broker's own
// lines but a stalled reader cannot block the worker's exit.
func startRPCWorker(name, binaryPath string, args []string, onNotify func(method string, params json.RawMessage)) (*rpcWorker, error) {
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

	w := &rpcWorker{
		name:     name,
		cmd:      cmd,
		stdin:    stdin,
		peer:     NewPeer(NewCodec(readWriter{stdout, stdin})),
		done:     make(chan struct{}),
		onNotify: onNotify,
	}

	go w.peer.Serve(nil, w.handleNotify)
	go func() {
		_ = cmd.Wait()
		w.peer.Close()
		close(w.done)
	}()

	return w, nil
}

func (w *rpcWorker) handleNotify(method string, params json.RawMessage) {
	if w.onNotify != nil {
		w.onNotify(method, params)
	}
}

// Stop signals the worker to exit by closing its stdin (observed as EOF) and
// waits for it to exit, escalating if it does not — see waitForStdinClose. Safe
// to call multiple times.
func (w *rpcWorker) Stop() {
	waitForStdinClose(w.name, w.cmd, w.stdin, w.done)
}
