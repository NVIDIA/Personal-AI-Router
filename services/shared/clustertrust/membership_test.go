// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeAdmission drops an admission.json into dir carrying the given active
// pair. clusterID="" / epoch=0 models a torn-down (leave/removal) record.
func writeAdmission(t *testing.T, dir, clusterID string, epoch uint64) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"counter":   epoch,
		"activated": epoch,
		"clusterId": clusterID,
		"epoch":     epoch,
		"retired":   clusterID == "",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, admissionFileName), body, 0o600))
}

// TestClustered_KeypairIsNotMembership is the regression guard for the invite
// deadlock: "clustered" must track live membership (admission / pins), never the
// mere presence of a keypair that survives a leave or removal by design.
func TestClustered_KeypairIsNotMembership(t *testing.T) {
	certPEM, keyPEM, _ := genLeaf(t, "uuid-self")

	// (a) A left/removed node: the durable keypair is still on disk, but there is
	// no active admission and no pinned peer. It must read as NOT clustered so it
	// stops advertising cluster-uuid= and can be invited back.
	leftover := t.TempDir()
	writeIdentity(t, leftover, certPEM, keyPEM)
	m := Open(leftover)
	assert.True(t, m.hasIdentity(), "the keypair on disk must load as an identity")
	assert.False(t, m.Clustered(), "a leftover keypair with no admission/pins must NOT be clustered")

	// (b) An already-clustered node: a live admission (even a cluster of one with
	// no pinned peers) reads as clustered, so it re-advertises.
	writeAdmission(t, leftover, "cluster-abc", 1)
	m.Refresh()
	assert.True(t, m.Clustered(), "an active admission must make the node clustered")

	// (c) A torn-down admission (clusterId cleared, epoch 0) is not membership,
	// even with the keypair still present.
	writeAdmission(t, leftover, "", 0)
	m.Refresh()
	assert.False(t, m.Clustered(), "a cleared admission must NOT be clustered")

	// (d) A pinned peer means membership even on a legacy dir with no
	// admission.json (older builds predate it).
	legacy := t.TempDir()
	writeIdentity(t, legacy, certPEM, keyPEM)
	peerPEM, _, _ := genLeaf(t, "uuid-peer")
	writePin(t, legacy, "uuid-peer", string(peerPEM))
	assert.True(t, Open(legacy).Clustered(), "a node with a pinned peer must be clustered")

	// (e) A dir with no keypair at all is never clustered, admission or not.
	bare := t.TempDir()
	writeAdmission(t, bare, "cluster-abc", 1)
	assert.False(t, Open(bare).Clustered(), "a node with no identity must never be clustered")

	// (f) A nil mesh (a service with no cluster dir) reads as unclustered rather
	// than panicking, so callers need no special case.
	var nilMesh *Mesh
	assert.False(t, nilMesh.Clustered(), "a nil mesh must read as permanently unclustered")
	assert.False(t, nilMesh.hasIdentity(), "a nil mesh must read as permanently unclustered")
	assert.Equal(t, "", nilMesh.NodeUUID(), "a nil mesh must read as permanently unclustered")
	assert.False(t, nilMesh.HasPin("uuid-self"), "a nil mesh must read as permanently unclustered")
}

