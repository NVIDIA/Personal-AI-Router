// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "encoding/json"

// Answer only over the existing child stdio channel. This snapshot is an
// availability observation, not a GPU reservation or exclusion claim.
func (b *Broker) replyDiagnosticWorkloads(origin *rpcWorker, raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.RequestID) != 32 {
		return
	}
	b.workersMu.Lock()
	current := origin != nil && b.engineMgr == origin
	b.workersMu.Unlock()
	if !current {
		return
	}
	b.workloadEmitMu.Lock()
	idle := len(b.workloads.ActiveSnapshot()) == 0
	b.workloadEmitMu.Unlock()
	// Reply to the process generation that asked. A replacement must never
	// inherit the prior worker's correlated diagnostic decision.
	_ = origin.Notify("engine:diagnostic-workload-state", map[string]any{"requestId": p.RequestID, "idle": idle})
}
