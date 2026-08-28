// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
)

// ErrPeerClosed is returned by Call/RelayRequest when the peer's read pump has
// stopped (the underlying stream closed) or Close was called.
var ErrPeerClosed = errors.New("jsonrpc peer closed")

// Peer is a bidirectional JSON-RPC endpoint over a Codec. It can originate
// id-bearing requests (Call/RelayRequest) and, via Serve, run a single read
// pump that demuxes inbound frames: a response wakes the matching Call waiter;
// requests and notifications are dispatched to the caller's handlers.
//
// Peer factors out the id-allocation + pending-response correlation that was
// reimplemented across the broker's worker handles, and it is the piece that
// lets a child originate an upward Call without deadlocking its own read loop
// (the read pump lives in Serve, separate from the Call goroutine).
//
// Peer does NOT own the process/transport lifecycle: the caller builds the
// Codec (e.g. over a child's stdout/stdin), runs Serve in a goroutine, and
// calls Close when the transport goes away. Serve also closes the peer when
// Read returns a terminal error, so in-flight and future Calls fail fast.
type Peer struct {
	codec *Codec

	idCounter atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan *Message
	closed  bool

	closeOnce sync.Once
	closedCh  chan struct{}
}

// NewPeer wraps a Codec with a Peer. Call Serve (usually in a goroutine) to run
// the read pump.
func NewPeer(codec *Codec) *Peer {
	return &Peer{
		codec:    codec,
		pending:  make(map[int64]chan *Message),
		closedCh: make(chan struct{}),
	}
}

// Codec returns the underlying codec (for callers that need raw framing).
func (p *Peer) Codec() *Codec { return p.codec }

// Done returns a channel closed when the peer is closed (read pump stopped or
// Close called).
func (p *Peer) Done() <-chan struct{} { return p.closedCh }

// Notify sends a fire-and-forget notification.
func (p *Peer) Notify(method string, params any) error { return p.codec.Notify(method, params) }

// Respond answers an inbound request with a result.
func (p *Peer) Respond(id *json.RawMessage, result any) error { return p.codec.Respond(id, result) }

// RespondError answers an inbound request with an error.
func (p *Peer) RespondError(id *json.RawMessage, code int, message string) error {
	return p.codec.RespondError(id, code, message)
}

// RespondErrorData answers an inbound request with an error carrying data.
func (p *Peer) RespondErrorData(id *json.RawMessage, code int, message string, data any) error {
	return p.codec.RespondErrorData(id, code, message, data)
}

// Call sends an id-bearing request and blocks until the response arrives, ctx
// is done, or the peer closes. It returns the raw result and any JSON-RPC error
// the peer reported separately; a non-nil error means the call itself couldn't
// complete (transport gone, ctx cancelled/deadline). Callers wanting a fixed
// timeout should pass a ctx with a deadline.
func (p *Peer) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	ch, id, err := p.register()
	if err != nil {
		return nil, nil, err
	}
	defer p.clearPending(id)

	if err := p.writeRequest(id, method, params); err != nil {
		return nil, nil, err
	}

	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-p.closedCh:
		return nil, nil, ErrPeerClosed
	case resp := <-ch:
		return resp.Result, resp.Error, nil
	}
}

// RelayRequest sends an id-bearing request and invokes respond when the
// response arrives (or the peer closes), with NO built-in timeout — for
// long-running relays (e.g. engine install / model pull that report progress
// via push events). respond runs on a dedicated goroutine. Returns an error
// only if the request couldn't be written at all.
func (p *Peer) RelayRequest(method string, params json.RawMessage, respond func(result json.RawMessage, rpcErr *RPCError, err error)) error {
	ch, id, err := p.register()
	if err != nil {
		return err
	}

	if err := p.writeRequest(id, method, params); err != nil {
		p.clearPending(id)
		return err
	}

	go func() {
		defer p.clearPending(id)
		select {
		case <-p.closedCh:
			respond(nil, nil, ErrPeerClosed)
		case resp := <-ch:
			respond(resp.Result, resp.Error, nil)
		}
	}()
	return nil
}

// register allocates a request id and its pending waiter, failing fast if the
// peer is already closed.
func (p *Peer) register() (chan *Message, int64, error) {
	id := p.idCounter.Add(1)
	ch := make(chan *Message, 1)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, 0, ErrPeerClosed
	}
	p.pending[id] = ch
	p.mu.Unlock()
	return ch, id, nil
}

func (p *Peer) writeRequest(id int64, method string, params json.RawMessage) error {
	idRaw := json.RawMessage(strconv.FormatInt(id, 10))
	return p.codec.write(&Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: params})
}

func (p *Peer) clearPending(id int64) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

func (p *Peer) deliverResponse(msg *Message) {
	var id int64
	if err := json.Unmarshal(*msg.ID, &id); err != nil {
		return
	}
	p.mu.Lock()
	ch := p.pending[id]
	p.mu.Unlock()
	if ch != nil {
		ch <- msg // ch is buffered (size 1); the id is unique so this never blocks
	}
}

// Close marks the peer closed and wakes all pending Calls with ErrPeerClosed.
// Idempotent; safe to call from Serve and from a process-exit watcher.
func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.closedCh)
	})
}

// Serve runs the read pump until the underlying stream returns a terminal
// error (e.g. EOF), then closes the peer. onRequest (may be nil) receives
// inbound id-bearing requests; onNotify (may be nil) receives notifications.
// A malformed frame (recoverable *DecodeError) is skipped and the pump
// continues; any other read error is terminal.
//
// Handlers run on the read-pump goroutine, so they must not block on this
// peer's own Call (that would stall the pump); spawn a goroutine for that.
func (p *Peer) Serve(onRequest func(*Message), onNotify func(method string, params json.RawMessage)) {
	defer p.Close()
	for {
		msg, err := p.codec.Read()
		if err != nil {
			var de *DecodeError
			if errors.As(err, &de) {
				continue // recoverable: skip the bad frame
			}
			return // terminal (EOF / transport): stop and close
		}
		switch {
		case msg.IsResponse():
			p.deliverResponse(msg)
		case msg.IsRequest():
			if onRequest != nil {
				onRequest(msg)
			}
		case msg.IsNotification():
			if onNotify != nil {
				onNotify(msg.Method, msg.Params)
			}
		}
	}
}
