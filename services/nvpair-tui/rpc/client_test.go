// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "read")
	assert.True(t, msg.IsNotification(), "unexpected frame (%v)", msg)
	assert.Equal(t, "hello", msg.Method, "unexpected frame (%v)", msg)
	assert.Equal(t, `{"x":1}`, string(msg.Params))
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
	require.NoError(t, err, "call")
	assert.Equal(t, `{"pong":true}`, string(resp.Result))
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
	require.Error(t, err, "expected error")
	rpcErr, ok := err.(*RPCError)
	require.True(t, ok, "expected *RPCError -32000 (%v)", err)
	assert.Equal(t, -32000, rpcErr.Code, "expected *RPCError -32000 (%v)", err)
}

// recordingWriter counts writes, to show a refused request never reached the
// wire.
type recordingWriter struct{ writes int }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes++
	return len(p), nil
}

// TestClientCallRefusesBeforeWriting checks a request is not sent when its
// caller has already given up or the connection is gone. Sent, the broker
// would carry it out regardless — for a mutation, while the caller reported
// that it had not happened.
func TestClientCallRefusesBeforeWriting(t *testing.T) {
	w := &recordingWriter{}
	client := NewClient(strings.NewReader(""), w)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Call(ctx, "engine:apply-settings", nil)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, w.writes, "a cancelled call wrote frames")

	// Run returns at once on the empty stream, which closes the connection.
	_ = client.Run(context.Background())
	_, err = client.Call(context.Background(), "engine:apply-settings", nil)
	assert.Error(t, err, "a call on a closed connection returned no error")
	assert.Zero(t, w.writes, "a call on a closed connection wrote frames")
}

func TestClientDeliversNotifications(t *testing.T) {
	client, server, _ := newPair(t)
	go func() {
		_ = server.Write(&Message{JSONRPC: "2.0", Method: "errors:update", Params: json.RawMessage(`[]`)})
	}()

	select {
	case msg, ok := <-client.Notifications():
		require.True(t, ok, "notifications channel closed")
		assert.Equal(t, "errors:update", msg.Method)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for notification")
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
		require.FailNow(t, "notifications channel was not closed after disconnect")
	}
}
