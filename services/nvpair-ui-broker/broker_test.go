// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
	"nvpair-ui-broker/workloadstore"
)

func TestLocalNodeInfo_PreservesAgeAcrossDiscoveryLockWait(t *testing.T) {
	broker := &Broker{nodeID: "self"}
	handler := broker.nodeInfoNotificationHandler(0)
	wait := holdDiscoveryNotification(t, &broker.discoveryMu, func() {
		handler(noderec.NotifyInfoUpdated, json.RawMessage(`{"info":{"hostUuid":"self","telemetryValid":true,"msSince":137}}`))
	})

	require.NotNil(t, broker.localInfo)
	info := broker.localInfo.info.Aged(time.Since(broker.localInfo.receivedAt))
	assert.GreaterOrEqual(t, info.MSSince, int64(137)+wait.Milliseconds())
}

func TestManualTelemetry_IncludesDiscoveryLockWaitInSampleAge(t *testing.T) {
	broker := newManualTestBroker()
	broker.clusterDir = t.TempDir()
	clustertrusttest.Join(t, broker.clusterDir, "cluster", "self", "peer")
	mesh := clustertrust.Open(broker.clusterDir)
	params, err := json.Marshal(manualNodeStatus{
		ID: "configured", Address: "192.0.2.20", HostUUID: "peer",
		DiscoveryRecordID: mesh.PeerRecordID("peer"), IdentityVerification: noderec.IdentityPinned,
		TelemetryValid: true, MSSince: 137,
	})
	require.NoError(t, err)
	handler := broker.manualNotificationHandler(0)
	wait := holdDiscoveryNotification(t, &broker.discoveryMu, func() {
		handler("node/updated", params)
	})

	samples := broker.telemetry.Snapshot(time.Now())
	require.Len(t, samples, 1)
	assert.GreaterOrEqual(t, samples[0].MSSince, int64(137)+wait.Milliseconds())
}

// Hold a received notification behind discovery processing, as happens while a
// manual update waits for proxy replies. This wait must contribute to sample age.
func holdDiscoveryNotification(t *testing.T, mu *sync.Mutex, notify func()) time.Duration {
	t.Helper()
	const minimumWait = 50 * time.Millisecond
	mu.Lock()
	entered, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(entered)
		notify()
		close(done)
	}()
	<-entered
	// Allow scheduling time between the start signal and the handler's receipt
	// timestamp while still requiring a substantial, observable lock wait.
	time.Sleep(2 * minimumWait)
	mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "notification did not finish after discovery processing resumed")
	}
	return minimumWait
}

func TestFanDiscoveryToScheduler_ExcludesPeersWhosePinWasRemoved(t *testing.T) {
	b := newManualTestBroker()
	b.clusterDir = t.TempDir()
	clustertrusttest.Join(t, b.clusterDir, "cluster", "self", "peer")
	mesh := clustertrust.Open(b.clusterDir)
	b.store.Upsert(EnrichedNode{HostUUID: "peer", DiscoveryRecordID: mesh.PeerRecordID("peer"), IdentityVerification: noderec.IdentityPinned, Trusted: true}, sourceScanner)
	var output bytes.Buffer
	b.setScheduler(&rpcWorker{peer: NewPeer(NewCodec(&output))})
	clustertrusttest.RemovePeerPin(t, b.clusterDir, "peer")

	b.fanDiscoveryToScheduler()

	message, err := NewCodec(&output).Read()
	require.NoError(t, err)
	var nodes []AvailableNode
	require.NoError(t, json.Unmarshal(message.Params, &nodes))
	assert.Empty(t, nodes)
}

