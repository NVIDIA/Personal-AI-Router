// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package rpc implements the client half of the newline-delimited
// JSON-RPC 2.0 protocol every NVPAIR subprocess speaks. The TUI uses it to
// drive nvpair-ui-broker: one JSON object per line, requests carry a numeric
// id and expect a matching response, notifications carry a method but no
// id and are pushed without a reply.
//
// The wire shapes here intentionally mirror nvpair-ui-broker/jsonrpc.go so
// the two can't drift; the only difference is the role (this side is the
// caller that originates requests and consumes the broker's pushes).
package rpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// maxFrame bounds a single JSON-RPC line. The broker uses a 1 MiB read
// buffer; we match it so a large discovery/errors snapshot from the
// broker is never truncated mid-frame.
const maxFrame = 1024 * 1024

// Message is a single JSON-RPC 2.0 frame. A frame is a request when it
// has both an id and a method, a notification when it has a method but no
// id, and a response when it has an id but no method (carrying Result or
// Error).
type Message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is the error object of a JSON-RPC response. The broker uses
// -32000 for "worker not available" relays and -32601 for unknown
// methods; callers should surface Message to the user verbatim.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// IsResponse reports whether the frame carries a response to a request we
// sent (has an id, no method).
func (m *Message) IsResponse() bool { return m.ID != nil && m.Method == "" }

// IsNotification reports whether the frame is a server push (has a
// method, no id).
func (m *Message) IsNotification() bool { return m.ID == nil && m.Method != "" }

// Codec frames JSON-RPC messages over a byte stream. Reads are
// line-oriented; writes are serialised by a mutex so concurrent senders
// can't interleave bytes mid-frame.
type Codec struct {
	scanner *bufio.Scanner
	writer  io.Writer
	wmu     sync.Mutex
}

// DecodeError marks a recoverable per-frame failure (bad JSON or wrong
// version): the stream is still positioned at the next line, so callers
// may skip the frame and continue reading. Mirrors nvpair-shared/jsonrpc.
type DecodeError struct{ Err error }

func (e *DecodeError) Error() string { return e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// NewCodec wraps a reader/writer pair (the broker's stdout/stdin) in a
// framing codec.
func NewCodec(r io.Reader, w io.Writer) *Codec {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, maxFrame), maxFrame)
	return &Codec{scanner: scanner, writer: w}
}

// Read returns the next frame, or io.EOF when the stream closes. A
// malformed frame (bad JSON or wrong version) is returned as a recoverable
// *DecodeError; a terminal scanner/transport error is a plain error.
func (c *Codec) Read() (*Message, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("read error: %w", err)
		}
		return nil, io.EOF
	}
	var msg Message
	if err := json.Unmarshal(c.scanner.Bytes(), &msg); err != nil {
		return nil, &DecodeError{fmt.Errorf("invalid JSON-RPC message: %w", err)}
	}
	if msg.JSONRPC != "2.0" {
		return nil, &DecodeError{fmt.Errorf("unsupported JSON-RPC version: %q", msg.JSONRPC)}
	}
	return &msg, nil
}

// Write marshals and emits a single frame terminated by a newline.
func (c *Codec) Write(msg *Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.writer.Write(data)
	return err
}
