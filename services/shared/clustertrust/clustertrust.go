// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clustertrust is the read side of the cluster mTLS trust fabric that
// nvpair-cluster-manager owns and persists. A non-cluster-manager service (e.g.
// nvpair-errors) points it at the same cluster config directory to load this
// node's mTLS leaf and the set of pinned peer certificates, and to build the
// pin-based TLS configs used to authenticate inter-node HTTP.
//
// The trust decision is identical to nvpair-cluster-manager's mtls.go: a server
// requires any client cert and accepts it only if its certificate DER matches a
// pinned peer byte-for-byte; a client presents this node's leaf and pins the
// peer's exact server cert DER. Because only paired cluster members hold a
// mutual pin, "can complete the handshake" is exactly "is a trusted member of
// my cluster" — so gating inter-node traffic on mTLS makes a separate
// cluster-membership check unnecessary.
//
// This package only ever reads the cluster dir; minting identities and writing
// pins stays in nvpair-cluster-manager.
package clustertrust

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// nodeURISANPrefix is the URI SAN scheme nvpair-cluster-manager embeds in every
// leaf, carrying the node UUID as a stable cryptographic principal. Kept in
// sync with nvpair-cluster-manager/identity.go.
const nodeURISANPrefix = "urn:nvpair:node:"

// Identity is this node's mTLS leaf loaded from the cluster dir. Read-only:
// minting lives in nvpair-cluster-manager.
type Identity struct {
	NodeUUID string
	Cert     tls.Certificate
}

// LoadIdentity reads node.crt + node.key from clusterDir and returns the leaf
// plus its UUID. It does not mint: a missing keypair is an error (the caller
// runs unclustered / plain-HTTP in that case).
func LoadIdentity(clusterDir string) (*Identity, error) {
	certPEM, err := os.ReadFile(filepath.Join(clusterDir, "node.crt"))
	if err != nil {
		return nil, fmt.Errorf("read node.crt: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(clusterDir, "node.key"))
	if err != nil {
		return nil, fmt.Errorf("read node.key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse node keypair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse node leaf: %w", err)
	}
	return &Identity{NodeUUID: UUIDFromCert(leaf), Cert: cert}, nil
}

// Trust is a reloadable, read-only view of clusterDir/trusted/*.json: the set of
// pinned peer certificate DERs keyed by peer UUID. nvpair-cluster-manager writes
// these on pairing; consumers Reload() to pick up newly-paired (or removed)
// peers without a restart.
type Trust struct {
	dir  string
	mu   sync.RWMutex
	ders map[string][]byte
}

// newTrust opens clusterDir/trusted. The pins are read by the owning Mesh's
// first Refresh, not here, so there is one place that decides when the cluster
// dir is read.
func newTrust(clusterDir string) *Trust {
	return &Trust{dir: filepath.Join(clusterDir, "trusted"), ders: map[string][]byte{}}
}

// Reload re-reads the trusted/ directory, replacing the in-memory pin set.
//
// A missing dir yields an empty set: that is the durable, authoritative
// statement that this node trusts no peer (an unclustered node, or one whose
// cluster was torn down). A dir that exists but cannot be READ yields no new
// information, so the previous set is kept rather than being replaced with an
// empty one — the same distinction Mesh.Refresh already draws for the identity,
// and for the same reason: a transient I/O error (an antivirus lock on a
// freshly written pin, a descriptor exhaustion) must not momentarily untrust
// every peer in the cluster. Callers act on this set on a short poll, so a
// single bad read would otherwise drop every peer out of routing and out of the
// directory's trust annotation for a tick, then restore them on the next.
//
// Teardown is unaffected: a successful read that no longer lists a pin removes
// it immediately, which is how a removal is meant to propagate.
func (t *Trust) Reload() {
	entries, err := os.ReadDir(t.dir)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	// A missing dir leaves entries nil, so the loop below is a no-op and the
	// empty set is published — the authoritative "no peers trusted".
	ders := make(map[string][]byte, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		fileUUID := strings.TrimSuffix(name, ".json")
		data, err := os.ReadFile(filepath.Join(t.dir, name))
		if err != nil {
			// The pin is listed but momentarily unreadable. Carry the value we
			// already hold for the same reason the whole set is kept when the
			// dir won't read: the file's presence says the peer is still
			// trusted, so failing to open it is missing information rather than
			// a revocation. A pin we have never read stays absent.
			if previous, ok := t.DER(fileUUID); ok {
				ders[fileUUID] = previous
			}
			continue
		}
		var pin struct {
			NodeUUID string `json:"nodeUuid"`
			CertPem  string `json:"certPem"`
		}
		// A pin that reads but does not parse or does not validate is dropped,
		// not carried: that is a content problem, and honoring a certificate we
		// can no longer verify the provenance of is the one failure this store
		// exists to prevent.
		if err := json.Unmarshal(data, &pin); err != nil {
			continue
		}
		der, err := pinDER(pin.NodeUUID, pin.CertPem, fileUUID)
		if err != nil {
			continue
		}
		ders[fileUUID] = der
	}
	t.mu.Lock()
	t.ders = ders
	t.mu.Unlock()
}

// pinDER validates that a pin file's inner nodeUuid, filename, and embedded
// certificate principal all agree (so a renamed/tampered file can't masquerade
// as another UUID) and returns the certificate DER. Mirrors
// nvpair-cluster-manager/truststore.go validatePin.
func pinDER(innerUUID, certPem, fileUUID string) ([]byte, error) {
	if innerUUID != fileUUID {
		return nil, fmt.Errorf("inner nodeUuid %q != filename %q", innerUUID, fileUUID)
	}
	block, _ := pem.Decode([]byte(certPem))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse cert: %w", err)
	}
	if got := UUIDFromCert(cert); got != fileUUID {
		return nil, fmt.Errorf("cert principal %q != filename %q", got, fileUUID)
	}
	return block.Bytes, nil
}

// MatchDER reports whether der byte-for-byte matches the pinned cert for uuid.
func (t *Trust) MatchDER(uuid string, der []byte) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	pinned, ok := t.ders[uuid]
	return ok && bytes.Equal(pinned, der)
}

