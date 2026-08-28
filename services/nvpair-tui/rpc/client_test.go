// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// newPair wires a Client to an in-memory server side over a full-duplex
// net.Pipe, runs the client read loop, and returns the client, a codec
// for the server end, and that end's connection (close it to simulate the
// broker disconnecting).
func newPair(t *testing.T) (*Client, *Codec, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	client := NewClient(c1, c1)
	ctx, cancel := context.WithCancel(context.Background())
	go client.Run(ctx)
	t.Cleanup(func() {
		cancel()
		c1.Close()
		c2.Close()
	})
	return client, NewCodec(c2, c2), c2
}

func TestCodecRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	a := NewCodec(c1, c1)
	b := NewCodec(c2, c2)

	go func() {
		_ = a.Write(&Message{JSONRPC: "2.0", Method: "hello", Params: json.RawMessage(`{"x":1}`)})
	}()

	msg, err := b.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !msg.IsNotification() || msg.Method != "hello" {
		t.Fatalf("unexpected frame: %+v", msg)
	}
	if string(msg.Params) != `{"x":1}` {
		t.Fatalf("params = %s", msg.Params)
	}
}

func TestClientCallMatchesResponse(t *testing.T) {
	client, server, _ := newPair(t)

	// Server: read the request and echo a result tagged with its id.
	go func() {
		req, err := server.Read()
		if err != nil {
			return
		}
		_ = server.Write(&Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"pong":true}`)})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.Call(ctx, "ping", map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(resp.Result) != `{"pong":true}` {
		t.Fatalf("result = %s", resp.Result)
	}
}

func TestClientCallSurfacesRPCError(t *testing.T) {
	client, server, _ := newPair(t)
	go func() {
		req, err := server.Read()
		if err != nil {
			return
		}
		_ = server.Write(&Message{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32000, Message: "boom"}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Call(ctx, "explode", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if rpcErr, ok := err.(*RPCError); !ok || rpcErr.Code != -32000 {
		t.Fatalf("expected *RPCError -32000, got %v", err)
	}
}

func TestClientDeliversNotifications(t *testing.T) {
	client, server, _ := newPair(t)
	go func() {
		_ = server.Write(&Message{JSONRPC: "2.0", Method: "errors:update", Params: json.RawMessage(`[]`)})
	}()

	select {
	case msg, ok := <-client.Notifications():
		if !ok {
			t.Fatal("notifications channel closed")
		}
		if msg.Method != "errors:update" {
			t.Fatalf("method = %s", msg.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestClientDisconnectClosesNotifications(t *testing.T) {
	client, _, srv := newPair(t)
	srv.Close() // broker drops the connection -> client read loop hits EOF

	done := make(chan struct{})
	go func() {
		// Drain until the channel closes; a closed channel yields the
		// zero value with ok=false.
		for range client.Notifications() {
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifications channel was not closed after disconnect")
	}
}