// Give the broker a verified peer, an unverified record claiming that peer's
// UUID, and an explicitly configured manual target. Read the scheduler's actual
// notification and check that it contains only the peer and manual target.
// A manual configuration authorizes routing without authorizing hardware data.
func TestFanDiscoveryToScheduler_ExcludesUnverifiedDiscoveries(t *testing.T) {
	const peerUUID = "peer"
	const verifiedName = "verified"
	const manualID = "configured"
	brokerSide, schedulerSide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, brokerSide.Close())
		assert.NoError(t, schedulerSide.Close())
	})
	broker := newManualTestBroker()
	broker.setScheduler(&rpcWorker{peer: NewPeer(NewCodec(brokerSide))})
	broker.store.Upsert(EnrichedNode{
		HostUUID:             peerUUID,
		ID:                   verifiedName,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
	}, sourceScanner)
	broker.store.Upsert(EnrichedNode{
		HostUUID:             peerUUID,
		ID:                   "rogue",
		DiscoveryRecordID:    "candidate:rogue",
		IdentityVerification: noderec.IdentityUnverified,
	}, sourceScanner)
	broker.store.Upsert(manualToEnriched(manualNodeStatus{
		ID:      manualID,
		Address: "192.0.2.30",
	}), sourceManual)
	// net.Pipe writes wait for a reader, so fanout must run alongside the read
	// below. The completion channel lets us join the writer before pipe cleanup.
	done := make(chan struct{})
	go func() {
		broker.fanDiscoveryToScheduler()
		close(done)
	}()
	require.NoError(t, schedulerSide.SetReadDeadline(time.Now().Add(2*time.Second)))

	message, err := NewCodec(schedulerSide).Read()
	require.NoError(t, err)
	var nodes []AvailableNode
	require.NoError(t, json.Unmarshal(message.Params, &nodes))
	require.Len(t, nodes, 2)
	assert.Equal(t, manualID, nodes[0].ID)
	assert.Equal(t, "manual:configured", nodes[0].HostUUID)
	assert.Equal(t, verifiedName, nodes[1].ID)
	<-done
}

// Both discovery sources can own one peer. Removing its pin must discard both
// verified records and cached measurements, while preserving independent discovery claims and
// the user's explicit manual engine configuration under its own key.
func TestClusterTrustChange_DiscardsVerifiedRecordsAndMeasurements(t *testing.T) {
	broker := newManualTestBroker()
	broker.clusterDir = t.TempDir()
	clustertrusttest.Join(t, broker.clusterDir, "cluster", "self", "peer")
	broker.relayDir = relay.NewDirectory()
	mesh := clustertrust.Open(broker.clusterDir)
	peer := noderec.DirectoryNode{
		HostUUID: "peer", ClusterUUID: "peer",
		DiscoveryRecordID:    mesh.PeerRecordID("peer"),
		IdentityVerification: noderec.IdentityPinned, Trusted: true,
	}
	broker.store.Upsert(directoryToEnriched(peer), sourceScanner)
	broker.relayDir.Apply(noderec.NotifyNodeDiscovered, peer)
	broker.upsertManualNode(manualNodeStatus{
		ID: "configured", HostUUID: "peer", DiscoveryRecordID: peer.DiscoveryRecordID,
		IdentityVerification: noderec.IdentityPinned, Address: "192.0.2.20",
		OllamaUp: true, OllamaPort: 11434, TelemetryValid: true,
	})
	broker.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: "peer", TelemetryValid: true})
	candidate := noderec.DirectoryNode{HostUUID: "peer", DiscoveryRecordID: "candidate", IdentityVerification: noderec.IdentityUnverified}
	broker.store.Upsert(directoryToEnriched(candidate), sourceScanner)
	broker.relayDir.Apply(noderec.NotifyNodeDiscovered, candidate)
	principal := "peer"
	value := noderec.NodeInfoEvent{
		HostUUID: "peer", ClusterUUID: "peer", DiscoveryRecordID: peer.DiscoveryRecordID,
		IdentityVerification: noderec.IdentityPinned,
		Info:                 noderec.NodeInfo{HostUUID: "peer", ClusterUUID: &principal},
	}
	require.True(t, broker.store.AcceptInfo(value))

	clustertrusttest.RemovePeerPin(t, broker.clusterDir, "peer")
	broker.applyClusterTrustChange()

	assert.Empty(t, broker.telemetry.Snapshot(time.Now()))
	assert.False(t, broker.store.verifiedSource("peer", sourceScanner))
	assert.False(t, broker.store.verifiedSource("peer", sourceManual))
	assert.Empty(t, broker.manualAliasForKey("peer"))
	assert.Equal(t, "configured", broker.manualAliasForKey("manual:configured"))
	assert.Equal(t, []noderec.DirectoryNode{candidate}, broker.relayDir.Snapshot(""))
	assert.False(t, broker.store.AcceptInfo(value))
}

