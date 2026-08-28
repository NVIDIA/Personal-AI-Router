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

	"nvpair-shared/clustertrust"
)

// genLeaf mints an Ed25519 self-signed leaf carrying uuid in CN + the
// urn:nvpair:node URI SAN, matching nvpair-cluster-manager's generateLeaf.
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

// newPinnedPeerMeshes builds the two sides of a minimal two-node cluster: self
// pins peer and peer pins self, so a test can drive the real inter-node path
// (cluster mTLS from a pinned caller) rather than a plaintext shortcut. There is
// no plaintext shortcut to take — the interface is mTLS unconditionally — so every
// receiver test goes through here.
func newPinnedPeerMeshes(t *testing.T) (self, peer *clustertrust.Mesh) {
	t.Helper()
	selfCert, selfKey := genLeaf(t, "uuid-self")
	peerCert, peerKey := genLeaf(t, "uuid-peer")
	selfDir := setupNode(t, selfCert, selfKey, map[string][]byte{"uuid-peer": peerCert})
	peerDir := setupNode(t, peerCert, peerKey, map[string][]byte{"uuid-self": selfCert})
	self, peer = clustertrust.Open(selfDir), clustertrust.Open(peerDir)
	if !self.Clustered() || !peer.Clustered() {
		t.Fatal("a dir holding a keypair and a pin must read as clustered")
	}
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
	if !ok {
		t.Fatal("the pinned peer must be able to build a client for self")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	return func(body []byte) int {
		resp, err := client.Post(ts.URL+eventsPath, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
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
	if !mtlsA.Clustered() || !mtlsB.Clustered() || !mtlsC.Clustered() {
		t.Fatal("a populated cluster dir must read as clustered")
	}

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
	params, _ := json.Marshal(lifecycleParams{WorkloadInfo: wl})
	frame, _ := json.Marshal(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: json.RawMessage(params)})

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
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	// Pinned member B -> A: accepted (200).
	if code, err := post(mtlsB, "uuid-a"); err != nil || code != http.StatusOK {
		t.Fatalf("pinned member broadcast: code=%d err=%v, want 200", code, err)
	}

	// C completes the handshake (it pins A) but A doesn't pin C -> 403 at the gate.
	if code, err := post(mtlsC, "uuid-a"); err != nil || code != http.StatusForbidden {
		t.Fatalf("non-member broadcast: code=%d err=%v, want 403", code, err)
	}

	// Client-side gate: B holds no pin for an unknown peer, so the DER lookup
	// fails and it would never build a client to contact a non-member.
	if mtlsB.HasPin("uuid-unknown") {
		t.Fatal("an unpinned peer must not resolve a pin (cluster gate)")
	}
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
	params, _ := json.Marshal(lifecycleParams{WorkloadInfo: wl})
	frame, _ := json.Marshal(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: json.RawMessage(params)})

	postPlain := func() int {
		resp, err := http.Post(ts.URL+eventsPath, "application/json", bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := postPlain(); code != http.StatusForbidden {
		t.Fatalf("unclustered unauthenticated POST: code=%d, want 403", code)
	}

	// Joining a cluster must not widen the gate either.
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	admission, _ := json.Marshal(map[string]any{"clusterId": "cluster-abc", "epoch": 1, "counter": 1, "activated": 1})
	if err := os.WriteFile(filepath.Join(dir, "admission.json"), admission, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := postPlain(); code != http.StatusForbidden {
		t.Fatalf("clustered unauthenticated POST: code=%d, want 403", code)
	}
	if emitted != 0 {
		t.Fatalf("emitted=%d, want no unauthenticated event ever relayed to the broker", emitted)
	}
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

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("unclustered broadcast reached the peer %d time(s), want 0", got)
	}
}
