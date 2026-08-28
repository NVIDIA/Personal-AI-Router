// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clustertrusttest builds cluster directories that clustertrust.Open
// resolves exactly as it resolves the real ones nvpair-cluster-manager writes:
// a node.crt/node.key keypair carrying urn:nvpair:node:<uuid>, an admission
// record, and trusted/<uuid>.json peer pins.
//
// It exists so every service that gates behavior on cluster membership tests
// against the same on-disk shape. Only _test.go files import it, so it is never
// linked into a shipped binary (the same arrangement as net/http/httptest).
package clustertrusttest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// nodeURISANPrefix mirrors the URI SAN scheme nvpair-cluster-manager embeds in
// every leaf, which is how clustertrust recovers a certificate's principal.
const nodeURISANPrefix = "urn:nvpair:node:"

// WriteKeypair mints an ed25519 leaf for uuid and writes node.crt/node.key into
// clusterDir. The keypair is the durable identity: it deliberately outlives a
// membership, so writing it alone does NOT make a node clustered (see Join).
func WriteKeypair(t *testing.T, clusterDir, uuid string) {
	t.Helper()
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := mintLeaf(t, uuid)
	if err := os.WriteFile(filepath.Join(clusterDir, "node.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

// WriteAdmission activates a membership record for clusterID at epoch. A live
// admission is what distinguishes a current member from a node that merely still
// holds a keypair from a cluster it has left.
func WriteAdmission(t *testing.T, clusterDir, clusterID string, epoch uint64) {
	t.Helper()
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"counter": epoch, "activated": epoch, "clusterId": clusterID, "epoch": epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir, "admission.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// WritePeerPin mints a leaf for peerUUID and pins it into clusterDir/trusted the
// way nvpair-cluster-manager does after a pairing completes, making peerUUID a
// contactable principal for the node that owns clusterDir.
func WritePeerPin(t *testing.T, clusterDir, peerUUID string) {
	t.Helper()
	certPEM, _ := mintLeaf(t, peerUUID)
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

// RemovePeerPin deletes peerUUID's pin, the on-disk effect of removing that peer
// from the cluster.
func RemovePeerPin(t *testing.T, clusterDir, peerUUID string) {
	t.Helper()
	if err := os.Remove(filepath.Join(clusterDir, "trusted", peerUUID+".json")); err != nil {
		t.Fatal(err)
	}
}

// Join writes everything a node needs to be a live member of clusterID as
// selfUUID with a pin for each peer: the sequence nvpair-cluster-manager lands on
// disk when a join completes.
func Join(t *testing.T, clusterDir, clusterID, selfUUID string, peerUUIDs ...string) {
	t.Helper()
	WriteKeypair(t, clusterDir, selfUUID)
	WriteAdmission(t, clusterDir, clusterID, 1)
	for _, peer := range peerUUIDs {
		WritePeerPin(t, clusterDir, peer)
	}
}

// mintLeaf returns a self-signed PEM certificate/key pair whose URI SAN carries
// uuid as the node principal.
func mintLeaf(t *testing.T, uuid string) (certPEM, keyPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(nodeURISANPrefix + uuid)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: uuid},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
