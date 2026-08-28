// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// WorkloadState is the lifecycle state carried in a Workload. The
// "initializing" value exists in the enum but has no lifecycle method and is
// never transmitted (see spec §4); it is accepted on the wire for forward
// compatibility but is not produced by this service.
type WorkloadState string

const (
	StateInitializing WorkloadState = "initializing"
	StateQueued       WorkloadState = "queued"
	StateRunning      WorkloadState = "running"
	StateCompleted    WorkloadState = "completed"
	StateFailed       WorkloadState = "failed"
)

// Lifecycle method names on both the local and inter-node interfaces.
const (
	MethodSubmitted = "workload:submitted"
	MethodStarted   = "workload:started"
	MethodCompleted = "workload:completed"
	MethodErrored   = "workload:errored"
	MethodRemove    = "workloads:remove"

	// MethodUpsert is emitted to the broker on stdout after a validated
	// remote lifecycle event. It is never accepted on the wire.
	MethodUpsert = "workloads:upsert"
)

// lifecycleMethods is the set of inbound lifecycle method names. The mapped
// value is the WorkloadState the method implies (spec §6); we don't reject a
// mismatch between method and Workload.state because the Broker is the source
// of truth for state — we pass workloadInfo through opaquely.
var lifecycleMethods = map[string]WorkloadState{
	MethodSubmitted: StateQueued,
	MethodStarted:   StateRunning,
	MethodCompleted: StateCompleted,
	MethodErrored:   StateFailed,
}

func isLifecycleMethod(method string) bool {
	_, ok := lifecycleMethods[method]
	return ok
}

// Workload mirrors the spec §6 object. Pointers / omitempty are used where
// the spec allows null or optional so we round-trip the peer's payload
// without inventing zero values. OriginatedFrom is the origin node;
// ScheduledOn is the node the workload was routed to / scheduled on (where it
// actually ran) — both supplied by the Broker/proxy and passed through
// opaquely (ScheduledOn optional/additive, absent until a target is chosen).
type Workload struct {
	ID             string        `json:"id"`
	Model          string        `json:"model"`
	Engine         string        `json:"engine"`
	RunID          string        `json:"runId,omitempty"`
	State          WorkloadState `json:"state"`
	OriginatedFrom string        `json:"originatedFrom"`
	ScheduledOn    string        `json:"scheduledOn,omitempty"`
	CreatedAt      int64         `json:"createdAt"`
	StartedAt      *int64        `json:"startedAt"`
	CompletedAt    *int64        `json:"completedAt"`
	Error          *string       `json:"error"`
	RequesterID    *string       `json:"requesterId"`
}

// lifecycleParams / removeParams are the params envelopes for the two kinds
// of notification.
type lifecycleParams struct {
	WorkloadInfo *Workload `json:"workloadInfo"`
}

type removeParams struct {
	WorkloadID string `json:"workloadId"`
	// OriginatedFrom disambiguates which node's workload to remove, matching
	// the Workload.originatedFrom origin field. Workload.id is only unique per
	// node (spec §11), so without it the same workloadId from two nodes
	// collides. Optional for backward compatibility (additive field per spec
	// §7.3) — legacy senders omit it and fall back to id-only keying.
	OriginatedFrom string `json:"originatedFrom,omitempty"`
}

// parseLifecycle extracts and validates a Workload from a lifecycle method's
// params. Validation is intentionally minimal (spec §7): required fields must
// be present and non-empty; engine is an opaque pass-through string whose
// value is not validated.
func parseLifecycle(params json.RawMessage) (*Workload, error) {
	var p lifecycleParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if p.WorkloadInfo == nil {
		return nil, fmt.Errorf("missing params.workloadInfo")
	}
	w := p.WorkloadInfo
	if w.ID == "" {
		return nil, fmt.Errorf("workloadInfo.id is required")
	}
	if w.Model == "" {
		return nil, fmt.Errorf("workloadInfo.model is required")
	}
	if w.Engine == "" {
		return nil, fmt.Errorf("workloadInfo.engine is required")
	}
	if w.State == "" {
		return nil, fmt.Errorf("workloadInfo.state is required")
	}
	if w.OriginatedFrom == "" {
		return nil, fmt.Errorf("workloadInfo.originatedFrom is required")
	}
	return w, nil
}

// parseRemove extracts and validates a workloadId (and the optional
// originatedFrom) from a workloads:remove method's params.
func parseRemove(params json.RawMessage) (workloadID, nodeID string, err error) {
	var p removeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.WorkloadID == "" {
		return "", "", fmt.Errorf("missing params.workloadId")
	}
	return p.WorkloadID, p.OriginatedFrom, nil
}
