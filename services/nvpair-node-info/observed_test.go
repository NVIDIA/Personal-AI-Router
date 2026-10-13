// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// stubConn reports fixed local/remote addresses; the observer only ever asks for
// those two, so nothing else needs implementing.
type stubConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (c stubConn) LocalAddr() net.Addr  { return c.local }
func (c stubConn) RemoteAddr() net.Addr { return c.remote }

func tcpAddr(t *testing.T, s string) *net.TCPAddr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", s)
	require.NoError(t, err, "resolve (%v, %v)", s, err)
	return a
}

func TestObserverRecordsTheAddressARemotePeerReached(t *testing.T) {
	o := newAddressObserver()
	o.connState(stubConn{
		local:  tcpAddr(t, "10.172.54.70:14318"),
		remote: tcpAddr(t, "10.172.55.129:51000"),
	}, http.StateActive)

	require.Equal(t, []string{"10.172.54.70"}, o.addresses(), "addresses")
}

// A loopback caller is this machine talking to itself and proves nothing about
// what another machine can reach, which is the entire point of the set.
func TestObserverIgnoresLoopbackPeers(t *testing.T) {
	o := newAddressObserver()
	o.connState(stubConn{
		local:  tcpAddr(t, "127.0.0.1:14318"),
		remote: tcpAddr(t, "127.0.0.1:51000"),
	}, http.StateActive)

	require.Empty(t, o.addresses(), "addresses")
}

// A connection that never became a request is not evidence; only StateActive is.
func TestObserverIgnoresConnectionsThatSendNothing(t *testing.T) {
	o := newAddressObserver()
	conn := stubConn{
		local:  tcpAddr(t, "10.172.54.70:14318"),
		remote: tcpAddr(t, "10.172.55.129:51000"),
	}
	o.connState(conn, http.StateNew)
	o.connState(conn, http.StateClosed)

	require.Empty(t, o.addresses(), "addresses")
}

// An address peers have stopped reaching must stop being reported, or a link that
// went away would keep outranking one that works.
func TestObserverExpiresStaleObservations(t *testing.T) {
	o := newAddressObserver()
	now := time.Now()
	o.now = func() time.Time { return now }
	o.connState(stubConn{
		local:  tcpAddr(t, "10.172.54.70:14318"),
		remote: tcpAddr(t, "10.172.55.129:51000"),
	}, http.StateActive)

	now = now.Add(observationTTL + time.Second)
	require.Empty(t, o.addresses(), "addresses")
}

func TestObserverReportsEmptySetAfterTheLastObservationExpires(t *testing.T) {
	o := newAddressObserver()
	now := time.Now()
	o.now = func() time.Time { return now }
	o.record("10.172.54.70")
	now = now.Add(observationTTL + time.Second)

	var out bytes.Buffer
	o.report(applog.NewNotifier(&out))
	var msg struct {
		Method string                          `json:"method"`
		Params noderec.ObservedAddressesParams `json:"params"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &msg), "decode report")
	require.Equal(t, noderec.NotifyObservedAddresses, msg.Method, "method")
	require.Empty(t, msg.Params.Addresses, "reported addresses")
}

func TestObserverReportsASortedSet(t *testing.T) {
	o := newAddressObserver()
	for _, local := range []string{"10.172.54.70:14318", "10.0.0.5:14318"} {
		o.connState(stubConn{
			local:  tcpAddr(t, local),
			remote: tcpAddr(t, "10.172.55.129:51000"),
		}, http.StateActive)
	}
	require.Equal(t, []string{"10.0.0.5", "10.172.54.70"}, o.addresses(), "addresses")
}
