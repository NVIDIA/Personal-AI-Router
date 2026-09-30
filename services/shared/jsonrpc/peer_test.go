// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pipePeers wires two Peers over a synchronous in-memory net.Pipe. Handlers
// that write a reply do so in a goroutine so a blocking write on the
// unbuffered pipe never stalls the read pump (real stdio pipes are buffered).
func pipePeers(t *testing.T) (a, b *Peer) {
	t.Helper()
	c1, c2 := net.Pipe()
	a = NewPeer(NewCodec(c1))
	b = NewPeer(NewCodec(c2))
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	return a, b
}

func TestPeerCallReturnsResult(t *testing.T) {
	a, b := pipePeers(t)
	writeErr := make(chan error, 1)
	go b.Serve(func(req *Message) {
		go func() { writeErr <- b.Respond(req.ID, map[string]string{"echo": req.Method}) }()
	}, nil)
	go a.Serve(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, rpcErr, err := a.Call(ctx, "ping", json.RawMessage(`{"x":1}`))
	require.NoError(t, err, "Call err")
	assert.NoError(t, <-writeErr, "respond to call")
	require.Nil(t, rpcErr, "Call err")
	var out map[string]string
	assert.NoError(t, json.Unmarshal(res, &out), "decode call result")
	assert.Equal(t, "ping", out["echo"], "unexpected result")
}

func TestPeerCallReturnsRPCError(t *testing.T) {
	a, b := pipePeers(t)
	writeErr := make(chan error, 1)
	go b.Serve(func(req *Message) {
		go func() {
			writeErr <- b.RespondError(req.ID, -32601, "method not found")
		}()
	}, nil)
	go a.Serve(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, rpcErr, err := a.Call(ctx, "nope", nil)
	require.NoError(t, err, "transport err")
	assert.NoError(t, <-writeErr, "respond with RPC error")
	require.NotNil(t, rpcErr, "want rpc error -32601")
	assert.Equal(t, -32601, rpcErr.Code)
}

func TestPeerSimultaneousInboundRequest(t *testing.T) {
	// Both peers Call each other while both are serving inbound requests. The
	// read pump is separate from Call, so neither side deadlocks.
	a, b := pipePeers(t)
	writeErrs := make(chan error, 2)
	handler := func(self *Peer) func(*Message) {
		return func(req *Message) {
			go func() {
				writeErrs <- self.Respond(req.ID, map[string]string{"from": req.Method})
			}()
		}
	}
	go a.Serve(handler(a), nil)
	go b.Serve(handler(b), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type res struct {
		raw json.RawMessage
		err error
	}
	ares := make(chan res, 1)
	bres := make(chan res, 1)
	go func() { r, _, e := a.Call(ctx, "a-calls-b", nil); ares <- res{r, e} }()
	go func() { r, _, e := b.Call(ctx, "b-calls-a", nil); bres <- res{r, e} }()

	for _, ch := range []chan res{ares, bres} {
		select {
		case r := <-ch:
			require.NoError(t, r.err, "simultaneous call failed")
			assert.NoError(t, <-writeErrs, "respond to simultaneous call")
		case <-time.After(3 * time.Second):
			require.FailNow(t, "simultaneous calls deadlocked")
		}
	}
}

func TestPeerNotificationOrdering(t *testing.T) {
	a, b := pipePeers(t)
	got := make(chan string, 16)
	go b.Serve(nil, func(method string, _ json.RawMessage) { got <- method })
	go a.Serve(nil, nil)

	const n = 8
	for i := 0; i < n; i++ {
		require.NoError(t, a.Notify(fmt.Sprintf("n%d", i), nil), "Notify")
	}
	for i := 0; i < n; i++ {
		select {
		case m := <-got:
			assert.Equal(t, fmt.Sprintf("n%d", i), m, "out of order")
		case <-time.After(2 * time.Second):
			require.FailNow(t, "timed out waiting for notification")
		}
	}
}

func TestPeerCallCtxCancel(t *testing.T) {
	a, b := pipePeers(t)
	go b.Serve(func(*Message) {}, nil) // never responds
	go a.Serve(nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	_, _, err := a.Call(ctx, "hang", nil)
	assert.ErrorIs(t, err, context.Canceled, "want context.Canceled")
}

func TestPeerCloseWakesPendingCall(t *testing.T) {
	c1, c2 := net.Pipe()
	a := NewPeer(NewCodec(c1))
	b := NewPeer(NewCodec(c2))
	go b.Serve(func(*Message) {}, nil) // never responds
	go a.Serve(nil, nil)

	errc := make(chan error, 1)
	go func() { _, _, e := a.Call(context.Background(), "hang", nil); errc <- e }()
	time.Sleep(50 * time.Millisecond)
	_ = c1.Close() // a's read pump errors -> a.Close() -> pending Call wakes
	_ = c2.Close()

	select {
	case e := <-errc:
		assert.Equal(t, ErrPeerClosed, e)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "pending Call was not woken on close")
	}
}

func TestPeerCallAfterCloseFailsFast(t *testing.T) {
	a, _ := pipePeers(t)
	a.Close()
	_, _, err := a.Call(context.Background(), "m", nil)
	assert.ErrorIs(t, err, ErrPeerClosed, "want ErrPeerClosed after Close")
}

func TestPeerRelayRequest(t *testing.T) {
	a, b := pipePeers(t)
	writeErr := make(chan error, 1)
	go b.Serve(func(req *Message) {
		go func() { writeErr <- b.Respond(req.ID, map[string]int{"ok": 1}) }()
	}, nil)
	go a.Serve(nil, nil)

	type relayed struct {
		res json.RawMessage
		err error
	}
	done := make(chan relayed, 1)
	require.NoError(t, a.RelayRequest("m", nil, func(res json.RawMessage, _ *RPCError, err error) {
		done <- relayed{res, err}
	}), "RelayRequest")
	select {
	case r := <-done:
		require.NoError(t, r.err, "relay err")
		assert.NoError(t, <-writeErr, "respond to relay")
		var out map[string]int
		assert.NoError(t, json.Unmarshal(r.res, &out), "decode relay result")
		assert.Equal(t, 1, out["ok"], "unexpected relay result")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "relay respond not invoked")
	}
}
