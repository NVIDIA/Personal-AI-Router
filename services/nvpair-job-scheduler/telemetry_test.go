// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		{
			got := pressureBand(test.utilization)
			assert.Equal(t, test.want, got, "pressureBand")
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
			{
				got := pressureWithHysteresis(test.utilization, test.previous)
				assert.Equal(t, test.want, got, "pressureWithHysteresis")
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

	assert.True(t, manager.applyTelemetryAt(sample, now), "first idle sample should move unknown pressure 1 to idle pressure 0")
	sample.GPUUtilizationPct = 100
	assert.False(t, manager.applyTelemetryAt(sample, now.Add(time.Second)), "one hot sample should not move the EWMA across 40%")
	manager.mu.Lock()
	firstEWMA := manager.telemetry["node-a"].ewma
	manager.mu.Unlock()
	assert.InDelta(t, 35, firstEWMA, 0.001, "EWMA after first hot sample")

	assert.True(t, manager.applyTelemetryAt(sample, now.Add(2*time.Second)), "second hot sample should move pressure from 0 to 1")
	manager.mu.Lock()
	secondEWMA := manager.telemetry["node-a"].ewma
	manager.mu.Unlock()
	assert.InDelta(t, 57.75, secondEWMA, 0.001, "EWMA after second hot sample")
	{
		got := manager.gpuPressureAt("node-a", now.Add(2*time.Second))
		assert.Equal(t, 1, got, "pressure after smoothed hot samples")
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
	assert.True(t, manager.applyTelemetryAt(hot, now), "fresh hot sample should move unknown pressure to 3")
	{
		got := manager.gpuPressureAt("node-a", now.Add(time.Millisecond))
		assert.Equal(t, 3, got, "pressure at exactly 10s effective age")
	}
	{
		got := manager.gpuPressureAt("node-a", now.Add(2*time.Millisecond))
		assert.Equal(t, unknownGPUPressure, got, "stale pressure")
	}

	cool := noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 20,
		TelemetryValid:    true,
	}
	recoveredAt := now.Add(2 * time.Millisecond)
	assert.True(t, manager.applyTelemetryAt(cool, recoveredAt), "fresh recovery should move unknown pressure to 0")
	manager.mu.Lock()
	recovered := manager.telemetry["node-a"]
	manager.mu.Unlock()
	assert.Equal(t, 20.0, recovered.ewma, "recovered EWMA")
	assert.Equal(t, 0, recovered.pressure, "recovered state (%v)", recovered)

	invalid := cool
	invalid.TelemetryValid = false
	assert.True(t, manager.applyTelemetryAt(invalid, recoveredAt.Add(time.Second)), "invalid telemetry should move pressure from 0 to unknown")
	{
		got := manager.gpuPressureAt("node-a", recoveredAt.Add(time.Second))
		assert.Equal(t, unknownGPUPressure, got, "invalid pressure")
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
	require.False(t, changed, "stale first sample should remain at unknown pressure")
	{
		got := manager.gpuPressureAt("node-a", now)
		assert.Equal(t, unknownGPUPressure, got, "stale first pressure")
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
	assert.NotContains(t, manager.telemetry, "node-a", "removed node retained telemetry state")
	manager.mu.Unlock()
}

func TestHandleMessageAppliesTelemetryNotification(t *testing.T) {
	manager := NewManager(NewCodec(nopRW{}), time.Second)
	params, err := json.Marshal(noderec.NodeTelemetry{
		HostUUID:          "node-a",
		GPUUtilizationPct: 90,
		TelemetryValid:    true,
	})
	require.NoError(t, err, "marshal telemetry")
	manager.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  schedulerwire.MethodTelemetry,
		Params:  params,
	})

	manager.mu.Lock()
	state, ok := manager.telemetry["node-a"]
	manager.mu.Unlock()
	require.True(t, ok, "handled telemetry state (%v, %v)", state, ok)
	assert.Equal(t, 3, state.pressure, "handled telemetry state (%v, %v)", state, ok)
	assert.True(t, state.valid, "handled telemetry state (%v, %v)", state, ok)
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
		require.NoError(t, err, "marshal telemetry")
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
		require.Len(t, got, 2, " (%v)", engine)
		assertStrs(t, got[1].Nodes, []string{"b", "a"})
		assert.Equal(t, 0, pressureOf(got[1].Ranks, "b"), " (%v)", engine)
		assert.Equal(t, 1, pressureOf(got[1].Ranks, "a"), " (%v)", engine)
	}
}
