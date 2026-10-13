// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package schedulerwire

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPriorityAcceptsLegacyNodesOnlyPayload(t *testing.T) {
	var got Priority
	require.NoError(t, json.Unmarshal([]byte(`{"nodes":["a","b"]}`), &got), "unmarshal nodes-only priority")
	assert.Equal(t, []string{"a", "b"}, got.Nodes)
	require.Empty(t, got.Ranks, "nodes-only ranks")

	encoded, err := json.Marshal(got)
	require.NoError(t, err, "marshal nodes-only priority")
	assert.NotContains(t, string(encoded), `"ranks"`, "nodes-only encoding unexpectedly included ranks")
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
	require.NoError(t, err, "marshal engine priority")
	assert.Contains(t, string(encoded), `"gpuPressure":3`, "encoded priority omitted gpuPressure")

	var got EnginePriority
	require.NoError(t, json.Unmarshal(encoded, &got), "unmarshal engine priority")
	assert.Equal(t, want, got, "round trip")
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

	assert.Equal(t, "a", clone.Nodes[0], "clone changed through an aliased slice")
	assert.Equal(t, 2, clone.Ranks[0].Pending, "clone changed through an aliased slice")
}
