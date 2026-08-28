// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

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
	if len(snap) != 1 {
		t.Fatalf("a rename must not duplicate the node: got %d entries, want 1", len(snap))
	}
	if snap[0].ID != "new-host" {
		t.Fatalf("store did not track the new name: got id=%q want %q", snap[0].ID, "new-host")
	}
	if snap[0].HostUUID != uuid {
		t.Fatalf("hostUuid not projected to the wire: got %q want %q", snap[0].HostUUID, uuid)
	}
}

// TestDiscoveryStoreDistinctUUIDsSameName verifies the same-hostname collision
// case: two different machines that share a hostname stay two entries.
func TestDiscoveryStoreDistinctUUIDsSameName(t *testing.T) {
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "samename", HostUUID: "aaaaaaaa-0000-0000-0000-000000000001"}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "samename", HostUUID: "bbbbbbbb-0000-0000-0000-000000000002"}, sourceScanner)
	if got := len(s.Snapshot()); got != 2 {
		t.Fatalf("two machines sharing a hostname must not merge: got %d entries, want 2", got)
	}
}

// TestManualToEnrichedHostUUID verifies the manual-node ingestion boundary: a
// manual node keys by the remote's real hostUuid once node-info reports one, and
// by its manual id until then.
func TestManualToEnrichedHostUUID(t *testing.T) {
	withUUID := manualToEnriched(manualNodeStatus{ID: "manual:10.0.0.9", HostUUID: "real-uuid"})
	if withUUID.storeKey() != "real-uuid" {
		t.Fatalf("store key = %q, want the real hostUuid", withUUID.storeKey())
	}
	noUUID := manualToEnriched(manualNodeStatus{ID: "manual:10.0.0.9"})
	if noUUID.storeKey() != "manual:10.0.0.9" {
		t.Fatalf("store key = %q, want the manual id fallback", noUUID.storeKey())
	}
}

// TestDiscoveryStoreRejectsEmptyKey verifies a node with no operational key is
// dropped rather than silently keyed by name.
func TestDiscoveryStoreRejectsEmptyKey(t *testing.T) {
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "no-uuid"}, sourceManual) // HostUUID empty
	if got := len(s.Snapshot()); got != 0 {
		t.Fatalf("a node with no hostUuid must be dropped, got %d entries", got)
	}
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
	if got := len(s.Snapshot()); got != 1 {
		t.Fatalf("shared uuid should be one record, got %d", got)
	}
	s.Remove(uuid, sourceManual)
	if got := len(s.Snapshot()); got != 1 {
		t.Fatalf("removing the manual claim evicted the live scanner node (got %d entries)", got)
	}
	s.Remove(uuid, sourceScanner)
	if got := len(s.Snapshot()); got != 0 {
		t.Fatalf("record should be gone once no source claims it, got %d", got)
	}

	// Symmetric: removing the scanner claim leaves the manual node.
	s = newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceManual)
	s.Remove(uuid, sourceScanner)
	if got := len(s.Snapshot()); got != 1 {
		t.Fatalf("removing the scanner claim evicted the manual node (got %d entries)", got)
	}
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
	if len(snap) != 1 {
		t.Fatalf("want one record, got %d", len(snap))
	}
	if snap[0].ID != "mdns-host" || !snap[0].Trusted {
		t.Fatalf("scanner projection should win when both claim: %+v", snap[0])
	}
	// Drop the scanner claim: the manual projection takes over.
	s.Remove(uuid, sourceScanner)
	snap = s.Snapshot()
	if len(snap) != 1 || snap[0].ID != "manual-host" {
		t.Fatalf("manual projection should surface after scanner removal: %+v", snap)
	}
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
			if got := len(b.store.Snapshot()); got != 1 {
				t.Fatalf("two aliases for one machine should be one record, got %d", got)
			}

			b.removeManualNode(tc.removeFirst)
			snap := b.store.Snapshot()
			if len(snap) != 1 {
				t.Fatalf("removing one of two aliases evicted the shared node (got %d)", len(snap))
			}
			// The surviving alias must be reprojected: its id and address, not
			// the removed alias's stale payload.
			if snap[0].ID != tc.survivorID || snap[0].IPAddress != tc.survivorAddr {
				t.Fatalf("survivor not reprojected: got id=%q ip=%q, want id=%q ip=%q",
					snap[0].ID, snap[0].IPAddress, tc.survivorID, tc.survivorAddr)
			}

			b.removeManualNode(tc.survivorID)
			if got := len(b.store.Snapshot()); got != 0 {
				t.Fatalf("record should be gone once the last alias left, got %d", got)
			}
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
	if len(snap) != 2 {
		t.Fatalf("want two records (shared survivor + rekeyed alias), got %d", len(snap))
	}
	// The shared key must now project alias-b (the surviving owner).
	var sharedNode *AvailableNode
	for i := range snap {
		if snap[i].HostUUID == shared {
			sharedNode = &snap[i]
		}
	}
	if sharedNode == nil {
		t.Fatalf("shared key %q dropped after a co-owner rekeyed away", shared)
	}
	if sharedNode.ID != "alias-b" || sharedNode.IPAddress != "10.0.0.2" {
		t.Fatalf("shared key not reprojected from the surviving alias: %+v", *sharedNode)
	}
}

