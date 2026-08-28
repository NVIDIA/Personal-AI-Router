// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
	"time"

	"nvpair-shared/noderec"
)

const (
	gpuTelemetryFreshness = 10 * time.Second
	gpuEWMAAlpha          = 0.35
	unknownGPUPressure    = 1
)

type gpuTelemetryState struct {
	ewma         float64
	pressure     int
	hasEWMA      bool
	valid        bool
	ageAtReceipt time.Duration
	receivedAt   time.Time
}

func (m *Manager) applyTelemetry(params json.RawMessage) bool {
	var value noderec.NodeTelemetry
	if err := json.Unmarshal(params, &value); err != nil {
		slog.Warn("invalid scheduler telemetry", "err", err)
		return false
	}
	return m.applyTelemetryAt(value, time.Now())
}

// applyTelemetryAt folds one utilization sample into the node's EWMA and
// pressure band. It reports whether effective pressure changed; raw utilization
// and sub-band EWMA movement intentionally do not trigger schedule emissions.
func (m *Manager) applyTelemetryAt(value noderec.NodeTelemetry, receivedAt time.Time) bool {
	if value.HostUUID == "" {
		return false
	}
	ageAtReceipt := normalizedTelemetryAge(value.MSSince)
	incomingFresh := value.TelemetryValid && ageAtReceipt <= gpuTelemetryFreshness
	utilization := value.GPUUtilizationPct
	if utilization > 100 {
		utilization = 100
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	previous, existed := m.telemetry[value.HostUUID]
	previousPressure := effectiveGPUPressure(previous, existed, receivedAt)
	next := previous
	next.valid = incomingFresh
	next.ageAtReceipt = ageAtReceipt
	next.receivedAt = receivedAt
	if incomingFresh {
		previousFresh := existed && telemetryFreshAt(previous, receivedAt)
		if !previousFresh || !previous.hasEWMA {
			next.ewma = float64(utilization)
			next.pressure = pressureBand(next.ewma)
		} else {
			next.ewma = gpuEWMAAlpha*float64(utilization) + (1-gpuEWMAAlpha)*previous.ewma
			next.pressure = pressureWithHysteresis(next.ewma, previous.pressure)
		}
		next.hasEWMA = true
	}
	m.telemetry[value.HostUUID] = next
	return previousPressure != effectiveGPUPressure(next, true, receivedAt)
}

func (m *Manager) gpuPressureAt(hostUUID string, now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.telemetry[hostUUID]
	return effectiveGPUPressure(state, ok, now)
}

func effectiveGPUPressure(state gpuTelemetryState, exists bool, now time.Time) int {
	if !exists || !telemetryFreshAt(state, now) {
		return unknownGPUPressure
	}
	return state.pressure
}

func telemetryFreshAt(state gpuTelemetryState, now time.Time) bool {
	if !state.valid {
		return false
	}
	elapsed := now.Sub(state.receivedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return state.ageAtReceipt+elapsed <= gpuTelemetryFreshness
}

func normalizedTelemetryAge(ms int64) time.Duration {
	if ms <= 0 {
		return 0
	}
	if ms > gpuTelemetryFreshness.Milliseconds() {
		return gpuTelemetryFreshness + time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

func pressureBand(utilization float64) int {
	switch {
	case utilization < 40:
		return 0
	case utilization < 70:
		return 1
	case utilization < 85:
		return 2
	default:
		return 3
	}
}

func pressureWithHysteresis(utilization float64, previous int) int {
	pressure := previous
	if pressure < 0 || pressure > 3 {
		return pressureBand(utilization)
	}
	up := [...]float64{40, 70, 85}
	down := [...]float64{0, 35, 65, 80}
	for pressure < 3 && utilization >= up[pressure] {
		pressure++
	}
	for pressure > 0 && utilization < down[pressure] {
		pressure--
	}
	return pressure
}
