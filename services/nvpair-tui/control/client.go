// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"nvpair-shared/ipc"
	"nvpair-shared/jsonrpc"
)

// ErrNotRunning is the "no nvpair-tui is running on this machine" case, which
// every subcommand has to report differently from a request that reached a
// running TUI and failed there.
var ErrNotRunning = errors.New("no running nvpair-tui was found")

// NotRunningError carries ErrNotRunning together with the endpoint that was
// tried and the reason it could not be reached, so the message can name both.
type NotRunningError struct {
	Path string
	Err  error
}

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("no nvpair-tui is listening on %s (%v)", e.Path, e.Err)
}

func (e *NotRunningError) Unwrap() error { return ErrNotRunning }

// Error is a JSON-RPC error the control endpoint returned. Where the request
// was relayed to nvpair-cluster-manager, Code is the manager's own, so a
// caller can key on its documented contract through this endpoint.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Client is one connection to a running TUI's control endpoint.
type Client struct {
	conn io.Closer
	peer *jsonrpc.Peer
}

// Dial connects to the control endpoint at path. A refused or missing
// endpoint means no TUI is running there and comes back as *NotRunningError
// rather than a bare transport error.
func Dial(path string) (*Client, error) {
	conn, err := ipc.Dial(path)
	if err != nil {
		return nil, &NotRunningError{Path: path, Err: err}
	}
	peer := jsonrpc.NewPeer(jsonrpc.NewCodec(conn))
	// The endpoint never sends requests or notifications, so the pump only
	// has to wake Call waiters.
	go peer.Serve(nil, nil)
	return &Client{conn: conn, peer: peer}, nil
}

// Close ends the session. Closing the transport also stops the read pump.
func (c *Client) Close() error {
	c.peer.Close()
	return c.conn.Close()
}

// Call issues one request and returns its raw result. A JSON-RPC error
// response comes back as *Error; anything that stopped the call from
// completing at all comes back as a plain error.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var encoded json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode %s params: %w", method, err)
		}
		encoded = b
	}
	result, rpcErr, err := c.peer.Call(ctx, method, encoded)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if rpcErr != nil {
		return nil, &Error{Code: rpcErr.Code, Message: rpcErr.Message, Data: rpcErr.Data}
	}
	return result, nil
}
