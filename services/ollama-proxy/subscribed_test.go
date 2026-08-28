// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

// TestSubscribedOverlayMerge covers the relay-sourced routing overlay: the broker
// pushes the full filtered set, SetSubscribed replaces the overlay wholesale, and
// subscribed nodes merge into Nodes() alongside the separate manual overlay. A
// later snapshot omitting a node drops it while manual nodes survive. The browser
// isn't running, so Nodes() reflects only the overlays.
func TestSubscribedOverlayMerge(t *testing.T) {
	d := NewDiscovery()

	d.AddManual(Node{ID: "m1", Port: 11434, Addresses: []string{"10.0.0.1"}})

	d.SetSubscribed([]Node{{ID: "s1", Port: 11434, Addresses: []string{"10.0.0.2"}, IP: "10.0.0.2"}})

	nodes := d.Nodes()
	ids := map[string]bool{}
	for _, n := range nodes {
		ids[n.ID] = true
	}
	if !ids["m1"] || !ids["s1"] {
		t.Fatalf("Nodes() should include both manual and subscribed entries, got %v", ids)
	}

	// A subsequent snapshot that omits s1 drops it (wholesale replace); the manual
	// overlay is untouched.
	d.SetSubscribed(nil)
	for _, n := range d.Nodes() {
		if n.ID == "s1" {
			t.Fatal("s1 should be gone from Nodes() after a snapshot omitting it")
		}
	}
	if !d.IsManual("m1") {
		t.Fatal("manual node m1 should survive a subscribed-set replace")
	}
}

// TestSubscribedDiff covers the diff SetSubscribed returns so the proxy can emit
// node/discovered|updated|removed for the relay-fed set (the signal the UI uses to
// show which peers run this engine).
func TestSubscribedDiff(t *testing.T) {
	d := NewDiscovery()

	s1 := Node{ID: "s1", Port: 11434, Addresses: []string{"10.0.0.2"}, IP: "10.0.0.2"}
	disc, upd, rem := d.SetSubscribed([]Node{s1})
	if len(disc) != 1 || disc[0].ID != "s1" || len(upd) != 0 || len(rem) != 0 {
		t.Fatalf("first snapshot: want discovered=[s1], got disc=%v upd=%v rem=%v", disc, upd, rem)
	}

	// Same set again: no events.
	disc, upd, rem = d.SetSubscribed([]Node{s1})
	if len(disc)+len(upd)+len(rem) != 0 {
		t.Fatalf("unchanged snapshot should emit nothing, got disc=%v upd=%v rem=%v", disc, upd, rem)
	}

	// Model inventory is routing data; changing it must update the subscribed
	// overlay even when the endpoint is unchanged.
	s1Models := s1
	s1Models.Models = []string{"llama"}
	disc, upd, rem = d.SetSubscribed([]Node{s1Models})
	if len(disc) != 0 || len(upd) != 1 || upd[0].ID != "s1" || len(rem) != 0 {
		t.Fatalf("changed models: want updated=[s1], got disc=%v upd=%v rem=%v", disc, upd, rem)
	}

	// Changed IP: an update.
	s1b := s1Models
	s1b.IP = "10.0.0.9"
	s1b.Addresses = []string{"10.0.0.9"}
	disc, upd, rem = d.SetSubscribed([]Node{s1b})
	if len(disc) != 0 || len(upd) != 1 || upd[0].ID != "s1" || len(rem) != 0 {
		t.Fatalf("changed node: want updated=[s1], got disc=%v upd=%v rem=%v", disc, upd, rem)
	}

	// Dropped from the snapshot: a removal.
	disc, upd, rem = d.SetSubscribed(nil)
	if len(disc) != 0 || len(upd) != 0 || len(rem) != 1 || rem[0].ID != "s1" {
		t.Fatalf("omitted node: want removed=[s1], got disc=%v upd=%v rem=%v", disc, upd, rem)
	}
}

// TestSubscribedToNode covers the DirectoryNode -> routable Node projection: the
// ol engine port is read from the service key, and a node without ol (or without
// an IP) is rejected.
func TestSubscribedToNode(t *testing.T) {
	withOl := noderec.DirectoryNode{
		HostUUID: "uuid-a",
		Name:     "host-a",
		IP:       "10.0.0.5",
		Models:   []string{"llama"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceOllama: {Port: 11434},
		},
	}
	got, ok := subscribedToNode(withOl)
	if !ok {
		t.Fatal("node with ol + IP should project")
	}
	if got.ID != "uuid-a" || got.Port != 11434 || got.IP != "10.0.0.5" || len(got.Models) != 1 || got.Models[0] != "llama" {
		t.Fatalf("unexpected projection: %+v", got)
	}

	noIP := withOl
	noIP.IP = ""
	if _, ok := subscribedToNode(noIP); ok {
		t.Fatal("node without IP should not project")
	}

	// A node that advertises only a non-ol service (e.g. node-info) must not be
	// treated as an ollama routing target.
	niOnly := noderec.DirectoryNode{
		Name:     "host-c",
		IP:       "10.0.0.6",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14318}},
	}
	if _, ok := subscribedToNode(niOnly); ok {
		t.Fatal("node without ol should not project")
	}

	// Per-engine attribution: a dual-engine node projects ONLY its Ollama models
	// into the ol proxy's routing inventory, never the cross-engine union — so a
	// model served solely via LM Studio isn't ranked as an Ollama owner.
	dual := noderec.DirectoryNode{
		HostUUID: "uuid-d",
		Name:     "host-d",
		IP:       "10.0.0.7",
		Models:   []string{"ollama-model", "lmstudio-model"},
		ModelsByEngine: map[string][]string{
			"ollama":   {"ollama-model"},
			"lmstudio": {"lmstudio-model"},
		},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceOllama: {Port: 11434},
		},
	}
	got, ok = subscribedToNode(dual)
	if !ok {
		t.Fatal("dual-engine node with ol should project")
	}
	if len(got.Models) != 1 || got.Models[0] != "ollama-model" {
		t.Fatalf("dual-engine projection Models = %v, want [ollama-model] only", got.Models)
	}
}

// TestSubscribedToNodeKeysByHostUUID: the routable Node keys on the stable
// hostUuid, not the hostname — so routing, scheduledOn, node selection, and the
// scheduler's priority list all survive a PC rename and never conflate two
// same-named machines. Host stays the hostname for display.
func TestSubscribedToNodeKeysByHostUUID(t *testing.T) {
	const uuid = "11111111-1111-1111-1111-111111111111"
	n := noderec.DirectoryNode{
		HostUUID: uuid,
		Name:     "host-a",
		IP:       "10.0.0.5",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
	}
	got, ok := subscribedToNode(n)
	if !ok {
		t.Fatal("node with ol + IP should project")
	}
	if got.ID != uuid {
		t.Fatalf("ID = %q, want hostUuid %q", got.ID, uuid)
	}
	if got.Host != "host-a" {
		t.Fatalf("Host = %q, want hostname for display", got.Host)
	}
}
