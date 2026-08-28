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
	go b.Serve(func(req *Message) {
		go b.Respond(req.ID, map[string]string{"echo": req.Method})
	}, nil)
	go a.Serve(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, rpcErr, err := a.Call(ctx, "ping", json.RawMessage(`{"x":1}`))
	if err != nil || rpcErr != nil {
		t.Fatalf("Call err=%v rpcErr=%v", err, rpcErr)
	}
	var out map[string]string
	if json.Unmarshal(res, &out); out["echo"] != "ping" {
		t.Fatalf("unexpected result: %s", res)
	}
}

func TestPeerCallReturnsRPCError(t *testing.T) {
	a, b := pipePeers(t)
	go b.Serve(func(req *Message) {
		go b.RespondError(req.ID, -32601, "method not found")
	}, nil)
	go a.Serve(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, rpcErr, err := a.Call(ctx, "nope", nil)
	if err != nil {
		t.Fatalf("transport err: %v", err)
	}
	if rpcErr == nil || rpcErr.Code != -32601 {
		t.Fatalf("want rpc error -32601, got %+v", rpcErr)
	}
}

func TestPeerSimultaneousInboundRequest(t *testing.T) {
	// Both peers Call each other while both are serving inbound requests. The
	// read pump is separate from Call, so neither side deadlocks.
	a, b := pipePeers(t)
	handler := func(self *Peer) func(*Message) {
		return func(req *Message) { go self.Respond(req.ID, map[string]string{"from": req.Method}) }
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
			if r.err != nil {
				t.Fatalf("simultaneous call failed: %v", r.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("simultaneous calls deadlocked")
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
		if err := a.Notify(fmt.Sprintf("n%d", i), nil); err != nil {
			t.Fatalf("Notify: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		select {
		case m := <-got:
			if m != fmt.Sprintf("n%d", i) {
				t.Fatalf("out of order: got %s want n%d", m, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for notification %d", i)
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
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
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
		if e != ErrPeerClosed {
			t.Fatalf("want ErrPeerClosed, got %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending Call was not woken on close")
	}
}

func TestPeerCallAfterCloseFailsFast(t *testing.T) {
	a, _ := pipePeers(t)
	a.Close()
	if _, _, err := a.Call(context.Background(), "m", nil); err != ErrPeerClosed {
		t.Fatalf("want ErrPeerClosed after Close, got %v", err)
	}
}

func TestPeerRelayRequest(t *testing.T) {
	a, b := pipePeers(t)
	go b.Serve(func(req *Message) {
		go b.Respond(req.ID, map[string]int{"ok": 1})
	}, nil)
	go a.Serve(nil, nil)

	type relayed struct {
		res json.RawMessage
		err error
	}
	done := make(chan relayed, 1)
	if err := a.RelayRequest("m", nil, func(res json.RawMessage, _ *RPCError, err error) {
		done <- relayed{res, err}
	}); err != nil {
		t.Fatalf("RelayRequest: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("relay err: %v", r.err)
		}
		var out map[string]int
		if json.Unmarshal(r.res, &out); out["ok"] != 1 {
			t.Fatalf("unexpected relay result: %s", r.res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay respond not invoked")
	}
}
