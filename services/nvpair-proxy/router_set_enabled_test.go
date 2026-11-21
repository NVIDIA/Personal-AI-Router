// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Lifecycle of the cluster routing endpoint under router/set-enabled: the
// endpoint is created on demand, a port somebody else already holds is
// refused without disturbing the listener that is already serving, and a
// turn-off closes the listener without cutting a generation that is
// mid-flight.
//
// Unlike the bound-state section in router_test.go — which reads one port to
// check what status reports — these tests treat the socket itself as the
// subject: "the endpoint answers" and "the port refuses" are socket facts,
// and the point of the method pair is that the control-plane answer follows
// the socket rather than the configured number. Every endpoint here is
// therefore a real listener on a port from freeTCPPort, and the turn-off
// assertions are made with a fresh dial.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callControl applies one control-plane request through handleMessage the
// way the read loop would, and returns the reply frame it appended. The
// single-threaded call stands in for the read loop deliberately: the
// turn-off is only "non-blocking" relative to the goroutine that applies it.
func callControl(t *testing.T, p *Proxy, rec *recRW, method string, params string) Message {
	t.Helper()
	id := json.RawMessage(`7`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  json.RawMessage(params),
	})
	return lastControlReply(t, rec)
}

// lastControlReply returns the newest reply frame the codec recorded.
// Lifecycle tests make several calls on one recorder, and the reply to the
// call under test is the newest one; a notification (a bring-up's peer-alias
// subscribe, a running job's workload events) carries neither result nor
// error and is skipped.
func lastControlReply(t *testing.T, rec *recRW) Message {
	t.Helper()
	lines := strings.Split(rec.String(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var frame Message
		require.NoErrorf(t, json.Unmarshal([]byte(line), &frame), "decode (%s)", line)
		if frame.Result != nil || frame.Error != nil {
			return frame
		}
	}
	t.Fatal("the codec recorded no reply frame")
	return Message{}
}

// replyStatus decodes a reply that carries the endpoint state, failing the
// test when the reply is an error.
func replyStatus(t *testing.T, frame Message) routerStatusResult {
	t.Helper()
	require.Nil(t, frame.Error, "the control call was refused: %+v", frame.Error)
	var status routerStatusResult
	require.NoError(t, json.Unmarshal(frame.Result, &status))
	return status
}

// getEndpoint asks a port the way a client would, so "the endpoint answers"
// is read off the socket.
func getEndpoint(t *testing.T, port int) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/v1/models")
	require.NoErrorf(t, err, "GET /v1/models on port %d did not complete", port)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(body)
}

// assertPortRefused proves the endpoint is down the only way a client can
// observe it: a new connection to that port gets nothing.
func assertPortRefused(t *testing.T, port int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
	}
	require.Errorf(t, err, "port %d still accepts new connections", port)
}

// lifecycleProxy is a process with no cluster endpoint, empty facades
// included: the tests below vary the endpoint, not the routing, unless they
// name a provider explicitly.
func lifecycleProxy(t *testing.T) (*Proxy, *recRW) {
	t.Helper()
	redirectConfigDir(t)
	rec := &recRW{}
	p := NewProxy(NewCodec(rec))
	p.runID = "run-under-test"
	p.serveCtx = t.Context()
	t.Cleanup(func() { p.shutdown(t.Context()) })
	return p, rec
}

// bindEndpoint brings the endpoint up the way process start does, so a
// test's opening state is "an endpoint that already answers" without
// spending the call under test to set it up.
func bindEndpoint(t *testing.T, p *Proxy, port int) *clusterRouter {
	t.Helper()
	router, err := p.bringUpRouter(t.Context(), port)
	require.NoErrorf(t, err, "no cluster endpoint came up on port %d", port)
	p.clusterRouter = router
	return router
}

// TestRouterSetEnabled_StartsTheEndpointOnDemandAndRefusesNewRequestsAfterItStops
// walks the whole on/off arc on one port: the enable creates the listener and
// answers through it, the disable closes it and new clients get nothing. The
// two directions are one test because they are one contract — the control
// plane's answer must track the socket in both.
func TestRouterSetEnabled_StartsTheEndpointOnDemandAndRefusesNewRequestsAfterItStops(t *testing.T) {
	p, rec := lifecycleProxy(t)
	port := freeTCPPort(t)

	frame := callControl(t, p, rec, "router/set-enabled", fmt.Sprintf(`{"enabled":true,"port":%d}`, port))
	status := replyStatus(t, frame)
	assert.True(t, status.Running, "the enable reported no endpoint")
	assert.Equal(t, port, status.Port, "the enable reported a port other than the one it bound")

	code, body := getEndpoint(t, port)
	require.Equal(t, http.StatusOK, code, "the endpoint the enable just reported did not answer")
	assert.JSONEq(t, `{"object":"list","data":[]}`, body)

	frame = callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)
	status = replyStatus(t, frame)
	assert.Equal(t, routerStatusResult{Running: false, Port: 0}, status,
		"the disable still claimed an endpoint")

	assertPortRefused(t, port)
}

