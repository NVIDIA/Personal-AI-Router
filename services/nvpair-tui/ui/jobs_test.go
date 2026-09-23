// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"testing"
)

// TestJobsHistoryIsBounded is the guard for a leak in a program meant to be left
// running. The broker never tells a client to forget a job, so without a cap
// every job the cluster has ever run accumulates for the life of the process,
// and the "show all" table grows with it.
func TestJobsHistoryIsBounded(t *testing.T) {
	v := newJobsView(nil)
	for i := range maxFinishedJobs + 50 {
		v.upsert(workload{
			ID:             fmt.Sprintf("job-%d", i),
			OriginatedFrom: "node",
			State:          "completed",
		})
	}

	if got := len(v.byKey); got > maxFinishedJobs {
		t.Errorf("kept %d finished jobs, want at most %d", got, maxFinishedJobs)
	}
	if len(v.order) != len(v.byKey) {
		t.Errorf("order (%d) and index (%d) disagree after eviction, so a key leaked",
			len(v.order), len(v.byKey))
	}

	// Eviction is oldest-first, so the most recent job must survive.
	newest := workloadKey("node", fmt.Sprintf("job-%d", maxFinishedJobs+49))
	if _, ok := v.byKey[newest]; !ok {
		t.Error("the newest finished job was evicted; eviction is not oldest-first")
	}
}

// TestJobsNeverEvictsActiveWork checks the cap only reclaims finished jobs. An
// in-flight job is the thing the operator is watching, and dropping one would
// make a busy cluster look idle.
func TestJobsNeverEvictsActiveWork(t *testing.T) {
	v := newJobsView(nil)
	v.upsert(workload{ID: "live", OriginatedFrom: "node", State: "running"})
	for i := range maxFinishedJobs + 50 {
		v.upsert(workload{
			ID:             fmt.Sprintf("done-%d", i),
			OriginatedFrom: "node",
			State:          "completed",
		})
	}

	if _, ok := v.byKey[workloadKey("node", "live")]; !ok {
		t.Error("a running job was evicted by history trimming")
	}
}

// TestJobsUpsertReplacesRatherThanDuplicating checks a job progressing through
// its states occupies one row, not one per update.
func TestJobsUpsertReplacesRatherThanDuplicating(t *testing.T) {
	v := newJobsView(nil)
	for _, state := range []string{"queued", "running", "completed"} {
		v.upsert(workload{ID: "j1", OriginatedFrom: "node", State: state})
	}

	if len(v.order) != 1 {
		t.Errorf("one job produced %d rows across its state changes", len(v.order))
	}
	if got := v.byKey[workloadKey("node", "j1")].State; got != "completed" {
		t.Errorf("state = %q, want the latest", got)
	}
}

// TestJobsKeyIsScopedByOrigin checks two nodes can use the same job id without
// colliding, since ids are only unique to the node that issued them.
func TestJobsKeyIsScopedByOrigin(t *testing.T) {
	v := newJobsView(nil)
	v.upsert(workload{ID: "1", OriginatedFrom: "node-a", State: "running"})
	v.upsert(workload{ID: "1", OriginatedFrom: "node-b", State: "running"})

	if len(v.order) != 2 {
		t.Errorf("same id from two nodes collapsed into %d row(s)", len(v.order))
	}
}

var _ View = (*jobsView)(nil)
