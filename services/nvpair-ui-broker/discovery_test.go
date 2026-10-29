// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func TestScannerTelemetry_IncludesDiscoveryLockWaitInSampleAge(t *testing.T) {
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{HostUUID: "self", IdentityVerification: noderec.IdentityLocal}, sourceScanner)
	var stateMu sync.Mutex
	var received noderec.NodeTelemetry
	scanner := &scannerProcess{
		store: store, stateMu: &stateMu,
		onTelemetry: func(sample noderec.NodeTelemetry) { received = sample },
	}
	wait := holdDiscoveryNotification(t, &stateMu, func() {
		scanner.handleNotify(noderec.NotifyNodeTelemetry, json.RawMessage(`{"hostUuid":"self","telemetryValid":true,"msSince":137}`))
	})

	assert.True(t, received.TelemetryValid)
	assert.GreaterOrEqual(t, received.MSSince, int64(137)+wait.Milliseconds())
}

func TestScannerNodeInfo_IncludesDiscoveryLockWaitInSampleAge(t *testing.T) {
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{HostUUID: "self", DiscoveryRecordID: "local:self", IdentityVerification: noderec.IdentityLocal}, sourceScanner)
	var stateMu sync.Mutex
	var received []noderec.NodeInfoEvent
	scanner := &scannerProcess{
		store: store, stateMu: &stateMu,
		onNodeInfo: func(value noderec.NodeInfoEvent) { received = append(received, value) },
	}
	wait := holdDiscoveryNotification(t, &stateMu, func() {
		scanner.handleNotify(noderec.NotifyNodeInfo, json.RawMessage(`{"hostUuid":"self","discoveryRecordId":"local:self","identityVerification":"local","info":{"hostUuid":"self","telemetryValid":true,"msSince":137}}`))
	})

	require.Len(t, received, 1)
	assert.GreaterOrEqual(t, received[0].Info.MSSince, int64(137)+wait.Milliseconds())
}

func TestScannerNodeInfo_RejectsSnapshotsForReplacedRecord(t *testing.T) {
	store := newDiscoveryStore()
	node := EnrichedNode{HostUUID: "peer", DiscoveryRecordID: "old-proof", IdentityVerification: noderec.IdentityPinned, Trusted: true}
	store.Upsert(node, sourceScanner)
	principal := "peer"
	value := noderec.NodeInfoEvent{
		HostUUID: "peer", ClusterUUID: principal, DiscoveryRecordID: "old-proof",
		IdentityVerification: noderec.IdentityPinned,
		Info:                 noderec.NodeInfo{HostUUID: "peer", ClusterUUID: &principal},
	}
	var received []noderec.NodeInfoEvent
	scanner := &scannerProcess{store: store, onNodeInfo: func(value noderec.NodeInfoEvent) {
		received = append(received, value)
	}}

	// Replacing a directory proof must prevent the previous worker's queued
	// hardware snapshot from reaching clients under the new record.
	node.DiscoveryRecordID = "new-proof"
	store.Upsert(node, sourceScanner)
	params, err := json.Marshal(value)
	require.NoError(t, err)
	scanner.handleNotify(noderec.NotifyNodeInfo, params)
	assert.Empty(t, received)

	value.DiscoveryRecordID = node.DiscoveryRecordID
	params, err = json.Marshal(value)
	require.NoError(t, err)
	scanner.handleNotify(noderec.NotifyNodeInfo, params)
	assert.Equal(t, []noderec.NodeInfoEvent{value}, received)
}

