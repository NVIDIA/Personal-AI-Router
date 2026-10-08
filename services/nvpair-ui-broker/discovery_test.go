// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// TestDiscoveryStoreRekeysOnRename verifies that a discovered node changing
// hostname (same host UUID, new mDNS instance name) updates its single
// store entry in place rather than leaving a ghost under the old name.
func TestDiscoveryStoreRekeysOnRename(t *testing.T) {
	s := newDiscoveryStore()
	const uuid = "11111111-1111-1111-1111-111111111111"

	s.Upsert(EnrichedNode{ID: "old-host", HostUUID: uuid}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "new-host", HostUUID: uuid}, sourceScanner)

	snap := s.Snapshot()
	require.Len(t, snap, 1, "a rename must not duplicate the node:")
	require.Equal(t, "new-host", snap[0].ID, "store did not track the new name: got id")
	require.Equal(t, uuid, snap[0].HostUUID, "hostUuid not projected to the wire: (%v)", uuid)
}

// TestDiscoveryStoreDistinctUUIDsSameName verifies the same-hostname collision
// case: two different machines that share a hostname stay two entries.
func TestDiscoveryStoreDistinctUUIDsSameName(t *testing.T) {
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "samename", HostUUID: "aaaaaaaa-0000-0000-0000-000000000001"}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "samename", HostUUID: "bbbbbbbb-0000-0000-0000-000000000002"}, sourceScanner)
	require.Len(t, s.Snapshot(), 2, "two machines sharing a hostname must not merge:")
}

// TestManualToEnrichedHostUUID verifies the manual-node ingestion boundary: a
// manual node keys by the remote's real hostUuid once node-info reports one, and
// by its manual id until then.
func TestManualToEnrichedHostUUID(t *testing.T) {
	withUUID := manualToEnriched(manualNodeStatus{ID: "manual:10.0.0.9", HostUUID: "real-uuid"})
	require.Equal(t, "real-uuid", withUUID.storeKey(), "store key")
	noUUID := manualToEnriched(manualNodeStatus{ID: "manual:10.0.0.9"})
	require.Equal(t, "manual:10.0.0.9", noUUID.storeKey(), "store key")
}

// TestDiscoveryStoreRejectsEmptyKey verifies a node with no operational key is
// dropped rather than silently keyed by name.
func TestDiscoveryStoreRejectsEmptyKey(t *testing.T) {
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "no-uuid"}, sourceManual) // HostUUID empty
	require.Empty(t, s.Snapshot(), "a node with no hostUuid must be dropped")
}

// TestDiscoveryStoreSourceOwnership verifies that a manual node and an mDNS
// node sharing a hostUuid occupy one record with two claims. Removing
// either source keeps the record alive under the survivor; only when no source
// claims it is it forgotten. This is what stops a manual remove (or a
// manual-nodes crash) from evicting a still-live scanner node.
func TestDiscoveryStoreSourceOwnership(t *testing.T) {
	const uuid = "shared-uuid"

	// Manual-remove overlap: scanner + manual both claim uuid; removing the
	// manual claim must leave the scanner's node in the snapshot.
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid, Port: 14318}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid, Port: 14318}, sourceManual)
	require.Len(t, s.Snapshot(), 1, "shared uuid should be one record")
	s.Remove(uuid, sourceManual)
	require.Len(t, s.Snapshot(), 1, "removing the manual claim evicted the live scanner node")
	s.Remove(uuid, sourceScanner)
	require.Empty(t, s.Snapshot(), "record should be gone once no source claims it")

	// Symmetric: removing the scanner claim leaves the manual node.
	s = newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceManual)
	s.Remove(uuid, sourceScanner)
	require.Len(t, s.Snapshot(), 1, "removing the scanner claim evicted the manual node")
}

// TestDiscoveryStoreScannerProjectionWins: when both sources claim a node, the
// scanner (mDNS) view is projected — it carries trusted/clusterUuid and
// per-engine models the manual probe lacks.
func TestDiscoveryStoreScannerProjectionWins(t *testing.T) {
	const uuid = "shared-uuid"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "manual-host", HostUUID: uuid, Trusted: false}, sourceManual)
	s.Upsert(EnrichedNode{ID: "mdns-host", HostUUID: uuid, Trusted: true}, sourceScanner)
	snap := s.Snapshot()
	require.Len(t, snap, 1, "want one record")
	require.Equal(t, "mdns-host", snap[0].ID, "scanner projection should win when both claim")
	require.True(t, snap[0].Trusted, "scanner projection should win when both claim")
	// Drop the scanner claim: the manual projection takes over.
	s.Remove(uuid, sourceScanner)
	snap = s.Snapshot()
	require.Len(t, snap, 1, "manual projection should surface after scanner removal")
	require.Equal(t, "manual-host", snap[0].ID, "manual projection should surface after scanner removal (%v)", snap)
}

