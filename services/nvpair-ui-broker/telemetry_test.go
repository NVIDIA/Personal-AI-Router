// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"nvpair-shared/schedulerwire"
)

func TestTelemetryReplay_DiscardsSamplesWhosePinWasRemoved(t *testing.T) {
	b := newManualTestBroker()
	b.clusterDir = t.TempDir()
	clustertrusttest.Join(t, b.clusterDir, "cluster", "self", "peer")
	mesh := clustertrust.Open(b.clusterDir)
	b.store.Upsert(EnrichedNode{HostUUID: "peer", DiscoveryRecordID: mesh.PeerRecordID("peer"), IdentityVerification: noderec.IdentityPinned, Trusted: true}, sourceScanner)
	b.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: "peer", TelemetryValid: true})
	clustertrusttest.RemovePeerPin(t, b.clusterDir, "peer")
	var output bytes.Buffer
	scheduler := &rpcWorker{peer: NewPeer(NewCodec(&output))}

	b.schedulerFeedMu.Lock()
	replayed := b.replayTelemetryToScheduler(scheduler)
	b.schedulerFeedMu.Unlock()

	assert.Zero(t, replayed)
	assert.Empty(t, output.Bytes())
}

type delayedTelemetryWriter struct {
	bytes.Buffer
	delay  time.Duration
	writes int
}

func (w *delayedTelemetryWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		time.Sleep(w.delay)
	}
	return w.Buffer.Write(data)
}

func TestTelemetryReplay_IncludesTimeWaitingBehindEarlierSamples(t *testing.T) {
	const delay = 100 * time.Millisecond
	b := newManualTestBroker()
	now := time.Now()
	b.telemetry.Upsert(sourceScanner, noderec.NodeTelemetry{HostUUID: "first", TelemetryValid: true}, now, now)
	b.telemetry.Upsert(sourceScanner, noderec.NodeTelemetry{HostUUID: "second", TelemetryValid: true}, now, now)
	output := &delayedTelemetryWriter{delay: delay}
	scheduler := &rpcWorker{peer: NewPeer(NewCodec(output))}

	b.schedulerFeedMu.Lock()
	replayed := b.replayTelemetryToScheduler(scheduler)
	b.schedulerFeedMu.Unlock()

	require.Equal(t, 2, replayed)
	codec := NewCodec(output)
	_, err := codec.Read()
	require.NoError(t, err)
	message, err := codec.Read()
	require.NoError(t, err)
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.Equal(t, "second", sample.HostUUID)
	assert.GreaterOrEqual(t, sample.MSSince, delay.Milliseconds())
}

func TestTelemetryCache_IncludesElapsedTimeInObservationAge(t *testing.T) {
	cache := newTelemetryCache()
	receivedAt := time.Unix(1_700_000_000, 0).UTC() // arbitrary fixed instant
	input := noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 84,
		TelemetryValid:    true,
		MSSince:           137,
	}
	projected, ok := cache.Upsert(sourceScanner, input, receivedAt, receivedAt)
	require.True(t, ok, "initial projection (%v)", projected)
	require.Equal(t, int64(137), projected.MSSince, "initial projection (%v)", projected)
	snapshot := cache.Snapshot(receivedAt.Add(250 * time.Millisecond))
	require.Len(t, snapshot, 1, "snapshot length")
	require.Equal(t, int64(387), snapshot[0].MSSince, "aged msSince")
	require.Equal(t, uint32(84), snapshot[0].GPUUtilizationPct, "cached utilization")
}

// Cache a verified scanner sample, then add an unverified manual entry claiming
// the same host with a low GPU reading. Removing the scanner sample must leave
// no scheduling telemetry, while both entries remain visible in discovery.
func TestManualTelemetry_PublicClaimsCannotBecomeSchedulingFallback(t *testing.T) {
	const peerUUID = "peer"
	broker := newManualTestBroker()
	broker.store.Upsert(EnrichedNode{
		HostUUID:             peerUUID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
	}, sourceScanner)
	broker.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{
		HostUUID:          peerUUID,
		GPUUtilizationPct: 80,
		TelemetryValid:    true,
	})
	broker.upsertManualNode(manualNodeStatus{
		ID:                   "configured",
		HostUUID:             peerUUID,
		Address:              "192.0.2.30",
		IdentityVerification: noderec.IdentityUnverified,
		TelemetryValid:       true,
		GPUs:                 []GPUInfo{{UtilizationPercent: 1}},
	})

	broker.removeTelemetry(sourceScanner, peerUUID)

	require.Empty(t, broker.telemetry.Snapshot(time.Now()))
	require.Len(t, broker.store.Snapshot(), 2)
}

