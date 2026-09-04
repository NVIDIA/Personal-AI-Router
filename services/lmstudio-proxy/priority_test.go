// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"testing"
)

// prRec is a thread-safe io.ReadWriter that records codec writes so a test can
// assert the response emitted for a request. Reads hit EOF immediately.
type prRec struct {
	mu sync.Mutex
	b  []byte
}

func (r *prRec) Read([]byte) (int, error) { return 0, io.EOF }

func (r *prRec) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	return len(p), nil
}

func (r *prRec) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.b) != "" && contains(string(r.b), s)
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// prNode builds a discovery Node with a single non-local address so
// resolveCandidates resolves it deterministically (single candidate → no TCP
// probe) and it never trips the loopback rewrite or self-forward guard. The
// octet is derived from the id's first byte so each id gets a distinct valid IP.
func prNode(id string) Node {
	return Node{ID: id, Addresses: []string{"192.0.2." + strconv.Itoa(int(id[0]))}, Port: 1234, Engine: "lmstudio"}
}

// prProxy returns a proxy whose discovery holds the given node ids.
func prProxy(t *testing.T, ids ...string) *Proxy {
	t.Helper()
	disc := NewDiscovery()
	for _, id := range ids {
		disc.AddManual(prNode(id))
	}
	return testProxy(disc, 1235)
}

func candidateIDs(p *Proxy) []string {
	return candidateIDsForModel(p, "")
}

func candidateIDsForModel(p *Proxy, model string) []string {
	cands := p.resolveCandidates(model)
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.id)
	}
	return out
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
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
	p.SetSelected("b")
	assertOrder(t, candidateIDs(p), []string{"b", "a", "c"})
}

func TestResolveCandidates_FiltersBeforeSelectionAndPriority(t *testing.T) {
	disc := NewDiscovery()
	a, c, d := prNode("a"), prNode("c"), prNode("d")
	a.Models, c.Models, d.Models = []string{"llama"}, []string{"llama"}, []string{"mistral"}
	for _, n := range []Node{a, prNode("b"), c, d} {
		disc.AddManual(n)
	}
	p := testProxy(disc, 1235)
	p.SetSelected("d")
	p.SetPriority([]string{"d", "b", "c", "a"})
	assertOrder(t, candidateIDsForModel(p, "llama"), []string{"c", "a"})

	p.SetSelected("a")
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
	if n := p.SetPriority([]string{"a", "b", "c"}); n != 3 {
		t.Fatalf("SetPriority count = %d, want 3", n)
	}
	got := p.PriorityList()
	got[0] = "mutated"
	if again := p.PriorityList(); again[0] != "a" {
		t.Fatalf("PriorityList returned an aliased slice: %v", again)
	}
}

// TestHandleSetPriority_Response: the node/set-priority request returns {count}.
func TestHandleSetPriority_Response(t *testing.T) {
	rec := &prRec{}
	p := NewProxy(NewCodec(rec), NewDiscovery(), 1235)

	id := json.RawMessage(`7`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/set-priority",
		Params:  json.RawMessage(`{"nodes":["x","y"]}`),
	})

	if !rec.has(`"count":2`) {
		t.Fatalf("expected response with count=2, got: %s", rec.b)
	}
	if got := p.PriorityList(); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("stored priority = %v, want [x y]", got)
	}
}
