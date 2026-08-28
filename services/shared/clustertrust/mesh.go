// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ClusterUUIDTXTKey is the mDNS TXT key carrying a node's cluster principal —
// the UUID of the mTLS leaf nvpair-cluster-manager issued it — so a peer can map
// an advertised service to the pin it holds for that node. It is deliberately
// distinct from node-info's existing uuid= key (the stable per-host node id):
// the cluster principal is a per-cluster cert UUID, not the host UUID, and the
// two are not equal. A service advertises this key only while clustered.
const ClusterUUIDTXTKey = "cluster-uuid"

// ErrNotClustered is returned by the TLS config resolvers while this node is not
// a cluster member, which aborts an inbound handshake on a cluster-scoped
// listener instead of serving it. Callers that want to answer at the HTTP layer
// should gate on Clustered instead of dialing into the TLS path.
var ErrNotClustered = errors.New("node is not a cluster member")

// ClusterUUIDFromTXT returns the cluster-uuid= value from a peer's mDNS TXT
// records, or "" if absent (an unclustered peer, or a service that doesn't
// advertise it).
func ClusterUUIDFromTXT(txt []string) string {
	prefix := ClusterUUIDTXTKey + "="
	for _, kv := range txt {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Mesh is a service's LIVE view of this node's cluster identity, its reloadable
// peer-pin set, and the self-trust rule, so every service that gates inter-node
// HTTP on cluster membership shares one implementation instead of re-deriving
// the glue.
//
// Live is the whole point. Cluster membership is mutable at runtime —
// nvpair-cluster-manager mints the keypair, activates an admission, and writes
// pins whenever the user creates, joins, or leaves a cluster, all of which can
// happen long after a service started, and in any order relative to it. A Mesh
// therefore holds the cluster dir rather than a snapshot of it, and Refresh
// re-derives membership from disk. Every gate below reads the current state, so
// a service transitions between plain HTTP and cluster mTLS in place. Deriving
// the answer once at startup is the bug this design exists to prevent: a service
// that read the dir a moment too early would otherwise stay unclustered for its
// whole life, staying in the roster while silently exchanging no cluster traffic.
//
// Self-trust: a node always trusts its own leaf (it isn't pinned in trusted/,
// which only holds peers). Without it a node could not reach its own gated
// endpoints — e.g. nvpair-node-scanner reading the local nvpair-node-info over the
// LAN, which it discovers via mDNS like any peer. With self-trust the local
// read completes over the same mTLS path as a peer read, so node-info needs no
// separate plaintext loopback listener.
//
// A nil *Mesh reads as permanently unclustered so a service with no cluster dir
// needs no special case.
type Mesh struct {
	// dir is the cluster subtree nvpair-cluster-manager owns (node.key/node.crt,
	// admission.json, trusted/). Empty means this service was given no cluster
	// dir and is unclustered for its whole life.
	dir string

	// mu guards the identity and the derived membership answer. trust carries
	// its own lock and is replaced only in Open.
	mu        sync.RWMutex
	id        *Identity
	idCertPEM []byte
	clustered bool

	trust *Trust
}

// Open returns a live Mesh for clusterDir and performs the first read, so a
// service that is already clustered serves cluster mTLS from its first request
// with no warm-up. An empty clusterDir yields a Mesh that is permanently
// unclustered. Never nil: absence of membership is a state of the returned Mesh,
// not a nil pointer, which is what lets a service bind its listeners once and
// change personality later.
func Open(clusterDir string) *Mesh {
	m := &Mesh{dir: clusterDir}
	if clusterDir != "" {
		m.trust = newTrust(clusterDir)
	}
	m.Refresh()
	return m
}

// Refresh re-derives this node's cluster state from the cluster dir: it picks up
// a keypair minted since the last read, a pin added or dropped by a pairing or
// removal, and an admission activated or torn down by a join or leave.
//
// It deliberately reports nothing. It used to return whether the membership
// answer had changed, computed by diffing the fresh reading against m.clustered
// — which the same call then advanced. That made the transition a one-shot
// signal held in state shared by every caller: whoever refreshed first after a
// join consumed it, and everyone else saw "no change". A watcher gating its
// callback on that return would silently never fire, and did. Callers that need
// to act on a transition compare Clustered() against their own last-seen value,
// which nobody else can consume; Watch does exactly that.
//
// A successfully-loaded identity is never discarded because of a later read
// failure — only a keypair that parses replaces it. The keypair outlives a
// membership by design, so it carries no membership information: losing the
// ability to read it (a transient I/O error, an antivirus quarantine of a
// freshly written private key) must not demote a running member, whereas
// teardown is reported authoritatively by the admission record and the pins.
func (m *Mesh) Refresh() {
	if m == nil || m.dir == "" {
		return
	}
	m.trust.Reload()

	certPEM, certErr := os.ReadFile(filepath.Join(m.dir, "node.crt"))
	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-parse only when the leaf on disk differs from the one we hold: the
	// certificate changes whenever the keypair does (a rejoin can mint a fresh
	// identity in place), so comparing its PEM is both a complete rotation
	// detector and cheaper than an X.509 parse on every refresh.
	if certErr == nil && !bytes.Equal(certPEM, m.idCertPEM) {
		if id, err := LoadIdentity(m.dir); err == nil {
			m.id, m.idCertPEM = id, certPEM
		}
	}
	// Membership is a usable identity AND either a live admission (authoritative
	// and durable across restarts, including a cluster of one with no peers) or
	// at least one pinned peer (a confirmed peer means clustered even on a
	// cluster dir written before admission.json existed).
	m.clustered = m.id != nil && (hasActiveAdmission(m.dir) || m.trust.Count() > 0)
}

// Clustered reports whether this node is CURRENTLY a cluster member, as of the
// last Refresh. It is THE gate for advertising a cluster identity
// (cluster-uuid=), serving a cluster-scoped surface, and dialing peers.
//
// It is deliberately not "do I hold a keypair": a leave or removal tears down
// live membership (clears the active admission, removes the peer pins) but keeps
// node.key/node.crt so the same cryptographic identity can be reused on a later
// rejoin. A node that has left still loads an identity, so gating on identity
// presence would make it advertise itself as clustered and block peers from ever
// inviting it back.
func (m *Mesh) Clustered() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clustered
}

// hasIdentity reports whether a usable keypair is loaded, regardless of
// membership. Unexported on purpose: no consumer should gate behavior on it —
// holding a keypair is not being a cluster member (see Clustered), and every
// gate in this package derives from membership. It exists so the package's own
// tests can distinguish "loaded an identity" from "is a member".
func (m *Mesh) hasIdentity() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.id != nil
}