func TestTelemetryCache_PrefersScannerAndFallsBackToManual(t *testing.T) {
	const hostUUID = "node-a"
	cache := newTelemetryCache()
	now := time.Unix(1_700_000_000, 0).UTC() // arbitrary fixed instant
	scanner := noderec.NodeTelemetry{
		HostUUID:          hostUUID,
		GPUUtilizationPct: 20,
		TelemetryValid:    true,
	}
	manual := noderec.NodeTelemetry{
		HostUUID:          hostUUID,
		GPUUtilizationPct: 70,
		TelemetryValid:    true,
	}

	cache.Upsert(sourceScanner, scanner, now, now)
	projected, ok := cache.Upsert(sourceManual, manual, now.Add(time.Second), now.Add(time.Second))
	require.True(t, ok, "scanner projection (%v)", projected)
	require.Equal(t, uint32(20), projected.GPUUtilizationPct, "scanner projection (%v)", projected)

	projected, ok = cache.Remove(hostUUID, sourceScanner, now.Add(2*time.Second))
	require.True(t, ok, "manual fallback (%v)", projected)
	require.Equal(t, uint32(70), projected.GPUUtilizationPct, "manual fallback (%v)", projected)
	require.Equal(t, int64(1_000), projected.MSSince, "manual fallback (%v)", projected)

	projected, ok = cache.Remove(hostUUID, sourceManual, now.Add(3*time.Second))
	require.True(t, ok, "final removal projection (%v)", projected)
	require.Equal(t, hostUUID, projected.HostUUID, "final removal projection (%v)", projected)
	require.False(t, projected.TelemetryValid, "final removal projection (%v)", projected)
	require.Equal(t, int64(0), projected.MSSince, "final removal projection (%v)", projected)
	require.Empty(t, cache.Snapshot(now.Add(4*time.Second)), "cache retained final removal")
}

func TestTelemetryCacheSnapshot_IsSortedAndNormalizesInvalidAge(t *testing.T) {
	const validUUID = "node-a"
	const invalidUUID = "node-z"
	cache := newTelemetryCache()
	now := time.Unix(1_700_000_000, 0).UTC() // arbitrary fixed instant
	cache.Upsert(sourceScanner, noderec.NodeTelemetry{
		HostUUID:       invalidUUID,
		TelemetryValid: false,
		MSSince:        999,
	}, now, now)
	cache.Upsert(sourceScanner, noderec.NodeTelemetry{
		HostUUID:       validUUID,
		TelemetryValid: true,
		MSSince:        -50,
	}, now, now)

	got := cache.Snapshot(now)
	require.Len(t, got, 2, "snapshot order")
	require.Equal(t, validUUID, got[0].HostUUID, "snapshot order (%v)", got)
	require.Equal(t, invalidUUID, got[1].HostUUID, "snapshot order (%v)", got)
	require.Equal(t, int64(0), got[0].MSSince, "normalized age")
	require.Equal(t, int64(0), got[1].MSSince, "normalized age")
}

func TestReplayTelemetryToScheduler_IncludesCurrentAge(t *testing.T) {
	const hostUUID = "node-a"
	brokerSide, schedulerSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = schedulerSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	b := &Broker{telemetry: newTelemetryCache()}
	b.telemetry.Upsert(sourceScanner, noderec.NodeTelemetry{
		HostUUID:          hostUUID,
		GPUUtilizationPct: 45,
		TelemetryValid:    true,
		MSSince:           100,
	}, time.Now().Add(-250*time.Millisecond), time.Now())

	replayed := make(chan int, 1)
	go func() { replayed <- b.replayTelemetryToScheduler(worker) }()
	require.NoError(t, schedulerSide.SetReadDeadline(time.Now().Add(2*time.Second)), "set read deadline")
	message := readSchedulerTestMessage(t, NewCodec(schedulerSide))
	require.Equal(t, schedulerwire.MethodTelemetry, message.Method, "replay method")
	var got noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &got), "decode replay")
	require.Equal(t, hostUUID, got.HostUUID, "replayed telemetry (%v)", got)
	require.GreaterOrEqual(t, got.MSSince, int64(350), "replayed telemetry (%v)", got)
	require.Equal(t, 1, <-replayed, "replayed count")
}

