// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// meshDir builds a cluster dir for a node and returns both the mesh and the dir,
// so a test can add or remove pins afterwards and Refresh into the change.
func meshDir(t *testing.T, certPEM, keyPEM []byte, pins map[string]string) (*Mesh, string) {
	t.Helper()
	dir := t.TempDir()
	writeIdentity(t, dir, certPEM, keyPEM)
	for uuid, pem := range pins {
		writePin(t, dir, uuid, pem)
	}
	m := Open(dir)
	if !m.Clustered() {
		t.Fatal("a populated cluster dir must read as clustered")
	}
	return m, dir
}

// TestPeerClientPool_ReusesOneConnection is the regression for the dropped
// inter-node event bug: building a client per request meant every event paid a
// fresh mTLS handshake and leaked the socket afterwards, so a burst exhausted
// the peer's ability to handshake at all. Several sequential requests through
// the pool must ride ONE connection.
func TestPeerClientPool_ReusesOneConnection(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, peerKey, _ := genLeaf(t, "uuid-peer")

	selfMesh, _ := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	peerMesh, _ := meshDir(t, peerPEM, peerKey, map[string]string{"uuid-self": string(selfPEM)})

	var mu sync.Mutex
	newConns := 0

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	srv.TLS = peerMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	pool := NewPeerClientPool(selfMesh, 5*time.Second)
	defer pool.CloseIdle()

	for i := 0; i < 5; i++ {
		client, ok := pool.Client("uuid-peer")
		if !ok {
			t.Fatalf("request %d: pinned peer must yield a client", i)
		}
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		// Drain before closing, exactly as a caller must: an undrained body
		// leaves the connection unusable and it never returns to the idle pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, resp.StatusCode)
		}
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	if got != 1 {
		t.Fatalf("server accepted %d connections for 5 requests, want 1 (connections are not being reused)", got)
	}
	if pool.len() != 1 {
		t.Fatalf("pool holds %d peers, want 1", pool.len())
	}
}

// TestPeerClientPool_TransportBoundsIdleConnections pins the two settings whose
// absence caused the leak: a hand-built Transport with no IdleConnTimeout never
// reaps an idle connection, and its read loop keeps the Transport alive so the
// socket outlives the request for the life of the process.
func TestPeerClientPool_TransportBoundsIdleConnections(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, _, _ := genLeaf(t, "uuid-peer")
	selfMesh, _ := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})

	pool := NewPeerClientPool(selfMesh, 3*time.Second)
	if _, ok := pool.Client("uuid-peer"); !ok {
		t.Fatal("pinned peer must yield a client")
	}
	entry := pool.entries["uuid-peer"]
	if entry.transport.IdleConnTimeout != PeerIdleTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", entry.transport.IdleConnTimeout, PeerIdleTimeout)
	}
	if entry.transport.MaxIdleConnsPerHost != peerMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", entry.transport.MaxIdleConnsPerHost, peerMaxIdleConnsPerHost)
	}
	// The bound that actually protects the peer. Retaining idle connections stops
	// the leak, but only capping TOTAL connections per host stops a per-event
	// fan-out from opening hundreds of simultaneous handshakes during a burst —
	// which is what exhausted the receiver in the field.
	if entry.transport.MaxConnsPerHost != peerMaxConnsPerHost {
		t.Errorf("MaxConnsPerHost = %d, want %d (an unbounded cap lets a burst storm the peer)", entry.transport.MaxConnsPerHost, peerMaxConnsPerHost)
	}
	// The client must reap an idle connection BEFORE the listener does, or it can
	// pick one the server is already closing.
	if PeerListenerIdleTimeout <= PeerIdleTimeout {
		t.Errorf("PeerListenerIdleTimeout (%v) must exceed PeerIdleTimeout (%v)", PeerListenerIdleTimeout, PeerIdleTimeout)
	}
	if entry.client.Timeout != 3*time.Second {
		t.Errorf("client Timeout = %v, want the pool timeout 3s", entry.client.Timeout)
	}
}

// TestPeerClientPool_RebuildsOnRepin: cache membership must never outrank the
// live pin. A peer that re-paired presents a different leaf, so its pooled
// connections were negotiated against a pin that no longer applies and the entry
// has to be rebuilt rather than reused.
func TestPeerClientPool_RebuildsOnRepin(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, _, _ := genLeaf(t, "uuid-peer")
	rePeerPEM, _, _ := genLeaf(t, "uuid-peer")

	selfMesh, dir := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	pool := NewPeerClientPool(selfMesh, time.Second)
	defer pool.CloseIdle()

	first, ok := pool.Client("uuid-peer")
	if !ok {
		t.Fatal("pinned peer must yield a client")
	}
	if again, _ := pool.Client("uuid-peer"); again != first {
		t.Fatal("an unchanged pin must reuse the pooled client")
	}

	writePin(t, dir, "uuid-peer", string(rePeerPEM))
	selfMesh.Refresh()

	second, ok := pool.Client("uuid-peer")
	if !ok {
		t.Fatal("re-pinned peer must still yield a client")
	}
	if second == first {
		t.Fatal("a re-pinned peer must be rebuilt, not served from cache with the old pin")
	}
	if pool.len() != 1 {
		t.Fatalf("pool holds %d peers, want 1 (the rebuilt entry replaces the old one)", pool.len())
	}
}

