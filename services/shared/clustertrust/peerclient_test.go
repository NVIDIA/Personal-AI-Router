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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.True(t, m.Clustered(), "a populated cluster dir must read as clustered")
	return m, dir
}

// checkPeerClientCertificates checks the TLS behavior as well as cache identity.
func checkPeerClientCertificates(t *testing.T, client *http.Client, selfDER, peerDER, rejectedPeerDER []byte) {
	t.Helper()
	require.NotNil(t, client)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok, "pooled client must use an HTTP transport")
	require.NotNil(t, transport)
	cfg := transport.TLSClientConfig
	require.NotNil(t, cfg)
	require.Len(t, cfg.Certificates, 1)
	require.NotEmpty(t, cfg.Certificates[0].Certificate)
	assert.Equal(t, selfDER, cfg.Certificates[0].Certificate[0], "client must present the current local certificate")
	require.NotNil(t, cfg.VerifyPeerCertificate)
	require.NoError(t, cfg.VerifyPeerCertificate([][]byte{peerDER}, nil), "client must accept the current peer pin")
	if len(rejectedPeerDER) > 0 {
		assert.Error(t, cfg.VerifyPeerCertificate([][]byte{rejectedPeerDER}, nil), "client must reject the superseded peer pin")
	}
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
		_, err := w.Write([]byte("ok"))
		assert.NoError(t, err, "write peer response")
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
		require.True(t, ok, "request %d", i)
		resp, err := client.Get(srv.URL)
		require.NoError(t, err, "request %d", i)
		// Drain before closing, exactly as a caller must: an undrained body
		// leaves the connection unusable and it never returns to the idle pool.
		_, err = io.Copy(io.Discard, resp.Body)
		assert.NoError(t, err, "drain peer response")
		assert.NoError(t, resp.Body.Close(), "close peer response")
		assert.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
	}

	mu.Lock()
	got := newConns
	mu.Unlock()
	assert.Equal(t, 1, got, "connections must be reused across requests")
	assert.Equal(t, 1, pool.len(), "pool holds")
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
	_, ok := pool.Client("uuid-peer")
	require.True(t, ok, "pinned peer must yield a client")
	entry := pool.entries["uuid-peer"]
	assert.Equal(t, PeerIdleTimeout, entry.transport.IdleConnTimeout)
	assert.Equal(t, peerMaxIdleConnsPerHost, entry.transport.MaxIdleConnsPerHost)
	// The bound that actually protects the peer. Retaining idle connections stops
	// the leak, but only capping TOTAL connections per host stops a per-event
	// fan-out from opening hundreds of simultaneous handshakes during a burst —
	// which is what exhausted the receiver in the field.
	assert.Equal(t, peerMaxConnsPerHost, entry.transport.MaxConnsPerHost)
	// The client must reap an idle connection BEFORE the listener does, or it can
	// pick one the server is already closing.
	assert.Greater(t, PeerListenerIdleTimeout, PeerIdleTimeout)
	assert.Equal(t, 3*time.Second, entry.client.Timeout, "client Timeout")
}

// TestPeerClientPool_RebuildsOnRepin: cache membership must never outrank the
// live pin. A peer that re-paired presents a different leaf, so its pooled
// connections were negotiated against a pin that no longer applies and the entry
// has to be rebuilt rather than reused.
func TestPeerClientPool_RebuildsOnRepin(t *testing.T) {
	selfPEM, selfKey, selfDER := genLeaf(t, "uuid-self")
	peerPEM, _, peerDER := genLeaf(t, "uuid-peer")
	rePeerPEM, _, rePeerDER := genLeaf(t, "uuid-peer")

	selfMesh, dir := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	pool := NewPeerClientPool(selfMesh, time.Second)
	defer pool.CloseIdle()

	first, ok := pool.Client("uuid-peer")
	require.True(t, ok, "pinned peer must yield a client")
	checkPeerClientCertificates(t, first, selfDER, peerDER, rePeerDER)
	again, ok := pool.Client("uuid-peer")
	require.True(t, ok, "unchanged pin must still yield a client")
	assert.Same(t, first, again, "an unchanged pin must reuse the pooled client")

	writePin(t, dir, "uuid-peer", string(rePeerPEM))
	selfMesh.Refresh()

	second, ok := pool.Client("uuid-peer")
	require.True(t, ok, "re-pinned peer must still yield a client")
	assert.NotSame(t, first, second, "a re-pinned peer must be rebuilt, not served from cache with the old pin")
	checkPeerClientCertificates(t, second, selfDER, rePeerDER, peerDER)
	assert.Equal(t, 1, pool.len(), "pool holds")
}

