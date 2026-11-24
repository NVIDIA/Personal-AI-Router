// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

// genLeaf mints an Ed25519 self-signed leaf carrying uuid in CN + urn:nvpair:node
// URI SAN, matching nvpair-cluster-manager's generateLeaf.
func genLeaf(t *testing.T, uuid string) (certPEM, keyPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	uri, err := url.Parse("urn:nvpair:node:" + uuid)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: uuid},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	require.NoError(t, err, "create cert")
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// setupNode writes a cluster dir holding this node's identity (node.crt/key) and
// a pin (trusted/<uuid>.json) for each peer uuid -> cert PEM in pins.
func setupNode(t *testing.T, certPEM, keyPEM []byte, pins map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600))
	for uuid, pcert := range pins {
		clustertrusttest.WritePeerPinFromCertificate(t, dir, uuid, pcert)
	}
	return dir
}

// Peer node inventory requires a pinned client, including on a new connection.
// Shared mesh self-trust is also accepted; local services use private IPC.
func TestNodeInfoHandler_RequiresPinnedClients(t *testing.T) {
	aCert, aKey := genLeaf(t, "uuid-a")
	bCert, bKey := genLeaf(t, "uuid-b")
	cCert, cKey := genLeaf(t, "uuid-c")

	// A trusts only B. B trusts A. C trusts A (so C can dial A) but A does NOT
	// trust C — the asymmetry that proves the receiver-side gate.
	dirA := setupNode(t, aCert, aKey, map[string][]byte{"uuid-b": bCert})
	dirB := setupNode(t, bCert, bKey, map[string][]byte{"uuid-a": aCert})
	dirC := setupNode(t, cCert, cKey, map[string][]byte{"uuid-a": aCert})

	meshA, meshB, meshC := clustertrust.Open(dirA), clustertrust.Open(dirB), clustertrust.Open(dirC)
	require.True(t, meshA.Clustered(), "a populated cluster dir must read as clustered")
	require.True(t, meshB.Clustered(), "a populated cluster dir must read as clustered")
	require.True(t, meshC.Clustered(), "a populated cluster dir must read as clustered")

	principal := "uuid-a"
	wantInfo := buildResponse(nil, nil, 0, statsSnapshot{}, principal, &principal)
	metadata := &peerMetadata{value: nodeInfoMetadataForTest(14318)}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", nodeInfoHandler(meshA, metadata, func() NodeInfoResponse { return wantInfo }))
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = meshA.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	get := func(m *clustertrust.Mesh, peerUUID string) (int, noderec.NodeInfo) {
		t.Helper()
		cfg, ok := m.ClientTLSConfig(peerUUID)
		require.True(t, ok, "no client config for (%v)", peerUUID)
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Get(srv.URL + "/v1/node-info")
		require.NoError(t, err, "get")
		defer func() { require.NoError(t, resp.Body.Close()) }()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		var info noderec.NodeInfo
		if resp.StatusCode == http.StatusOK {
			require.NoError(t, json.Unmarshal(b, &info))
		}
		return resp.StatusCode, info
	}

	// Pinned member B -> A: accepted, real body.
	code, info := get(meshB, "uuid-a")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "uuid-a", info.HostUUID)
	assert.NotNil(t, info.Peer)

	// Self-read A -> A: accepted via self-trust (A isn't in its own trusted/).
	code, info = get(meshA, "uuid-a")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "uuid-a", info.HostUUID)
	assert.NotNil(t, info.Peer)

	// C completes the handshake (it pins A) but A doesn't pin C -> 403 at the gate.
	code, _ = get(meshC, "uuid-a")
	require.Equal(t, http.StatusForbidden, code, "non-member read")
}

func TestNodeInfoHandler_PreservesHardwareForPinnedPeers(t *testing.T) {
	// A pinned peer must receive the populated hardware snapshot alongside
	// registry metadata, including sample validity and its original age.
	serverCert, serverKey := genLeaf(t, "uuid-a")
	clientCert, clientKey := genLeaf(t, "uuid-b")
	serverDir := setupNode(t, serverCert, serverKey, map[string][]byte{"uuid-b": clientCert})
	clientDir := setupNode(t, clientCert, clientKey, map[string][]byte{"uuid-a": serverCert})
	serverMesh, clientMesh := clustertrust.Open(serverDir), clustertrust.Open(clientDir)
	principal := "uuid-a"
	snapshot := NodeInfoResponse{
		GPUs:           []GPUInfo{{Name: "GPU 0", VramBytes: 48 << 30, VramUsedBytes: 12 << 30, UtilizationPercent: 42}},
		CPU:            &CPUInfo{Name: "CPU 0", Cores: 64, UtilizationPercent: 7},
		Memory:         &MemoryInfo{TotalBytes: 128 << 30, UsedBytes: 32 << 30},
		TelemetryValid: true, MSSince: 137,
		HostUUID: principal, ClusterUUID: &principal,
	}
	metadata := &peerMetadata{value: nodeInfoMetadataForTest(14318)}
	server := httptest.NewUnstartedServer(nodeInfoHandler(serverMesh, metadata, func() NodeInfoResponse { return snapshot }))
	server.TLS = serverMesh.ServerTLSConfig()
	server.StartTLS()
	t.Cleanup(server.Close)
	clientTLS, ok := clientMesh.ClientTLSConfig("uuid-a")
	require.True(t, ok)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS}}
	t.Cleanup(client.CloseIdleConnections)

	response, err := client.Get(server.URL + "/v1/node-info")
	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var received noderec.NodeInfo
	require.NoError(t, json.NewDecoder(response.Body).Decode(&received))

	assert.Equal(t, []noderec.GPUInfo{{Name: "GPU 0", VramBytes: 48 << 30, VramUsedBytes: 12 << 30, UtilizationPercent: 42}}, received.GPUs)
	assert.Equal(t, &noderec.CPUInfo{Name: "CPU 0", Cores: 64, UtilizationPercent: 7}, received.CPU)
	assert.Equal(t, &noderec.MemoryInfo{TotalBytes: 128 << 30, UsedBytes: 32 << 30}, received.Memory)
	assert.True(t, received.TelemetryValid)
	assert.Equal(t, int64(137), received.MSSince)
	assert.Equal(t, &metadata.value.Peer, received.Peer)
}
