// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"nvpair-shared/nodeid"
)

// nodeURISAN is the URI SAN scheme embedded in every leaf certificate, carrying
// the node's UUID as a stable cryptographic principal: urn:nvpair:node:<uuid>.
const nodeURISANPrefix = "urn:nvpair:node:"

// identityFile is the on-disk shape of cluster/identity.json.
type identityFile struct {
	NodeUUID  string `json:"node_uuid"`
	CreatedAt int64  `json:"created_at"`
}

// NodeIdentity is this node's cryptographic identity: a stable UUID, the
// hostname-derived display fields, and the self-signed leaf used for mTLS.
type NodeIdentity struct {
	NodeUUID        string
	NodeID          string // OS hostname (display/logical, never trusted)
	Name            string // OS hostname (display)
	CertPEM         []byte
	KeyPEM          []byte
	Cert            tls.Certificate
	CertFingerprint string             // "sha256:" + lowercase-hex(SHA-256(cert DER))
	Signer          ed25519.PrivateKey // the leaf's private key, used to sign endorsements/tombstones
}

// loadOrMintIdentity reads the node identity from clusterDir, minting a fresh
// one on genuine first launch (no identity.json). If identity.json exists but
// the keypair is gone or unreadable it fails loudly rather than minting a new
// UUID — silently re-minting would orphan every pin peers hold for this node.
func loadOrMintIdentity(clusterDir string) (*NodeIdentity, error) {
	idPath := filepath.Join(clusterDir, "identity.json")
	keyPath := filepath.Join(clusterDir, "node.key")
	crtPath := filepath.Join(clusterDir, "node.crt")

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}

	idData, idErr := os.ReadFile(idPath)
	if idErr == nil {
		var idf identityFile
		if err := json.Unmarshal(idData, &idf); err != nil || idf.NodeUUID == "" {
			return nil, fmt.Errorf("identity.json is present but unreadable (%v); refusing to mint a new UUID — restore the file or remove the cluster dir to re-pair", err)
		}
		certPEM, keyPEM, kerr := readKeypair(crtPath, keyPath)
		if kerr != nil {
			return nil, fmt.Errorf("identity.json exists (uuid %s) but the keypair is missing or corrupt: %w; refusing to mint a new identity (would orphan peer pins) — restore node.key/node.crt or re-pair this node", idf.NodeUUID, kerr)
		}
		return assembleIdentity(idf.NodeUUID, hostname, certPEM, keyPEM)
	}
	if !os.IsNotExist(idErr) {
		return nil, fmt.Errorf("read identity.json: %w", idErr)
	}

	// Genuine first launch: adopt this host's stable UUID rather than minting an
	// independent one. nodeid.Resolve prefers cluster/identity.json (absent here)
	// and otherwise returns the node-id.json value an earlier-starting worker
	// (broker/node-info/errors/workload-manager, which all resolve identity via
	// nodeid before we write identity.json) already minted — or mints it now.
	// Reusing it keeps identity.json and node-id.json in agreement, so every
	// process converges on one node UUID with no A/B identity divergence.
	uuid := nodeid.Resolve(filepath.Dir(clusterDir))
	if uuid == "" {
		return nil, fmt.Errorf("mint node uuid: system CSPRNG unavailable")
	}
	certPEM, keyPEM, err := generateLeaf(uuid, hostname)
	if err != nil {
		return nil, fmt.Errorf("generate leaf cert: %w", err)
	}
	if err := atomicWrite(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write node.key: %w", err)
	}
	if err := atomicWrite(crtPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write node.crt: %w", err)
	}
	idf := identityFile{NodeUUID: uuid, CreatedAt: time.Now().UnixMilli()}
	idJSON, _ := json.MarshalIndent(idf, "", "  ")
	if err := atomicWrite(idPath, idJSON, 0o600); err != nil {
		return nil, fmt.Errorf("write identity.json: %w", err)
	}
	return assembleIdentity(uuid, hostname, certPEM, keyPEM)
}

func readKeypair(crtPath, keyPath string) (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(crtPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read node.crt: %w", err)
	}
	keyPEM, err = os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read node.key: %w", err)
	}
	return certPEM, keyPEM, nil
}

func assembleIdentity(uuid, hostname string, certPEM, keyPEM []byte) (*NodeIdentity, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse keypair: %w", err)
	}
	fp, err := certFingerprintFromPEM(certPEM)
	if err != nil {
		return nil, err
	}
	signer, ok := cert.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("node key is %T, want ed25519.PrivateKey", cert.PrivateKey)
	}
	return &NodeIdentity{
		NodeUUID:        uuid,
		NodeID:          hostname,
		Name:            hostname,
		CertPEM:         certPEM,
		KeyPEM:          keyPEM,
		Cert:            cert,
		CertFingerprint: fp,
		Signer:          signer,
	}, nil
}

// generateLeaf creates an Ed25519 keypair and a long-lived self-signed leaf
// whose subject CN and URI SAN both carry the node UUID; the hostname rides
// along only as a (display-only) DNS SAN.
func generateLeaf(nodeUUID, hostname string) (certPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	uri, err := url.Parse(nodeURISANPrefix + nodeUUID)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: nodeUUID},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{uri},
	}
	if hostname != "" {
		tmpl.DNSNames = []string{hostname}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// certFingerprintFromPEM computes "sha256:" + lowercase-hex(SHA-256(DER)) over
// the first CERTIFICATE block in the supplied PEM.
func certFingerprintFromPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no CERTIFICATE block in PEM")
	}
	return certFingerprintFromDER(block.Bytes), nil
}

func certFingerprintFromDER(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// uuidFromCert extracts the claimed node UUID from a parsed certificate's URI
// SAN (preferred) or subject CN. Returns "" if neither carries a UUID.
func uuidFromCert(cert *x509.Certificate) string {
	for _, u := range cert.URIs {
		if u != nil {
			s := u.String()
			if len(s) > len(nodeURISANPrefix) && s[:len(nodeURISANPrefix)] == nodeURISANPrefix {
				return s[len(nodeURISANPrefix):]
			}
		}
	}
	return cert.Subject.CommonName
}

// newUUIDv4 generates a random RFC 4122 version-4 UUID string without pulling
// in an external dependency.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
