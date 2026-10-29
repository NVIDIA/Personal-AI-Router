// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeIdentity drops a node.crt/node.key keypair into dir so a Mesh opened on
// it finds an identity there. (genLeaf + writePin come from clustertrust_test.go.)
func writeIdentity(t *testing.T, dir string, certPEM, keyPEM []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600))
}

func TestMesh_VerifyPeerCertificateUsesCurrentExactPin(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"

	selfPEM, selfKey, _ := genLeaf(t, selfUUID)
	peerPEM, _, peerDER := genLeaf(t, peerUUID)
	replacementPEM, _, replacementDER := genLeaf(t, peerUUID)
	mesh := loadMeshDir(t, selfPEM, selfKey, map[string]string{peerUUID: string(peerPEM)})
	peer, err := x509.ParseCertificate(peerDER)
	require.NoError(t, err)
	replacement, err := x509.ParseCertificate(replacementDER)
	require.NoError(t, err)
	require.True(t, mesh.VerifyPeerCertificate(peerUUID, peer))
	assert.False(t, mesh.VerifyPeerCertificate("another-peer", peer))
	assert.False(t, mesh.VerifyPeerCertificate(peerUUID, replacement))
	assert.False(t, mesh.VerifyPeerCertificate(peerUUID, nil))
	writePin(t, mesh.dir, peerUUID, string(replacementPEM))
	mesh.Refresh()
	assert.False(t, mesh.VerifyPeerCertificate(peerUUID, peer))
	require.True(t, mesh.VerifyPeerCertificate(peerUUID, replacement))
	require.NoError(t, os.Remove(filepath.Join(mesh.dir, "trusted", peerUUID+".json")))
	mesh.Refresh()
	assert.False(t, mesh.VerifyPeerCertificate(peerUUID, replacement))
}

// A record ID binds an authenticated result to the current authorization, so
// cached results cannot survive membership or certificate changes. Unchanged
// authorization keeps the ID stable, and removing authorization leaves no ID.
func TestMesh_PeerRecordIDChangesWithAuthorization(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const clusterID = "cluster"

	selfPEM, selfKey, _ := genLeaf(t, selfUUID)
	peerPEM, _, _ := genLeaf(t, peerUUID)
	mesh := loadMeshDir(t, selfPEM, selfKey, map[string]string{peerUUID: string(peerPEM)})
	previous := mesh.PeerRecordID(peerUUID)
	require.NotEmpty(t, previous)
	assert.Empty(t, mesh.PeerRecordID("unapproved"))
	// Reloading unchanged trust must not invalidate an authenticated result.
	mesh.Refresh()
	assert.Equal(t, previous, mesh.PeerRecordID(peerUUID))

	// A new local admission invalidates earlier results, even when both nodes
	// keep their certificates. Advancing the epoch models leaving and rejoining.
	writeAdmission(t, mesh.dir, clusterID, 1)
	mesh.Refresh()
	admitted := mesh.PeerRecordID(peerUUID)
	assert.NotEqual(t, previous, admitted)
	writeAdmission(t, mesh.dir, clusterID, 2)
	mesh.Refresh()
	assert.NotEqual(t, admitted, mesh.PeerRecordID(peerUUID))

	// Replacing the local certificate also invalidates earlier peer results.
	previous = mesh.PeerRecordID(peerUUID)
	replacementPEM, replacementKey, _ := genLeaf(t, selfUUID)
	writeIdentity(t, mesh.dir, replacementPEM, replacementKey)
	mesh.Refresh()
	assert.NotEqual(t, previous, mesh.PeerRecordID(peerUUID))

	// Re-admitting the peer starts a new authorization even if its UUID and
	// certificate are unchanged. The saved admission details must affect the ID.
	previous = mesh.PeerRecordID(peerUUID)
	type savedPin struct {
		NodeUUID       string `json:"nodeUuid"`
		CertPEM        string `json:"certPem"`
		AdmissionEpoch uint64 `json:"admissionEpoch"`
		PinnedAt       int64  `json:"pinnedAt"`
	}
	pinPath := filepath.Join(mesh.dir, "trusted", peerUUID+".json")
	data, err := os.ReadFile(pinPath)
	require.NoError(t, err)
	var pin savedPin
	require.NoError(t, json.Unmarshal(data, &pin))
	pin.AdmissionEpoch = 2
	pin.PinnedAt = 1000
	data, err = json.Marshal(pin)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pinPath, data, 0o600))
	mesh.Refresh()
	assert.NotEqual(t, previous, mesh.PeerRecordID(peerUUID))

	// Replacing the peer's certificate invalidates its earlier results. Removing
	// its pin then leaves the peer unauthorized, so it must have no record ID.
	previous = mesh.PeerRecordID(peerUUID)
	replacementPeerPEM, _, _ := genLeaf(t, peerUUID)
	writePin(t, mesh.dir, peerUUID, string(replacementPeerPEM))
	mesh.Refresh()
	assert.NotEqual(t, previous, mesh.PeerRecordID(peerUUID))
	require.NoError(t, os.Remove(pinPath))
	mesh.Refresh()
	assert.Empty(t, mesh.PeerRecordID(peerUUID))
}

