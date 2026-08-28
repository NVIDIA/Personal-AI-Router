// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeIdentity drops a node.crt/node.key keypair into dir so a Mesh opened on
// it finds an identity there. (genLeaf + writePin come from clustertrust_test.go.)
func writeIdentity(t *testing.T, dir string, certPEM, keyPEM []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadMeshDir(t *testing.T, certPEM, keyPEM []byte, pins map[string]string) *Mesh {
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
	return m
}

// TestMesh_Open_UnclusteredCases: an empty or identity-less dir yields a usable
// but unclustered mesh, so the caller serves plain HTTP from the same object it
// will later serve cluster mTLS from.
func TestMesh_Open_UnclusteredCases(t *testing.T) {
	for name, dir := range map[string]string{"no cluster dir": "", "empty cluster dir": t.TempDir()} {
		m := Open(dir)
		if m == nil {
			t.Fatalf("%s: Open must never return nil", name)
		}
		if m.Clustered() || m.hasIdentity() {
			t.Fatalf("%s: must read as unclustered", name)
		}
		m.Refresh()
		if m.Clustered() {
			t.Fatalf("%s: refreshing must not manufacture a membership", name)
		}
	}
}

// TestMesh_GateSelfTrustAndAnyPin drives the full server/client handshake the
// way every consumer uses it, and pins down the behaviors node-info/scanner/
// manual-nodes specifically need: a pinned peer is accepted, a node's own leaf
// is accepted (self-trust) even though it isn't in trusted/, the UUID-less
// any-pin client works, and a node that completes the handshake but isn't
// pinned by the server is rejected at the gate.
func TestMesh_GateSelfTrustAndAnyPin(t *testing.T) {
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, peerKey, _ := genLeaf(t, "uuid-peer")
	strangerPEM, strangerKey, _ := genLeaf(t, "uuid-stranger")

	// self pins peer (only). peer + stranger both pin self (so both can complete
	// a handshake TO self) — but self does not pin stranger.
	selfMesh := loadMeshDir(t, selfPEM, selfKey, map[string]string{"uuid-peer": string(peerPEM)})
	peerMesh := loadMeshDir(t, peerPEM, peerKey, map[string]string{"uuid-self": string(selfPEM)})
	strangerMesh := loadMeshDir(t, strangerPEM, strangerKey, map[string]string{"uuid-self": string(selfPEM)})

	if selfMesh.NodeUUID() != "uuid-self" {
		t.Fatalf("NodeUUID=%q, want uuid-self", selfMesh.NodeUUID())
	}
	if !selfMesh.HasPin("uuid-peer") {
		t.Fatal("peer must be a pin")
	}
	if !selfMesh.HasPin("uuid-self") {
		t.Fatal("self must be trusted (self-trust)")
	}
	if selfMesh.HasPin("uuid-stranger") {
		t.Fatal("stranger must not be a pin")
	}

	// self is the mTLS server, gated by VerifyClientPin.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := selfMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = selfMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	do := func(cfg *tls.Config) (int, error) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Get(srv.URL)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	mustConfig := func(cfg *tls.Config, ok bool) *tls.Config {
		t.Helper()
		if !ok {
			t.Fatal("expected a client config")
		}
		return cfg
	}

	// Pinned peer -> self, specific pin: accepted.
	if code, err := do(mustConfig(peerMesh.ClientTLSConfig("uuid-self"))); err != nil || code != http.StatusOK {
		t.Fatalf("peer->self (specific pin): code=%d err=%v, want 200", code, err)
	}
	// Pinned peer -> self, any-pin (no UUID known up front): accepted.
	if code, err := do(mustConfig(peerMesh.ClientTLSConfigAny())); err != nil || code != http.StatusOK {
		t.Fatalf("peer->self (any-pin): code=%d err=%v, want 200", code, err)
	}
	// self -> self over mTLS: accepted via self-trust (self isn't in trusted/).
	if code, err := do(mustConfig(selfMesh.ClientTLSConfig("uuid-self"))); err != nil || code != http.StatusOK {
		t.Fatalf("self->self: code=%d err=%v, want 200 (self-trust)", code, err)
	}
	// Stranger completes the handshake (it pins self) but self doesn't pin it: 403.
	if code, _ := do(mustConfig(strangerMesh.ClientTLSConfig("uuid-self"))); code != http.StatusForbidden {
		t.Fatalf("stranger->self: code=%d, want 403", code)
	}
	// self cannot even build a client to an unpinned stranger (client-side gate).
	if _, ok := selfMesh.ClientTLSConfig("uuid-stranger"); ok {
		t.Fatal("must not build a client for an unpinned peer")
	}
}

// TestMesh_ServerTLSConfig_FollowsMembershipOnOneListener is the server-side half
// of the half-clustered regression guard. A cluster-scoped service binds its
// listener once, at startup, when it may not yet be a member; the TLS
// personality of that ALREADY-BOUND listener must follow membership. Before the
// cluster dir is populated a peer handshake is refused; afterwards the very same
// listener serves the pinned mTLS surface, with no rebind and no restart.
func TestMesh_ServerTLSConfig_FollowsMembershipOnOneListener(t *testing.T) {
	selfDir := t.TempDir()
	selfPEM, selfKey, _ := genLeaf(t, "uuid-self")
	peerPEM, peerKey, _ := genLeaf(t, "uuid-peer")
	peerMesh := loadMeshDir(t, peerPEM, peerKey, map[string]string{"uuid-self": string(selfPEM)})

	selfMesh := Open(selfDir)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := selfMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = selfMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	get := func() (int, error) {
		cfg, ok := peerMesh.ClientTLSConfig("uuid-self")
		if !ok {
			t.Fatal("peer must be able to build a client for self")
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Get(srv.URL)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	// Not a member yet: the handshake is refused rather than served plaintext.
	if code, err := get(); err == nil {
		t.Fatalf("an unclustered listener must refuse the handshake, got code=%d", code)
	}

	// The node joins: identity + admission + the peer's pin land on disk.
	writeIdentity(t, selfDir, selfPEM, selfKey)
	writeAdmission(t, selfDir, "cluster-abc", 1)
	writePin(t, selfDir, "uuid-peer", string(peerPEM))
	selfMesh.Refresh()
	if !selfMesh.Clustered() {
		t.Fatal("the mesh must be clustered once the dir is populated")
	}

	if code, err := get(); err != nil || code != http.StatusOK {
		t.Fatalf("the same listener must serve the pinned peer after the join: code=%d err=%v", code, err)
	}
}

// TestClusterUUIDFromTXT: reads cluster-uuid= and never node-info's host uuid=.
func TestClusterUUIDFromTXT(t *testing.T) {
	if got := ClusterUUIDFromTXT([]string{"http=0", "cluster-uuid=abc-123", "mtls=1"}); got != "abc-123" {
		t.Fatalf("got %q, want abc-123", got)
	}
	if got := ClusterUUIDFromTXT([]string{"uuid=host-level-id"}); got != "" {
		t.Fatalf("got %q, must not match the host uuid= key", got)
	}
	if got := ClusterUUIDFromTXT(nil); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