// DER returns a copy of the pinned cert DER for uuid, used to pin a peer's
// server cert on an outbound call.
func (t *Trust) DER(uuid string) ([]byte, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	der, ok := t.ders[uuid]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(der))
	copy(cp, der)
	return cp, true
}

// Count returns the number of currently-pinned peers.
func (t *Trust) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.ders)
}

// ServerTLSConfig builds the inbound mTLS config from a node's leaf: present it
// and require a client cert at the TLS layer (RequireAnyClientCert). The
// byte-for-byte pin match is enforced per-request by VerifyClientPin, so a
// non-pinned client gets a real HTTP 403 rather than an opaque handshake
// failure. Takes the leaf directly so every holder of a cluster identity —
// nvpair-cluster-manager included — shares this one builder.
func ServerTLSConfig(leaf tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
}

// VerifyClientPin reads the authenticated client cert from r, extracts its
// claimed UUID, and accepts it only when match reports that cert's DER is pinned
// for that UUID byte-for-byte. match is the caller's trust store — both
// clustertrust.Trust.MatchDER and nvpair-cluster-manager's TrustStore.MatchDER
// satisfy it — so the gate decision lives in one place. Returns ("", false) for
// a missing/unpinned client; the caller answers 403.
func VerifyClientPin(r *http.Request, match func(uuid string, der []byte) bool) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false
	}
	cert := r.TLS.PeerCertificates[0]
	uuid := UUIDFromCert(cert)
	if uuid == "" || !match(uuid, cert.Raw) {
		return "", false
	}
	return uuid, true
}

// ClientTLSConfig builds the outbound mTLS config: present this node's leaf and
// pin the peer's server cert to pinnedPeerDER byte-for-byte (the CA chain is
// irrelevant). The caller resolves the peer's pinned DER first — an unpinned
// peer is not a trusted member and must not be contacted, so that lookup is the
// cluster gate. Takes primitives so nvpair-cluster-manager shares this builder.
func ClientTLSConfig(leaf tls.Certificate, pinnedPeerDER []byte) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{leaf},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // CA chain is irrelevant; we pin the exact DER below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 || !bytes.Equal(rawCerts[0], pinnedPeerDER) {
				return fmt.Errorf("server cert not pinned")
			}
			return nil
		},
	}
}

// UUIDFromCert extracts the node UUID from a certificate's URI SAN
// (urn:nvpair:node:<uuid>, preferred) or its subject CN. Mirrors
// nvpair-cluster-manager/identity.go uuidFromCert.
func UUIDFromCert(cert *x509.Certificate) string {
	for _, u := range cert.URIs {
		if u == nil {
			continue
		}
		if s := u.String(); strings.HasPrefix(s, nodeURISANPrefix) {
			return s[len(nodeURISANPrefix):]
		}
	}
	return cert.Subject.CommonName
}