func TestClusterTrustChange_PreservesProofsForUnchangedPins(t *testing.T) {
	b := newManualTestBroker()
	b.clusterDir = t.TempDir()
	clustertrusttest.Join(t, b.clusterDir, "cluster", "self", "peer")
	mesh := clustertrust.Open(b.clusterDir)
	peer := EnrichedNode{HostUUID: "peer", DiscoveryRecordID: mesh.PeerRecordID("peer"), IdentityVerification: noderec.IdentityPinned, Trusted: true}
	b.store.Upsert(peer, sourceScanner)
	b.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: "peer", TelemetryValid: true, MSSince: 137})

	clustertrusttest.WritePeerPin(t, b.clusterDir, "new-peer")
	b.applyClusterTrustChange()

	node, present := b.store.sourceNode("peer", sourceScanner)
	assert.True(t, present)
	assert.Equal(t, peer, node)
	samples := b.telemetry.Snapshot(time.Now())
	require.Len(t, samples, 1)
	assert.True(t, samples[0].TelemetryValid)
	assert.GreaterOrEqual(t, samples[0].MSSince, int64(137))
}

func TestClusterTrustChange_FailsWorkloadsForRemovedPeer(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const workloadID = "running-on-peer"

	test := func(name string, sources ...nodeSource) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			broker := newManualTestBroker()
			broker.nodeID = selfUUID
			broker.clusterDir = t.TempDir()
			broker.workloads = workloadstore.New()
			clustertrusttest.Join(t, broker.clusterDir, "cluster", selfUUID, peerUUID)
			mesh := clustertrust.Open(broker.clusterDir)
			for _, source := range sources {
				switch source {
				case sourceScanner:
					broker.store.Upsert(EnrichedNode{
						HostUUID: peerUUID, DiscoveryRecordID: mesh.PeerRecordID(peerUUID),
						IdentityVerification: noderec.IdentityPinned, Trusted: true,
					}, sourceScanner)
				case sourceManual:
					broker.upsertManualNode(manualNodeStatus{
						ID: "configured", Address: "192.0.2.20", HostUUID: peerUUID,
						DiscoveryRecordID: mesh.PeerRecordID(peerUUID), IdentityVerification: noderec.IdentityPinned,
					})
				}
				require.True(t, broker.store.verifiedSource(peerUUID, source))
			}
			require.True(t, broker.workloads.Apply(storeIncoming(t, workloadID, selfUUID, "ollama", "run", "running", peerUUID)))

			// Removing trust must retire the peer's jobs after every source has
			// withdrawn its record, regardless of which source is removed last.
			clustertrusttest.RemovePeerPin(t, broker.clusterDir, peerUUID)
			broker.applyClusterTrustChange()

			assert.False(t, broker.store.verifiedSource(peerUUID, sourceScanner))
			assert.False(t, broker.store.verifiedSource(peerUUID, sourceManual))
			workload, present := broker.workloads.Get(selfUUID, workloadID)
			require.True(t, present)
			assert.Equal(t, "failed", workload.State)
			assert.True(t, workload.Inferred)
		})
	}

	test("scanner only", sourceScanner)
	test("manual only", sourceManual)
	test("scanner and manual", sourceScanner, sourceManual)
}