// TestRouterSetEnabled_KeepsTheOldListenerWhenTheNewPortIsTaken covers the
// second invariant line: a port somebody else holds is a refusal that changes
// nothing. Moving a working endpoint off a port because a second enable lost
// a bind race would turn one wrong number into downtime on the path that was
// fine.
func TestRouterSetEnabled_KeepsTheOldListenerWhenTheNewPortIsTaken(t *testing.T) {
	p, rec := lifecycleProxy(t)
	oldPort := freeTCPPort(t)
	bindEndpoint(t, p, oldPort)

	// The second port is held for the whole test, so the refused bind is
	// the everyday collision (another app owns the number), not a race.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()
	takenPort := occupied.Addr().(*net.TCPAddr).Port

	frame := callControl(t, p, rec, "router/set-enabled", fmt.Sprintf(`{"enabled":true,"port":%d}`, takenPort))
	require.NotNil(t, frame.Error, "a taken port was accepted as a success")
	assert.Equal(t, -32011, frame.Error.Code,
		"a bind failure must answer with the code that means another port could fix it")
	assert.Contains(t, frame.Error.Message, strconv.Itoa(takenPort),
		"the refusal must name the port that could not be bound")

	// The refusal changed nothing: the listener that predates it still
	// serves, and status still reports it.
	code, _ := getEndpoint(t, oldPort)
	require.Equal(t, http.StatusOK, code, "the old endpoint stopped answering after the refusal")

	frame = callControl(t, p, rec, "router/status", "")
	assert.Equal(t, routerStatusResult{Running: true, Port: oldPort}, replyStatus(t, frame),
		"status stopped reporting the endpoint that still answers")
}

// TestRouterSetEnabled_TurnsOffWhileARequestIsInFlightWithoutCuttingItOrBlockingTheReadLoop
// is the first invariant line: the turn-off must answer at once and the
// stream must finish. A listener close takes nothing away from connections
// already accepted — that is why the stop path refuses to call
// http.Server.Shutdown, which would wait and then cut. The held upstream
// pins the moment of the turn-off to "a generation mid-flight" instead of a
// sleep.
func TestRouterSetEnabled_TurnsOffWhileARequestIsInFlightWithoutCuttingItOrBlockingTheReadLoop(t *testing.T) {
	p, rec := lifecycleProxy(t)

	upstream := newRouterUpstream(t)
	release := upstream.holdResponsesUntilRelease(t)
	defer release()

	enableOnFreePort(t, p, "ollama")
	p.facadeFor("ollama").discovery.AddManual(Node{
		ID:        "hold-host",
		Host:      "127.0.0.1",
		Addresses: []string{"127.0.0.1"},
		Port:      upstream.port,
		Models:    []string{"held-model"},
	})

	port := freeTCPPort(t)
	bindEndpoint(t, p, port)

	completed := make(chan string, 1)
	go func() {
		client := &http.Client{Timeout: 30 * time.Second}
		response, err := client.Post(
			"http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
			"application/json",
			strings.NewReader(`{"model":"held-model","messages":[{"role":"user","content":"hold"}]}`))
		if err != nil {
			completed <- "request did not complete: " + err.Error()
			return
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			completed <- "response body was cut short: " + err.Error()
			return
		}
		completed <- fmt.Sprintf("%d %s", response.StatusCode, body)
	}()

	waitForCond(t, 10*time.Second, "the held request to reach the mock upstream", func() bool {
		return upstream.requestCount() > 0
	})

	started := time.Now()
	frame := callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)
	elapsed := time.Since(started)

	status := replyStatus(t, frame)
	assert.Equal(t, routerStatusResult{Running: false, Port: 0}, status)
	assert.Less(t, elapsed, 250*time.Millisecond,
		"the turn-off waited on the in-flight request instead of answering at once "+
			"(took %v — a Shutdown-style drain wait would take seconds)", elapsed)

	// Release the held generation only after the listener is closed: the
	// rest of its trip rides the connection that predates the turn-off,
	// which is the invariant under test. A drain-then-cut stop would show up
	// here as a cut short or never-completing request, not just a slow one.
	release()

	select {
	case got := <-completed:
		require.True(t, strings.HasPrefix(got, "200 "),
			"the in-flight request was not served to completion after the endpoint went off: %s", got)
	case <-time.After(20 * time.Second):
		t.Fatal("the released request never completed")
	}

	assertPortRefused(t, port)
}

