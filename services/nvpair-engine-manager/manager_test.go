// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestManagerErrorResponses drives handleMessage / runOp / runAction over
// an in-memory codec and asserts the JSON-RPC error responses for the
// failure paths the e2e harness can't easily reach.
func TestManager_ReturnsProtocolErrorsForInvalidRequests(t *testing.T) {
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

func TestRunOp_RejectsBadPort(t *testing.T) {
	var out bytes.Buffer
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:start",
		Params: json.RawMessage(`{"engine":"fake","port":70000}`)})
	mustContain(t, out.String(), "port must be between")
}

func TestRunOp_RejectsBadBind(t *testing.T) {
	var out bytes.Buffer
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:start",
		Params: json.RawMessage(`{"engine":"fake","bind":"not-an-ip"}`)})
	mustContain(t, out.String(), "bind must be a valid IP")
}

func TestRunOp_StartsEngineAfterInstallation(t *testing.T) {
	var out bytes.Buffer
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	t.Cleanup(func() { _ = ex.Stop("fake") })
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:install",
		Params: json.RawMessage(`{"engine":"fake","start":true}`)})
	st, err := ex.Status("fake")
	require.NoError(t, err)
	require.True(t, st.Running, "expected running after install+autostart (%v)", st)
}

func TestStatusQueries_DoNotBlockMessageDispatchDuringEngineOperation(t *testing.T) {
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
				require.NoError(t, <-readErr)
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

// A SIGTERM cancels the manager's context while the read loop is blocked on
// stdin, which stays open and silent. Shutdown must not wait for the next
// message: it has to stop the engines and return, or the process hangs with its
// engines running until the parent closes the pipe.
func TestRunShutsDownWhenCancelledWhileStdinIsIdle(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	if err := ex.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}

	stdinRead, stdinWrite := io.Pipe() // never written to, never closed
	t.Cleanup(func() { _ = stdinWrite.Close() })
	var out syncBuffer
	m := NewManager(NewCodec(struct {
		io.Reader
		io.Writer
	}{stdinRead, &out}), ex, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(200 * time.Millisecond) // let it reach the blocking read
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if status, _ := ex.Status("fake"); status.Running {
		t.Fatal("the engine was left running by the shutdown")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
