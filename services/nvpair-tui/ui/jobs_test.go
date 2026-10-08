// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	assert.LessOrEqual(t, len(v.byKey), maxFinishedJobs, "finished jobs are bounded")
	assert.Len(t, v.order, len(v.byKey), "order and index disagree after eviction, so a key leaked")

	// Eviction is oldest-first, so the most recent job must survive.
	newest := workloadKey(workload{OriginatedFrom: "node", ID: fmt.Sprintf("job-%d", maxFinishedJobs+49)})
	assert.Contains(t, v.byKey, newest, "the newest finished job was evicted; eviction is not oldest-first")
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

	assert.Contains(t, v.byKey, workloadKey(workload{OriginatedFrom: "node", ID: "live"}), "a running job was evicted by history trimming")
}

// TestJobsUpsertReplacesRatherThanDuplicating checks a job progressing through
// its states occupies one row, not one per update.
func TestJobsUpsertReplacesRatherThanDuplicating(t *testing.T) {
	v := newJobsView(nil)
	for _, state := range []string{"queued", "running", "completed"} {
		v.upsert(workload{ID: "j1", OriginatedFrom: "node", State: state})
	}

	assert.Len(t, v.order, 1, "one row across a job's state changes")
	assert.Equal(t, "completed", v.byKey[workloadKey(workload{OriginatedFrom: "node", ID: "j1"})].State, "latest state")
}

// TestJobsKeyIsScopedByOrigin checks two nodes can use the same job id without
// colliding, since ids are only unique to the node that issued them.
func TestJobsKeyIsScopedByOrigin(t *testing.T) {
	v := newJobsView(nil)
	v.upsert(workload{ID: "1", OriginatedFrom: "node-a", State: "running"})
	v.upsert(workload{ID: "1", OriginatedFrom: "node-b", State: "running"})

	assert.Len(t, v.order, 2, "same id from two nodes must remain distinct")
}

// TestJobsKeyIsScopedByEngineAndRun is the same guard one level down. The ID
// is a counter each engine's facade starts at 1 and every proxy restart resets,
// so a concurrent Ollama and LM Studio job, or a job from before a restart and
// one after it, share an origin and an ID while being different work.
func TestJobsKeyIsScopedByEngineAndRun(t *testing.T) {
	v := newJobsView(nil)
	v.upsert(workload{ID: "1", OriginatedFrom: "node", Engine: "ollama", RunID: "r1", State: "running"})
	v.upsert(workload{ID: "1", OriginatedFrom: "node", Engine: "lmstudio", RunID: "r2", State: "running"})
	v.upsert(workload{ID: "1", OriginatedFrom: "node", Engine: "ollama", RunID: "r3", State: "queued"})

	assert.Len(t, v.order, 3, "three distinct jobs sharing an id must remain distinct")
}

// TestJobsRemovalDropsEveryGeneration checks a removal takes out every job it
// names. It carries only the origin and ID, so it cannot say which engine or
// run it meant — the broker's own store drops them all, and so must this.
func TestJobsRemovalDropsEveryGeneration(t *testing.T) {
	v := newJobsView(nil)
	v.upsert(workload{ID: "1", OriginatedFrom: "node", Engine: "ollama", RunID: "r1", State: "completed"})
	v.upsert(workload{ID: "1", OriginatedFrom: "node", Engine: "lmstudio", RunID: "r2", State: "completed"})
	v.upsert(workload{ID: "2", OriginatedFrom: "node", Engine: "ollama", RunID: "r1", State: "completed"})

	v.remove(workloadRef{origin: "node", id: "1"})

	require.Len(t, v.order, 1, "only id 2 remains after removing id 1")
	require.Len(t, v.byKey, 1, "only id 2 remains after removing id 1")
	assert.Equal(t, "2", v.byKey[v.order[0]].ID, "surviving job")
}

