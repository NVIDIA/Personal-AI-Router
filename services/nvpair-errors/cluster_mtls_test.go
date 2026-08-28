// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/errors"
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

// servePinnedErrorsMux starts mgr's /v1/errors surface behind cluster mTLS and
// returns a client that dials it as a pinned peer, plus the server's own mesh.
// The surface is mTLS unconditionally, so there is no plaintext way to reach the
// handlers — every handler test goes through here.
func servePinnedErrorsMux(t *testing.T, mgr *Manager) (*httptest.Server, *http.Client) {
	t.Helper()
	selfCert, selfKey := genLeaf(t, "uuid-self")
	peerCert, peerKey := genLeaf(t, "uuid-peer")
	selfMesh := clustertrust.Open(setupNode(t, selfCert, selfKey, map[string][]byte{"uuid-peer": peerCert}))
	peerMesh := clustertrust.Open(setupNode(t, peerCert, peerKey, map[string][]byte{"uuid-self": selfCert}))

	srv := httptest.NewUnstartedServer(newErrorsMux(mgr, selfMesh))
	srv.TLS = selfMesh.ServerTLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)

	cfg, ok := peerMesh.ClientTLSConfig("uuid-self")
	if !ok {
		t.Fatal("the pinned peer must be able to build a client for self")
	}
	return srv, &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

// TestErrorsPeerSync_MTLSGate is the end-to-end evidence: error peer
// sync over mTLS authenticates and gates to pinned cluster members. A pinned
// member is accepted; a node that can complete the TLS handshake but is not
// pinned by the receiver is rejected at the pin gate; and pushing to a peer we
// hold no pin for can't even build a client.
func TestErrorsPeerSync_MTLSGate(t *testing.T) {
	aCert, aKey := genLeaf(t, "uuid-a")
	bCert, bKey := genLeaf(t, "uuid-b")
	cCert, cKey := genLeaf(t, "uuid-c")

	// A trusts only B. B trusts A. C trusts A (so C can dial A) but A does NOT
	// trust C — the asymmetry that proves the receiver-side gate.
	dirA := setupNode(t, aCert, aKey, map[string][]byte{"uuid-b": bCert})
	dirB := setupNode(t, bCert, bKey, map[string][]byte{"uuid-a": aCert})
	dirC := setupNode(t, cCert, cKey, map[string][]byte{"uuid-a": aCert})

	mtlsA, mtlsB, mtlsC := clustertrust.Open(dirA), clustertrust.Open(dirB), clustertrust.Open(dirC)
	if !mtlsA.Clustered() || !mtlsB.Clustered() || !mtlsC.Clustered() {
		t.Fatal("a populated cluster dir must read as clustered")
	}

	// A serves its errors ingest over mTLS.
	mgrA := NewManager(NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), io.Discard}))
	srv := httptest.NewUnstartedServer(newErrorsMux(mgrA, mtlsA))
	srv.TLS = mtlsA.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	body, _ := json.Marshal(errors.SyncEnvelope{NodeID: "uuid-b"})
	push := func(m *clustertrust.Mesh, peerUUID string) (int, error) {
		cfg, ok := m.ClientTLSConfig(peerUUID)
		if !ok {
			return 0, fmt.Errorf("no pin for peer %s", peerUUID)
		}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Post(srv.URL+"/v1/errors", "application/json", bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	// Pinned member B -> A: accepted (204).
	if code, err := push(mtlsB, "uuid-a"); err != nil || code != http.StatusNoContent {
		t.Fatalf("pinned member push: code=%d err=%v, want 204", code, err)
	}

	// C completes the handshake (it pins A) but A doesn't pin C -> 403 at the gate.
	if code, err := push(mtlsC, "uuid-a"); err != nil || code != http.StatusForbidden {
		t.Fatalf("non-member push: code=%d err=%v, want 403", code, err)
	}

	// Client-side gate: B holds no pin for an unknown peer, so the DER lookup
	// fails and it would never build a client to contact a non-member.
	if mtlsB.HasPin("uuid-unknown") {
		t.Fatal("an unpinned peer must not resolve a pin (cluster gate)")
	}
}