func TestManualNodes_FailsWorkloadsWhenTrustWithdrawalPrecedesNotification(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const manualID = "configured"
	const manualAddress = "192.0.2.20"
	const workloadID = "running-on-peer"

	test := func(name string, verification noderec.IdentityVerification) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			broker := newManualTestBroker()
			broker.nodeID = selfUUID
			broker.clusterDir = t.TempDir()
			broker.workloads = workloadstore.New()
			clustertrusttest.Join(t, broker.clusterDir, "cluster", selfUUID, peerUUID)
			mesh := clustertrust.Open(broker.clusterDir)
			status := manualNodeStatus{
				ID: manualID, Address: manualAddress, HostUUID: peerUUID,
				DiscoveryRecordID: mesh.PeerRecordID(peerUUID), IdentityVerification: noderec.IdentityPinned,
			}
			broker.upsertManualNode(status)
			require.True(t, broker.store.verifiedSource(peerUUID, sourceManual))
			require.True(t, broker.workloads.Apply(storeIncoming(t, workloadID, selfUUID, "ollama", "run", "running", peerUUID)))

			update := manualNodeStatus{ID: manualID, Address: manualAddress, IdentityVerification: verification}
			if verification == noderec.IdentityPinned {
				// A queued update still carries the authorization that just expired.
				update.HostUUID = peerUUID
				update.DiscoveryRecordID = status.DiscoveryRecordID
			}
			params, err := json.Marshal(update)
			require.NoError(t, err)

			// Different workers use independent streams. The manual update may
			// erase the peer's last record before the trust notification arrives.
			clustertrusttest.RemovePeerPin(t, broker.clusterDir, peerUUID)
			broker.manualNotificationHandler(broker.manualGeneration)("node/updated", params)
			require.False(t, broker.store.verifiedSource(peerUUID, sourceManual))
			broker.applyClusterTrustChange()

			workload, present := broker.workloads.Get(selfUUID, workloadID)
			require.True(t, present)
			assert.Equal(t, "failed", workload.State)
			assert.True(t, workload.Inferred)
		})
	}

	test("worker reports an unverified withdrawal", noderec.IdentityUnverified)
	test("broker downgrades a queued verified update", noderec.IdentityPinned)
}

func TestManualNodes_PreservesWorkloadsAfterUnverifiedUpdateWithCurrentPin(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const manualID = "configured"
	const manualAddress = "192.0.2.20"
	const workloadID = "running-on-peer"
	const runningState = "running"

	broker := newManualTestBroker()
	broker.nodeID = selfUUID
	broker.clusterDir = t.TempDir()
	broker.workloads = workloadstore.New()
	clustertrusttest.Join(t, broker.clusterDir, "cluster", selfUUID, peerUUID)
	mesh := clustertrust.Open(broker.clusterDir)
	broker.upsertManualNode(manualNodeStatus{
		ID: manualID, Address: manualAddress, HostUUID: peerUUID,
		DiscoveryRecordID: mesh.PeerRecordID(peerUUID), IdentityVerification: noderec.IdentityPinned,
	})
	require.True(t, broker.store.verifiedSource(peerUUID, sourceManual))
	require.True(t, broker.workloads.Apply(storeIncoming(t, workloadID, selfUUID, "ollama", "run", runningState, peerUUID)))
	params, err := json.Marshal(manualNodeStatus{
		ID: manualID, Address: manualAddress, IdentityVerification: noderec.IdentityUnverified,
	})
	require.NoError(t, err)

	// An unsuccessful hardware probe does not revoke the peer's authorization
	// or establish that an inference request running on that peer has failed.
	broker.manualNotificationHandler(broker.manualGeneration)("node/updated", params)
	broker.applyClusterTrustChange()

	assert.False(t, broker.store.verifiedSource(peerUUID, sourceManual))
	workload, present := broker.workloads.Get(selfUUID, workloadID)
	require.True(t, present)
	assert.Equal(t, runningState, workload.State)
	assert.False(t, workload.Inferred)
}

