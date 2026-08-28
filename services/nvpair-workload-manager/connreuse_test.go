// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"nvpair-shared/clustertrust"
)

// TestBroadcast_ReusesPeerConnections is the regression for stale "running"
// workload lines that never clear.
//
// The fan-out used to build a fresh http.Client (and Transport) per peer per
// event, so every lifecycle event paid a full mTLS handshake and then leaked the
// socket — a hand-built Transport has no IdleConnTimeout and its read loop keeps
// it reachable. Under an inference burst that exhausts the ability to handshake
// at all, and the events that go missing include the terminal
// completed/errored, which is what leaves a peer showing "running" forever.
//
// Several broadcast rounds must therefore ride ONE connection per peer.
func TestBroadcast_ReusesPeerConnections(t *testing.T) {
	self, peer := newPinnedPeerMeshes(t)

	var mu sync.Mutex
	newConns := 0

	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	// The peer is the server here: we are the pinned caller dialing it.
	ts.TLS = peer.ServerTLSConfig()
	ts.StartTLS()
	defer ts.Close()

	host, portStr, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	peers := newPeerSet(14320)
	peers.Replace([]PeerNode{{
		ID:        "peer-1",
		Host:      host,
		Addresses: []string{host},
		Port:      port,
		TXT:       []string{clustertrust.ClusterUUIDTXTKey + "=uuid-peer"},
	}})

	b := NewBroadcaster(peers, self)
	defer b.CloseIdle()

	const rounds = 5
	for i := 0; i < rounds; i++ {
		b.Broadcast(context.Background(), []byte(`{"jsonrpc":"2.0","method":"workload:started","params":{}}`))
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	if got != 1 {
		t.Fatalf("peer accepted %d connections for %d broadcast rounds, want 1 (a handshake per event is the leak that drops terminal events)", got, rounds)
	}
}

// TestInterNodeServer_ReapsIdleConnections: the listener half of connection
// reuse. Without an IdleTimeout an idle peer keep-alive pins a file descriptor
// for the life of the process. The listener's lifetime must also be strictly
// longer than the calling pool's, so the client is always the side that discards
// a doubtful connection rather than picking one this side is closing.
func TestInterNodeServer_ReapsIdleConnections(t *testing.T) {
	srv := newInterNodeServer(http.NewServeMux())
	if srv.IdleTimeout != clustertrust.PeerListenerIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, clustertrust.PeerListenerIdleTimeout)
	}
	if clustertrust.PeerListenerIdleTimeout <= clustertrust.PeerIdleTimeout {
		t.Errorf("listener idle (%v) must exceed the client's (%v), or the client can pick a connection the server is closing",
			clustertrust.PeerListenerIdleTimeout, clustertrust.PeerIdleTimeout)
	}
	if srv.ReadHeaderTimeout != interNodeReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, interNodeReadHeaderTimeout)
	}
}