// Send a verified sample whose recorded arrival was 250 ms ago through the
// live broker-to-scheduler pipe. Its 100 ms source age must become at least
// 350 ms on the wire, preserving time already spent waiting in the broker.
func TestIngestTelemetry_IncludesTimeWaitingInBroker(t *testing.T) {
	const peerUUID = "peer"
	brokerSide, schedulerSide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, brokerSide.Close())
		assert.NoError(t, schedulerSide.Close())
	})
	broker := newManualTestBroker()
	broker.store.Upsert(EnrichedNode{
		HostUUID:             peerUUID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
	}, sourceScanner)
	broker.setScheduler(&rpcWorker{peer: NewPeer(NewCodec(brokerSide))})
	// Let the unbuffered pipe's writer and reader run together, then wait for
	// ingestion to return before cleanup closes either end of the connection.
	done := make(chan struct{})
	go func() {
		broker.ingestTelemetryAt(sourceScanner, noderec.NodeTelemetry{
			HostUUID:       peerUUID,
			TelemetryValid: true,
			MSSince:        100,
		}, time.Now().Add(-250*time.Millisecond))
		close(done)
	}()
	require.NoError(t, schedulerSide.SetReadDeadline(time.Now().Add(2*time.Second)))

	message := readSchedulerTestMessage(t, NewCodec(schedulerSide))
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.GreaterOrEqual(t, sample.MSSince, int64(350))
	<-done
}

func TestManualTelemetry_ReprojectsSurvivingAliasAtOriginalAge(t *testing.T) {
	b := newManualTestBroker()
	first := manualStatus("first", "10.0.0.1", "node-a")
	first.GPUs = []GPUInfo{{UtilizationPercent: 25}}
	first.TelemetryValid = true
	first.MSSince = 100
	b.upsertManualNode(first)

	b.manualMu.Lock()
	entry := b.manualNodeStatuses["first"]
	entry.receivedAt = time.Now().Add(-2 * time.Second)
	b.manualNodeStatuses["first"] = entry
	b.manualMu.Unlock()

	second := manualStatus("second", "10.0.0.2", "node-a")
	second.GPUs = []GPUInfo{{UtilizationPercent: 80}}
	second.TelemetryValid = true
	b.upsertManualNode(second)
	b.removeManualNode("second")

	snapshot := b.telemetry.Snapshot(time.Now())
	require.Len(t, snapshot, 1, "surviving manual telemetry")
	require.Equal(t, "node-a", snapshot[0].HostUUID, "surviving manual telemetry (%v)", snapshot)
	require.Equal(t, uint32(25), snapshot[0].GPUUtilizationPct, "surviving alias reset telemetry age")
	require.GreaterOrEqual(t, snapshot[0].MSSince, int64(2_100), "surviving alias reset telemetry age")

	b.removeManualNode("first")
	require.Empty(t, b.telemetry.Snapshot(time.Now()), "final manual alias left telemetry cached")
}

func TestLiveTelemetry_IncludesTimeWaitingForSchedulerFeed(t *testing.T) {
	const nodeUUID = "local-node"
	const sourceAgeMillis = int64(137)
	const feedDelay = 100 * time.Millisecond
	broker := newManualTestBroker()
	broker.store.Upsert(EnrichedNode{HostUUID: nodeUUID, IdentityVerification: noderec.IdentityLocal}, sourceScanner)
	var output bytes.Buffer
	broker.setScheduler(&rpcWorker{peer: NewPeer(NewCodec(&output))})
	broker.schedulerFeedMu.Lock()
	feedLocked := true
	done := make(chan struct{})
	t.Cleanup(func() {
		if feedLocked {
			broker.schedulerFeedMu.Unlock()
		}
		<-done
	})
	go func() {
		defer close(done)
		broker.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{
			HostUUID: nodeUUID, TelemetryValid: true, MSSince: sourceAgeMillis,
		})
	}()
	// Cache insertion precedes the feed lock. Hold the sample behind a replay
	// long enough to distinguish send-time age from its pre-lock projection.
	require.Eventually(t, func() bool {
		return len(broker.telemetry.Snapshot(time.Now())) == 1
	}, 2*time.Second, time.Millisecond)
	time.Sleep(feedDelay)
	broker.schedulerFeedMu.Unlock()
	feedLocked = false
	<-done

	message, err := NewCodec(&output).Read()
	require.NoError(t, err)
	require.Equal(t, schedulerwire.MethodTelemetry, message.Method)
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.True(t, sample.TelemetryValid)
	assert.GreaterOrEqual(t, sample.MSSince, sourceAgeMillis+feedDelay.Milliseconds())
}