// A failed admission read is missing information, not a membership change.
// Existing local membership and peer record IDs must survive until a later
// successful read can establish whether authorization changed.
func TestMesh_PreservesAuthorizationWhenAdmissionReadFails(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const clusterID = "cluster"

	test := func(name, principal string, pins map[string]string) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			selfPEM, selfKey, _ := genLeaf(t, selfUUID)
			writeIdentity(t, dir, selfPEM, selfKey)
			writeAdmission(t, dir, clusterID, 1)
			for uuid, certPEM := range pins {
				writePin(t, dir, uuid, certPEM)
			}
			mesh := Open(dir)
			require.True(t, mesh.Clustered())
			previous := mesh.PeerRecordID(principal)
			require.NotEmpty(t, previous)

			// A directory at the file path makes ReadFile fail on every platform,
			// without relying on permission changes that Windows may ignore.
			path := filepath.Join(mesh.dir, admissionFileName)
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Mkdir(path, 0o700))
			mesh.Refresh()

			assert.True(t, mesh.Clustered())
			assert.Equal(t, previous, mesh.PeerRecordID(principal))

			// Once reads recover, a new admission must invalidate the old ID.
			require.NoError(t, os.Remove(path))
			writeAdmission(t, dir, clusterID, 2)
			mesh.Refresh()
			assert.True(t, mesh.Clustered())
			assert.NotEqual(t, previous, mesh.PeerRecordID(principal))
		})
	}

	peerPEM, _, _ := genLeaf(t, peerUUID)
	test("local membership without peers", selfUUID, nil)
	test("verified peer record", peerUUID, map[string]string{peerUUID: string(peerPEM)})
}

func TestMesh_ClearsAdmissionWhenFileIsAbsentOrInvalid(t *testing.T) {
	const selfUUID = "self"
	const clusterID = "cluster"

	test := func(name string, change func(*testing.T, string)) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			selfPEM, selfKey, _ := genLeaf(t, selfUUID)
			writeIdentity(t, dir, selfPEM, selfKey)
			writeAdmission(t, dir, clusterID, 1)
			mesh := Open(dir)
			require.True(t, mesh.Clustered())

			change(t, dir)
			mesh.Refresh()

			assert.False(t, mesh.Clustered())
			assert.Empty(t, mesh.PeerRecordID(selfUUID))
		})
	}

	test("admission file removed", func(t *testing.T, dir string) {
		require.NoError(t, os.Remove(filepath.Join(dir, admissionFileName)))
	})
	test("admission explicitly cleared", func(t *testing.T, dir string) {
		writeAdmission(t, dir, "", 0)
	})
	test("malformed JSON", func(t *testing.T, dir string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, admissionFileName), []byte(`{"clusterId":`), 0o600))
	})
	test("invalid field type", func(t *testing.T, dir string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, admissionFileName), []byte(`{"clusterId":"cluster","epoch":"invalid"}`), 0o600))
	})
}

func loadMeshDir(t *testing.T, certPEM, keyPEM []byte, pins map[string]string) *Mesh {
	t.Helper()
	dir := t.TempDir()
	writeIdentity(t, dir, certPEM, keyPEM)
	for uuid, pem := range pins {
		writePin(t, dir, uuid, pem)
	}
	m := Open(dir)
	require.True(t, m.Clustered(), "a populated cluster dir must read as clustered")
	return m
}

// TestMesh_Open_UnclusteredCases: an empty or identity-less dir yields a usable
// but unclustered mesh, so the caller serves plain HTTP from the same object it
// will later serve cluster mTLS from.
func TestMeshOpen_LeavesUnclusteredNodesWithoutPeerAuthorization(t *testing.T) {
	test := func(name, dir string) {
		t.Run(name, func(t *testing.T) {
			m := Open(dir)
			require.NotNil(t, m)
			assert.False(t, m.Clustered())
			assert.False(t, m.hasIdentity())
			m.Refresh()
			assert.False(t, m.Clustered(), "after refresh")
		})
	}
	test("no cluster dir", "")
	test("empty cluster dir", t.TempDir())
}

