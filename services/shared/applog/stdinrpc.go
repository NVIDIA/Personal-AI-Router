// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package applog

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// StdinMessage is a minimal subset of a JSON-RPC message usable by
// subprocesses that only care about a handful of control methods
// (log/set-level, shutdown) and don't speak a full RPC protocol.
type StdinMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// StdinRPC runs a minimal newline-delimited JSON-RPC reader on os.Stdin for
// subprocesses that don't already have a codec. It dispatches the built-in
// log/set-level method itself and forwards everything else to onMessage.
//
// onClose is called exactly once when stdin closes (parent died) or
// otherwise becomes unreadable. It MUST be goroutine-safe and is the
// canonical "shut me down" signal for these simple subprocesses.
//
// writer, if non-nil, is used to send responses/ack frames. If nil,
// responses are dropped (the advertiser historically has no stdout writer
// and the parent doesn't expect one — it only sends notifications).
func StdinRPC(writer io.Writer, onMessage func(StdinMessage), onClose func()) {
	var once sync.Once
	closeOnce := func() { once.Do(onClose) }

	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = []byte(strings.TrimRight(string(line), "\r\n"))
			if len(line) > 0 {
				handleStdinLine(line, writer, onMessage)
			}
		}
		if err != nil {
			slog.Debug("stdin closed, shutting down", "err", err)
			closeOnce()
			return
		}
	}
}

func handleStdinLine(line []byte, writer io.Writer, onMessage func(StdinMessage)) {
	var msg StdinMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.Debug("ignoring non-JSON stdin line", "err", err)
		return
	}
	if msg.Method == SetLevelMethod {
		resolved, err := HandleSetLevelParams(msg.Params)
		if len(msg.ID) > 0 && writer != nil {
			if err != nil {
				writeStdinError(writer, msg.ID, -32602, err.Error())
			} else {
				writeStdinResult(writer, msg.ID, map[string]string{"level": resolved})
			}
		}
		if err != nil {
			slog.Warn("log/set-level rejected", "err", err)
		} else {
			slog.Info("log level changed", "level", resolved)
		}
		return
	}
	if onMessage != nil {
		onMessage(msg)
	}
}

// Notifier serializes newline-delimited JSON-RPC frames onto one writer. A
// subprocess that reports something its parent asked no question about — an
// observation it makes while serving traffic, say — emits it from whichever
// goroutine noticed, and those frames must not interleave with the responses
// StdinRPC writes. Pass the same Notifier as StdinRPC's writer and both go
// through this lock.
type Notifier struct {
	mu sync.Mutex
	w  io.Writer
}

// NewNotifier returns a Notifier writing to w, or nil when w is nil so a
// subprocess with no stdout channel needs no special case.
func NewNotifier(w io.Writer) *Notifier {
	if w == nil {
		return nil
	}
	return &Notifier{w: w}
}

// Write makes a Notifier usable as StdinRPC's response writer, so responses and
// notifications share one lock.
func (n *Notifier) Write(p []byte) (int, error) {
	if n == nil {
		return len(p), nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.w.Write(p)
}

// Notify emits one JSON-RPC notification. A nil Notifier drops it, which is what
// a subprocess started without a stdout reader wants.
func (n *Notifier) Notify(method string, params any) error {
	if n == nil {
		return nil
	}
	payload := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", method, params}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = n.Write(data)
	return err
}

func writeStdinResult(w io.Writer, id json.RawMessage, result any) {
	payload := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = w.Write(data)
}

func writeStdinError(w io.Writer, id json.RawMessage, code int, message string) {
	payload := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	payload.Error.Code = code
	payload.Error.Message = message
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = w.Write(data)
}