func TestScannerProcess_RejectsQueuedEventsAfterPinReplacement(t *testing.T) {
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", "self", "peer")
	mesh := clustertrust.Open(dir)
	store := newDiscoveryStore()
	scanner := &scannerProcess{store: store, mesh: mesh}
	node := noderec.DirectoryNode{HostUUID: "peer", DiscoveryRecordID: mesh.PeerRecordID("peer"), IdentityVerification: noderec.IdentityPinned, Trusted: true}
	send := func(method string, node noderec.DirectoryNode) {
		t.Helper()
		params, err := json.Marshal(noderec.NodeEvent{Node: node})
		require.NoError(t, err)
		scanner.handleNotify(method, params)
	}
	send(noderec.NotifyNodeDiscovered, node)
	require.True(t, store.verifiedSource("peer", sourceScanner))
	store.Remove("peer", sourceScanner)
	clustertrusttest.WritePeerPin(t, dir, "peer")

	send(noderec.NotifyNodeUpdated, node)
	assert.Empty(t, store.Snapshot())
	mesh.Refresh()
	replacement := node
	replacement.DiscoveryRecordID = mesh.PeerRecordID("peer")
	send(noderec.NotifyNodeDiscovered, replacement)
	send(noderec.NotifyNodeRemoved, node)

	snapshot := store.Snapshot()
	require.Len(t, snapshot, 1)
	assert.Equal(t, replacement.DiscoveryRecordID, snapshot[0].DiscoveryRecordID)
}

// Store a verified peer alongside an unverified advertisement copying its UUID
// and name. Removing the advertisement must leave the peer's record, endpoint,
// and trust intact, rather than overwriting or removing that peer.
func TestDiscoveryStore_IsolatesUnverifiedClaimsFromVerifiedPeers(t *testing.T) {
	store := newDiscoveryStore()
	const knownUUID = "host-a"
	const peerRecordID = "peer:host-a"
	const peerName = "peer"
	const peerAddress = "192.0.2.20"
	peer := noderec.DirectoryNode{
		DiscoveryRecordID:    peerRecordID,
		HostUUID:             knownUUID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		Name:                 peerName,
		IP:                   peerAddress,
	}
	candidate := noderec.DirectoryNode{
		DiscoveryRecordID:    "candidate:rogue",
		HostUUID:             knownUUID,
		IdentityVerification: noderec.IdentityUnverified,
		Name:                 peerName,
		IP:                   "192.0.2.30",
	}
	store.Upsert(directoryToEnriched(peer), sourceScanner)
	store.Upsert(directoryToEnriched(candidate), sourceScanner)

	require.Len(t, store.Snapshot(), 2)
	store.Remove(candidate.DiscoveryRecordID, sourceScanner)
	snapshot := store.Snapshot()
	require.Len(t, snapshot, 1)
	assert.Equal(t, peerRecordID, snapshot[0].DiscoveryRecordID)
	assert.Equal(t, peerAddress, snapshot[0].IPAddress)
	assert.True(t, snapshot[0].Trusted)
}

// Process a removal for an unverified record that claims a host UUID. Neither
// host-level callback may fire: clearing telemetry or failing workloads would
// incorrectly attribute the candidate's disappearance to that claimed host.
func TestScannerProcess_CandidateRemovalPreservesPeerTelemetryAndWorkloads(t *testing.T) {
	var removed, lost int
	store := newDiscoveryStore()
	sp := &scannerProcess{
		store:              store,
		onTelemetryRemoved: func(string) { removed++ },
		onNodeLost:         func(string, string) { lost++ },
	}
	candidate := noderec.DirectoryNode{
		DiscoveryRecordID:    "candidate:rogue",
		HostUUID:             "host-a",
		IdentityVerification: noderec.IdentityUnverified,
	}
	store.Upsert(directoryToEnriched(candidate), sourceScanner)
	params, err := json.Marshal(noderec.NodeEvent{Node: candidate})
	require.NoError(t, err)

	sp.handleNotify(noderec.NotifyNodeRemoved, params)

	assert.Zero(t, removed)
	assert.Zero(t, lost)
}