func TestClusterTrustChange_FailsWorkloadsWhenWorkerCleanupPrecedesNotification(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const workloadID = "running-on-peer"

	test := func(name string, source nodeSource, cleanup func(*Broker)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			broker := newManualTestBroker()
			broker.nodeID = selfUUID
			broker.clusterDir = t.TempDir()
			broker.workloads = workloadstore.New()
			clustertrusttest.Join(t, broker.clusterDir, "cluster", selfUUID, peerUUID)
			mesh := clustertrust.Open(broker.clusterDir)
			switch source {
			case sourceScanner:
				broker.store.Upsert(EnrichedNode{
					HostUUID: peerUUID, DiscoveryRecordID: mesh.PeerRecordID(peerUUID),
					IdentityVerification: noderec.IdentityPinned, Trusted: true,
				}, sourceScanner)
			case sourceManual:
				broker.upsertManualNode(manualNodeStatus{
					ID: "configured", Address: "192.0.2.20", HostUUID: peerUUID,
					DiscoveryRecordID: mesh.PeerRecordID(peerUUID), IdentityVerification: noderec.IdentityPinned,
				})
			}
			require.True(t, broker.store.verifiedSource(peerUUID, source))
			require.True(t, broker.workloads.Apply(storeIncoming(t, workloadID, selfUUID, "ollama", "run", "running", peerUUID)))

			// The crash discards the directory record before the broker learns
			// why the peer's authorization changed. Trust loss still retires jobs.
			clustertrusttest.RemovePeerPin(t, broker.clusterDir, peerUUID)
			cleanup(broker)
			broker.applyClusterTrustChange()

			assert.False(t, broker.store.verifiedSource(peerUUID, source))
			workload, present := broker.workloads.Get(selfUUID, workloadID)
			require.True(t, present)
			assert.Equal(t, "failed", workload.State)
			assert.True(t, workload.Inferred)
		})
	}

	test("scanner crashes after pin removal", sourceScanner, (*Broker).clearScannerState)
	test("manual worker crashes after pin removal", sourceManual, (*Broker).clearManualNodesState)
}

func TestClusterTrustChange_PreservesWorkloadsWhileAnotherSourceIsCurrent(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const clusterID = "cluster"
	const workloadID = "running-on-peer"
	const runningState = "running"

	broker := newManualTestBroker()
	broker.nodeID = selfUUID
	broker.clusterDir = t.TempDir()
	broker.workloads = workloadstore.New()
	clustertrusttest.Join(t, broker.clusterDir, clusterID, selfUUID, peerUUID)
	mesh := clustertrust.Open(broker.clusterDir)
	broker.store.Upsert(EnrichedNode{
		HostUUID: peerUUID, DiscoveryRecordID: mesh.PeerRecordID(peerUUID),
		IdentityVerification: noderec.IdentityPinned, Trusted: true,
	}, sourceScanner)
	require.True(t, broker.workloads.Apply(storeIncoming(t, workloadID, selfUUID, "ollama", "run", runningState, peerUUID)))

	// The manual worker authenticated again before the broker processed the
	// trust change. Its current record survives the scanner's obsolete one.
	clustertrusttest.WriteAdmission(t, broker.clusterDir, clusterID, 2)
	mesh.Refresh()
	broker.upsertManualNode(manualNodeStatus{
		ID: "configured", Address: "192.0.2.20", HostUUID: peerUUID,
		DiscoveryRecordID: mesh.PeerRecordID(peerUUID), IdentityVerification: noderec.IdentityPinned,
	})

	broker.applyClusterTrustChange()

	assert.False(t, broker.store.verifiedSource(peerUUID, sourceScanner))
	assert.True(t, broker.store.verifiedSource(peerUUID, sourceManual))
	workload, present := broker.workloads.Get(selfUUID, workloadID)
	require.True(t, present)
	assert.Equal(t, runningState, workload.State)
	assert.False(t, workload.Inferred)
}

