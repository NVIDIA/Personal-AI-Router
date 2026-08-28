// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genLeaf mints an Ed25519 self-signed leaf carrying the node UUID in both the
// CN and the urn:nvpair:node: URI SAN, mirroring nvpair-cluster-manager's
// generateLeaf so the test exercises the real principal-extraction path.
func genLeaf(t *testing.T, uuid string) (certPEM, keyPEM, der []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	uri, _ := url.Parse(nodeURISANPrefix + uuid)
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
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, der
}

func writePin(t *testing.T, clusterDir, uuid, certPEM string) {
	t.Helper()
	dir := filepath.Join(clusterDir, "trusted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir trusted: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"nodeUuid": uuid, "certPem": certPEM})
	if err := os.WriteFile(filepath.Join(dir, uuid+".json"), body, 0o600); err != nil {
		t.Fatalf("write pin: %v", err)
	}
}

func TestLoadIdentityAndUUID(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, _ := genLeaf(t, "uuid-self")
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if id.NodeUUID != "uuid-self" {
		t.Fatalf("NodeUUID = %q, want uuid-self", id.NodeUUID)
	}
	if len(id.Cert.Certificate) == 0 {
		t.Fatal("identity cert not loaded")
	}

	// Missing keypair is an error (caller falls back to plain HTTP).
	if _, err := LoadIdentity(t.TempDir()); err == nil {
		t.Fatal("expected error loading identity from an empty dir")
	}
}

func TestTrustPinMatchAndGate(t *testing.T) {
	dir := t.TempDir()
	selfCert, selfKey, _ := genLeaf(t, "uuid-self")
	_ = os.WriteFile(filepath.Join(dir, "node.crt"), selfCert, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "node.key"), selfKey, 0o600)
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}

	peerCertPEM, _, peerDER := genLeaf(t, "uuid-peer")
	writePin(t, dir, "uuid-peer", string(peerCertPEM))

	tr := newTrust(dir)
	tr.Reload()
	if tr.Count() != 1 {
		t.Fatalf("trust count = %d, want 1", tr.Count())
	}
	if !tr.MatchDER("uuid-peer", peerDER) {
		t.Fatal("pinned peer DER should match")
	}
	if tr.MatchDER("uuid-peer", []byte("nope")) {
		t.Fatal("wrong DER must not match")
	}
	if tr.MatchDER("uuid-unknown", peerDER) {
		t.Fatal("unknown uuid must not match")
	}

	// The cluster gate is the pin lookup: a pinned peer resolves a DER (so the
	// caller can build a pinned client), an unknown peer does not.
	der, okDER := tr.DER("uuid-peer")
	if !okDER {
		t.Fatal("pinned peer DER should resolve")
	}
	if cfg := ClientTLSConfig(id.Cert, der); cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatal("ClientTLSConfig should present our leaf")
	}
	if _, ok := tr.DER("uuid-unknown"); ok {
		t.Fatal("an unpinned peer must not resolve a DER (cluster gate)")
	}

	// Server config presents our leaf and requires a client cert.
	sc := ServerTLSConfig(id.Cert)
	if sc.ClientAuth != tls.RequireAnyClientCert || len(sc.Certificates) != 1 {
		t.Fatalf("server config: ClientAuth=%v certs=%d", sc.ClientAuth, len(sc.Certificates))
	}
}

func TestVerifyClientPin(t *testing.T) {
	dir := t.TempDir()
	peerCertPEM, _, _ := genLeaf(t, "uuid-peer")
	writePin(t, dir, "uuid-peer", string(peerCertPEM))
	otherCertPEM, _, _ := genLeaf(t, "uuid-other")
	tr := newTrust(dir)
	tr.Reload()

	parse := func(pemBytes []byte) *x509.Certificate {
		block, _ := pem.Decode(pemBytes)
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return c
	}
	reqWith := func(cert *x509.Certificate) *http.Request {
		r := &http.Request{}
		if cert != nil {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		}
		return r
	}

	if uuid, ok := VerifyClientPin(reqWith(parse(peerCertPEM)), tr.MatchDER); !ok || uuid != "uuid-peer" {
		t.Fatalf("pinned client should verify, got (%q,%v)", uuid, ok)
	}
	if _, ok := VerifyClientPin(reqWith(parse(otherCertPEM)), tr.MatchDER); ok {
		t.Fatal("an unpinned client must be rejected")
	}
	if _, ok := VerifyClientPin(reqWith(nil), tr.MatchDER); ok {
		t.Fatal("a request with no client cert must be rejected")
	}
}

// TestMeshClusterOfOneTrustsOnlyItself covers the "identity, zero peers" state
// (the scanner annotates a browsed peer trusted via mesh.HasPin): a node that
// minted its own cluster identity but paired with nobody must not mark any
// browsed peer trusted. Self-trust still holds for a live member, so
// DirectoryNode.Trusted stays false for real peers while the node can still
// reach its own gated endpoints.
func TestMeshClusterOfOneTrustsOnlyItself(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, _ := genLeaf(t, "self-uuid")
	writeIdentity(t, dir, certPEM, keyPEM)
	writeAdmission(t, dir, "cluster-abc", 1)

	m := Open(dir)
	if !m.Clustered() {
		t.Fatal("an active admission must make the node clustered")
	}
	if m.PeerCount() != 0 {
		t.Fatalf("PeerCount = %d, want 0 (no pins)", m.PeerCount())
	}
	if m.HasPin("some-browsed-peer") {
		t.Error("a node with an identity but no pins must not trust a browsed peer")
	}
	if !m.HasPin("self-uuid") {
		t.Error("self-trust: the node must trust its own principal")
	}

	// Membership is what opens the gate, not the keypair: with the admission torn
	// down the same loaded identity trusts nobody, including itself, so no
	// cluster-scoped surface can be served or dialed.
	writeAdmission(t, dir, "", 0)
	m.Refresh()
	if m.HasPin("self-uuid") {
		t.Error("a non-member must not resolve any principal, including its own")
	}
}

func TestTrustReloadAndTamperSkip(t *testing.T) {
	dir := t.TempDir()
	aPEM, _, _ := genLeaf(t, "uuid-a")
	writePin(t, dir, "uuid-a", string(aPEM))
	tr := newTrust(dir)
	tr.Reload()
	if tr.Count() != 1 {
		t.Fatalf("count = %d, want 1", tr.Count())
	}

	// A newly-paired peer appears; Reload picks it up.
	bPEM, _, _ := genLeaf(t, "uuid-b")
	writePin(t, dir, "uuid-b", string(bPEM))
	tr.Reload()
	if tr.Count() != 2 {
		t.Fatalf("after reload count = %d, want 2", tr.Count())
	}

	// A tampered file (filename UUID != cert principal) is skipped.
	cPEM, _, _ := genLeaf(t, "uuid-c")
	body, _ := json.Marshal(map[string]string{"nodeUuid": "uuid-wrong", "certPem": string(cPEM)})
	_ = os.WriteFile(filepath.Join(dir, "trusted", "uuid-wrong.json"), body, 0o600)
	tr.Reload()
	if tr.Count() != 2 {
		t.Fatalf("tampered pin must be skipped, count = %d, want 2", tr.Count())
	}
}
