// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/schedulerwire"
)

// prNode builds a discovery Node with a single non-local address so
// resolveCandidates resolves it deterministically (single candidate → no TCP
// probe) and it never trips the loopback rewrite or self-forward guard. The
// octet is derived from the id's first byte so each id gets a distinct valid IP.
func prNode(id string) Node {
	return Node{ID: id, Addresses: []string{"192.0.2." + strconv.Itoa(int(id[0]))}, Port: 11434}
}

// prProxy returns a proxy whose discovery holds the given node ids (a, b, c...).
func prProxy(t *testing.T, ids ...string) *Proxy {
	t.Helper()
	disc := NewDiscovery()
	for _, id := range ids {
		disc.AddManual(prNode(id))
	}
	return testProxy(anyProfile(t), disc, 11435)
}

func candidateIDs(p *Proxy) []string {
	return candidateIDsForModel(p, "")
}

func candidateIDsForModel(p *Proxy, model string) []string {
	cands := p.soleFacade().resolveCandidates(model)
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.id)
	}
	return out
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	require.Equal(t, want, got, "candidate order")
}

// TestResolveCandidates_PriorityOrder: the priority list dictates auto order.
func TestResolveCandidates_PriorityOrder(t *testing.T) {
	p := prProxy(t, "a", "b", "c")
	p.SetPriority([]string{"c", "a", "b"})
	assertOrder(t, candidateIDs(p), []string{"c", "a", "b"})
}

// TestResolveCandidates_UnlistedFallback: nodes absent from the priority list
// come last, in stable ID order.
func TestResolveCandidates_UnlistedFallback(t *testing.T) {
	p := prProxy(t, "a", "b", "c")
	p.SetPriority([]string{"b"})
	assertOrder(t, candidateIDs(p), []string{"b", "a", "c"})
}

// TestResolveCandidates_UnknownIgnored: an id not in discovery is skipped.
func TestResolveCandidates_UnknownIgnored(t *testing.T) {
	p := prProxy(t, "a", "b", "c")
	p.SetPriority([]string{"zzz", "c"})
	assertOrder(t, candidateIDs(p), []string{"c", "a", "b"})
}

// TestResolveCandidates_ManualPinOverridesPriority: an explicit node/select pin
// wins over the priority list; the rest follow priority order.
func TestResolveCandidates_ManualPinOverridesPriority(t *testing.T) {
	p := prProxy(t, "a", "b", "c")
	p.SetPriority([]string{"a", "b", "c"})
	p.soleFacade().SetSelected("b")
	assertOrder(t, candidateIDs(p), []string{"b", "a", "c"})
}

func TestResolveCandidates_FiltersBeforeSelectionAndPriority(t *testing.T) {
	disc := NewDiscovery()
	a, c, d := prNode("a"), prNode("c"), prNode("d")
	a.Models, c.Models, d.Models = []string{"llama"}, []string{"llama"}, []string{"mistral"}
	for _, n := range []Node{a, prNode("b"), c, d} {
		disc.AddManual(n)
	}
	p := testProxy(anyProfile(t), disc, 11435)
	p.soleFacade().SetSelected("d")
	p.SetPriority([]string{"d", "b", "c", "a"})
	assertOrder(t, candidateIDsForModel(p, "llama"), []string{"c", "a"})

	p.soleFacade().SetSelected("a")
	assertOrder(t, candidateIDsForModel(p, "llama"), []string{"a", "c"})
}

// TestResolveCandidates_EmptyReverts: an empty priority list reverts to the
// default stable ID order.
func TestResolveCandidates_EmptyReverts(t *testing.T) {
	p := prProxy(t, "c", "a", "b")
	p.SetPriority([]string{"c", "a"})
	p.SetPriority(nil) // clear
	assertOrder(t, candidateIDs(p), []string{"a", "b", "c"})
}

// TestSetPriority_CountAndCopy: SetPriority returns the stored length and
// PriorityList hands back an independent copy.
func TestSetPriority_CountAndCopy(t *testing.T) {
	p := prProxy(t, "a")
	require.Equal(t, 3, p.SetPriority([]string{"a", "b", "c"}), "SetPriority count")
	got := p.PriorityList()
	got[0] = "mutated"
	require.Equal(t, "a", p.PriorityList()[0], "PriorityList returned an aliased slice")
}

// TestHandleSetPriority_Response: the node/set-priority request returns {count}.
func TestHandleSetPriority_Response(t *testing.T) {
	rec := &recRW{}
	p := newTestProxy(anyProfile(t), NewCodec(rec), NewDiscovery(), 11435)

	id := json.RawMessage(`7`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/set-priority",
		Params:  json.RawMessage(`{"generation":1,"nodes":["x","y"]}`),
	})

	require.Contains(t, rec.String(), `"count":2`, "expected response with count=2")
	require.Equal(t, []string{"x", "y"}, p.PriorityList(), "stored priority")
}

// A generation is required, because this method is relayed verbatim from any
// client. An unversioned snapshot would clear the reservations without
// advancing the epoch, so a reservation taken before it would still match and
// release one taken after — leaving the node reading idle and attracting every
// later dispatch.
func TestHandleSetPriority_RejectsUnversionedSnapshot(t *testing.T) {
	rec := &recRW{}
	p := newTestProxy(anyProfile(t), NewCodec(rec), NewDiscovery(), 11435)

	// Establish a baseline plus a live reservation against it.
	applySnapshot(p, schedulerwire.Priority{Nodes: []string{"x", "y"}})
	_, held := p.reserveCandidate(p.soleFacade(), reservationCandidates("x", "y"))
	require.True(t, held.held, "no reservation was taken from the baseline")

	id := json.RawMessage(`8`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/set-priority",
		Params:  json.RawMessage(`{"nodes":["y","x"]}`),
	})

	require.Contains(t, rec.String(), `"code":-32602`, "unversioned snapshot was not rejected")
	// The baseline and the reservation both survive the rejection.
	got := p.PriorityList()
	require.Len(t, got, 2, "a rejected snapshot changed the baseline")
	assert.Equal(t, "x", got[0], "a rejected snapshot changed the baseline (%v)", got)
	assert.Equal(t, 1, reservationCount(p, held.nodeID), "a rejected snapshot cleared the reservation on")
}
