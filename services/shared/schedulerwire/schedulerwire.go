// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package schedulerwire defines the shared JSON-RPC payloads used to carry
// scheduler rankings through the broker to the inference proxies.
package schedulerwire

const (
	MethodTelemetry = "scheduler:telemetry"
	MaxGPUPressure  = 3
)

// NodeRank is one node's position in a node-wide workload and GPU-pressure
// ranking.
type NodeRank struct {
	ID          string `json:"id"`
	Pending     int    `json:"pending"`
	GPUPressure int    `json:"gpuPressure"`
	Rank        int    `json:"rank"`
}

// Priority is the payload accepted by a proxy's node/set-priority method.
// Ranks is optional so a newer broker can still drive an older nodes-only
// producer or consumer during a rolling upgrade.
type Priority struct {
	Nodes []string   `json:"nodes"`
	Ranks []NodeRank `json:"ranks,omitempty"`
}

// Clone returns an independently owned snapshot safe to cache across goroutines.
func (p Priority) Clone() Priority {
	return Priority{
		Nodes: append([]string(nil), p.Nodes...),
		Ranks: append([]NodeRank(nil), p.Ranks...),
	}
}

// EnginePriority is the schedule:priority notification emitted by the scheduler.
type EnginePriority struct {
	Engine string     `json:"engine"`
	Nodes  []string   `json:"nodes"`
	Ranks  []NodeRank `json:"ranks,omitempty"`
}

// Snapshot strips the engine routing key and returns an independently owned
// node/set-priority payload for the matching proxy.
func (p EnginePriority) Snapshot() Priority {
	return Priority{
		Nodes: append([]string(nil), p.Nodes...),
		Ranks: append([]NodeRank(nil), p.Ranks...),
	}
}