func newManualTestBroker() *Broker {
	return &Broker{
		manualNodeKeys:     make(map[string]string),
		manualNodeStatuses: make(map[string]manualNodeStatusEntry),
		store:              newDiscoveryStore(),
		telemetry:          newTelemetryCache(),
	}
}

func manualStatus(id, addr, uuid string) manualNodeStatus {
	return manualNodeStatus{ID: id, Address: addr, HostUUID: uuid, NodeInfoPort: 14318}
}

// TestManualAliasesShareKeyUntilLastRemoved covers both removal orders: two
// manual entries (distinct names/addresses for one machine) resolve
// to one HostUUID and share a single sourceManual slot. Removing one alias must
// keep the node — reprojected from the surviving alias's payload so the removed
// alias's address doesn't linger — and only the last removal releases the key.
func TestManualAliasesShareKeyUntilLastRemoved(t *testing.T) {
	const uuid = "shared-uuid"
	for _, tc := range []struct {
		name         string
		removeFirst  string
		survivorID   string
		survivorAddr string
	}{
		{"remove-A-first", "alias-a", "alias-b", "10.0.0.2"},
		{"remove-B-first", "alias-b", "alias-a", "10.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newManualTestBroker()
			b.upsertManualNode(manualStatus("alias-a", "10.0.0.1", uuid))
			b.upsertManualNode(manualStatus("alias-b", "10.0.0.2", uuid))
			require.Len(t, b.store.Snapshot(), 1, "two aliases for one machine should be one record")

			b.removeManualNode(tc.removeFirst)
			snap := b.store.Snapshot()
			require.Len(t, snap, 1, "removing one of two aliases evicted the shared node (got")
			// The surviving alias must be reprojected: its id and address, not
			// the removed alias's stale payload.
			require.Equal(t, tc.survivorID, snap[0].ID, "survivor not reprojected: got id")
			require.Equal(t, tc.survivorAddr, snap[0].IPAddress, "survivor not reprojected: got id")

			b.removeManualNode(tc.survivorID)
			require.Empty(t, b.store.Snapshot(), "record should be gone once the last alias left")
		})
	}
}

// TestManualRekeyReprojectsSharedOldKey: when one of two aliases sharing a key
// rekeys onto its own distinct UUID (host replacement), the shared old key must
// stay — reprojected from the alias that still owns it — alongside the rekeyed
// alias's new record.
func TestManualRekeyReprojectsSharedOldKey(t *testing.T) {
	b := newManualTestBroker()
	const shared = "shared-uuid"

	b.upsertManualNode(manualStatus("alias-a", "10.0.0.1", shared))
	b.upsertManualNode(manualStatus("alias-b", "10.0.0.2", shared))

	// alias-a rekeys onto its own UUID; alias-b still owns the shared key.
	b.upsertManualNode(manualStatus("alias-a", "10.0.0.1", "alias-a-uuid"))

	snap := b.store.Snapshot()
	require.Len(t, snap, 2, "want two records (shared survivor + rekeyed alias)")
	// The shared key must now project alias-b (the surviving owner).
	var sharedNode *AvailableNode
	for i := range snap {
		if snap[i].HostUUID == shared {
			sharedNode = &snap[i]
		}
	}
	require.NotNil(t, sharedNode, "shared key (%v)", shared)
	require.Equal(t, "alias-b", sharedNode.ID, "shared key not reprojected from the surviving alias")
	require.Equal(t, "10.0.0.2", sharedNode.IPAddress, "shared key not reprojected from the surviving alias")
}

// TestDiscoveryStoreRemoveWrongSourceNoop: removing a source that never claimed
// the record leaves it (and the other source) intact.
func TestDiscoveryStoreRemoveWrongSourceNoop(t *testing.T) {
	const uuid = "u"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceScanner)
	// manual never claimed it
	require.False(t, s.Remove(uuid, sourceManual), "removing an unowned source must report false (nothing removed)")
	assert.Len(t, s.Snapshot(), 1, "removing an unowned source must not drop the record")
}

