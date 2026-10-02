// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"

	"nvpair-ui-broker/workloadstore"
)

// TestMarkWorkloadFailedKeepsSeq: the sweeps' synthesized failure keeps the
// stored record's seq, which is what lets the store reject a delayed origin
// event older than the record the guess replaced.
func TestMarkWorkloadFailedKeepsSeq(t *testing.T) {
	info, err := json.Marshal(map[string]any{
		"id":             "1",
		"originatedFrom": "origin",
		"engine":         "ollama",
		"runId":          "run",
		"state":          "running",
		"scheduledOn":    "nodeB",
		"createdAt":      100,
		"model":          "m",
		"seq":            4,
	})
	if err != nil {
		t.Fatalf("marshal workloadInfo: %v", err)
	}

	params := markWorkloadFailed(info, time.UnixMilli(200), "node lost")
	if params == nil {
		t.Fatal("markWorkloadFailed returned nil for a valid record")
	}
	var env struct {
		WorkloadInfo json.RawMessage `json:"workloadInfo"`
	}
	if err := json.Unmarshal(params, &env); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	in, ok := workloadstore.ParseIncoming(env.WorkloadInfo)
	if !ok {
		t.Fatalf("ParseIncoming rejected %s", env.WorkloadInfo)
	}
	if in.State != "failed" || in.Seq != 4 {
		t.Fatalf("synthesized event = state %q seq %d, want failed at seq 4", in.State, in.Seq)
	}
}