// A crashed scanner must lose its directory records and measurements, and late
// notifications from that process must not restore them. Independently owned
// manual entries, cached local inventory, and running workloads must survive.
func TestScannerRestart_WithdrawsRecordsAndRejectsOldWorkerEvents(t *testing.T) {
	const selfUUID = "self"
	const peerUUID = "peer"
	const workloadID = "running-on-peer"
	const runningState = "running"

	b := newManualTestBroker()
	b.nodeID = selfUUID
	b.clusterDir = t.TempDir()
	b.workloads = workloadstore.New()
	clustertrusttest.Join(t, b.clusterDir, "cluster", selfUUID, peerUUID)
	mesh := clustertrust.Open(b.clusterDir)
	b.relayDir = relay.NewDirectory()
	scanner := &scannerProcess{store: b.store, relayDir: b.relayDir, stateMu: &b.discoveryMu}
	b.setScanner(scanner)

	// Give the scanner a verified peer record and a cached GPU usage sample.
	peer := noderec.DirectoryNode{HostUUID: peerUUID, DiscoveryRecordID: mesh.PeerRecordID(peerUUID), IdentityVerification: noderec.IdentityPinned, Trusted: true}
	params, err := json.Marshal(noderec.NodeEvent{Node: peer})
	require.NoError(t, err)
	scanner.handleNotify(noderec.NotifyNodeDiscovered, params)
	b.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: peerUUID, TelemetryValid: true})
	// The peer keeps executing this job even when our local scanner crashes.
	require.True(t, b.workloads.Apply(storeIncoming(t, workloadID, selfUUID, "ollama", "run", runningState, peerUUID)))

	// These belong to other workers. Keep an older local receipt time so the
	// final assertion also checks that scanner cleanup does not make it look new.
	b.upsertManualNode(manualNodeStatus{ID: "manual", Address: "192.0.2.30"})
	owned := localInfoObservation{info: noderec.NodeInfo{HostUUID: selfUUID, TelemetryValid: true}, receivedAt: time.Now().Add(-time.Minute)}
	b.localInfo = &owned

	// Run the cleanup used after a scanner crash, then simulate an update that
	// arrives late from the retired process and tries to restore its peer record.
	b.clearScannerState()
	scanner.handleNotify(noderec.NotifyNodeUpdated, params)

	// The old scanner cannot restore its records or scheduling measurements.
	assert.Nil(t, b.getScanner())
	assert.Empty(t, b.telemetry.Snapshot(time.Now()))
	assert.Empty(t, b.relayDir.Snapshot(""))

	// Manual configuration and the original local inventory observation survive.
	snapshot := b.store.Snapshot()
	require.Len(t, snapshot, 1)
	assert.Equal(t, "manual:manual", snapshot[0].HostUUID)
	assert.Equal(t, owned, *b.localInfo)
	workload, present := b.workloads.Get(selfUUID, workloadID)
	require.True(t, present)
	assert.Equal(t, runningState, workload.State)
	assert.False(t, workload.Inferred)
}

func TestManualWorkerRestart_RejectsNotificationsFromPreviousWorker(t *testing.T) {
	b := newManualTestBroker()
	oldWorker := b.manualNotificationHandler(b.manualGeneration)
	status := manualNodeStatus{ID: "manual", Address: "192.0.2.20", OllamaUp: true, OllamaPort: 11434}
	params, err := json.Marshal(status)
	require.NoError(t, err)
	oldWorker("node/updated", params)
	require.Len(t, b.store.Snapshot(), 1)

	b.clearManualNodesState()
	oldWorker("node/updated", params)
	assert.Empty(t, b.store.Snapshot())
	assert.Empty(t, b.manualNodeStatuses)

	b.manualNotificationHandler(b.manualGeneration)("node/updated", params)
	assert.Len(t, b.store.Snapshot(), 1)
}

func TestManualNodes_RejectsQueuedProofAfterPinReplacement(t *testing.T) {
	b := newManualTestBroker()
	b.clusterDir = t.TempDir()
	clustertrusttest.Join(t, b.clusterDir, "cluster", "self", "peer")
	mesh := clustertrust.Open(b.clusterDir)
	status := manualNodeStatus{
		ID: "configured", HostUUID: "peer", Address: "192.0.2.20",
		DiscoveryRecordID: mesh.PeerRecordID("peer"), IdentityVerification: noderec.IdentityPinned,
		OllamaUp: true, OllamaPort: 11434, TelemetryValid: true,
	}
	notify := b.manualNotificationHandler(b.manualGeneration)
	send := func() {
		t.Helper()
		params, err := json.Marshal(status)
		require.NoError(t, err)
		notify("node/updated", params)
	}
	send()
	require.True(t, b.store.verifiedSource("peer", sourceManual))
	clustertrusttest.WritePeerPin(t, b.clusterDir, "peer")

	send()

	assert.False(t, b.store.verifiedSource("peer", sourceManual))
	assert.Empty(t, b.telemetry.Snapshot(time.Now()))
	assert.Equal(t, "configured", b.manualAliasForKey("manual:configured"))
	mesh.Refresh()
	status.DiscoveryRecordID = mesh.PeerRecordID("peer")
	send()
	assert.True(t, b.store.verifiedSource("peer", sourceManual))
	assert.Len(t, b.telemetry.Snapshot(time.Now()), 1)
}

