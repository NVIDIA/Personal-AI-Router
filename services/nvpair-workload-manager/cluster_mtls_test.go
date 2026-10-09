// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

// genLeaf mints an Ed25519 self-signed leaf carrying uuid in CN + the
// urn:nvpair:node URI SAN, matching nvpair-cluster-manager's generateLeaf.
func genLeaf(t *testing.T, uuid string) (certPEM, keyPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "generate key")
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	assert.NoError(t, err)
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
	require.NoError(t, err, "create certificate")
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	assert.NoError(t, err)
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
	td := filepath.Join(dir, "trusted")
	require.NoError(t, os.MkdirAll(td, 0o700))
	for uuid, pcert := range pins {
		body, err := json.Marshal(map[string]string{"nodeUuid": uuid, "certPem": string(pcert)})
		assert.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(td, uuid+".json"), body, 0o600))
	}
	return dir
}

// newPinnedPeerDirs builds two mutually pinned nodes. Tests that construct a
// Manager need selfDir because NewManager opens its own mesh from that path.
func newPinnedPeerDirs(t *testing.T) (selfDir, peerDir string) {
	t.Helper()
	selfCert, selfKey := genLeaf(t, "uuid-self")
	peerCert, peerKey := genLeaf(t, "uuid-peer")
	selfDir = setupNode(t, selfCert, selfKey, map[string][]byte{"uuid-peer": peerCert})
	peerDir = setupNode(t, peerCert, peerKey, map[string][]byte{"uuid-self": selfCert})
	return selfDir, peerDir
}

// newPinnedPeerMeshes builds the two sides of a minimal two-node cluster: self
// pins peer and peer pins self, so a test can drive the real inter-node path
// (cluster mTLS from a pinned caller) rather than a plaintext shortcut. There is
// no plaintext shortcut to take — the interface is mTLS unconditionally — so every
// receiver test goes through here.
func newPinnedPeerMeshes(t *testing.T) (self, peer *clustertrust.Mesh) {
	t.Helper()
	selfDir, peerDir := newPinnedPeerDirs(t)
	self, peer = clustertrust.Open(selfDir), clustertrust.Open(peerDir)
	require.True(t, self.Clustered(), "a dir holding a keypair and a pin must read as clustered")
	require.True(t, peer.Clustered(), "a dir holding a keypair and a pin must read as clustered")
	return self, peer
}

// serveEventsOverMTLS starts srv's events endpoint behind cluster mTLS and
// returns a post func that dials it as the pinned peer.
func serveEventsOverMTLS(t *testing.T, srv *Server, self, peer *clustertrust.Mesh) func(body []byte) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, srv.handleEvents)
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = self.ServerTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	cfg, ok := peer.ClientTLSConfig("uuid-self")
	require.True(t, ok, "the pinned peer must be able to build a client for self")
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	return func(body []byte) int {
		resp, err := client.Post(ts.URL+eventsPath, "application/json", bytes.NewReader(body))
		require.NoError(t, err, "post events")
		defer func() { assert.NoError(t, resp.Body.Close()) }()
		return resp.StatusCode
	}
}

