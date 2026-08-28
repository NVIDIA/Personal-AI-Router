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

	"nvpair-shared/clustertrust"
)

// genLeaf mints an Ed25519 self-signed leaf carrying uuid in CN + urn:nvpair:node
// URI SAN, matching nvpair-cluster-manager's generateLeaf.
func genLeaf(t *testing.T, uuid string) (certPEM, keyPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	uri, _ := url.Parse("urn:nvpair:node:" + uuid)
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
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// setupNode writes a cluster dir holding this node's identity (node.crt/key) and
// a pin (trusted/<uuid>.json) for each peer uuid -> cert PEM in pins.
func setupNode(t *testing.T, certPEM, keyPEM []byte, pins map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	td := filepath.Join(dir, "trusted")
	if err := os.MkdirAll(td, 0o700); err != nil {
		t.Fatal(err)
	}
	for uuid, pcert := range pins {
		body, _ := json.Marshal(map[string]string{"nodeUuid": uuid, "certPem": string(pcert)})
		if err := os.WriteFile(filepath.Join(td, uuid+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestNodeInfoHandler_MTLSGate is the evidence for node-info: the GPU
// inventory is served only to pinned cluster members. A pinned peer is accepted
// (200), a node that completes the handshake but isn't pinned by the server is
// rejected at the gate (403), and — the behavior node-info specifically needs —
// the node can read its OWN inventory over the same mTLS path via self-trust
// (so the local scanner's self-read still works with no plaintext loopback).
func TestNodeInfoHandler_MTLSGate(t *testing.T) {
	aCert, aKey := genLeaf(t, "uuid-a")
	bCert, bKey := genLeaf(t, "uuid-b")
	cCert, cKey := genLeaf(t, "uuid-c")

	// A trusts only B. B trusts A. C trusts A (so C can dial A) but A does NOT
	// trust C — the asymmetry that proves the receiver-side gate.
	dirA := setupNode(t, aCert, aKey, map[string][]byte{"uuid-b": bCert})
	dirB := setupNode(t, bCert, bKey, map[string][]byte{"uuid-a": aCert})
	dirC := setupNode(t, cCert, cKey, map[string][]byte{"uuid-a": aCert})

	meshA, meshB, meshC := clustertrust.Open(dirA), clustertrust.Open(dirB), clustertrust.Open(dirC)
	if !meshA.Clustered() || !meshB.Clustered() || !meshC.Clustered() {
		t.Fatal("a populated cluster dir must read as clustered")
	}

	const wantBody = `{"GPUs":[]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", nodeInfoHandler(meshA, func() []byte { return []byte(wantBody) }))
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = meshA.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	get := func(m *clustertrust.Mesh, peerUUID string) (int, string) {
		cfg, ok := m.ClientTLSConfig(peerUUID)
		if !ok {
			t.Fatalf("no client config for %s", peerUUID)
		}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Get(srv.URL + "/v1/node-info")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Pinned member B -> A: accepted, real body.
	if code, body := get(meshB, "uuid-a"); code != http.StatusOK || body != wantBody {
		t.Fatalf("pinned member read: code=%d body=%q, want 200 %q", code, body, wantBody)
	}

	// Self-read A -> A: accepted via self-trust (A isn't in its own trusted/).
	if code, body := get(meshA, "uuid-a"); code != http.StatusOK || body != wantBody {
		t.Fatalf("self read: code=%d body=%q, want 200 %q (self-trust)", code, body, wantBody)
	}

	// C completes the handshake (it pins A) but A doesn't pin C -> 403 at the gate.
	if code, _ := get(meshC, "uuid-a"); code != http.StatusForbidden {
		t.Fatalf("non-member read: code=%d, want 403", code)
	}
}
