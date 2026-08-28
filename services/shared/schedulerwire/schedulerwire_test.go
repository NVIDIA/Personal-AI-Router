// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package schedulerwire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPriorityAcceptsLegacyNodesOnlyPayload(t *testing.T) {
	var got Priority
	if err := json.Unmarshal([]byte(`{"nodes":["a","b"]}`), &got); err != nil {
		t.Fatalf("unmarshal nodes-only priority: %v", err)
	}
	if !reflect.DeepEqual(got.Nodes, []string{"a", "b"}) {
		t.Fatalf("nodes = %v, want [a b]", got.Nodes)
	}
	if len(got.Ranks) != 0 {
		t.Fatalf("nodes-only ranks = %v, want empty", got.Ranks)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal nodes-only priority: %v", err)
	}
	if strings.Contains(string(encoded), `"ranks"`) {
		t.Fatalf("nodes-only encoding unexpectedly included ranks: %s", encoded)
	}
}

func TestEnginePriorityRoundTripsGPUAwareRanks(t *testing.T) {
	want := EnginePriority{
		Engine: "ollama",
		Nodes:  []string{"b", "a"},
		Ranks: []NodeRank{
			{ID: "b", Pending: 1, GPUPressure: 0, Rank: 0},
			{ID: "a", Pending: 4, GPUPressure: 3, Rank: 1},
		},
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal engine priority: %v", err)
	}
	if !strings.Contains(string(encoded), `"gpuPressure":3`) {
		t.Fatalf("encoded priority omitted gpuPressure: %s", encoded)
	}

	var got EnginePriority
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal engine priority: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestPriorityCopiesOwnTheirSlices(t *testing.T) {
	source := EnginePriority{
		Engine: "ollama",
		Nodes:  []string{"a"},
		Ranks:  []NodeRank{{ID: "a", Pending: 2}},
	}
	snapshot := source.Snapshot()
	clone := snapshot.Clone()

	source.Nodes[0] = "source-mutated"
	snapshot.Nodes[0] = "snapshot-mutated"
	source.Ranks[0].Pending = 9
	snapshot.Ranks[0].Pending = 8

	if clone.Nodes[0] != "a" || clone.Ranks[0].Pending != 2 {
		t.Fatalf("clone changed through an aliased slice: %#v", clone)
	}
}