// TestPeerClientPool_RebuildsOnLocalCertRotation: the pooled TLS config freezes
// BOTH certificates — the peer's pin and our own leaf. A rejoin or key rotation
// re-mints node.crt while membership continues, and an entry built from the old
// leaf would go on presenting it, so the peer's per-request pin gate would refuse
// every request until this process restarted. Silent, total loss of inter-node
// delivery is precisely the failure this pool exists to prevent, so rotation must
// rebuild the entry.
func TestPeerClientPool_RebuildsOnLocalCertRotation(t *testing.T) {
	selfPEM, selfKey, selfDER := genLeaf(t, "uuid-self")
	rotatedPEM, rotatedKey, rotatedDER := genLeaf(t, "uuid-self")
	peerPEM, _, peerDER := genLeaf(t, "uuid-peer")

	selfMesh, dir := meshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	pool := NewPeerClientPool(selfMesh, time.Second)
	defer pool.CloseIdle()

	first, ok := pool.Client("uuid-peer")
	require.True(t, ok, "pinned peer must yield a client")
	checkPeerClientCertificates(t, first, selfDER, peerDER, nil)

	// Our own leaf is re-minted; the peer's pin is untouched.
	writeIdentity(t, dir, rotatedPEM, rotatedKey)
	selfMesh.Refresh()

	second, ok := pool.Client("uuid-peer")
	require.True(t, ok, "peer must still yield a client after our leaf rotated")
	assert.NotSame(t, first, second, "our leaf was re-minted, so the entry must be rebuilt — a cached client keeps presenting the superseded certificate and the peer refuses it")
	checkPeerClientCertificates(t, second, rotatedDER, peerDER, nil)

	// DropUnpinned must reach the same conclusion for an entry nobody re-requested.
	third, ok := pool.Client("uuid-peer")
	require.True(t, ok, "peer must yield a client before the second rotation")
	rotatedAgainPEM, rotatedAgainKey, rotatedAgainDER := genLeaf(t, "uuid-self")
	writeIdentity(t, dir, rotatedAgainPEM, rotatedAgainKey)
	selfMesh.Refresh()
	pool.DropUnpinned()
	assert.Equal(t, 0, pool.len(), "DropUnpinned left")
	fourth, ok := pool.Client("uuid-peer")
	require.True(t, ok, "peer must yield a client after the second rotation")
	assert.NotSame(t, third, fourth, "entry survived a local rotation via DropUnpinned")
	checkPeerClientCertificates(t, fourth, rotatedAgainDER, peerDER, nil)
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
	_, ok := pool.Client("uuid-peer")
	require.True(t, ok, "pinned peer must yield a client")
	_, ok = pool.Client("uuid-stranger")
	require.False(t, ok, "an unpinned peer must not yield a client")

	require.NoError(t, os.Remove(filepath.Join(dir, "trusted", "uuid-peer.json")))
	selfMesh.Refresh()

	_, ok = pool.Client("uuid-peer")
	require.False(t, ok, "a de-pinned peer must no longer yield a client")
	assert.Equal(t, 0, pool.len(), "pool holds")

	// DropUnpinned is the sweep the fan-out runs per round; it must reach the
	// same conclusion for an entry nobody has asked for since the change.
	writePin(t, dir, "uuid-peer", string(peerPEM))
	selfMesh.Refresh()
	_, ok = pool.Client("uuid-peer")
	require.True(t, ok, "re-pinned peer must yield a client again")
	require.NoError(t, os.Remove(filepath.Join(dir, "trusted", "uuid-peer.json")))
	selfMesh.Refresh()
	pool.DropUnpinned()
	assert.Equal(t, 0, pool.len(), "DropUnpinned left")
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
	require.True(t, ok, "mesh must retain the on-disk peer pin")

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

	_, ok = pool.Client("uuid-peer")
	require.True(t, ok, "resolver-authorized peer must yield a client")
	authorized = false
	selfMesh.Refresh()
	assert.True(t, selfMesh.HasPin("uuid-peer"), "test requires the stale disk pin to remain visible to the mesh")
	_, ok = pool.Client("uuid-peer")
	require.False(t, ok, "resolver-revoked peer must not yield a client despite its disk pin")
	assert.Equal(t, 0, pool.len(), "pool holds")

	authorized = true
	_, ok = pool.Client("uuid-peer")
	require.True(t, ok, "re-authorized peer must yield a client")
	authorized = false
	pool.DropUnpinned()
	assert.Equal(t, 0, pool.len(), "DropUnpinned left")
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
		_, ok := pool.Client(uuid)
		require.True(t, ok)
	}
	assert.Equal(t, 2, pool.len(), "pool holds")
	pool.CloseIdle()
	assert.Equal(t, 0, pool.len(), "pool holds")
}

// TestPeerClientPool_UnclusteredYieldsNothing: a node that belongs to no cluster
// holds no pins, so it can address no peer — the pool must not invent a
// plaintext or unpinned path.
func TestPeerClientPool_UnclusteredYieldsNothing(t *testing.T) {
	pool := NewPeerClientPool(Open(t.TempDir()), time.Second)
	_, ok := pool.Client("uuid-peer")
	require.False(t, ok, "an unclustered node must yield no peer client")
	_, ok = NewPeerClientPool(nil, time.Second).Client("uuid-peer")
	require.False(t, ok, "a nil mesh must yield no peer client")
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
		_, err := w.Write([]byte("ok"))
		assert.NoError(t, err, "write streaming response")
	}))
	srv.TLS = peerMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	pool := NewPeerClientPoolOpts(selfMesh, PeerClientOptions{Timeout: 0})
	defer pool.CloseIdle()
	client, ok := pool.Client("uuid-peer")
	require.True(t, ok, "pinned peer must yield a client")
	assert.Equal(t, time.Duration(0), client.Timeout, "client Timeout")
	resp, err := client.Get(srv.URL)
	require.NoError(t, err, "Timeout=0 must wait out a slow body")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err, "read streaming response")
	assert.Equal(t, "ok", string(body))
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
	require.True(t, ok, "pinned peer must yield a client")
	assert.Equal(t, 40*time.Millisecond, pool.entries["uuid-peer"].transport.ResponseHeaderTimeout)
	_, err := client.Get(srv.URL)
	require.Error(t, err, "want a response-header timeout")
}
