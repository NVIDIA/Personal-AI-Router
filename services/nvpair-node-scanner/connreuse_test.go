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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// Repeated model inventory reads reuse one authenticated peer connection.
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
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"models": []string{"m"}}))
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
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	// "localhost" is not a loopback IP literal, so modelsClient takes the
	// pinned mTLS path instead of the plaintext self-fetch.
	const rounds = 5
	for i := 0; i < rounds; i++ {
		inventory, ok := d.fetchModels("localhost", port, "uuid-peer")
		require.True(t, ok, "round %d", i)
		assert.Equal(t, []string{"m"}, inventory.models, "round %d", i)
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	require.Equal(t, 1, got, "peer accepted (%v, %v)", got, rounds)
}

func writePinFromCert(t *testing.T, clusterDir, peerUUID, certPath string) {
	t.Helper()
	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	clustertrusttest.WritePeerPinFromCertificate(t, clusterDir, peerUUID, certPEM)
}
