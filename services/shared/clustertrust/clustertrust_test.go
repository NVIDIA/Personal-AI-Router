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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genLeaf mints an Ed25519 self-signed leaf carrying the node UUID in both the
// CN and the urn:nvpair:node: URI SAN, mirroring nvpair-cluster-manager's
// generateLeaf so the test exercises the real principal-extraction path.
func genLeaf(t *testing.T, uuid string) (certPEM, keyPEM, der []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "genkey")
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	assert.NoError(t, err, "generate certificate serial")
	uri, err := url.Parse(nodeURISANPrefix + uuid)
	require.NoError(t, err, "parse certificate URI")
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
	require.NoError(t, err, "create cert")
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	assert.NoError(t, err, "marshal private key")
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, der
}

func writePin(t *testing.T, clusterDir, uuid, certPEM string) {
	t.Helper()
	dir := filepath.Join(clusterDir, "trusted")
	require.NoError(t, os.MkdirAll(dir, 0o700), "mkdir trusted")
	body, err := json.Marshal(map[string]string{"nodeUuid": uuid, "certPem": certPEM})
	assert.NoError(t, err, "marshal peer pin")
	require.NoError(t, os.WriteFile(filepath.Join(dir, uuid+".json"), body, 0o600), "write pin")
}

func TestLoadIdentityAndUUID(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, _ := genLeaf(t, "uuid-self")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600))

	id, err := LoadIdentity(dir)
	require.NoError(t, err, "LoadIdentity")
	assert.Equal(t, "uuid-self", id.NodeUUID)
	assert.NotEmpty(t, id.Cert.Certificate, "identity cert not loaded")

	// Missing keypair is an error (caller falls back to plain HTTP).
	_, err = LoadIdentity(t.TempDir())
	require.Error(t, err, "expected error loading identity from an empty dir")
}

func TestTrustPinMatchAndGate(t *testing.T) {
	dir := t.TempDir()
	selfCert, selfKey, _ := genLeaf(t, "uuid-self")
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "node.crt"), selfCert, 0o644))
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"), selfKey, 0o600))
	id, err := LoadIdentity(dir)
	require.NoError(t, err, "LoadIdentity")

	peerCertPEM, _, peerDER := genLeaf(t, "uuid-peer")
	writePin(t, dir, "uuid-peer", string(peerCertPEM))

	tr := newTrust(dir)
	tr.Reload()
	assert.Equal(t, 1, tr.Count(), "trust count")
	assert.True(t, tr.MatchDER("uuid-peer", peerDER), "pinned peer DER should match")
	assert.False(t, tr.MatchDER("uuid-peer", []byte("nope")), "wrong DER must not match")
	assert.False(t, tr.MatchDER("uuid-unknown", peerDER), "unknown uuid must not match")

	// The cluster gate is the pin lookup: a pinned peer resolves a DER (so the
	// caller can build a pinned client), an unknown peer does not.
	der, okDER := tr.DER("uuid-peer")
	require.True(t, okDER, "pinned peer DER should resolve")
	cfg := ClientTLSConfig(id.Cert, der)
	require.NotNil(t, cfg, "ClientTLSConfig should present our leaf")
	require.Len(t, cfg.Certificates, 1, "ClientTLSConfig should present our leaf")
	_, ok := tr.DER("uuid-unknown")
	require.False(t, ok, "an unpinned peer must not resolve a DER (cluster gate)")

	// Server config presents our leaf and requires a client cert.
	sc := ServerTLSConfig(id.Cert)
	assert.Equal(t, tls.RequireAnyClientCert, sc.ClientAuth, "server config: ClientAuth")
	require.Len(t, sc.Certificates, 1, "server config: ClientAuth")
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
		require.NoError(t, err, "parse")
		return c
	}
	reqWith := func(cert *x509.Certificate) *http.Request {
		r := &http.Request{}
		if cert != nil {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		}
		return r
	}

	uuid, ok := VerifyClientPin(reqWith(parse(peerCertPEM)), tr.MatchDER)
	require.True(t, ok, "pinned client should verify")
	assert.Equal(t, "uuid-peer", uuid, "pinned client should verify")
	_, ok = VerifyClientPin(reqWith(parse(otherCertPEM)), tr.MatchDER)
	require.False(t, ok, "an unpinned client must be rejected")
	_, ok = VerifyClientPin(reqWith(nil), tr.MatchDER)
	require.False(t, ok, "a request with no client cert must be rejected")
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
	assert.True(t, m.Clustered(), "an active admission must make the node clustered")
	assert.Equal(t, 0, m.PeerCount())
	assert.False(t, m.HasPin("some-browsed-peer"), "a node with an identity but no pins must not trust a browsed peer")
	assert.True(t, m.HasPin("self-uuid"), "self-trust: the node must trust its own principal")

	// Membership is what opens the gate, not the keypair: with the admission torn
	// down the same loaded identity trusts nobody, including itself, so no
	// cluster-scoped surface can be served or dialed.
	writeAdmission(t, dir, "", 0)
	m.Refresh()
	assert.False(t, m.HasPin("self-uuid"), "a non-member must not resolve any principal, including its own")
}

func TestTrustReloadAndTamperSkip(t *testing.T) {
	dir := t.TempDir()
	aPEM, _, _ := genLeaf(t, "uuid-a")
	writePin(t, dir, "uuid-a", string(aPEM))
	tr := newTrust(dir)
	tr.Reload()
	assert.Equal(t, 1, tr.Count())

	// A newly-paired peer appears; Reload picks it up.
	bPEM, _, _ := genLeaf(t, "uuid-b")
	writePin(t, dir, "uuid-b", string(bPEM))
	tr.Reload()
	assert.Equal(t, 2, tr.Count(), "after reload count")

	// A tampered file (filename UUID != cert principal) is skipped.
	cPEM, _, _ := genLeaf(t, "uuid-c")
	body, err := json.Marshal(map[string]string{"nodeUuid": "uuid-wrong", "certPem": string(cPEM)})
	assert.NoError(t, err, "marshal mismatched peer pin")
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "trusted", "uuid-wrong.json"), body, 0o600))
	tr.Reload()
	assert.Equal(t, 2, tr.Count(), "tampered pin must be skipped, count")
}
