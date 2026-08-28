// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// TestRemoteClient_ReusesPeerConnections is the regression for the
// engine:remote-* leak: remoteClient used to build a fresh Transport per call,
// so every remote op paid a full mTLS handshake and then leaked the socket.
func TestRemoteClient_ReusesPeerConnections(t *testing.T) {
	selfDir := t.TempDir()
	peerDir := t.TempDir()
	clustertrusttest.WriteKeypair(t, selfDir, "uuid-self")
	clustertrusttest.WriteKeypair(t, peerDir, "uuid-peer")
	clustertrusttest.WriteAdmission(t, selfDir, "cluster-1", 1)
	clustertrusttest.WriteAdmission(t, peerDir, "cluster-1", 1)
	writePinFromCert(t, selfDir, "uuid-peer", filepath.Join(peerDir, "node.crt"))
	writePinFromCert(t, peerDir, "uuid-self", filepath.Join(selfDir, "node.crt"))

	selfMesh := clustertrust.Open(selfDir)
	peerMesh := clustertrust.Open(peerDir)
	if !selfMesh.Clustered() || !peerMesh.Clustered() {
		t.Fatal("both nodes must read as clustered")
	}

	var mu sync.Mutex
	newConns := 0
	mux := http.NewServeMux()
	mux.HandleFunc(controlEnginesPath, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"engines": []any{}})
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	ts.TLS = peerMesh.ServerTLSConfig()
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

	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, selfMesh)
	defer m.remoteHTTP.CloseIdle()
	defer m.readyHTTP.CloseIdle()

	client, err := m.remoteClient(context.Background(), ecPeer{
		nodeID:      "host-peer",
		addresses:   []string{host},
		port:        port,
		clusterUUID: "uuid-peer",
	})
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 5
	for i := 0; i < rounds; i++ {
		if _, gerr := client.getEngines(context.Background()); gerr != nil {
			t.Fatalf("round %d: %v", i, gerr)
		}
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	if got != 1 {
		t.Fatalf("peer accepted %d connections for %d getEngines calls, want 1", got, rounds)
	}
}

func writePinFromCert(t *testing.T, clusterDir, peerUUID, certPath string) {
	t.Helper()
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(clusterDir, "trusted")
	if err := os.MkdirAll(trusted, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"nodeUuid": peerUUID, "certPem": string(certPEM)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trusted, peerUUID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}