// Only hardware matching the current verified record and authenticated principal
// may reach clients. Missing or conflicting identity and late samples must not.
func TestScannerNodeInfo_ForwardsSnapshotsOnlyForTheirVerifiedRecord(t *testing.T) {
	const hostUUID = "host-a"
	const peerRecordID = "peer:host-a"
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{
		HostUUID:             hostUUID,
		DiscoveryRecordID:    peerRecordID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
	}, sourceScanner)
	principal := hostUUID
	value := noderec.NodeInfoEvent{
		HostUUID:             hostUUID,
		ClusterUUID:          principal,
		DiscoveryRecordID:    peerRecordID,
		IdentityVerification: noderec.IdentityPinned,
		Info: noderec.NodeInfo{
			HostUUID:       hostUUID,
			ClusterUUID:    &principal,
			TelemetryValid: true,
			MSSince:        100,
			GPUs:           []GPUInfo{{Name: "accepted GPU"}},
		},
	}
	var received []noderec.NodeInfoEvent
	scanner := &scannerProcess{store: store, onNodeInfo: func(value noderec.NodeInfoEvent) {
		received = append(received, value)
	}}
	send := func(value noderec.NodeInfoEvent) {
		t.Helper()
		params, err := json.Marshal(value)
		require.NoError(t, err)
		scanner.handleNotify(noderec.NotifyNodeInfo, params)
	}
	send(value)
	require.Len(t, received, 1)
	assert.Equal(t, value.Info.GPUs, received[0].Info.GPUs)
	assert.GreaterOrEqual(t, received[0].Info.MSSince, value.Info.MSSince)

	reject := func(name string, change func(*noderec.NodeInfoEvent)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			forged := value
			change(&forged)
			params, err := json.Marshal(forged)
			require.NoError(t, err)
			scanner.handleNotify(noderec.NotifyNodeInfo, params)
			assert.Len(t, received, 1, "unauthorized hardware must not reach clients")
		})
	}
	reject("different record", func(value *noderec.NodeInfoEvent) { value.DiscoveryRecordID = "candidate:rogue" })
	reject("unverified identity", func(value *noderec.NodeInfoEvent) { value.IdentityVerification = noderec.IdentityUnverified })
	reject("different hardware host", func(value *noderec.NodeInfoEvent) { value.Info.HostUUID = "other" })
	reject("different principal", func(value *noderec.NodeInfoEvent) { value.ClusterUUID = "other" })
	reject("missing hardware principal", func(value *noderec.NodeInfoEvent) { value.Info.ClusterUUID = nil })
	reject("different hardware principal", func(value *noderec.NodeInfoEvent) {
		other := "other"
		value.Info.ClusterUUID = &other
	})

	// A late snapshot cannot recreate the missing verified directory record.
	store.Remove(hostUUID, sourceScanner)
	send(value)
	assert.Len(t, received, 1)
}

// TestDiscoveryStoreRekeysOnRename verifies that a discovered node changing
// hostname (same host UUID, new mDNS instance name) updates its single
// store entry in place rather than leaving a ghost under the old name.
func TestDiscoveryStore_RekeysOnRename(t *testing.T) {
	s := newDiscoveryStore()
	const uuid = "11111111-1111-1111-1111-111111111111"
	const newName = "new-host"

	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   "old-host",
		HostUUID:             uuid,
	}, sourceScanner)
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   newName,
		HostUUID:             uuid,
	}, sourceScanner)

	snap := s.Snapshot()
	require.Len(t, snap, 1, "a rename must not duplicate the node")
	require.Equal(t, newName, snap[0].ID, "store did not track the new name")
	require.Equal(t, uuid, snap[0].HostUUID, "hostUuid not projected to the wire")
}

// TestDiscoveryStoreDistinctUUIDsSameName verifies the same-hostname collision
// case: two different machines that share a hostname stay two entries.
func TestDiscoveryStore_KeepsNodesWithSameNameAndDifferentUUIDs(t *testing.T) {
	const name = "samename"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             "aaaaaaaa-0000-0000-0000-000000000001",
	}, sourceScanner)
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             "bbbbbbbb-0000-0000-0000-000000000002",
	}, sourceScanner)
	require.Len(t, s.Snapshot(), 2, "two machines sharing a hostname must not merge")
}