// TestPeerClientPool_RebuildsOnLocalCertRotation: the pooled TLS config freezes
// BOTH certificates — the peer's pin and our own leaf. A rejoin or key rotation
// re-mints node.crt while membership continues, and an entry built from the old
// leaf would go on presenting it, so the peer's per-request pin gate would refuse
// every request until this process restarted. Silent, total loss of inter-node
// delivery is precisely the failure this pool exists to prevent, so rotation must
// rebuild the entry.
func TestPeerClientPool_RebuildsOnLocalCertRotation(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	rotatedPEM, rotatedKey, _ := genLeaf(t, "uuid-self")
	peerPEM, _, _ := genLeaf(t, "uuid-peer")

	selfMesh, dir := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	pool := NewPeerClientPool(selfMesh, time.Second)
	defer pool.CloseIdle()

	first, ok := pool.Client("uuid-peer")
	if !ok {
		t.Fatal("pinned peer must yield a client")
	}

	// Our own leaf is re-minted; the peer's pin is untouched.
	writeIdentity(t, dir, rotatedPEM, rotatedKey)
	selfMesh.Refresh()

	second, ok := pool.Client("uuid-peer")
	if !ok {
		t.Fatal("peer must still yield a client after our leaf rotated")
	}
	if second == first {
		t.Fatal("our leaf was re-minted, so the entry must be rebuilt — a cached client keeps presenting the superseded certificate and the peer refuses it")
	}

	// DropUnpinned must reach the same conclusion for an entry nobody re-requested.
	third, _ := pool.Client("uuid-peer")
	rotatedAgainPEM, rotatedAgainKey, _ := genLeaf(t, "uuid-self")
	writeIdentity(t, dir, rotatedAgainPEM, rotatedAgainKey)
	selfMesh.Refresh()
	pool.DropUnpinned()
	if pool.len() != 0 {
		t.Fatalf("DropUnpinned left %d entries after a local rotation, want 0", pool.len())
	}
	fourth, ok := pool.Client("uuid-peer")
	if !ok {
		t.Fatal("peer must yield a client after the second rotation")
	}
	if fourth == third {
		t.Fatal("entry survived a local rotation via DropUnpinned")
	}
}

// TestPeerClientPool_UnpinnedIsRefusedAndEvicted: losing a pin (peer removed, or
// this node leaving the cluster) must close that peer's pooled connections and
// refuse to hand out a client, so the pool can never be more permissive than the
// mesh.
func TestPeerClientPool_UnpinnedIsRefusedAndEvicted(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, _, _ := genLeaf(t, "uuid-peer")
	selfMesh, dir := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})

	pool := NewPeerClientPool(selfMesh, time.Second)
	if _, ok := pool.Client("uuid-peer"); !ok {
		t.Fatal("pinned peer must yield a client")
	}
	if _, ok := pool.Client("uuid-stranger"); ok {
		t.Fatal("an unpinned peer must not yield a client")
	}

	if err := os.Remove(filepath.Join(dir, "trusted", "uuid-peer.json")); err != nil {
		t.Fatal(err)
	}
	selfMesh.Refresh()

	if _, ok := pool.Client("uuid-peer"); ok {
		t.Fatal("a de-pinned peer must no longer yield a client")
	}
	if pool.len() != 0 {
		t.Fatalf("pool holds %d peers after de-pin, want 0", pool.len())
	}

	// DropUnpinned is the sweep the fan-out runs per round; it must reach the
	// same conclusion for an entry nobody has asked for since the change.
	writePin(t, dir, "uuid-peer", string(peerPEM))
	selfMesh.Refresh()
	if _, ok := pool.Client("uuid-peer"); !ok {
		t.Fatal("re-pinned peer must yield a client again")
	}
	if err := os.Remove(filepath.Join(dir, "trusted", "uuid-peer.json")); err != nil {
		t.Fatal(err)
	}
	selfMesh.Refresh()
	pool.DropUnpinned()
	if pool.len() != 0 {
		t.Fatalf("DropUnpinned left %d peers, want 0", pool.len())
	}
}