// TestMesh_GateSelfTrustAndAnyPin drives the full server/client handshake the
// way every consumer uses it, and pins down the behaviors node-info/scanner/
// manual-nodes specifically need: a pinned peer is accepted, a node's own leaf
// is accepted (self-trust) even though it isn't in trusted/, the UUID-less
// any-pin client works, and a node that completes the handshake but isn't
// pinned by the server is rejected at the gate.
func TestMeshGate_RequiresMembershipAndPinnedIdentity(t *testing.T) {
	const selfUUID = "uuid-self"
	const peerUUID = "uuid-peer"
	const strangerUUID = "uuid-stranger"

	selfPEM, selfKey, _ := genLeaf(t, selfUUID)
	peerPEM, peerKey, _ := genLeaf(t, peerUUID)
	strangerPEM, strangerKey, _ := genLeaf(t, strangerUUID)

	// self pins peer (only). peer + stranger both pin self (so both can complete
	// a handshake TO self) — but self does not pin stranger.
	selfMesh := loadMeshDir(t, selfPEM, selfKey, map[string]string{peerUUID: string(peerPEM)})
	peerMesh := loadMeshDir(t, peerPEM, peerKey, map[string]string{selfUUID: string(selfPEM)})
	strangerMesh := loadMeshDir(t, strangerPEM, strangerKey, map[string]string{selfUUID: string(selfPEM)})

	assert.Equal(t, selfUUID, selfMesh.NodeUUID())
	assert.True(t, selfMesh.HasPin(peerUUID), "peer must be a pin")
	assert.True(t, selfMesh.HasPin(selfUUID), "self must be trusted (self-trust)")
	assert.False(t, selfMesh.HasPin(strangerUUID), "stranger must not be a pin")

	// self is the mTLS server, gated by VerifyClientPin.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := selfMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, err := w.Write([]byte("ok"))
		assert.NoError(t, err, "write peer response")
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
		require.True(t, ok, "expected a client config")
		return cfg
	}

	// Pinned peer -> self, specific pin: accepted.
	const specificPinContext = "peer->self (specific pin)"
	code, err := do(mustConfig(peerMesh.ClientTLSConfig(selfUUID)))
	require.NoError(t, err, specificPinContext)
	assert.Equal(t, http.StatusOK, code, specificPinContext)
	// Pinned peer -> self, any-pin (no UUID known up front): accepted.
	const anyPinContext = "peer->self (any-pin)"
	code, err = do(mustConfig(peerMesh.ClientTLSConfigAny()))
	require.NoError(t, err, anyPinContext)
	assert.Equal(t, http.StatusOK, code, anyPinContext)
	// self -> self over mTLS: accepted via self-trust (self isn't in trusted/).
	const selfTrustContext = "self->self"
	code, err = do(mustConfig(selfMesh.ClientTLSConfig(selfUUID)))
	require.NoError(t, err, selfTrustContext)
	assert.Equal(t, http.StatusOK, code, selfTrustContext)
	// Stranger completes the handshake (it pins self) but self doesn't pin it: 403.
	code, err = do(mustConfig(strangerMesh.ClientTLSConfig(selfUUID)))
	require.NoError(t, err, "request as unpinned client")
	assert.Equal(t, http.StatusForbidden, code, "stranger->self")
	// self cannot even build a client to an unpinned stranger (client-side gate).
	_, ok := selfMesh.ClientTLSConfig(strangerUUID)
	require.False(t, ok, "must not build a client for an unpinned peer")
}

// TestMesh_ServerTLSConfig_FollowsMembershipOnOneListener is the server-side half
// of the half-clustered regression guard. A cluster-scoped service binds its
// listener once, at startup, when it may not yet be a member; the TLS
// personality of that ALREADY-BOUND listener must follow membership. Before the
// cluster dir is populated a peer handshake is refused; afterwards the very same
// listener serves the pinned mTLS surface, with no rebind and no restart.
func TestMeshServerTLSConfig_TracksMembershipOnExistingListener(t *testing.T) {
	const selfUUID = "uuid-self"
	const peerUUID = "uuid-peer"

	selfDir := t.TempDir()
	selfPEM, selfKey, _ := genLeaf(t, selfUUID)
	peerPEM, peerKey, _ := genLeaf(t, peerUUID)
	peerMesh := loadMeshDir(t, peerPEM, peerKey, map[string]string{selfUUID: string(selfPEM)})

	selfMesh := Open(selfDir)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := selfMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, err := w.Write([]byte("ok"))
		assert.NoError(t, err, "write peer response")
	}))
	srv.TLS = selfMesh.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	get := func() (int, error) {
		cfg, ok := peerMesh.ClientTLSConfig(selfUUID)
		require.True(t, ok, "peer must be able to build a client for self")
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Get(srv.URL)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	// Not a member yet: the handshake is refused rather than served plaintext.
	code, err := get()
	require.Error(t, err, "an unclustered listener must refuse the handshake, got code %d", code)

	// The node joins: identity + admission + the peer's pin land on disk.
	writeIdentity(t, selfDir, selfPEM, selfKey)
	writeAdmission(t, selfDir, "cluster-abc", 1)
	writePin(t, selfDir, peerUUID, string(peerPEM))
	selfMesh.Refresh()
	require.True(t, selfMesh.Clustered(), "the mesh must be clustered once the dir is populated")

	const joinedListenerContext = "the same listener must serve the pinned peer after the join"
	code, err = get()
	require.NoError(t, err, joinedListenerContext)
	assert.Equal(t, http.StatusOK, code, joinedListenerContext)
}

// TestClusterUUIDFromTXT: reads cluster-uuid= and never node-info's host uuid=.
func TestClusterUUIDFromTXT_ExtractsClusterIdentity(t *testing.T) {
	assert.Equal(t, "abc-123", ClusterUUIDFromTXT([]string{"http=0", "cluster-uuid=abc-123", "mtls=1"}))
	assert.Equal(t, "", ClusterUUIDFromTXT([]string{"uuid=host-level-id"}))
	assert.Equal(t, "", ClusterUUIDFromTXT(nil))
}