// TestWorkloadBroadcast_MTLSGate is the end-to-end evidence for the
// workload relay: the inter-node events endpoint authenticates and gates to
// pinned cluster members. A pinned member's broadcast is accepted (200); a node
// that completes the TLS handshake but isn't pinned by the receiver is rejected
// at the pin gate (403); and a peer we hold no pin for can't even build a client.
func TestWorkloadBroadcast_MTLSGate(t *testing.T) {
	aCert, aKey := genLeaf(t, "uuid-a")
	bCert, bKey := genLeaf(t, "uuid-b")
	cCert, cKey := genLeaf(t, "uuid-c")

	// A trusts only B. B trusts A. C trusts A (so C can dial A) but A does NOT
	// trust C — the asymmetry that proves the receiver-side gate.
	dirA := setupNode(t, aCert, aKey, map[string][]byte{"uuid-b": bCert})
	dirB := setupNode(t, bCert, bKey, map[string][]byte{"uuid-a": aCert})
	dirC := setupNode(t, cCert, cKey, map[string][]byte{"uuid-a": aCert})

	mtlsA, mtlsB, mtlsC := clustertrust.Open(dirA), clustertrust.Open(dirB), clustertrust.Open(dirC)
	require.True(t, mtlsA.Clustered(), "node A must be clustered")
	require.True(t, mtlsB.Clustered(), "node B must be clustered")
	require.True(t, mtlsC.Clustered(), "node C must be clustered")

	// A serves its events endpoint over mTLS with the pin gate.
	srvA := NewServer(0, newDedupIndex(16), mtlsA,
		func(*Workload) error { return nil },
		func(string, string) error { return nil })
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, srvA.handleEvents)
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = mtlsA.ServerTLSConfig()
	ts.StartTLS()
	defer ts.Close()

	// A valid lifecycle frame (workload:started) so a member's POST reaches 200,
	// not just "past the gate".
	wl := &Workload{ID: "wl-1", Model: "llama", Engine: "ollama", State: StateRunning, OriginatedFrom: "uuid-b", CreatedAt: 1}
	params, err := json.Marshal(lifecycleParams{WorkloadInfo: wl})
	assert.NoError(t, err)
	frame, err := json.Marshal(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: json.RawMessage(params)})
	assert.NoError(t, err)

	post := func(m *clustertrust.Mesh, peerUUID string) (int, error) {
		cfg, ok := m.ClientTLSConfig(peerUUID)
		if !ok {
			return 0, fmt.Errorf("no pin for peer %s", peerUUID)
		}
		client := &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: cfg},
		}
		resp, err := client.Post(ts.URL+eventsPath, "application/json", bytes.NewReader(frame))
		if err != nil {
			return 0, err
		}
		defer func() { assert.NoError(t, resp.Body.Close()) }()
		return resp.StatusCode, nil
	}

	// Pinned member B -> A: accepted (200).
	code, err := post(mtlsB, "uuid-a")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code, "pinned member broadcast")

	// C completes the handshake (it pins A) but A doesn't pin C -> 403 at the gate.
	code, err = post(mtlsC, "uuid-a")
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, code, "non-member broadcast")

	// Client-side gate: B holds no pin for an unknown peer, so the DER lookup
	// fails and it would never build a client to contact a non-member.
	assert.False(t, mtlsB.HasPin("uuid-unknown"), "an unpinned peer must not resolve a pin")
}

// TestWorkloadEvents_UnauthenticatedIsAlwaysRefused pins the posture that makes
// this a cluster data plane rather than a LAN service: an unauthenticated caller
// is refused whether or not this node belongs to a cluster, and nothing it sends
// reaches the broker.
//
// The unclustered case is the one that matters. Gating the pin check on "am I
// clustered" left a node with no cluster identity accepting workload events from
// any host on the network and relaying them into its catalog as though they were
// a peer's — the confidentiality half of the reported issue. The listener is mTLS
// only, so in production such a caller never completes a handshake; this drives
// the handler directly to prove the gate itself does not depend on membership.
func TestWorkloadEvents_UnauthenticatedIsAlwaysRefused(t *testing.T) {
	certPEM, keyPEM := genLeaf(t, "uuid-self")
	dir := t.TempDir()
	mesh := clustertrust.Open(dir)

	emitted := 0
	srv := NewServer(0, newDedupIndex(16), mesh,
		func(*Workload) error { emitted++; return nil },
		func(string, string) error { return nil })
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, srv.handleEvents)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wl := &Workload{ID: "wl-1", Model: "llama", Engine: "ollama", State: StateRunning, OriginatedFrom: "uuid-peer", CreatedAt: 1}
	params, err := json.Marshal(lifecycleParams{WorkloadInfo: wl})
	assert.NoError(t, err)
	frame, err := json.Marshal(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: json.RawMessage(params)})
	assert.NoError(t, err)

	postPlain := func() int {
		resp, err := http.Post(ts.URL+eventsPath, "application/json", bytes.NewReader(frame))
		require.NoError(t, err, "post events")
		defer func() { assert.NoError(t, resp.Body.Close()) }()
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusForbidden, postPlain(), "unclustered unauthenticated POST")

	// Joining a cluster must not widen the gate either.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600))
	admission, err := json.Marshal(map[string]any{"clusterId": "cluster-abc", "epoch": 1, "counter": 1, "activated": 1})
	assert.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "admission.json"), admission, 0o600))

	assert.Equal(t, http.StatusForbidden, postPlain(), "clustered unauthenticated POST")
	assert.Zero(t, emitted, "no unauthenticated event should reach the broker")
}

// TestBroadcast_UnclusteredNodeSendsNothing: a node that belongs to no cluster
// must not fan workload events out in the clear. The stub peer records any
// request that arrives; the broadcaster must never reach it.
func TestBroadcast_UnclusteredNodeSendsNothing(t *testing.T) {
	var hits int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer peer.Close()

	host := strings.TrimPrefix(peer.URL, "http://")
	peers := newPeerSet(14320)
	peers.Replace([]PeerNode{{ID: "peer-1", Host: host, Addresses: []string{host}}})

	b := NewBroadcaster(peers, clustertrust.Open(t.TempDir()))
	b.Broadcast(context.Background(), []byte(`{"jsonrpc":"2.0","method":"workload:started"}`))

	assert.Zero(t, atomic.LoadInt32(&hits), "unclustered broadcasts must not reach peers")
}
