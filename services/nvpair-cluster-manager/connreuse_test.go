// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

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
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := generateLeaf(peerUUID, "node-peer")
	if err != nil {
		t.Fatal(err)
	}
	fp, ferr := certFingerprintFromPEM(certPEM)
	if ferr != nil {
		t.Fatal(ferr)
	}
	pinTrusted(t, m, peerUUID, string(certPEM), fp)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	newConns := 0
	mux := http.NewServeMux()
	mux.HandleFunc(rosterPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clusterId":"cluster-1"}`))
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
		if outcome, _ := m.reconcileWith([]string{addr}, peerUUID); outcome != reconcileAccepted {
			t.Fatalf("round %d: outcome = %v, want accepted", i, outcome)
		}
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	if got != 1 {
		t.Fatalf("peer accepted %d connections for %d reconciles, want 1 (a handshake per pass is the leak)", got, rounds)
	}
}

// TestPeerClient_ForgetRevokesWithPinStillOnDisk covers a failed durable pin
// delete during pairing rollback. Forget is the live TrustStore revocation; the
// leftover file must not let Mesh or the connection pool authorize outbound
// traffic after inbound traffic already rejects the peer.
func TestPeerClient_ForgetRevokesWithPinStillOnDisk(t *testing.T) {
	m := newTestManager(t)
	defer m.clients.CloseIdle()

	peerUUID, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := generateLeaf(peerUUID, "node-peer")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := certFingerprintFromPEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	pinTrusted(t, m, peerUUID, string(certPEM), fp)

	if _, err := m.peerClient(peerUUID); err != nil {
		t.Fatalf("pinned peer must yield a client: %v", err)
	}
	pinPath := m.trust.pinPath(peerUUID)
	if _, err := os.Stat(pinPath); err != nil {
		t.Fatalf("stat pin before Forget: %v", err)
	}

	m.trust.Forget(peerUUID)

	if _, err := os.Stat(pinPath); err != nil {
		t.Fatalf("Forget must leave the durable pin in place: %v", err)
	}
	if _, ok := m.trust.DER(peerUUID); ok {
		t.Fatal("Forget must remove the TrustStore authorization")
	}
	if _, err := m.peerClient(peerUUID); err == nil {
		t.Fatal("forgotten peer yielded a client because Mesh re-read the leftover pin")
	}
}
