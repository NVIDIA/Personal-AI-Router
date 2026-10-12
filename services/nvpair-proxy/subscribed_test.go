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

// TestSubscribedToNode covers the DirectoryNode -> routable Node projection:
// this engine's port is read from its own service key, and a node lacking that
// key, or an IP, is rejected.
//
// The two negative fixtures matter for different reasons and the old suites
// carried one each — Ollama rejected a non-engine service, LM Studio rejected
// the other engine. Both run for both engines here, because the second is what
// regresses if the projection ever loses its service-key filter.
func TestSubscribedToNode(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		other := otherEngine(t, tc)

		withService := noderec.DirectoryNode{
			HostUUID: "uuid-a",
			Name:     "host-a",
			IP:       "10.0.0.5",
			Models:   []string{"llama"},
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				tc.profile.DiscoveryService: {Port: tc.profile.FacadePort},
			},
		}
		got, ok := subscribedToNode(tc.profile, withService)
		if !ok {
			t.Fatal("node advertising this engine + IP should project")
		}
		if got.ID != "uuid-a" || got.Port != tc.profile.FacadePort || got.IP != "10.0.0.5" ||
			len(got.Models) != 1 || got.Models[0] != "llama" {
			t.Fatalf("unexpected projection: %+v", got)
		}

		noIP := withService
		noIP.IP = ""
		if _, ok := subscribedToNode(tc.profile, noIP); ok {
			t.Fatal("node without IP should not project")
		}

		// A node advertising only a non-engine service (node-info) is not a
		// routing target.
		niOnly := noderec.DirectoryNode{
			Name:     "host-c",
			IP:       "10.0.0.6",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14318}},
		}
		if _, ok := subscribedToNode(tc.profile, niOnly); ok {
			t.Fatal("node without this engine's service should not project")
		}

		// Nor is a node running only the *other* engine.
		otherOnly := noderec.DirectoryNode{
			Name: "host-e",
			IP:   "10.0.0.8",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				other.DiscoveryService: {Port: other.FacadePort},
			},
		}
		if _, ok := subscribedToNode(tc.profile, otherOnly); ok {
			t.Fatalf("a node running only %s should not project as a %s target",
				other.DisplayName, tc.profile.DisplayName)
		}

		// Per-engine attribution: a dual-engine node projects ONLY this
		// engine's models, never the cross-engine union, so a model served
		// solely via the other engine is not ranked as an owner here.
		dual := noderec.DirectoryNode{
			HostUUID: "uuid-d",
			Name:     "host-d",
			IP:       "10.0.0.7",
			Models:   []string{"mine", "theirs"},
			ModelsByEngine: map[string][]string{
				tc.profile.Name: {"mine"},
				other.Name:      {"theirs"},
			},
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				tc.profile.DiscoveryService: {Port: tc.profile.FacadePort},
			},
		}
		got, ok = subscribedToNode(tc.profile, dual)
		if !ok {
			t.Fatal("dual-engine node advertising this engine should project")
		}
		if len(got.Models) != 1 || got.Models[0] != "mine" {
			t.Fatalf("dual-engine projection Models = %v, want [mine] only", got.Models)
		}
	})
}

// TestSubscribedToNodeKeysByHostUUID: the routable Node keys on the stable
// hostUuid, not the hostname — so routing, scheduledOn, node selection, and the
// scheduler's priority list all survive a PC rename and never conflate two
// same-named machines. Host stays the hostname for display.
func TestSubscribedToNodeKeysByHostUUID(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		const uuid = "11111111-1111-1111-1111-111111111111"
		n := noderec.DirectoryNode{
			HostUUID: uuid,
			Name:     "host-a",
			IP:       "10.0.0.5",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				tc.profile.DiscoveryService: {Port: tc.profile.FacadePort},
			},
		}
		got, ok := subscribedToNode(tc.profile, n)
		if !ok {
			t.Fatal("node advertising this engine + IP should project")
		}
		if got.ID != uuid {
			t.Fatalf("ID = %q, want hostUuid %q", got.ID, uuid)
		}
		if got.Host != "host-a" {
			t.Fatalf("Host = %q, want hostname for display", got.Host)
		}
	})
}