func TestManualToEnriched_UnverifiedHostClaimsKeepTheirConfigurationKey(t *testing.T) {
	// The worker assigns unnamed entries an ID of "manual:<address>". The broker
	// prefixes that entry ID with "manual:" to keep configuration keys separate
	// from host UUIDs, so an unnamed entry has two prefixes.
	const manualID = "manual:10.0.0.9"
	const storeKey = "manual:manual:10.0.0.9"
	withUUID := manualToEnriched(manualNodeStatus{
		ID:       manualID,
		HostUUID: "real-uuid",
	})
	require.Equal(t, storeKey, withUUID.storeKey(), "store key")
	noUUID := manualToEnriched(manualNodeStatus{ID: manualID})
	require.Equal(t, storeKey, noUUID.storeKey(), "store key")
}

// TestDiscoveryStoreRejectsEmptyKey verifies a node with no operational key is
// dropped rather than silently keyed by name.
func TestDiscoveryStore_RejectsEmptyKey(t *testing.T) {
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   "no-uuid",
	}, sourceManual) // HostUUID empty
	require.Empty(t, s.Snapshot(), "a node with no hostUuid must be dropped")
}

// TestDiscoveryStoreSourceOwnership verifies that a manual node and an mDNS
// node sharing a hostUuid occupy one record with two claims. Removing
// either source keeps the record alive under the survivor; only when no source
// claims it is it forgotten. This is what stops a manual remove (or a
// manual-nodes crash) from evicting a still-live scanner node.
func TestDiscoveryStore_PreservesClaimsFromOtherSources(t *testing.T) {
	const uuid = "shared-uuid"
	const name = "host"
	const nodeInfoPort = 14318

	// Manual-remove overlap: scanner + manual both claim uuid; removing the
	// manual claim must leave the scanner's node in the snapshot.
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
		Port:                 nodeInfoPort,
	}, sourceScanner)
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
		Port:                 nodeInfoPort,
	}, sourceManual)
	require.Len(t, s.Snapshot(), 1, "shared uuid should be one record")
	s.Remove(uuid, sourceManual)
	require.Len(t, s.Snapshot(), 1, "removing the manual claim evicted the live scanner node")
	s.Remove(uuid, sourceScanner)
	require.Empty(t, s.Snapshot(), "record should be gone once no source claims it")

	// Symmetric: removing the scanner claim leaves the manual node.
	s = newDiscoveryStore()
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
	}, sourceScanner)
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
	}, sourceManual)
	s.Remove(uuid, sourceScanner)
	require.Len(t, s.Snapshot(), 1, "removing the scanner claim evicted the manual node")
}

// TestDiscoveryStoreScannerProjectionWins: when both sources claim a node, the
// scanner (mDNS) view is projected — it carries trusted/clusterUuid and
// per-engine models the manual probe lacks.
func TestDiscoveryStoreScannerProjection_Wins(t *testing.T) {
	const uuid = "shared-uuid"
	const manualName = "manual-host"
	const scannerName = "mdns-host"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   manualName,
		HostUUID:             uuid,
	}, sourceManual)
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   scannerName,
		HostUUID:             uuid,
	}, sourceScanner)
	snap := s.Snapshot()
	require.Len(t, snap, 1, "want one record")
	require.Equal(t, scannerName, snap[0].ID, "scanner projection should win when both claim")
	require.True(t, snap[0].Trusted, "scanner projection should win when both claim")
	// Drop the scanner claim: the manual projection takes over.
	s.Remove(uuid, sourceScanner)
	snap = s.Snapshot()
	require.Len(t, snap, 1, "manual projection should surface after scanner removal")
	require.Equal(t, manualName, snap[0].ID, "manual projection should surface after scanner removal (%v)", snap)
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
	return manualNodeStatus{
		IdentityVerification: noderec.IdentityPinned,
		ID:                   id,
		Address:              addr,
		HostUUID:             uuid,
		NodeInfoPort:         14318,
	}
}

