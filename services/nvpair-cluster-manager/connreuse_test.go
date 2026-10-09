// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

// TestReconcile_ReusesPeerConnections is the regression for the roster
// heartbeat leak: reconcileWith used to build a fresh http.Client (and
// Transport) per peer per pass, so every 30s tick paid a full mTLS handshake
// and then leaked the socket — a hand-built Transport has no IdleConnTimeout
// and its read loop keeps it reachable. Several reconciles must ride ONE
// connection per peer.
func TestReconcile_ReusesPeerConnections(t *testing.T) {
	m := newTestManager(t)
	defer m.clients.CloseIdle()

	peerUUID, err := newUUIDv4()
	require.NoError(t, err)
	certPEM, keyPEM, err := generateLeaf(peerUUID, "node-peer")
	require.NoError(t, err)
	fp, ferr := certFingerprintFromPEM(certPEM)
	require.NoError(t, ferr)
	pinTrusted(t, m, peerUUID, string(certPEM), fp)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	var mu sync.Mutex
	newConns := 0
	mux := http.NewServeMux()
	mux.HandleFunc(rosterPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, []byte(`{"clusterId":"cluster-1"}`))
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	ts.TLS = clustertrust.ServerTLSConfig(cert)
	ts.StartTLS()
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	const rounds = 5
	for i := 0; i < rounds; i++ {
		outcome, _ := m.reconcileWith([]string{addr}, peerUUID)
		require.Equal(t, reconcileAccepted, outcome, "round (%v)", i)
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	require.Equal(t, 1, got, "peer accepted (%v, %v)", got, rounds)
}

// TestPeerClient_ForgetRevokesWithPinStillOnDisk covers a failed durable pin
// delete during pairing rollback. Forget is the live TrustStore revocation; the
// leftover file must not let Mesh or the connection pool authorize outbound
// traffic after inbound traffic already rejects the peer.
func TestPeerClient_ForgetRevokesWithPinStillOnDisk(t *testing.T) {
	m := newTestManager(t)
	defer m.clients.CloseIdle()

	peerUUID, err := newUUIDv4()
	require.NoError(t, err)
	certPEM, _, err := generateLeaf(peerUUID, "node-peer")
	require.NoError(t, err)
	fp, err := certFingerprintFromPEM(certPEM)
	require.NoError(t, err)
	pinTrusted(t, m, peerUUID, string(certPEM), fp)

	_, err = m.peerClient(peerUUID)
	require.NoError(t, err, "pinned peer must yield a client")
	pinPath := m.trust.pinPath(peerUUID)
	require.FileExists(t, pinPath, "stat pin before Forget")

	m.trust.Forget(peerUUID)

	require.FileExists(t, pinPath, "Forget must leave the durable pin in place")
	_, ok := m.trust.DER(peerUUID)
	require.False(t, ok, "Forget must remove the TrustStore authorization")
	_, err = m.peerClient(peerUUID)
	require.Error(t, err, "forgotten peer yielded a client because Mesh re-read the leftover pin")
}
