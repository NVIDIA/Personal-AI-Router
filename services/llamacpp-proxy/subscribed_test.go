// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

// TestSubscribedToNode covers the DirectoryNode -> routable Node projection for
// the lc service, including per-engine model attribution: the proxy ranks on the
// node's llama.cpp models only, never the cross-engine union.
func TestSubscribedToNode(t *testing.T) {
	withLC := noderec.DirectoryNode{
		HostUUID: "uuid-a",
		Name:     "host-a",
		IP:       "10.0.0.5",
		Models:   []string{"gguf"},
		LoadedByEngine: map[string][]string{
			"llamacpp": {"gguf"},
		},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceLlamaCpp: {Port: 1234},
		},
	}
	got, ok := subscribedToNode(withLC)
	if !ok {
		t.Fatal("node with lc + IP should project")
	}
	if got.ID != "uuid-a" || got.Port != 1234 || got.IP != "10.0.0.5" ||
		len(got.Models) != 1 || got.Models[0] != "gguf" {
		t.Fatalf("unexpected projection: %+v", got)
	}

	noIP := withLC
	noIP.IP = ""
	if _, ok := subscribedToNode(noIP); ok {
		t.Fatal("node without IP should not project")
	}

	// A node advertising only a non-lc service must not be an lc routing target.
	olOnly := noderec.DirectoryNode{
		Name:     "host-b",
		IP:       "10.0.0.6",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
	}
	if _, ok := subscribedToNode(olOnly); ok {
		t.Fatal("node without lc should not project")
	}

	// Per-engine attribution: a dual-engine node projects ONLY its loaded
	// llama.cpp models, never the union — so an Ollama-only model isn't ranked
	// as a llama.cpp owner.
	dual := noderec.DirectoryNode{
		HostUUID: "uuid-d",
		Name:     "host-d",
		IP:       "10.0.0.7",
		Models:   []string{"ollama-model", "llamacpp-model"},
		ModelsByEngine: map[string][]string{
			"ollama":   {"ollama-model"},
			"llamacpp": {"llamacpp-model"},
		},
		LoadedByEngine: map[string][]string{
			"llamacpp": {"llamacpp-model"},
		},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceLlamaCpp: {Port: 1234},
		},
	}
	got, ok = subscribedToNode(dual)
	if !ok {
		t.Fatal("dual-engine node with lc should project")
	}
	if len(got.Models) != 1 || got.Models[0] != "llamacpp-model" {
		t.Fatalf("dual-engine projection Models = %v, want [llamacpp-model] only", got.Models)
	}
}

// TestSubscribedToNodeKeysByHostUUID: the routable Node keys on the stable
// hostUuid, not the hostname, so routing/scheduledOn/selection survive a PC
// rename and never conflate same-named machines. Host stays the hostname for
// display.
func TestSubscribedToNodeKeysByHostUUID(t *testing.T) {
	const uuid = "22222222-2222-2222-2222-222222222222"
	n := noderec.DirectoryNode{
		HostUUID: uuid,
		Name:     "host-a",
		IP:       "10.0.0.5",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceLlamaCpp: {Port: 1234}},
	}
	got, ok := subscribedToNode(n)
	if !ok {
		t.Fatal("node with lc + IP should project")
	}
	if got.ID != uuid {
		t.Fatalf("ID = %q, want hostUuid %q", got.ID, uuid)
	}
	if got.Host != "host-a" {
		t.Fatalf("Host = %q, want hostname for display", got.Host)
	}
}
