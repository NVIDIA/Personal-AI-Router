// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// notifyBuffer bounds the queue of server pushes awaiting consumption. A
// healthy TUI drains it promptly; the buffer just absorbs bursts (e.g. a
// flurry of discovery/workload events) without forcing the read loop to
// block on the UI between frames.
const notifyBuffer = 256

// Client multiplexes requests and server pushes over a single Codec. It
// assigns monotonic numeric ids to outbound requests, matches each
// response back to its waiting caller, and fans notifications out on a
// channel for the UI to consume. A Client is single-connection: create a
// new one per broker process.
type Client struct {
	codec  *Codec
	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan *Message

	notifications chan *Message

	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
}

// NewClient builds a Client over the broker's stdout (r) and stdin (w).
// Call Run to start the read loop before issuing requests.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{
		codec:         NewCodec(r, w),
		pending:       make(map[int64]chan *Message),
		notifications: make(chan *Message, notifyBuffer),
		done:          make(chan struct{}),
	}
}

// Notifications returns the channel of server-pushed frames (broker
// notifications such as app:ready, errors:update, discovery:nodes-changed).
// The channel is closed when Run returns.
func (c *Client) Notifications() <-chan *Message { return c.notifications }

// Run drives the read loop until the stream closes or ctx is cancelled.
// On exit it unblocks every pending Call and closes the notifications
// channel so consumers observe the disconnect. It is safe to call once.
func (c *Client) Run(ctx context.Context) error {
	defer c.shutdown()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg, err := c.codec.Read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			// A single malformed frame should not kill the session; the
			// broker may emit a frame we don't model. Skip and continue.
			// Anything else (over-long frame, transport error) is terminal:
			// bufio.Scanner cannot resync, so continuing would spin.
			var de *DecodeError
			if errors.As(err, &de) {
				continue
			}
			return err
		}
		switch {
		case msg.IsResponse():
			c.deliverResponse(msg)
		case msg.IsNotification():
			select {
			case c.notifications <- msg:
			case <-ctx.Done():
				return ctx.Err()
			case <-c.done:
				return c.closeErr
			}
		}
	}
}

func (c *Client) deliverResponse(msg *Message) {
	var id int64
	if err := json.Unmarshal(*msg.ID, &id); err != nil {
		return
	}
	c.pendingMu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (c *Client) shutdown() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		close(c.notifications)
	})
}

// Call sends a request and blocks until the matching response arrives,
// ctx is cancelled, or the connection closes. A JSON-RPC error response
// is returned as a non-nil error (*RPCError).
func (c *Client) Call(ctx context.Context, method string, params any) (*Message, error) {
	id := c.nextID.Add(1)
	ch := make(chan *Message, 1)

	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writeRequest(id, method, params); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("connection closed before response to %q", method)
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("connection closed before response to %q", method)
		}
		if msg.Error != nil {
			return msg, msg.Error
		}
		return msg, nil
	}
}

func (c *Client) writeRequest(id int64, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	rawID := json.RawMessage(strconv.FormatInt(id, 10))
	return c.codec.Write(&Message{
		JSONRPC: "2.0",
		ID:      &rawID,
		Method:  method,
		Params:  raw,
	})
}

// Notify sends a fire-and-forget notification (no id, no response).
func (c *Client) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.codec.Write(&Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
	})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}