// TestDiscoveryStoreRemoveReportsFinalClaim: Remove reports whether it dropped
// the FINAL claim, so the node-loss sweep only fires when a node is truly gone.
func TestDiscoveryStoreRemoveReportsFinalClaim(t *testing.T) {
	const uuid = "shared-uuid"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceManual)

	require.False(t, s.Remove(uuid, sourceScanner), "removing one of two claims must report false (node still present)")
	require.True(t, s.Remove(uuid, sourceManual), "removing the surviving claim must report true (node now gone)")
	require.False(t, s.Remove(uuid, sourceScanner), "removing from an absent record must report false")
}

// nodeRemovedFrame builds the params of a discovery:node-removed for a node with
// the given display name and stable host UUID.
func nodeRemovedFrame(t *testing.T, name, uuid string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(noderec.NodeEvent{Node: noderec.DirectoryNode{Name: name, HostUUID: uuid}})
	require.NoError(t, err, "marshal node event")
	return b
}

// TestHandleNotifyNodeLostOnlyOnFinalClaim: a dual-source node (mDNS + manual)
// whose scanner claim drops is still present via the manual claim, so the
// node-loss sweep must NOT fire — otherwise a still-reachable node's jobs get
// wrongly failed.
func TestHandleNotifyNodeLostOnlyOnFinalClaim(t *testing.T) {
	const uuid = "peer-uuid"
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{ID: "peer-friendly-name", HostUUID: uuid}, sourceScanner)
	store.Upsert(EnrichedNode{ID: "peer-friendly-name", HostUUID: uuid}, sourceManual)

	var calls int
	sp := &scannerProcess{store: store, onNodeLost: func(string, string) { calls++ }}

	sp.handleNotify(noderec.NotifyNodeRemoved, nodeRemovedFrame(t, "peer-friendly-name", uuid))

	require.Equal(t, 0, calls, "onNodeLost fired")
	require.Len(t, store.Snapshot(), 1, "dual-source node should remain after one source leaves")
}

// TestHandleNotifyNodeLostUsesHostUUID: removing a node's final claim fires the
// sweep with the node's stable HostUUID (what workloads are keyed by), not its
// display hostname — the name != uuid case is exactly the bug.
func TestHandleNotifyNodeLostUsesHostUUID(t *testing.T) {
	const uuid = "peer-uuid"
	const name = "peer-friendly-name"
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{ID: name, HostUUID: uuid}, sourceScanner)

	var gotUUID, gotName string
	var calls int
	sp := &scannerProcess{store: store, onNodeLost: func(u, n string) { calls++; gotUUID = u; gotName = n }}

	sp.handleNotify(noderec.NotifyNodeRemoved, nodeRemovedFrame(t, name, uuid))

	require.Equal(t, 1, calls, "onNodeLost calls")
	assert.Equal(t, uuid, gotUUID, "onNodeLost uuid (%v, %v)", gotUUID, uuid)
	assert.Equal(t, name, gotName, "onNodeLost name (%v, %v)", gotName, name)
}

func TestScannerProcessRoutesAndRemovesTelemetry(t *testing.T) {
	want := noderec.NodeTelemetry{
		HostUUID:          "peer-uuid",
		GPUUtilizationPct: 84,
		TelemetryValid:    true,
		MSSince:           137,
	}
	params, err := json.Marshal(want)
	require.NoError(t, err, "marshal telemetry")

	var got noderec.NodeTelemetry
	var removed string
	sp := &scannerProcess{
		onTelemetry:        func(value noderec.NodeTelemetry) { got = value },
		onTelemetryRemoved: func(hostUUID string) { removed = hostUUID },
	}
	sp.handleNotify(noderec.NotifyNodeTelemetry, params)
	require.Equal(t, want.HostUUID, got.HostUUID, "routed telemetry (%v, %v)", got, want)
	require.True(t, got.TelemetryValid, "routed telemetry (%v, %v)", got, want)
	require.Equal(t, want.MSSince, got.MSSince, "routed telemetry (%v, %v)", got, want)
	require.Equal(t, uint32(84), got.GPUUtilizationPct, "routed utilization")

	sp.handleNotify(noderec.NotifyNodeRemoved, nodeRemovedFrame(t, "peer", "peer-uuid"))
	require.Equal(t, "peer-uuid", removed, "removed telemetry host")
}