// A directory update becomes visible before its new GPU sample is cached.
// Replay can run in that interval, so a replacement record must first discard
// measurements belonging to the previous authorization.
func TestManualNodes_RecordReplacementDiscardsTelemetryBeforeDiscovery(t *testing.T) {
	const clusterID = "cluster"
	const selfUUID = "self"
	const peerUUID = "peer"
	const oldUtilization = uint32(80)
	const newUtilization = uint32(10)

	test := func(name string, admissionEpoch uint64, wantReplay []uint32) {
		t.Run(name, func(t *testing.T) {
			broker := newManualTestBroker()
			broker.clusterDir = t.TempDir()
			clustertrusttest.Join(t, broker.clusterDir, clusterID, selfUUID, peerUUID)
			mesh := clustertrust.Open(broker.clusterDir)
			status := manualNodeStatus{
				ID: "configured", HostUUID: peerUUID, Address: "192.0.2.20",
				DiscoveryRecordID: mesh.PeerRecordID(peerUUID), IdentityVerification: noderec.IdentityPinned,
				TelemetryValid: true, GPUs: []GPUInfo{{UtilizationPercent: oldUtilization}},
			}
			broker.upsertManualNode(status)

			// A new admission changes the record ID without changing the host UUID.
			// Rewriting the same epoch leaves authorization unchanged in the control case.
			clustertrusttest.WriteAdmission(t, broker.clusterDir, clusterID, admissionEpoch)
			mesh.Refresh()
			status.DiscoveryRecordID = mesh.PeerRecordID(peerUUID)
			status.GPUs = []GPUInfo{{UtilizationPercent: newUtilization}}
			params, err := json.Marshal(status)
			require.NoError(t, err)

			// Replay at the directory-change hook reproduces the interval before
			// the update reaches telemetry ingestion, without timing-dependent goroutines.
			var output bytes.Buffer
			scheduler := &rpcWorker{peer: NewPeer(NewCodec(&output))}
			var replayed int
			broker.store.SetOnChange(func() {
				broker.schedulerFeedMu.Lock()
				defer broker.schedulerFeedMu.Unlock()
				replayed = broker.replayTelemetryToScheduler(scheduler)
			})

			broker.manualNotificationHandler(broker.manualGeneration)("node/updated", params)

			require.Equal(t, len(wantReplay), replayed)
			codec := NewCodec(&output)
			for _, utilization := range wantReplay {
				message, err := codec.Read()
				require.NoError(t, err)
				var sample noderec.NodeTelemetry
				require.NoError(t, json.Unmarshal(message.Params, &sample))
				assert.Equal(t, peerUUID, sample.HostUUID)
				assert.True(t, sample.TelemetryValid)
				assert.Equal(t, utilization, sample.GPUUtilizationPct)
			}
			_, err = codec.Read()
			assert.ErrorIs(t, err, io.EOF)

			// The newly authenticated measurement remains available after the update.
			samples := broker.telemetry.Snapshot(time.Now())
			require.Len(t, samples, 1)
			assert.True(t, samples[0].TelemetryValid)
			assert.Equal(t, newUtilization, samples[0].GPUUtilizationPct)
		})
	}

	test("new admission discards the previous measurement", 2, nil)
	test("unchanged authorization retains the previous measurement", 1, []uint32{oldUtilization})
}
