// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Gate tests for the em model-list surface. A node's model inventory is cluster
// data: a LAN caller must be a pinned cluster peer, while this node's own scanner
// keeps reading it over loopback in plaintext in every membership state (that is
// what makes a standalone machine still show its own models).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

// mintLeaf returns a self-signed Ed25519 leaf carrying uuid in its CN and its
// urn:nvpair:node: URI SAN, matching what nvpair-cluster-manager issues.
func mintLeaf(t *testing.T, uuid string) (certPEM, keyPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "genkey")
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	san, _ := url.Parse("urn:nvpair:node:" + uuid)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: uuid},
		URIs:                  []*url.URL{san},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	require.NoError(t, err, "create cert")
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// clusterDirFor writes a cluster dir holding this node's identity, an active
// admission, and a pin for each peer uuid -> cert PEM.
func clusterDirFor(t *testing.T, certPEM, keyPEM []byte, pins map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600))
	admission, _ := json.Marshal(map[string]any{"counter": 1, "activated": 1, "clusterId": "gate-test", "epoch": 1})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "admission.json"), admission, 0o600))
	trusted := filepath.Join(dir, "trusted")
	require.NoError(t, os.MkdirAll(trusted, 0o700))
	for uuid, cert := range pins {
		body, _ := json.Marshal(map[string]string{"nodeUuid": uuid, "certPem": string(cert)})
		require.NoError(t, os.WriteFile(filepath.Join(trusted, uuid+".json"), body, 0o600))
	}
	return dir
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":["llama"]}`))
	})
}

// TestModelSurface_LoopbackOnlyForPlaintext: the plaintext personality serves this
// node's own scanner and nothing else. It must keep working while unclustered —
// a standalone machine reads its own inventory this way — and must refuse a LAN
// caller, which is the leak this gate closes.
func TestModelSurface_LoopbackOnlyForPlaintext(t *testing.T) {
	h := loopbackOnly(okHandler())

	for _, remote := range []string{"127.0.0.1:51234", "[::1]:51234"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
		req.RemoteAddr = remote
		h(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "loopback (%v)", remote)
	}

	for _, remote := range []string{"192.168.1.42:51234", "10.0.0.7:51234"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
		req.RemoteAddr = remote
		h(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, "LAN (%v)", remote)
	}
}

// TestModelSurface_PinGateIsUnconditional: the mTLS personality admits a pinned
// cluster peer and nobody else — including while this node belongs to no cluster,
// where it must admit no one at all rather than falling open.
func TestModelSurface_PinGateIsUnconditional(t *testing.T) {
	selfCert, selfKey := mintLeaf(t, "uuid-self")
	peerCert, peerKey := mintLeaf(t, "uuid-peer")
	strangerCert, strangerKey := mintLeaf(t, "uuid-stranger")

	// self pins peer only. peer and stranger both pin self, so both can complete a
	// handshake to self — the asymmetry that proves the pin gate does the work.
	selfMesh := clustertrust.Open(clusterDirFor(t, selfCert, selfKey, map[string][]byte{"uuid-peer": peerCert}))
	peerMesh := clustertrust.Open(clusterDirFor(t, peerCert, peerKey, map[string][]byte{"uuid-self": selfCert}))
	strangerMesh := clustertrust.Open(clusterDirFor(t, strangerCert, strangerKey, map[string][]byte{"uuid-self": selfCert}))

	srv := httptest.NewUnstartedServer(requirePinnedPeer(selfMesh, okHandler()))
	srv.TLS = selfMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	get := func(m *clustertrust.Mesh) (int, error) {
		cfg, ok := m.ClientTLSConfig("uuid-self")
		require.True(t, ok, "a mesh pinning uuid-self must build a client for it")
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Get(srv.URL + modelsPath)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	code, err := get(peerMesh)
	require.NoError(t, err, "pinned peer: code (%v, %v)", code, err)
	require.Equal(t, http.StatusOK, code, "pinned peer")
	code, err = get(strangerMesh)
	require.NoError(t, err, "unpinned cluster identity: code (%v, %v)", code, err)
	require.Equal(t, http.StatusForbidden, code, "unpinned cluster identity")

	// An unauthenticated request never carries a client cert, so it is refused
	// whatever this node's membership is — the gate has no membership branch.
	for _, name := range []string{"clustered", "unclustered"} {
		mesh := selfMesh
		if name == "unclustered" {
			mesh = clustertrust.Open(t.TempDir())
		}
		rec := httptest.NewRecorder()
		requirePinnedPeer(mesh, okHandler())(rec, httptest.NewRequest(http.MethodGet, modelsPath, nil))
		require.Equal(t, http.StatusForbidden, rec.Code, " (%v)", name)
	}
}