// TestDiscoveryStoreRemoveWrongSourceNoop: removing a source that never claimed
// the record leaves it (and the other source) intact.
func TestDiscoveryStoreRemoveWrongSourceNoop(t *testing.T) {
	const uuid = "u"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceScanner)
	if s.Remove(uuid, sourceManual) { // manual never claimed it
		t.Fatal("removing an unowned source must report false (nothing removed)")
	}
	if got := len(s.Snapshot()); got != 1 {
		t.Fatalf("removing an unowned source must not drop the record, got %d", got)
	}
}

// TestDiscoveryStoreRemoveReportsFinalClaim: Remove reports whether it dropped
// the FINAL claim, so the node-loss sweep only fires when a node is truly gone.
func TestDiscoveryStoreRemoveReportsFinalClaim(t *testing.T) {
	const uuid = "shared-uuid"
	s := newDiscoveryStore()
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceScanner)
	s.Upsert(EnrichedNode{ID: "host", HostUUID: uuid}, sourceManual)

	if s.Remove(uuid, sourceScanner) {
		t.Fatal("removing one of two claims must report false (node still present)")
	}
	if s.Remove(uuid, sourceManual) == false {
		t.Fatal("removing the surviving claim must report true (node now gone)")
	}
	if s.Remove(uuid, sourceScanner) {
		t.Fatal("removing from an absent record must report false")
	}
}

// nodeRemovedFrame builds the params of a discovery:node-removed for a node with
// the given display name and stable host UUID.
func nodeRemovedFrame(t *testing.T, name, uuid string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(noderec.NodeEvent{Node: noderec.DirectoryNode{Name: name, HostUUID: uuid}})
	if err != nil {
		t.Fatalf("marshal node event: %v", err)
	}
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

	if calls != 0 {
		t.Fatalf("onNodeLost fired %d time(s) for a node still held by a manual claim, want 0", calls)
	}
	if got := len(store.Snapshot()); got != 1 {
		t.Fatalf("dual-source node should remain after one source leaves, got %d entries", got)
	}
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

	if calls != 1 {
		t.Fatalf("onNodeLost calls = %d, want 1 (final claim removed)", calls)
	}
	if gotUUID != uuid {
		t.Errorf("onNodeLost uuid = %q, want %q (must be HostUUID, not the hostname)", gotUUID, uuid)
	}
	if gotName != name {
		t.Errorf("onNodeLost name = %q, want %q", gotName, name)
	}
}

func TestScannerProcessRoutesAndRemovesTelemetry(t *testing.T) {
	want := noderec.NodeTelemetry{
		HostUUID:          "peer-uuid",
		GPUUtilizationPct: 84,
		TelemetryValid:    true,
		MSSince:           137,
	}
	params, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal telemetry: %v", err)
	}

	var got noderec.NodeTelemetry
	var removed string
	sp := &scannerProcess{
		onTelemetry:        func(value noderec.NodeTelemetry) { got = value },
		onTelemetryRemoved: func(hostUUID string) { removed = hostUUID },
	}
	sp.handleNotify(noderec.NotifyNodeTelemetry, params)
	if got.HostUUID != want.HostUUID || !got.TelemetryValid || got.MSSince != want.MSSince {
		t.Fatalf("routed telemetry = %+v, want %+v", got, want)
	}
	if got.GPUUtilizationPct != 84 {
		t.Fatalf("routed utilization = %d, want 84", got.GPUUtilizationPct)
	}

	sp.handleNotify(noderec.NotifyNodeRemoved, nodeRemovedFrame(t, "peer", "peer-uuid"))
	if removed != "peer-uuid" {
		t.Fatalf("removed telemetry host = %q, want peer-uuid", removed)
	}
}
