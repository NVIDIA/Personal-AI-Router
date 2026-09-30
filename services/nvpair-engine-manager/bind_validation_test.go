// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// codecCapture is an io.ReadWriter that discards reads and accumulates writes
// so a test can inspect the JSON-RPC frames a Manager emits.
type codecCapture struct{ buf bytes.Buffer }

func (c *codecCapture) Read(p []byte) (int, error)  { return 0, io.EOF }
func (c *codecCapture) Write(p []byte) (int, error) { return c.buf.Write(p) }

// runOpError drives Manager.runOp with the given method/params and returns the
// JSON-RPC error frame it emitted, or "" if it emitted none. reachedExec is
// true when validation passed and runOp proceeded to the (nil, in tests)
// executor — the test only cares that no -32602 bind rejection was emitted.
func runOpError(t *testing.T, method, params string) (out string, reachedExec bool) {
	t.Helper()
	cap := &codecCapture{}
	m := NewManager(NewCodec(cap), &Executor{}, nil)
	// NewManager wires the executor into the settings relay, so it needs a
	// non-nil one at construction; nil it back out right after to keep the
	// fixture's panic-on-reach signal for validation-passing calls.
	m.exec = nil
	msg := &Message{Method: method, Params: json.RawMessage(params)}
	id := json.RawMessage(`"test-1"`)
	msg.ID = &id
	defer func() {
		// The executor is nil in this fixture: validation-passing calls panic
		// when they reach it, which is exactly the signal we want.
		if recover() != nil {
			reachedExec = true
		}
	}()
	m.runOp(context.Background(), msg)
	return cap.buf.String(), false
}

// TestRunOpRejectsNonLoopbackBind: engine:start (and install+start) used to
// accept any valid IP as a bind override, so a broker caller could put the
// unauthenticated engine API on 0.0.0.0. Remote starts hard-bind 127.0.0.1
// (controllifecycle.go); local starts now require loopback too.
func TestRunOpRejectsNonLoopbackBind(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "192.168.1.5", "::"} {
		out, reachedExec := runOpError(t, "engine:start", `{"engine":"ollama","bind":"`+bind+`"}`)
		if reachedExec || !strings.Contains(out, "bind must be a loopback address") {
			t.Errorf("bind %q: response %q (reachedExec=%v), want loopback rejection", bind, out, reachedExec)
		}
	}

	// Invalid IPs are still rejected as invalid.
	out, reachedExec := runOpError(t, "engine:start", `{"engine":"ollama","bind":"not-an-ip"}`)
	if reachedExec || !strings.Contains(out, "bind must be a valid IP address") {
		t.Errorf("invalid bind: response %q (reachedExec=%v), want invalid-address rejection", out, reachedExec)
	}

	// Loopback binds pass validation and reach the executor (which is nil in
	// this fixture — the panic-recovery reports reachedExec).
	for _, bind := range []string{"127.0.0.1", "::1"} {
		out, reachedExec := runOpError(t, "engine:start", `{"engine":"ollama","bind":"`+bind+`"}`)
		if !reachedExec || strings.Contains(out, "bind must be") {
			t.Errorf("loopback bind %q: response %q (reachedExec=%v), want validation to pass", bind, out, reachedExec)
		}
	}
}