// NodeUUID is this node's cluster principal (the value to advertise under
// ClusterUUIDTXTKey). Empty when no identity is loaded.
func (m *Mesh) NodeUUID() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.id == nil {
		return ""
	}
	return m.id.NodeUUID
}

// PeerCount returns the number of currently-pinned peers (excludes self).
func (m *Mesh) PeerCount() int {
	if m == nil || m.trust == nil {
		return 0
	}
	return m.trust.Count()
}

// identity returns the loaded leaf, or false when none is loaded.
func (m *Mesh) identity() (*Identity, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.id == nil {
		return nil, false
	}
	return m.id, true
}

// trustedDER returns the cert DER expected for uuid: a pinned peer's, or this
// node's own leaf when uuid is its own (self-trust). ok is false for a uuid
// that is neither a pinned member nor us, and while unclustered — a node that
// isn't a member trusts no one, so the same lookup is the membership gate.
func (m *Mesh) trustedDER(uuid string) ([]byte, bool) {
	if !m.Clustered() || uuid == "" {
		return nil, false
	}
	id, ok := m.identity()
	if !ok {
		return nil, false
	}
	if uuid == id.NodeUUID {
		return id.Cert.Certificate[0], true
	}
	return m.trust.DER(uuid)
}

// HasPin reports whether uuid is a contactable cluster principal — a pinned
// peer, or this node itself.
func (m *Mesh) HasPin(uuid string) bool {
	_, ok := m.trustedDER(uuid)
	return ok
}

// matchDER is the server-side gate: accept der for uuid when it byte-for-byte
// matches the pinned peer cert, or when uuid is this node and der is its own
// leaf (self-trust).
func (m *Mesh) matchDER(uuid string, der []byte) bool {
	expected, ok := m.trustedDER(uuid)
	return ok && bytes.Equal(expected, der)
}

// ServerTLSConfig builds the inbound mTLS config for a cluster-scoped listener.
// It resolves this node's leaf per handshake through GetConfigForClient, so one
// bound listener follows the node into and out of a cluster: a connection
// arriving while unclustered fails the handshake with ErrNotClustered, and one
// arriving after a join is served with the freshly-minted leaf — no rebind, and
// no process restart. The per-request pin match is VerifyClientPin.
func (m *Mesh) ServerTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			id, ok := m.identity()
			if !ok || !m.Clustered() {
				return nil, ErrNotClustered
			}
			return ServerTLSConfig(id.Cert), nil
		},
	}
}

// VerifyClientPin is the per-request server gate: the authenticated client must
// present a cert pinned for its claimed UUID (or this node's own leaf, for a
// self-read). Returns (uuid, true) when accepted; ("", false) → caller answers
// 403.
func (m *Mesh) VerifyClientPin(r *http.Request) (string, bool) {
	if !m.Clustered() {
		return "", false
	}
	return VerifyClientPin(r, m.matchDER)
}

// ClientTLSConfig builds the outbound mTLS config for dialing peerUUID: present
// our leaf and pin the peer's exact server cert. ok is false when this node
// isn't a member, or when peerUUID is not a trusted member (nor us) — an
// unpinned peer must not be contacted, so this lookup is the client-side cluster
// gate. Refresh first to see fresh pins.
func (m *Mesh) ClientTLSConfig(peerUUID string) (*tls.Config, bool) {
	der, ok := m.trustedDER(peerUUID)
	if !ok {
		return nil, false
	}
	id, ok := m.identity()
	if !ok {
		return nil, false
	}
	return ClientTLSConfig(id.Cert, der), true
}

// ClientTLSConfigAny builds an outbound mTLS config for dialing a cluster peer
// whose UUID isn't known in advance — e.g. a manually-added node addressed by
// IP, with no cluster-uuid= TXT to key a specific pin on. It presents our leaf
// and accepts the server's cert only when that cert's own UUID is currently
// pinned (or is ours, via self-trust) AND its DER matches byte-for-byte, so it
// is no looser than ClientTLSConfig — it just resolves the pin from the
// presented cert instead of from a caller-supplied UUID. The match runs at
// handshake time against live state, so a pin (or a membership) that changed
// since the config was built is reflected. false while unclustered.
func (m *Mesh) ClientTLSConfigAny() (*tls.Config, bool) {
	id, ok := m.identity()
	if !ok || !m.Clustered() {
		return nil, false
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{id.Cert},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // the exact pinned-DER check below is the gate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse server cert: %w", err)
			}
			if !m.matchDER(UUIDFromCert(cert), rawCerts[0]) {
				return fmt.Errorf("server cert is not a pinned cluster member")
			}
			return nil
		},
	}, true
}
