// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package jsonrpc is the shared newline-delimited JSON-RPC 2.0 codec used by
// every NVPAIR subprocess over stdio (or an --ipc socket/pipe).
//
// It is a drop-in extraction of the per-service codec that was copy-pasted
// across ~10 binaries: the wire types (Message, RPCError), the request/
// notification/response classifiers, and the Codec itself (newline-framed
// read + concurrency-safe, short-write-safe write). A worker that only sends
// notifications and responds to requests can adopt this with no behavior
// change; the bidirectional peer/correlation layer (a child originating
// id-bearing requests upward) is built separately on top of these types.
package jsonrpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Message is a single JSON-RPC 2.0 frame. A frame is a request (id+method), a
// notification (method, no id), or a response (id, no method) — see the
// IsRequest/IsNotification/IsResponse classifiers.
type Message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// DecodeError marks a recoverable per-frame decode failure: the scanner
// advanced past the bad line, so the next Read can continue. Terminal
// transport/scanner errors are returned as plain errors instead, so a read
// loop stops rather than spinning on a permanently-failing read. Callers that
// want to continue-on-bad-frame can type-assert / errors.As for *DecodeError.
type DecodeError struct{ Err error }

func (e *DecodeError) Error() string { return e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// IsRequest reports whether the frame is an id-bearing request.
func (m *Message) IsRequest() bool {
	return m.ID != nil && m.Method != ""
}

// IsNotification reports whether the frame is a fire-and-forget notification.
func (m *Message) IsNotification() bool {
	return m.ID == nil && m.Method != ""
}

// IsResponse reports whether the frame is a response to a request (id, no
// method). Used by the parent-side demux and any bidirectional peer.
func (m *Message) IsResponse() bool {
	return m.ID != nil && m.Method == ""
}

// Codec handles newline-delimited JSON-RPC 2.0 over an io.ReadWriter.
// Writes are safe for concurrent use.
type Codec struct {
	scanner *bufio.Scanner
	writer  io.Writer
	wmu     sync.Mutex
}

// NewCodec wraps rw with a Codec. Reads accept frames up to 1 MiB.
func NewCodec(rw io.ReadWriter) *Codec {
	scanner := bufio.NewScanner(rw)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	return &Codec{
		scanner: scanner,
		writer:  rw,
	}
}

// NewCodecMaxFrame is like NewCodec but caps a single inbound frame at
// maxFrame bytes (initial buffer 64 KiB, grown on demand). Used by peers that
// exchange larger frames than the 1 MiB default (e.g. nvpair-engine-manager). An
// inbound frame larger than maxFrame surfaces as a terminal scanner read error
// (bufio.Scanner cannot resync past an over-long line), so the read loop exits
// cleanly rather than spinning.
func NewCodecMaxFrame(rw io.ReadWriter, maxFrame int) *Codec {
	scanner := bufio.NewScanner(rw)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrame)
	return &Codec{
		scanner: scanner,
		writer:  rw,
	}
}

// Read returns the next frame, or io.EOF when the stream closes. A malformed
// frame (bad JSON or wrong version) is returned as a recoverable *DecodeError;
// a terminal scanner/transport error is returned as a plain error.
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

func (c *Codec) write(msg *Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Loop on Write so a short-write doesn't truncate the JSON-RPC frame
	// the peer's bufio.Scanner is about to parse — a half-frame would either
	// fail to decode or silently merge with the next frame's bytes.
	for off := 0; off < len(data); {
		n, werr := c.writer.Write(data[off:])
		if werr != nil {
			return werr
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		off += n
	}
	return nil
}

// Notify sends a fire-and-forget notification (method, params, no id).
func (c *Codec) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(&Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
	})
}

// Respond sends a successful response carrying result for the given request id.
func (c *Codec) Respond(id *json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(&Message{
		JSONRPC: "2.0",
		ID:      id,
		Result:  raw,
	})
}

// RespondError sends an error response for the given request id.
func (c *Codec) RespondError(id *json.RawMessage, code int, message string) error {
	return c.write(&Message{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	})
}

// RespondErrorData is like RespondError but carries an optional structured
// data payload (JSON-RPC 2.0 error object's "data" field). data may be nil.
func (c *Codec) RespondErrorData(id *json.RawMessage, code int, message string, data any) error {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		raw = b
	}
	return c.write(&Message{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message, Data: raw},
	})
}