// TestPeerClientPool_ResolverRevocationOverridesDiskPin covers an owner with a
// stronger live authority than the Mesh's disk view. A failed durable delete
// can leave the pin file behind, but revoking it in that authority must still
// refuse future clients and evict the pooled sockets.
func TestPeerClientPool_ResolverRevocationOverridesDiskPin(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, _, _ := genLeaf(t, "uuid-peer")
	selfMesh, _ := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	peerDER, ok := selfMesh.trustedDER("uuid-peer")
	if !ok {
		t.Fatal("mesh must retain the on-disk peer pin")
	}

	authorized := true
	pool := NewPeerClientPoolOpts(selfMesh, PeerClientOptions{
		Timeout: time.Second,
		ResolvePin: func(uuid string) ([]byte, bool) {
			if !authorized || uuid != "uuid-peer" {
				return nil, false
			}
			return peerDER, true
		},
	})
	defer pool.CloseIdle()

	if _, ok := pool.Client("uuid-peer"); !ok {
		t.Fatal("resolver-authorized peer must yield a client")
	}
	authorized = false
	selfMesh.Refresh()
	if !selfMesh.HasPin("uuid-peer") {
		t.Fatal("test requires the stale disk pin to remain visible to the mesh")
	}
	if _, ok := pool.Client("uuid-peer"); ok {
		t.Fatal("resolver-revoked peer must not yield a client despite its disk pin")
	}
	if pool.len() != 0 {
		t.Fatalf("pool holds %d peers after resolver revocation, want 0", pool.len())
	}

	authorized = true
	if _, ok := pool.Client("uuid-peer"); !ok {
		t.Fatal("re-authorized peer must yield a client")
	}
	authorized = false
	pool.DropUnpinned()
	if pool.len() != 0 {
		t.Fatalf("DropUnpinned left %d resolver-revoked peers, want 0", pool.len())
	}
}

// TestPeerClientPool_CloseIdleEmptiesPool: shutdown must not leave sockets open.
func TestPeerClientPool_CloseIdleEmptiesPool(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, _, _ := genLeaf(t, "uuid-peer")
	otherPEM, _, _ := genLeaf(t, "uuid-other")
	selfMesh, _ := meshDir(t, selfPEM, selfKey, map[string]string{
		"uuid-peer":  string(peerPEM),
		"uuid-other": string(otherPEM),
	})

	pool := NewPeerClientPool(selfMesh, time.Second)
	for _, uuid := range []string{"uuid-peer", "uuid-other"} {
		if _, ok := pool.Client(uuid); !ok {
			t.Fatalf("%s must yield a client", uuid)
		}
	}
	if pool.len() != 2 {
		t.Fatalf("pool holds %d peers, want 2", pool.len())
	}
	pool.CloseIdle()
	if pool.len() != 0 {
		t.Fatalf("pool holds %d peers after CloseIdle, want 0", pool.len())
	}
}

// TestPeerClientPool_UnclusteredYieldsNothing: a node that belongs to no cluster
// holds no pins, so it can address no peer — the pool must not invent a
// plaintext or unpinned path.
func TestPeerClientPool_UnclusteredYieldsNothing(t *testing.T) {
	pool := NewPeerClientPool(Open(t.TempDir()), time.Second)
	if _, ok := pool.Client("uuid-peer"); ok {
		t.Fatal("an unclustered node must yield no peer client")
	}
	if _, ok := NewPeerClientPool(nil, time.Second).Client("uuid-peer"); ok {
		t.Fatal("a nil mesh must yield no peer client")
	}
}

// TestPeerClientPoolOpts_TimeoutZeroKeepsStreamingAlive: engine-manager install
// and pull streams run for minutes and must not be cut by http.Client.Timeout.
func TestPeerClientPoolOpts_TimeoutZeroKeepsStreamingAlive(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, peerKey, _ := genLeaf(t, "uuid-peer")
	selfMesh, _ := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	peerMesh, _ := meshDir(t, peerPEM, peerKey, map[string]string{"uuid-self": string(selfPEM)})

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = peerMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	pool := NewPeerClientPoolOpts(selfMesh, PeerClientOptions{Timeout: 0})
	defer pool.CloseIdle()
	client, ok := pool.Client("uuid-peer")
	if !ok {
		t.Fatal("pinned peer must yield a client")
	}
	if client.Timeout != 0 {
		t.Fatalf("client Timeout = %v, want 0", client.Timeout)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Timeout=0 must wait out a slow body: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}

// TestPeerClientPoolOpts_ResponseHeaderTimeoutFires: a peer that accepts the
// connection but never writes headers must be cut by the transport, not by a
// whole-request deadline.
func TestPeerClientPoolOpts_ResponseHeaderTimeoutFires(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, peerKey, _ := genLeaf(t, "uuid-peer")
	selfMesh, _ := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	peerMesh, _ := meshDir(t, peerPEM, peerKey, map[string]string{"uuid-self": string(selfPEM)})

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = peerMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	pool := NewPeerClientPoolOpts(selfMesh, PeerClientOptions{
		ResponseHeaderTimeout: 40 * time.Millisecond,
	})
	defer pool.CloseIdle()
	client, ok := pool.Client("uuid-peer")
	if !ok {
		t.Fatal("pinned peer must yield a client")
	}
	if entry := pool.entries["uuid-peer"]; entry.transport.ResponseHeaderTimeout != 40*time.Millisecond {
		t.Fatalf("ResponseHeaderTimeout = %v, want 40ms", entry.transport.ResponseHeaderTimeout)
	}
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("want a response-header timeout")
	}
}
