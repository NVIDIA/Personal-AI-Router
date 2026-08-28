// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"nvpair-shared/noderec"
	"nvpair-shared/schedulerwire"
)

func TestPressureBandsAndDownwardHysteresis(t *testing.T) {
	bands := []struct {
		utilization float64
		want        int
	}{
		{0, 0},
		{39.999, 0},
		{40, 1},
		{69.999, 1},
		{70, 2},
		{84.999, 2},
		{85, 3},
		{100, 3},
	}
	for _, test := range bands {
		if got := pressureBand(test.utilization); got != test.want {
			t.Errorf("pressureBand(%v) = %d, want %d", test.utilization, got, test.want)
		}
	}

	hysteresis := []struct {
		name        string
		utilization float64
		previous    int
		want        int
	}{
		{"hold one above 35", 35, 1, 1},
		{"drop one below 35", 34.9, 1, 0},
		{"hold two above 65", 65, 2, 2},
		{"drop two below 65", 64.9, 2, 1},
		{"hold three above 80", 80, 3, 3},
		{"drop three below 80", 79.9, 3, 2},
		{"promote at ordinary boundary", 70, 1, 2},
		{"cross multiple bands upward", 90, 0, 3},
		{"cross multiple bands downward", 20, 3, 0},
	}
	for _, test := range hysteresis {
		t.Run(test.name, func(t *testing.T) {
			if got := pressureWithHysteresis(test.utilization, test.previous); got != test.want {
				t.Fatalf("pressureWithHysteresis(%v, %d) = %d, want %d",
					test.utilization, test.previous, got, test.want)
			}
		})
	}
}

func TestApplyTelemetrySmoothsUtilizationBeforeChangingPressure(t *testing.T) {
	manager := NewManager(NewCodec(nopRW{}), time.Second)
	now := time.Unix(1_700_000_000, 0)
	sample := noderec.NodeTelemetry{
		HostUUID:       "node-a",
		TelemetryValid: true,
	}

	if !manager.applyTelemetryAt(sample, now) {
		t.Fatal("first idle sample should move unknown pressure 1 to idle pressure 0")
	}
	sample.GPUUtilizationPct = 100
	if manager.applyTelemetryAt(sample, now.Add(time.Second)) {
		t.Fatal("one hot sample should not move the EWMA across 40%")
	}
	manager.mu.Lock()
	firstEWMA := manager.telemetry["node-a"].ewma
	manager.mu.Unlock()
	if math.Abs(firstEWMA-35) > 0.001 {
		t.Fatalf("EWMA after first hot sample = %.3f, want 35", firstEWMA)
	}

	if !manager.applyTelemetryAt(sample, now.Add(2*time.Second)) {
		t.Fatal("second hot sample should move pressure from 0 to 1")
	}
	manager.mu.Lock()
	secondEWMA := manager.telemetry["node-a"].ewma
	manager.mu.Unlock()
	if math.Abs(secondEWMA-57.75) > 0.001 {
		t.Fatalf("EWMA after second hot sample = %.3f, want 57.75", secondEWMA)
	}
	if got := manager.gpuPressureAt("node-a", now.Add(2*time.Second)); got != 1 {
		t.Fatalf("pressure after smoothed hot samples = %d, want 1", got)
	}
}

func TestTelemetryFreshnessUnknownAndRecovery(t *testing.T) {
	manager := NewManager(NewCodec(nopRW{}), time.Second)
	now := time.Unix(1_700_000_000, 0)
	hot := noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 90,
		TelemetryValid:    true,
		MSSince:           9_999,
	}
	if !manager.applyTelemetryAt(hot, now) {
		t.Fatal("fresh hot sample should move unknown pressure to 3")
	}
	if got := manager.gpuPressureAt("node-a", now.Add(time.Millisecond)); got != 3 {
		t.Fatalf("pressure at exactly 10s effective age = %d, want 3", got)
	}
	if got := manager.gpuPressureAt("node-a", now.Add(2*time.Millisecond)); got != unknownGPUPressure {
		t.Fatalf("stale pressure = %d, want unknown %d", got, unknownGPUPressure)
	}

	cool := noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 20,
		TelemetryValid:    true,
	}
	recoveredAt := now.Add(2 * time.Millisecond)
	if !manager.applyTelemetryAt(cool, recoveredAt) {
		t.Fatal("fresh recovery should move unknown pressure to 0")
	}
	manager.mu.Lock()
	recovered := manager.telemetry["node-a"]
	manager.mu.Unlock()
	if recovered.ewma != 20 || recovered.pressure != 0 {
		t.Fatalf("recovered state = %+v, want reset EWMA 20 and pressure 0", recovered)
	}

	invalid := cool
	invalid.TelemetryValid = false
	if !manager.applyTelemetryAt(invalid, recoveredAt.Add(time.Second)) {
		t.Fatal("invalid telemetry should move pressure from 0 to unknown")
	}
	if got := manager.gpuPressureAt("node-a", recoveredAt.Add(time.Second)); got != unknownGPUPressure {
		t.Fatalf("invalid pressure = %d, want %d", got, unknownGPUPressure)
	}
}

