// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
	"nvpair-shared/schedulerwire"
)

func TestTelemetryCacheAgesObservations(t *testing.T) {
	cache := newTelemetryCache()
	receivedAt := time.Unix(1_700_000_000, 0)
	input := noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 84,
		TelemetryValid:    true,
		MSSince:           137,
	}
	projected, ok := cache.Upsert(sourceScanner, input, receivedAt)
	require.True(t, ok, "initial projection (%v, %v)", projected, ok)
	require.Equal(t, int64(137), projected.MSSince, "initial projection (%v, %v)", projected, ok)
	snapshot := cache.Snapshot(receivedAt.Add(250 * time.Millisecond))
	require.Len(t, snapshot, 1, "snapshot length")
	require.Equal(t, int64(387), snapshot[0].MSSince, "aged msSince")
	require.Equal(t, uint32(84), snapshot[0].GPUUtilizationPct, "cached utilization")
}

func TestTelemetryCachePrefersScannerAndFallsBackToManual(t *testing.T) {
	cache := newTelemetryCache()
	now := time.Unix(1_700_000_000, 0)
	scanner := noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 20,
		TelemetryValid:    true,
	}
	manual := noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 70,
		TelemetryValid:    true,
	}

	cache.Upsert(sourceScanner, scanner, now)
	projected, ok := cache.Upsert(sourceManual, manual, now.Add(time.Second))
	require.True(t, ok, "scanner projection (%v, %v)", projected, ok)
	require.Equal(t, uint32(20), projected.GPUUtilizationPct, "scanner projection (%v, %v)", projected, ok)

	projected, ok = cache.Remove("node-a", sourceScanner, now.Add(2*time.Second))
	require.True(t, ok, "manual fallback (%v, %v)", projected, ok)
	require.Equal(t, uint32(70), projected.GPUUtilizationPct, "manual fallback (%v, %v)", projected, ok)
	require.Equal(t, int64(1_000), projected.MSSince, "manual fallback (%v, %v)", projected, ok)

	projected, ok = cache.Remove("node-a", sourceManual, now.Add(3*time.Second))
	require.True(t, ok, "final removal projection (%v, %v)", projected, ok)
	require.Equal(t, "node-a", projected.HostUUID, "final removal projection (%v, %v)", projected, ok)
	require.False(t, projected.TelemetryValid, "final removal projection (%v, %v)", projected, ok)
	require.Equal(t, int64(0), projected.MSSince, "final removal projection (%v, %v)", projected, ok)
	{
		snapshot := cache.Snapshot(now.Add(4 * time.Second))
		require.Empty(t, snapshot, "cache retained final removal")
	}
}

func TestTelemetryCacheSnapshotIsSortedAndNormalizesInvalidAge(t *testing.T) {
	cache := newTelemetryCache()
	now := time.Unix(1_700_000_000, 0)
	cache.Upsert(sourceScanner, noderec.NodeTelemetry{
		HostUUID:       "node-z",
		TelemetryValid: false,
		MSSince:        999,
	}, now)
	cache.Upsert(sourceScanner, noderec.NodeTelemetry{
		HostUUID:       "node-a",
		TelemetryValid: true,
		MSSince:        -50,
	}, now)

	got := cache.Snapshot(now)
	require.Len(t, got, 2, "snapshot order")
	require.Equal(t, "node-a", got[0].HostUUID, "snapshot order (%v)", got)
	require.Equal(t, "node-z", got[1].HostUUID, "snapshot order (%v)", got)
	require.Equal(t, int64(0), got[0].MSSince, "normalized ages = [")
	require.Equal(t, int64(0), got[1].MSSince, "normalized ages = [")
}

func TestReplayTelemetryToSchedulerIncludesCurrentAge(t *testing.T) {
	brokerSide, schedulerSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = schedulerSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	b := &Broker{telemetry: newTelemetryCache()}
	b.telemetry.Upsert(sourceScanner, noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 45,
		TelemetryValid:    true,
		MSSince:           100,
	}, time.Now().Add(-250*time.Millisecond))

	replayed := make(chan int, 1)
	go func() { replayed <- b.replayTelemetryToScheduler(worker) }()
	require.NoError(t, schedulerSide.SetReadDeadline(time.Now().Add(2*time.Second)), "set read deadline")
	message := readSchedulerTestMessage(t, NewCodec(schedulerSide))
	require.Equal(t, schedulerwire.MethodTelemetry, message.Method, "replay method")
	var got noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &got), "decode replay")
	require.Equal(t, "node-a", got.HostUUID, "replayed telemetry (%v)", got)
	require.GreaterOrEqual(t, got.MSSince, int64(350), "replayed telemetry (%v)", got)
	{
		count := <-replayed
		require.Equal(t, 1, count, "replayed count")
	}
}

func TestManualTelemetryReprojectsSurvivingAliasAtOriginalAge(t *testing.T) {
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
	{
		snapshot := b.telemetry.Snapshot(time.Now())
		require.Empty(t, snapshot, "final manual alias left telemetry cached")
	}
}