// TestManualAliasesShareKeyUntilLastRemoved covers both removal orders: two
// manual entries (distinct names/addresses for one machine) resolve
// to one HostUUID and share a single sourceManual slot. Removing one alias must
// keep the node — reprojected from the surviving alias's payload so the removed
// alias's address doesn't linger — and only the last removal releases the key.
func TestManualAliases_KeepSharedKeyUntilLastAliasIsRemoved(t *testing.T) {
	const uuid = "shared-uuid"
	test := func(name, removeFirst, survivorID, survivorAddr string) {
		t.Run(name, func(t *testing.T) {
			b := newManualTestBroker()
			b.upsertManualNode(manualStatus("alias-a", "10.0.0.1", uuid))
			b.upsertManualNode(manualStatus("alias-b", "10.0.0.2", uuid))
			require.Len(t, b.store.Snapshot(), 1, "two aliases for one machine should be one record")

			b.removeManualNode(removeFirst)
			snap := b.store.Snapshot()
			require.Len(t, snap, 1, "removing one of two aliases evicted the shared node")
			// The surviving alias must be reprojected: its id and address, not
			// the removed alias's stale payload.
			require.Equal(t, survivorID, snap[0].ID, "survivor not reprojected")
			require.Equal(t, survivorAddr, snap[0].IPAddress, "survivor not reprojected")

			b.removeManualNode(survivorID)
			require.Empty(t, b.store.Snapshot(), "record should be gone once the last alias left")
		})
	}
	test("remove A first", "alias-a", "alias-b", "10.0.0.2")
	test("remove B first", "alias-b", "alias-a", "10.0.0.1")
}

// TestManualRekeyReprojectsSharedOldKey: when one of two aliases sharing a key
// rekeys onto its own distinct UUID (host replacement), the shared old key must
// stay — reprojected from the alias that still owns it — alongside the rekeyed
// alias's new record.
func TestManualRekey_ReprojectsSharedOldKey(t *testing.T) {
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
func TestDiscoveryStore_IgnoresRemovalFromUnrelatedSource(t *testing.T) {
	const uuid = "u"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   "host",
		HostUUID:             uuid,
	}, sourceScanner)
	// manual never claimed it
	require.False(t, s.Remove(uuid, sourceManual), "removing an unowned source must report false (nothing removed)")
	assert.Len(t, s.Snapshot(), 1, "removing an unowned source must not drop the record")
}

// TestDiscoveryStoreRemoveReportsFinalClaim: Remove reports whether it dropped
// the FINAL claim, so the node-loss sweep only fires when a node is truly gone.
func TestDiscoveryStoreRemove_ReportsFinalClaim(t *testing.T) {
	const uuid = "shared-uuid"
	const name = "host"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
	}, sourceScanner)
	s.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
	}, sourceManual)

	require.False(t, s.Remove(uuid, sourceScanner), "removing one of two claims must report false (node still present)")
	require.True(t, s.Remove(uuid, sourceManual), "removing the surviving claim must report true (node now gone)")
	require.False(t, s.Remove(uuid, sourceScanner), "removing from an absent record must report false")
}

// nodeRemovedFrame builds the params of a discovery:node-removed for a node with
// the given display name and stable host UUID.
func nodeRemovedFrame(t *testing.T, name, uuid string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(noderec.NodeEvent{
		Node: noderec.DirectoryNode{
			IdentityVerification: noderec.IdentityPinned,
			Name:                 name,
			HostUUID:             uuid,
		},
	})
	require.NoError(t, err, "marshal node event")
	return b
}

// TestHandleNotifyNodeLostOnlyOnFinalClaim: a dual-source node (mDNS + manual)
// whose scanner claim drops is still present via the manual claim, so the
// node-loss sweep must NOT fire — otherwise a still-reachable node's jobs get
// wrongly failed.
func TestHandleNotify_ReportsNodeLostOnlyAfterFinalClaimIsRemoved(t *testing.T) {
	const uuid = "peer-uuid"
	const name = "peer-friendly-name"
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
	}, sourceScanner)
	store.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
	}, sourceManual)

	var calls int
	sp := &scannerProcess{
		store:      store,
		onNodeLost: func(string, string) { calls++ },
	}

	sp.handleNotify(noderec.NotifyNodeRemoved, nodeRemovedFrame(t, name, uuid))

	require.Equal(t, 0, calls, "onNodeLost fired")
	require.Len(t, store.Snapshot(), 1, "dual-source node should remain after one source leaves")
}

