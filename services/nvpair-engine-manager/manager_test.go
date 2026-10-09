// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagerErrorResponses drives handleMessage / runOp / runAction over
// an in-memory codec and asserts the JSON-RPC error responses for the
// failure paths the e2e harness can't easily reach.
func TestManagerErrorResponses(t *testing.T) {
	test := func(name, method, params, want string, dispatch func(*Manager, context.Context, *Message)) {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
			m := NewManager(NewCodec(&out), ex, nil)
			id := json.RawMessage("1")
			dispatch(m, context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
			require.Contains(t, out.String(), want)
		})
	}
	test("unknown method", "bogus", "", "-32601", (*Manager).handleMessage)
	test("unknown engine", "engine:describe", `{"engine":"nope"}`, "unknown engine", (*Manager).handleMessage)
	test("missing engine", "engine:start", `{}`, "engine is required", (*Manager).runOp)
	test("invalid params", "engine:start", `{bad}`, "invalid params", (*Manager).runOp)
	test("missing action", "engine:action", `{"engine":"x"}`, "engine and action are required", (*Manager).runAction)
}

func TestRunOpRejectsBadPort(t *testing.T) {
	var out bytes.Buffer
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:start",
		Params: json.RawMessage(`{"engine":"fake","port":70000}`)})
	mustContain(t, out.String(), "port must be between")
}

func TestRunOpRejectsBadBind(t *testing.T) {
	var out bytes.Buffer
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:start",
		Params: json.RawMessage(`{"engine":"fake","bind":"not-an-ip"}`)})
	mustContain(t, out.String(), "bind must be a valid IP")
}

func TestRunOpInstallAutostart(t *testing.T) {
	var out bytes.Buffer
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	t.Cleanup(func() { _ = ex.Stop("fake") })
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:install",
		Params: json.RawMessage(`{"engine":"fake","start":true}`)})
	st, err := ex.Status("fake")
	assert.NoError(t, err)
	require.True(t, st.Running, "expected running after install+autostart (%v)", st)
}

func TestStatusQueriesDoNotBlockMessageDispatchDuringEngineOperation(t *testing.T) {
	test := func(name, method string, params json.RawMessage) {
		t.Run(name, func(t *testing.T) {
			ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
			st, err := ex.state("fake")
			require.NoError(t, err)
			managerConn, clientConn := net.Pipe()
			defer managerConn.Close()
			defer clientConn.Close()
			m := NewManager(NewCodec(managerConn), ex, nil)
			id := json.RawMessage("1")
			response := make(chan string, 1)
			readErr := make(chan error, 1)
			go func() {
				line, err := bufio.NewReader(clientConn).ReadString('\n')
				readErr <- err
				response <- line
			}()

			// A lifecycle operation holds opMu for its full duration. Status
			// reconciliation may wait for that operation, but dispatch must return
			// immediately so the manager can keep reading shutdown and other RPCs.
			st.opMu.Lock()
			returned := make(chan struct{})
			go func() {
				m.handleMessage(context.Background(), &Message{
					JSONRPC: "2.0",
					ID:      &id,
					Method:  method,
					Params:  params,
				})
				close(returned)
			}()

			select {
			case <-returned:
				// Expected: the potentially blocking status work was dispatched.
			case <-time.After(time.Second):
				st.opMu.Unlock()
				require.FailNow(t, "message dispatch blocked while an engine operation held opMu")
			}

			st.opMu.Unlock()
			select {
			case line := <-response:
				assert.NoError(t, <-readErr)
				require.Contains(t, line, `"id":1`)
			case <-time.After(2 * time.Second):
				require.FailNow(t, "no response after the engine operation completed")
			}
		})
	}
	test("status", "engine:status", json.RawMessage(`{"engine":"fake"}`))
	test("get installed", "engine:get-installed", nil)
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	require.Contains(t, s, sub)
}