// TestMesh_ConvergesOnACluster_JoinedAfterOpen is the regression guard for the
// half-clustered node: a service that opened its Mesh BEFORE
// nvpair-cluster-manager wrote the cluster dir must converge on the cluster in
// place. Deriving membership once at startup is what previously left such a
// service permanently on plain HTTP — in the roster, ranked by the scheduler,
// yet exchanging no cluster traffic — with a process restart as the only cure.
func TestMesh_ConvergesOnACluster_JoinedAfterOpen(t *testing.T) {
	dir := t.TempDir()

	// The cluster dir exists but is empty: this is a service that started before
	// the cluster-manager minted anything (the startup-order race).
	m := Open(dir)
	assert.False(t, m.Clustered(), "an empty cluster dir must read as unclustered")
	assert.False(t, m.hasIdentity(), "an empty cluster dir must read as unclustered")
	{
		_, ok := m.ClientTLSConfig("uuid-peer")
		require.False(t, ok, "an unclustered mesh must not build a peer client")
	}
	{
		_, ok := m.ClientTLSConfigAny()
		require.False(t, ok, "an unclustered mesh must not build an any-pin client")
	}

	// The user creates/joins a cluster: the cluster-manager mints the keypair,
	// activates the admission, and writes the peer's pin.
	certPEM, keyPEM, _ := genLeaf(t, "uuid-self")
	writeIdentity(t, dir, certPEM, keyPEM)
	writeAdmission(t, dir, "cluster-abc", 1)
	peerPEM, _, _ := genLeaf(t, "uuid-peer")
	writePin(t, dir, "uuid-peer", string(peerPEM))

	m.Refresh()
	assert.True(t, m.Clustered(), "the mesh must be clustered once the dir is populated")
	assert.Equal(t, "uuid-self", m.NodeUUID())
	{
		_, ok := m.ClientTLSConfig("uuid-peer")
		require.True(t, ok, "a pinned peer must be dialable after the transition")
	}
	{
		_, ok := m.ClientTLSConfigAny()
		require.True(t, ok, "the any-pin client must be available after the transition")
	}
	// Refreshing again with nothing on disk changed must leave the answer alone:
	// a repeat read is not a teardown.
	m.Refresh()
	assert.True(t, m.Clustered(), "a no-op Refresh must not disturb a live membership")
	assert.Equal(t, "uuid-self", m.NodeUUID(), "a no-op Refresh must not disturb a live membership")
}

// TestMesh_UnreadableKeypairDoesNotDemoteAMember: once an identity is loaded, a
// later failure to READ the keypair must not demote a live member. The keypair
// outlives a membership by design, so it carries no membership signal; teardown
// is reported by the admission record. A transient read failure (an antivirus
// quarantine of a freshly written private key, a locked file) previously had the
// same effect as leaving the cluster.
func TestMesh_UnreadableKeypairDoesNotDemoteAMember(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, _ := genLeaf(t, "uuid-self")
	writeIdentity(t, dir, certPEM, keyPEM)
	writeAdmission(t, dir, "cluster-abc", 1)

	m := Open(dir)
	assert.True(t, m.Clustered(), "a populated dir must be clustered")

	require.NoError(t, os.Remove(filepath.Join(dir, "node.key")))
	m.Refresh()
	assert.True(t, m.Clustered(), "a live member must keep serving after a failed keypair read")
	assert.Equal(t, "uuid-self", m.NodeUUID(), "a live member must keep serving after a failed keypair read")

	// Teardown, on the other hand, is authoritative: clearing the admission
	// drops membership even though the identity stays loaded in memory.
	writeAdmission(t, dir, "", 0)
	m.Refresh()
	assert.False(t, m.Clustered(), "a cleared admission must drop membership")
}

// TestMesh_RotatedKeypairIsPickedUp: a rejoin can mint a new identity in place,
// so a keypair whose stamp moved must be re-parsed rather than served from the
// cached leaf.
func TestMesh_RotatedKeypairIsPickedUp(t *testing.T) {
	dir := t.TempDir()
	firstCert, firstKey, _ := genLeaf(t, "uuid-first")
	writeIdentity(t, dir, firstCert, firstKey)
	writeAdmission(t, dir, "cluster-abc", 1)

	m := Open(dir)
	assert.Equal(t, "uuid-first", m.NodeUUID())

	secondCert, secondKey, _ := genLeaf(t, "uuid-second")
	writeIdentity(t, dir, secondCert, secondKey)
	m.Refresh()
	assert.Equal(t, "uuid-second", m.NodeUUID())
	assert.True(t, m.HasPin("uuid-second"), "self-trust must follow the rotated principal")
	assert.False(t, m.HasPin("uuid-first"), "the retired principal must no longer be self-trusted")
}
