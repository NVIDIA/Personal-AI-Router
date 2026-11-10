// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"

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
	test("pull naming no model", "engine:action", `{"engine":"x","action":"pull_model","params":{}}`, "model is required", (*Manager).runAction)
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

// The read loop claims a pull before it dispatches the pull's goroutine, and
// engine:cancel-pull dispatches a goroutine of its own, so a cancel sent right
// behind a pull can reach the executor before the download registers. It then
// finds only the claim, which has to travel with the request to stop it.
func TestManager_StopsAPullCancelledBeforeItsDownloadRegisters(t *testing.T) {
	ex := newHTTPPullTestExecutor(t, stalledTransfer(t), Action{HTTP: &ActionHTTP{Method: http.MethodPost, Path: "/api/pull"}})
	managerConn, clientConn := net.Pipe()
	defer managerConn.Close()
	defer clientConn.Close()
	m := NewManager(NewCodec(managerConn), ex, nil)
	response := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(clientConn).ReadBytes('\n')
		readErr <- err
		response <- line
	}()
	// With a single P, the goroutine handleMessage starts for the pull cannot
	// run until this one blocks, so the cancel reaches the executor first.
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })

	id := json.RawMessage("1")
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:action",
		Params: json.RawMessage(`{"engine":"fake","action":"pull_model","params":{"name":"demo"}}`)})
	require.NoError(t, ex.CancelModelPull(context.Background(), "fake", "demo"))

	select {
	case line := <-response:
		require.NoError(t, <-readErr)
		var msg Message
		require.NoError(t, json.Unmarshal(line, &msg), "decode response %s", line)
		require.Empty(t, msg.Error, "the pull should settle as cancelled, not fail")
		assertPullStatus(t, msg.Result, "cancelled")
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the pull kept running after its cancel was acknowledged")
	}
}

// A cancel stops the requests the client sent before it, so a retry sent right
// behind it downloads. engine:action claims its pull on the read loop, and a
// cancel left to act from a goroutine of its own would find the retry already
// claimed and stop it too: the retry would answer "cancelled" without
// downloading.
func TestManager_DownloadsARetrySentRightAfterACancel(t *testing.T) {
	transfer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success"}` + "\n"))
	}))
	t.Cleanup(transfer.Close)
	ex := newHTTPPullTestExecutor(t, transfer, Action{HTTP: &ActionHTTP{Method: http.MethodPost, Path: "/api/pull"}})
	managerConn, clientConn := net.Pipe()
	defer managerConn.Close()
	defer clientConn.Close()
	m := NewManager(NewCodec(managerConn), ex, nil)
	replies := make(chan *Message, 2)
	go func() {
		client := NewCodec(clientConn)
		for {
			msg, err := client.Read()
			if err != nil {
				return
			}
			if msg.ID != nil {
				replies <- msg
			}
		}
	}()
	// With a single P, neither goroutine handleMessage dispatches can run until
	// this one blocks, so the retry is claimed before a cancel acting from its
	// own goroutine could reach the executor.
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })

	cancelID, retryID := json.RawMessage("1"), json.RawMessage("2")
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &cancelID, Method: "engine:cancel-pull",
		Params: json.RawMessage(`{"engine":"fake","model":"demo"}`)})
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &retryID, Method: "engine:action",
		Params: json.RawMessage(`{"engine":"fake","action":"pull_model","params":{"name":"demo"}}`)})

	timeout := time.After(10 * time.Second)
	for {
		select {
		case reply := <-replies:
			if string(*reply.ID) != string(retryID) {
				continue
			}
			require.Empty(t, reply.Error, "the retry should download, not fail")
			assertPullStatus(t, reply.Result, "success")
			return
		case <-timeout:
			require.FailNow(t, "the retry never answered")
		}
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	require.Contains(t, s, sub)
}