// TestRouterSetEnabled_RepeatingTheSameStateChangesNothing covers the fifth
// invariant line. A redelivered enable must not tear down the listener that
// answers it (the broker can resend after a reconnect), and a redelivered
// disable — including on a process that never had an endpoint — must be a
// plain answer, not an error or a double close.
func TestRouterSetEnabled_RepeatingTheSameStateChangesNothing(t *testing.T) {
	t.Run("enabling the port that already answers keeps the same endpoint", func(t *testing.T) {
		p, rec := lifecycleProxy(t)
		port := freeTCPPort(t)

		first := callControl(t, p, rec, "router/set-enabled", fmt.Sprintf(`{"enabled":true,"port":%d}`, port))
		before := replyStatus(t, first)
		require.Equal(t, routerStatusResult{Running: true, Port: port}, before,
			"the opening enable did not bring the endpoint up, so a re-enable proves nothing")
		router := p.clusterRouter

		second := callControl(t, p, rec, "router/set-enabled", fmt.Sprintf(`{"enabled":true,"port":%d}`, port))
		after := replyStatus(t, second)

		assert.Equal(t, before, after, "the re-enable answered a different state than the first")
		assert.Same(t, router, p.clusterRouter, "the re-enable replaced a working endpoint")
	})

	t.Run("disabling twice answers the same way", func(t *testing.T) {
		p, rec := lifecycleProxy(t)
		port := freeTCPPort(t)
		bindEndpoint(t, p, port)

		frame := callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)
		assert.Equal(t, routerStatusResult{Running: false, Port: 0}, replyStatus(t, frame),
			"the first disable did not report the endpoint off")

		frame = callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)
		assert.Equal(t, routerStatusResult{Running: false, Port: 0}, replyStatus(t, frame),
			"the second disable changed the answer")
	})

	t.Run("a process that never had an endpoint turns nothing off", func(t *testing.T) {
		p, rec := lifecycleProxy(t)

		frame := callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)
		require.Nil(t, frame.Error,
			"disabling an endpoint that never existed must be a plain answer, not a failure")
		assert.Equal(t, routerStatusResult{Running: false, Port: 0}, replyStatus(t, frame))
	})
}

// TestRouterSetEnabled_TakesTheDefaultPortWhenThePayloadNamesNone: "port
// omitted" means the default, not "no port". The default knob is a var for
// exactly this kind of test, so the assertion runs against a number the test
// knows is free rather than against 9999 belonging to somebody else.
func TestRouterSetEnabled_TakesTheDefaultPortWhenThePayloadNamesNone(t *testing.T) {
	p, rec := lifecycleProxy(t)
	port := freeTCPPort(t)
	setForTest(t, &routerDefaultPort, port)

	frame := callControl(t, p, rec, "router/set-enabled", `{"enabled":true}`)
	status := replyStatus(t, frame)
	require.Equal(t, routerStatusResult{Running: true, Port: port}, status,
		"a payload naming no port did not take the default")

	code, _ := getEndpoint(t, port)
	assert.Equal(t, http.StatusOK, code, "the reported default port did not answer")
}

// TestRouterStatus_ReportsNoEndpointAfterTheEndpointIsTurnedOffAtRuntime is
// the lie this change exists to kill: a closed net.TCPListener still reports
// its address, so a status read that only asks the listener keeps
// advertising a port nobody answers. The desktop's Endpoints list is
// trustworthy only because a row means something on that port answers.
func TestRouterStatus_ReportsNoEndpointAfterTheEndpointIsTurnedOffAtRuntime(t *testing.T) {
	p, rec := lifecycleProxy(t)
	port := freeTCPPort(t)
	bindEndpoint(t, p, port)

	frame := callControl(t, p, rec, "router/status", "")
	require.Equal(t, routerStatusResult{Running: true, Port: port}, replyStatus(t, frame),
		"the opening state is not an answering endpoint, so the turn-off proves nothing")

	callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)

	frame = callControl(t, p, rec, "router/status", "")
	assert.Equal(t, routerStatusResult{Running: false, Port: 0}, replyStatus(t, frame),
		"status still advertised the port after the endpoint was closed")
	assertPortRefused(t, port)
}

// TestRouterTeardownAndRuntimeStopBothCloseTheSameEndpoint: process teardown
// and a runtime disable can reach the same listener in either order, and
// each must tolerate the other having closed it first. A second close that
// reaches the caller as an error, or a nil dereference recovered into a
// -32603 reply, would turn an ordinary race between shutdown and a queued
// disable into a failure the desktop shows.
func TestRouterTeardownAndRuntimeStopBothCloseTheSameEndpoint(t *testing.T) {
	t.Run("teardown after a runtime turn-off", func(t *testing.T) {
		p, rec := lifecycleProxy(t)
		port := freeTCPPort(t)
		bindEndpoint(t, p, port)

		callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)

		// Teardown finds no endpoint to stop; it must not panic on the
		// process that already stopped one, and it must not close anything
		// a later read mistakes for the endpoint.
		p.shutdown(t.Context())

		frame := callControl(t, p, rec, "router/status", "")
		require.Nil(t, frame.Error, "teardown left the control plane answering with an error")
		assert.Equal(t, routerStatusResult{Running: false, Port: 0}, replyStatus(t, frame))
	})

	t.Run("runtime turn-off after teardown", func(t *testing.T) {
		p, rec := lifecycleProxy(t)
		port := freeTCPPort(t)
		bindEndpoint(t, p, port)

		p.shutdown(t.Context())
		assertPortRefused(t, port)

		frame := callControl(t, p, rec, "router/set-enabled", `{"enabled":false}`)
		require.Nil(t, frame.Error,
			"a disable after teardown panicked or was refused instead of being a plain no-op")
		assert.Equal(t, routerStatusResult{Running: false, Port: 0}, replyStatus(t, frame),
			"the disable after teardown still claimed an endpoint that teardown had closed")
	})
}
