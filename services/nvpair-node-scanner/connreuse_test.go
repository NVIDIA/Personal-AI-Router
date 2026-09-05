// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
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

// TestModelsClient_ReusesPeerConnections is the regression for the 15s
// enrichment handshake storm: modelsClient used to build a fresh Transport
// per peer fetch. Several /v1/models GETs must ride ONE connection.
func TestModelsClient_ReusesPeerConnections(t *testing.T) {
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
	d := &daemon{
		mesh:          selfMesh,
		modelsClients: clustertrust.NewPeerClientPool(selfMesh, modelsFetchTimeout),
	}
	defer d.modelsClients.CloseIdle()

	var mu sync.Mutex
	newConns := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []string{"m"}})
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

	_, portStr, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	// "localhost" is not a loopback IP literal, so modelsClient takes the
	// pinned mTLS path instead of the plaintext self-fetch.
	const rounds = 5
	for i := 0; i < rounds; i++ {
		models, _, _, _, ok := d.fetchModels("localhost", port, "uuid-peer")
		if !ok || len(models) != 1 || models[0] != "m" {
			t.Fatalf("round %d: ok=%v models=%v", i, ok, models)
		}
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	if got != 1 {
		t.Fatalf("peer accepted %d connections for %d model fetches, want 1", got, rounds)
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