// TestHandleNotifyNodeLostUsesHostUUID: removing a node's final claim fires the
// sweep with the node's stable HostUUID (what workloads are keyed by), not its
// display hostname — the name != uuid case is exactly the bug.
func TestHandleNotifyNodeLost_UsesHostUUID(t *testing.T) {
	const uuid = "peer-uuid"
	const name = "peer-friendly-name"
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		ID:                   name,
		HostUUID:             uuid,
	}, sourceScanner)

	var gotUUID, gotName string
	var calls int
	sp := &scannerProcess{
		store: store,
		onNodeLost: func(u, n string) {
			calls++
			gotUUID = u
			gotName = n
		},
	}

	sp.handleNotify(noderec.NotifyNodeRemoved, nodeRemovedFrame(t, name, uuid))

	require.Equal(t, 1, calls, "onNodeLost calls")
	assert.Equal(t, uuid, gotUUID, "onNodeLost must use HostUUID, not the hostname")
	assert.Equal(t, name, gotName, "onNodeLost name")
}

func TestScannerProcess_RoutesAndRemovesTelemetry(t *testing.T) {
	const hostUUID = "peer-uuid"
	want := noderec.NodeTelemetry{
		HostUUID:          hostUUID,
		GPUUtilizationPct: 84,
		TelemetryValid:    true,
		MSSince:           137,
	}
	params, err := json.Marshal(want)
	require.NoError(t, err, "marshal telemetry")

	var got noderec.NodeTelemetry
	var removed string
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{
		HostUUID:             hostUUID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
	}, sourceScanner)
	sp := &scannerProcess{
		store:              store,
		onTelemetry:        func(value noderec.NodeTelemetry) { got = value },
		onTelemetryRemoved: func(hostUUID string) { removed = hostUUID },
	}
	sp.handleNotify(noderec.NotifyNodeTelemetry, params)
	require.Equal(t, want.HostUUID, got.HostUUID, "routed telemetry (%v, %v)", got, want)
	require.True(t, got.TelemetryValid, "routed telemetry (%v, %v)", got, want)
	require.GreaterOrEqual(t, got.MSSince, want.MSSince, "routed telemetry age includes broker processing")
	require.Equal(t, uint32(84), got.GPUUtilizationPct, "routed utilization")

	sp.handleNotify(noderec.NotifyNodeRemoved, nodeRemovedFrame(t, "peer", hostUUID))
	require.Equal(t, hostUUID, removed, "removed telemetry host")
}

func TestDiscoveryStore_SnapshotPreservesScannerIdentityFields(t *testing.T) {
	const body = `{"hostUuid":"host-a","name":"workstation","discoveryRecordId":"observation:candidate-a","identityVerification":"unverified"}`
	var node noderec.DirectoryNode
	require.NoError(t, json.Unmarshal([]byte(body), &node))
	store := newDiscoveryStore()
	store.Upsert(directoryToEnriched(node), sourceScanner)
	snapshot := store.Snapshot()
	require.Len(t, snapshot, 1)
	wire, err := json.Marshal(snapshot[0])
	require.NoError(t, err)
	type nodeIdentityFields struct {
		DiscoveryRecordID    string `json:"discoveryRecordId"`
		IdentityVerification string `json:"identityVerification"`
	}
	var got nodeIdentityFields
	require.NoError(t, json.Unmarshal(wire, &got))
	assert.Equal(t, "observation:candidate-a", got.DiscoveryRecordID)
	assert.Equal(t, "unverified", got.IdentityVerification)
}
