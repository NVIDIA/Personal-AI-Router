// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A peer whose node-info address silently drops packets is the failure these
// cover. It cost a full request timeout on every sweep and said so only at debug
// level, so an address that had never once answered was indistinguishable in the
// log from one that was working.

// captureLogs redirects the default logger for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// TestNodeInfoOutageIsStatedOnceAtEachEnd pins the reporting to the transitions.
// The sweep repeats for as long as the node is advertised, so reporting every
// attempt is what made this unreadable — and reporting none of them is what hid
// it entirely.
func TestNodeInfoOutageIsStatedOnceAtEachEnd(t *testing.T) {
	logs := captureLogs(t)
	d := &daemon{}

	for range 3 {
		d.noteNodeInfo("peer-uuid", "10.0.0.5:14300", false)
	}
	{
		got := strings.Count(logs.String(), "node-info is not answering")
		require.Equal(t, 1, got, "outage reported")
	}
	require.Contains(t, logs.String(), "level=WARN", "an unreachable peer must be visible above debug")
	require.Contains(t, logs.String(), "10.0.0.5:14300", "the report must name where the node was asked")

	for range 2 {
		d.noteNodeInfo("peer-uuid", "10.0.0.5:14300", true)
	}
	{
		got := strings.Count(logs.String(), "node-info is answering again")
		require.Equal(t, 1, got, "recovery reported")
	}
}

// TestNodeInfoSilenceWhenNothingChanges keeps the steady state quiet: a peer that
// has always answered has nothing to say about itself.
func TestNodeInfoSilenceWhenNothingChanges(t *testing.T) {
	logs := captureLogs(t)
	d := &daemon{}

	for range 5 {
		d.noteNodeInfo("peer-uuid", "10.0.0.5:14300", true)
	}
	require.Empty(t, logs.Bytes(), "a healthy peer must log nothing")
}

// TestNodeInfoConnectGivesUpBeforeTheRequestBudget covers the split itself. The
// address is reserved for documentation and is routed nowhere, so connecting to
// it either fails immediately (the local network says so) or hangs — and hanging
// is the case that mattered: it must end at the connect budget rather than
// consume the whole request timeout the enrichment client allows.
func TestNodeInfoConnectGivesUpBeforeTheRequestBudget(t *testing.T) {
	require.Less(t, nodeInfoDialTimeout, nodeInfoFetchTimeout, "a connect budget of")

	start := time.Now()
	conn, err := nodeInfoTransport(nil).DialContext(context.Background(), "tcp", "192.0.2.1:14300")
	if err == nil {
		_ = conn.Close()
		require.FailNow(t, "test expectation failed", "nothing may answer at a documentation address")
	}
	{
		elapsed := time.Since(start)
		require.Less(t, elapsed, nodeInfoFetchTimeout, "connect took (%v, %v, %v)", elapsed, nodeInfoDialTimeout, nodeInfoFetchTimeout)
	}
}

func TestNodeInfoTransportBoundsConnectionsPerHost(t *testing.T) {
	transport := nodeInfoTransport(nil)
	require.Equal(t, 1, transport.MaxConnsPerHost)
	require.Equal(t, 1, transport.MaxIdleConnsPerHost)
}

func TestNodeInfoConcurrentFetchesGetIndependentRequestBudgets(t *testing.T) {
	const responseDelay = 200 * time.Millisecond
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(responseDelay)
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "peer-uuid"})
	}))
	var connections atomic.Int32
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err, "parse server port")
	transport := nodeInfoTransport(nil)
	defer transport.CloseIdleConnections()
	d := &daemon{http: &http.Client{Timeout: 300 * time.Millisecond, Transport: transport}}

	start := make(chan struct{})
	results := make(chan bool, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok := d.fetchNodeInfo(serverURL.Hostname(), port)
			results <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	for ok := range results {
		require.True(t, ok, "a queued healthy request consumed its timeout before reaching the peer")
	}
	{
		got := requests.Load()
		require.Equal(t, int32(2), got, "server received")
	}
	{
		got := connections.Load()
		require.Equal(t, int32(1), got, "requests used")
	}
}

func TestNodeInfoOriginGateHonorsCanceledWaiter(t *testing.T) {
	var gate nodeInfoOriginGate
	release, acquired := gate.acquire(context.Background(), "http://127.0.0.1:14318")
	require.True(t, acquired, "first origin request did not acquire the gate")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitingRelease, waitingAcquired := gate.acquire(ctx, "http://127.0.0.1:14318")
	if waitingAcquired {
		waitingRelease()
		require.FailNow(t, "test expectation failed", "canceled origin request acquired the occupied gate")
	}
}

func TestNodeInfoErrorBodyIsDrainedForConnectionReuse(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("temporarily unavailable"))
			return
		}
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "peer-uuid"})
	}))
	var connections atomic.Int32
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err, "parse server port")
	transport := nodeInfoTransport(nil)
	defer transport.CloseIdleConnections()
	d := &daemon{http: &http.Client{Timeout: time.Second, Transport: transport}}

	{
		_, ok := d.fetchNodeInfo(serverURL.Hostname(), port)
		require.False(t, ok, "HTTP 503 unexpectedly succeeded")
	}
	info, ok := d.fetchNodeInfo(serverURL.Hostname(), port)
	require.True(t, ok, "second fetch (%v, %v)", info, ok)
	require.Equal(t, "peer-uuid", info.HostUUID, "second fetch (%v, %v)", info, ok)
	{
		got := connections.Load()
		require.Equal(t, int32(1), got, "requests used")
	}
}
