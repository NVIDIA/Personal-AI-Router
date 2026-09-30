// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// nonLoopbackIPv4 returns an address of this host that a peer could plausibly
// reach it on. The observer deliberately ignores loopback peers, so a loopback
// listener cannot exercise the hook the way real traffic does.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err, "enumerate interface addresses")
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String()
	}
	t.Skip("host has no non-loopback IPv4 address to serve a peer on")
	return ""
}

// TestServedRequestReportsTheAddressTheClientReached exercises the wiring rather
// than the observer: an http.Server built the way main.go builds it must feed
// every served connection into the observer, and the resulting set must go out as
// the notification the broker listens for. Both halves are one line of production
// setup each, and removing either leaves the observer's own tests passing while no
// address evidence ever reaches address selection.
func TestServedRequestReportsTheAddressTheClientReached(t *testing.T) {
	local := nonLoopbackIPv4(t)

	observer := newAddressObserver()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"GPUs":[],"telemetryValid":false,"msSince":0}`))
		assert.NoError(t, err)
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         observer.connState,
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(local, "0"))
	if err != nil {
		t.Skipf("cannot serve on %s: %v", local, err)
	}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() { _ = server.Close() })

	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + listener.Addr().String() + "/v1/node-info")
	if err != nil {
		t.Skipf("cannot reach this host at %s: %v", listener.Addr(), err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	assert.NoError(t, err)
	assert.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "inventory status")

	var out bytes.Buffer
	observer.report(applog.NewNotifier(&out))

	var frame struct {
		JSONRPC string                          `json:"jsonrpc"`
		Method  string                          `json:"method"`
		Params  noderec.ObservedAddressesParams `json:"params"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &frame), "decode report")
	require.Equal(t, "2.0", frame.JSONRPC)
	require.Equal(t, noderec.NotifyObservedAddresses, frame.Method, "report")
	require.Contains(t, frame.Params.Addresses, local, "reported addresses")
}

// addrConn presents a connection under the addresses a remote peer's connection
// would carry, so the server's hook sees a peer it must not ignore.
type addrConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (c addrConn) LocalAddr() net.Addr  { return c.local }
func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// oneConnListener hands the server a single already-established connection.
type oneConnListener struct {
	conn   net.Conn
	served bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.served {
		return nil, net.ErrClosed
	}
	l.served = true
	return l.conn, nil
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// TestServerRecordsTheServingConnectionWhenARequestArrives covers the same wiring
// as the test above without depending on this host being reachable at one of its
// own LAN addresses, which a firewall or a container with only loopback can deny.
// It also pins the part that is easy to get subtly wrong: the hook must fire for
// the connection that served a request, not merely for one that was accepted.
func TestServerRecordsTheServingConnectionWhenARequestArrives(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	observer := newAddressObserver()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(`{"GPUs":[],"telemetryValid":false,"msSince":0}`))
		assert.NoError(t, err)
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         observer.connState,
	}
	listener := &oneConnListener{conn: addrConn{
		Conn:   serverConn,
		local:  tcpAddr(t, "10.172.54.70:14318"),
		remote: tcpAddr(t, "10.172.55.129:51000"),
	}}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() { _ = server.Close() })

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
	}}
	resp, err := client.Get("http://10.172.54.70:14318/v1/node-info")
	require.NoError(t, err, "inventory request")
	_, err = io.Copy(io.Discard, resp.Body)
	assert.NoError(t, err)
	assert.NoError(t, resp.Body.Close())

	require.Equal(t, []string{"10.172.54.70"}, observer.addresses(), "observed")
}
