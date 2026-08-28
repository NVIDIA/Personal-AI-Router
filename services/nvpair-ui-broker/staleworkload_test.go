// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

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
	if !b.workloads.Apply(storeIncoming("7", "peer-uuid", "ollama", "r1", "running", "peer-uuid")) {
		t.Fatal("remote running apply should be accepted")
	}
	// Local-origin: our own proxies are the authority and never re-assert here.
	if !b.workloads.Apply(storeIncoming("8", "self", "ollama", "r1", "running", "self")) {
		t.Fatal("local running apply should be accepted")
	}
	// Remote-origin but executing HERE. Still the sweep's business: lifecycle
	// events come from the origin's proxy, not from our engine, so nothing else
	// will ever clear this record.
	if !b.workloads.Apply(storeIncoming("9", "peer-uuid", "ollama", "r1", "running", "self")) {
		t.Fatal("remote-origin local-executor apply should be accepted")
	}

	// Nothing is stale while the origin is still within its silence budget.
	b.failStaleForeignWorkloads(time.Hour)
	if r, _ := b.workloads.Get("peer-uuid", "7"); r.State != "running" {
		t.Fatalf("state = %q, want running (origin has not gone silent yet)", r.State)
	}

	// A negative budget puts every sighting past the cutoff, which is the same
	// condition as an origin that has said nothing for the real timeout.
	b.failStaleForeignWorkloads(-time.Second)

	r, _ := b.workloads.Get("peer-uuid", "7")
	if r.State != "failed" {
		t.Fatalf("remote state = %q, want failed once the origin went silent", r.State)
	}
	if !r.Inferred {
		t.Fatal("the retirement must be inferred so the origin can reconcile it away")
	}
	if local, _ := b.workloads.Get("self", "8"); local.State != "running" {
		t.Fatalf("local state = %q, want running (own workloads are never swept)", local.State)
	}
	if here, _ := b.workloads.Get("peer-uuid", "9"); here.State != "failed" {
		t.Fatalf("locally-executing state = %q, want failed — the executing node has no other way to learn this ended", here.State)
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
	if r, _ := b.workloads.Get("peer-uuid", "9"); r.State != "failed" {
		t.Fatalf("state = %q, want failed", r.State)
	}

	if !b.workloads.Apply(storeIncoming("9", "peer-uuid", "ollama", "r1", "running", "peer-uuid")) {
		t.Fatal("the origin's authoritative running must override the inferred failure")
	}
	r, _ := b.workloads.Get("peer-uuid", "9")
	if r.State != "running" || r.Inferred {
		t.Fatalf("record = %+v, want authoritative running", r)
	}

	// And the origin's real terminal still lands afterwards.
	if !b.workloads.Apply(storeIncoming("9", "peer-uuid", "ollama", "r1", "completed", "peer-uuid")) {
		t.Fatal("the origin's terminal must apply")
	}
	if r, _ := b.workloads.Get("peer-uuid", "9"); r.State != "completed" || r.Inferred {
		t.Fatalf("record = %+v, want authoritative completed", r)
	}
}
