// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

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
	if !ok || projected.MSSince != 137 {
		t.Fatalf("initial projection = %+v, ok=%v", projected, ok)
	}
	snapshot := cache.Snapshot(receivedAt.Add(250 * time.Millisecond))
	if len(snapshot) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(snapshot))
	}
	if snapshot[0].MSSince != 387 {
		t.Fatalf("aged msSince = %d, want 387", snapshot[0].MSSince)
	}
	if snapshot[0].GPUUtilizationPct != 84 {
		t.Fatalf("cached utilization = %d, want 84", snapshot[0].GPUUtilizationPct)
	}
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
	if !ok || projected.GPUUtilizationPct != 20 {
		t.Fatalf("scanner projection = %+v, ok=%v", projected, ok)
	}

	projected, ok = cache.Remove("node-a", sourceScanner, now.Add(2*time.Second))
	if !ok || projected.GPUUtilizationPct != 70 || projected.MSSince != 1_000 {
		t.Fatalf("manual fallback = %+v, ok=%v", projected, ok)
	}

	projected, ok = cache.Remove("node-a", sourceManual, now.Add(3*time.Second))
	if !ok || projected.HostUUID != "node-a" || projected.TelemetryValid || projected.MSSince != 0 {
		t.Fatalf("final removal projection = %+v, ok=%v", projected, ok)
	}
	if snapshot := cache.Snapshot(now.Add(4 * time.Second)); len(snapshot) != 0 {
		t.Fatalf("cache retained final removal: %+v", snapshot)
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
	if len(got) != 2 || got[0].HostUUID != "node-a" || got[1].HostUUID != "node-z" {
		t.Fatalf("snapshot order = %+v", got)
	}
	if got[0].MSSince != 0 || got[1].MSSince != 0 {
		t.Fatalf("normalized ages = [%d %d], want [0 0]", got[0].MSSince, got[1].MSSince)
	}
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
	if err := schedulerSide.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	message := readSchedulerTestMessage(t, NewCodec(schedulerSide))
	if message.Method != schedulerwire.MethodTelemetry {
		t.Fatalf("replay method = %q, want %q", message.Method, schedulerwire.MethodTelemetry)
	}
	var got noderec.NodeTelemetry
	if err := json.Unmarshal(message.Params, &got); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if got.HostUUID != "node-a" || got.MSSince < 350 {
		t.Fatalf("replayed telemetry = %+v, want aged node-a observation", got)
	}
	if count := <-replayed; count != 1 {
		t.Fatalf("replayed count = %d, want 1", count)
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
	if len(snapshot) != 1 || snapshot[0].HostUUID != "node-a" {
		t.Fatalf("surviving manual telemetry = %+v", snapshot)
	}
	if snapshot[0].GPUUtilizationPct != 25 || snapshot[0].MSSince < 2_100 {
		t.Fatalf("surviving alias reset telemetry age: %+v", snapshot[0])
	}

	b.removeManualNode("first")
	if snapshot := b.telemetry.Snapshot(time.Now()); len(snapshot) != 0 {
		t.Fatalf("final manual alias left telemetry cached: %+v", snapshot)
	}
}