// TestRemovedJobStaysRemovedWhenTheSnapshotLandsLater is the regression guard
// for a job coming back from the dead at startup.
//
// The snapshot reply and the pushes reach the view by different paths, so a
// removal can be handled before a snapshot taken while the job still existed.
// Merging that snapshot re-added the job, and nothing would ever remove it
// again.
func TestRemovedJobStaysRemovedWhenTheSnapshotLandsLater(t *testing.T) {
	v := newJobsView(nil)
	gone := workload{ID: "1", OriginatedFrom: "node", Engine: "ollama", RunID: "r1", State: "completed"}
	kept := workload{ID: "2", OriginatedFrom: "node", Engine: "ollama", RunID: "r1", State: "running"}

	v.remove(workloadRef{origin: "node", id: "1"})
	v.Update(workloadsLoadedMsg{workloads: []workload{gone, kept}})

	assert.NotContains(t, v.byKey, workloadKey(gone), "a job removed before the snapshot landed was restored by it")
	assert.Contains(t, v.byKey, workloadKey(kept), "the snapshot's other job was not merged")

	// Once the baseline is in, the list is live and a new job with a reused
	// id is simply new work.
	again := workload{ID: "1", OriginatedFrom: "node", Engine: "ollama", RunID: "r2", State: "running"}
	v.upsert(again)
	assert.Contains(t, v.byKey, workloadKey(again), "a later job reusing a removed id was refused")
	assert.Empty(t, v.removedEarly, "removals were still being remembered after the baseline landed")
}

// TestNewestJobsLead checks new work is at the top of the table. In arrival
// order, a backlog of older queued jobs pushed each new one below the fold.
func TestNewestJobsLead(t *testing.T) {
	v := newJobsView(nil)
	v.upsert(workload{ID: "old", OriginatedFrom: "n", State: "queued", CreatedAt: 1000})
	v.upsert(workload{ID: "new", OriginatedFrom: "n", State: "queued", CreatedAt: 3000})
	v.upsert(workload{ID: "mid", OriginatedFrom: "n", State: "queued", CreatedAt: 2000})

	rows := v.table.Rows()
	got := []string{rows[0][0], rows[1][0], rows[2][0]}
	assert.Equal(t, []string{"new", "mid", "old"}, got, "newest first")
}

// TestJobIDTellsSimultaneousJobsApart checks two jobs for the same model, from
// the same node, started at the same moment, do not read as one row twice.
func TestJobIDTellsSimultaneousJobsApart(t *testing.T) {
	cases := map[string]string{"1": "1", "123456": "123456", "burst-1042": "…-1042"}
	for id, want := range cases {
		assert.Equal(t, want, shortJobID(id), "shortJobID(%q)", id)
	}

	v := newJobsView(nil)
	v.upsert(workload{ID: "7", Model: "m", OriginatedFrom: "n", State: "running", CreatedAt: 5})
	v.upsert(workload{ID: "8", Model: "m", OriginatedFrom: "n", State: "running", CreatedAt: 5})
	rows := v.table.Rows()
	assert.NotEqual(t, rows[0][0], rows[1][0], "two different jobs must show different IDs")
}

// TestFailedStartupReadsAreRetried checks a subscription or identity read that
// failed is tried again, rather than leaving the table empty, or this
// machine's jobs unnamed, for the rest of the session.
func TestFailedStartupReadsAreRetried(t *testing.T) {
	v := newJobsView(nil)
	v.Update(workloadsSubscribedMsg{err: errFake{}})
	v.Update(jobsIdentityMsg{err: errFake{}})

	var cmd tea.Cmd
	for range proxyPollTicks {
		cmd = v.Update(TickMsg{})
	}
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok, "the poll tick must return a batch")
	// The proxy refresh, the feed retry, and the identity retry.
	assert.GreaterOrEqual(t, len(batch), 3, "the two retries alongside the proxy read")

	// Once both have succeeded, nothing is left to retry.
	v.Update(workloadsSubscribedMsg{})
	v.Update(workloadsLoadedMsg{})
	v.Update(jobsIdentityMsg{id: clusterIdentity{NodeUUID: "self"}})
	assert.True(t, v.subscribed, "after both reads succeeded")
	assert.True(t, v.baselined, "after both reads succeeded")
	assert.True(t, v.identified, "after both reads succeeded")
}

var _ View = (*jobsView)(nil)
