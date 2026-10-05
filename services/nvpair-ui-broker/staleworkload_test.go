// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-ui-broker/workloadstore"
)

// TestFailStaleForeignWorkloadsRetiresSilentRemoteWork is the broker half of the
// stale-"running" fix. A peer's terminal event can be lost outright — the origin
// only re-asserts a finished workload for a short window, and if every copy
// misses us there is no later event to reconcile from. The sweep must retire such
// a record so the line stops displaying as in-flight, while leaving this node's
// own workloads alone (its proxies are their authority and never re-assert into
// this store).
func TestFailStaleForeignWorkloadsRetiresSilentRemoteWork(t *testing.T) {
	b := &Broker{workloads: workloadstore.New(), nodeID: "self"}

	// Remote-origin, executing on the origin: the sweep's business.
	require.True(t, b.workloads.Apply(storeIncoming("7", "peer-uuid", "ollama", "r1", "running", "peer-uuid")), "remote running apply should be accepted")
	// Local-origin: our own proxies are the authority and never re-assert here.
	require.True(t, b.workloads.Apply(storeIncoming("8", "self", "ollama", "r1", "running", "self")), "local running apply should be accepted")
	// Remote-origin but executing HERE. Still the sweep's business: lifecycle
	// events come from the origin's proxy, not from our engine, so nothing else
	// will ever clear this record.
	require.True(t, b.workloads.Apply(storeIncoming("9", "peer-uuid", "ollama", "r1", "running", "self")), "remote-origin local-executor apply should be accepted")

	// Nothing is stale while the origin is still within its silence budget.
	b.failStaleForeignWorkloads(time.Hour)
	{
		r, _ := b.workloads.Get("peer-uuid", "7")
		require.Equal(t, "running", r.State)
	}

	// A negative budget puts every sighting past the cutoff, which is the same
	// condition as an origin that has said nothing for the real timeout.
	b.failStaleForeignWorkloads(-time.Second)

	r, _ := b.workloads.Get("peer-uuid", "7")
	require.Equal(t, "failed", r.State, "remote state")
	require.True(t, r.Inferred, "the retirement must be inferred so the origin can reconcile it away")
	{
		local, _ := b.workloads.Get("self", "8")
		require.Equal(t, "running", local.State, "local state")
	}
	{
		here, _ := b.workloads.Get("peer-uuid", "9")
		require.Equal(t, "failed", here.State, "locally-executing state")
	}
}

// TestFailStaleForeignWorkloadsYieldsToTheOrigin: the sweep is a guess, so an
// origin that was merely unable to deliver to us must be able to take its
// workload back with its next authoritative event.
func TestFailStaleForeignWorkloadsYieldsToTheOrigin(t *testing.T) {
	b := &Broker{workloads: workloadstore.New(), nodeID: "self"}
	// Executing on the origin, so the sweep is entitled to judge it.
	b.workloads.Apply(storeIncoming("9", "peer-uuid", "ollama", "r1", "running", "peer-uuid"))

	b.failStaleForeignWorkloads(-time.Second)
	{
		r, _ := b.workloads.Get("peer-uuid", "9")
		require.Equal(t, "failed", r.State)
	}

	require.True(t, b.workloads.Apply(storeIncoming("9", "peer-uuid", "ollama", "r1", "running", "peer-uuid")), "the origin's authoritative running must override the inferred failure")
	r, _ := b.workloads.Get("peer-uuid", "9")
	require.Equal(t, "running", r.State, "record (%v)", r)
	require.False(t, r.Inferred, "record (%v)", r)

	// And the origin's real terminal still lands afterwards.
	require.True(t, b.workloads.Apply(storeIncoming("9", "peer-uuid", "ollama", "r1", "completed", "peer-uuid")), "the origin's terminal must apply")
	{
		r, _ := b.workloads.Get("peer-uuid", "9")
		require.Equal(t, "completed", r.State, "record (%v)", r)
		require.False(t, r.Inferred, "record (%v)", r)
	}
}