func TestTelemetryOlderThanFreshnessStartsUnknown(t *testing.T) {
	manager := NewManager(NewCodec(nopRW{}), time.Second)
	now := time.Unix(1_700_000_000, 0)
	changed := manager.applyTelemetryAt(noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 100,
		TelemetryValid:    true,
		MSSince:           10_001,
	}, now)
	if changed {
		t.Fatal("stale first sample should remain at unknown pressure")
	}
	if got := manager.gpuPressureAt("node-a", now); got != unknownGPUPressure {
		t.Fatalf("stale first pressure = %d, want %d", got, unknownGPUPressure)
	}
}

func TestNodeRemovalDropsTelemetryState(t *testing.T) {
	manager := NewManager(NewCodec(nopRW{}), time.Second)
	manager.applyTelemetryAt(noderec.NodeTelemetry{
		HostUUID:       "node-a",
		TelemetryValid: true,
	}, time.Now())
	manager.applyNodesChanged(json.RawMessage(`[{"hostUuid":"node-b"}]`))
	manager.mu.Lock()
	_, retained := manager.telemetry["node-a"]
	manager.mu.Unlock()
	if retained {
		t.Fatal("removed node retained telemetry state")
	}
}

func TestHandleMessageAppliesTelemetryNotification(t *testing.T) {
	manager := NewManager(NewCodec(nopRW{}), time.Second)
	params, err := json.Marshal(noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 90,
		TelemetryValid:    true,
	})
	if err != nil {
		t.Fatalf("marshal telemetry: %v", err)
	}
	manager.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  schedulerwire.MethodTelemetry,
		Params:  params,
	})

	manager.mu.Lock()
	state, ok := manager.telemetry["node-a"]
	manager.mu.Unlock()
	if !ok || state.pressure != 3 || !state.valid {
		t.Fatalf("handled telemetry state = %+v, present=%t", state, ok)
	}
}

func TestTelemetryNotificationEmitsOnlyOnPressureChange(t *testing.T) {
	recorder := &capRW{}
	manager := NewManager(NewCodec(recorder), time.Second)
	manager.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  "discovery:nodes-changed",
		Params:  json.RawMessage(`[{"hostUuid":"a"},{"hostUuid":"b"}]`),
	})

	send := func(node string, utilization uint32) {
		t.Helper()
		params, err := json.Marshal(noderec.NodeTelemetry{
			HostUUID:          node,
			GPUUtilizationPct: utilization,
			TelemetryValid:    true,
		})
		if err != nil {
			t.Fatalf("marshal telemetry: %v", err)
		}
		manager.handleMessage(&Message{
			JSONRPC: "2.0",
			Method:  schedulerwire.MethodTelemetry,
			Params:  params,
		})
	}

	send("a", 50) // pressure 1 matches the unknown baseline
	send("b", 0)  // pressure changes from unknown 1 to idle 0
	send("b", 20) // EWMA changes within pressure band 0

	for _, engine := range schedulerEngines {
		got := recorder.priorities(engine)
		if len(got) != 2 {
			t.Fatalf("%s emissions = %d, want discovery plus one pressure change", engine, len(got))
		}
		assertStrs(t, got[1].Nodes, []string{"b", "a"})
		if pressureOf(got[1].Ranks, "b") != 0 || pressureOf(got[1].Ranks, "a") != 1 {
			t.Fatalf("%s pressure snapshot = %+v", engine, got[1].Ranks)
		}
	}
}
