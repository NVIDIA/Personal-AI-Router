// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

// TestSubscribedToNode covers the DirectoryNode -> routable Node projection for
// the lm service, including per-engine model attribution: the proxy ranks on the
// node's LM Studio models only, never the cross-engine union.
func TestSubscribedToNode(t *testing.T) {
	withLM := noderec.DirectoryNode{
		HostUUID: "uuid-a",
		Name:     "host-a",
		IP:       "10.0.0.5",
		Models:   []string{"gguf"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceLMStudio: {Port: 1234},
		},
	}
	got, ok := subscribedToNode(withLM)
	if !ok {
		t.Fatal("node with lm + IP should project")
	}
	// No attribution -> fall back to the flat union (single-engine peer).
	if got.ID != "uuid-a" || got.Port != 1234 || got.IP != "10.0.0.5" ||
		len(got.Models) != 1 || got.Models[0] != "gguf" {
		t.Fatalf("unexpected projection: %+v", got)
	}

	noIP := withLM
	noIP.IP = ""
	if _, ok := subscribedToNode(noIP); ok {
		t.Fatal("node without IP should not project")
	}

	// A node advertising only a non-lm service must not be an lm routing target.
	olOnly := noderec.DirectoryNode{
		Name:     "host-b",
		IP:       "10.0.0.6",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
	}
	if _, ok := subscribedToNode(olOnly); ok {
		t.Fatal("node without lm should not project")
	}

	// Per-engine attribution: a dual-engine node projects ONLY its LM Studio
	// models, never the union — so an Ollama-only model isn't ranked as an
	// LM Studio owner.
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
			noderec.ServiceLMStudio: {Port: 1234},
		},
	}
	got, ok = subscribedToNode(dual)
	if !ok {
		t.Fatal("dual-engine node with lm should project")
	}
	if len(got.Models) != 1 || got.Models[0] != "lmstudio-model" {
		t.Fatalf("dual-engine projection Models = %v, want [lmstudio-model] only", got.Models)
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
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceLMStudio: {Port: 1234}},
	}
	got, ok := subscribedToNode(n)
	if !ok {
		t.Fatal("node with lm + IP should project")
	}
	if got.ID != uuid {
		t.Fatalf("ID = %q, want hostUuid %q", got.ID, uuid)
	}
	if got.Host != "host-a" {
		t.Fatalf("Host = %q, want hostname for display", got.Host)
	}
}
