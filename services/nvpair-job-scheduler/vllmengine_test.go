// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"testing"
)

// TestSchedulerEnginesCoversVLLM proves vLLM gets its own priority contract.
// A proxy only applies the snapshot published for the engine it is routing, so
// an engine missing from this list would route with no scheduler ordering at
// all and every request would fall back to the stable-ID pass.
func TestSchedulerEnginesCoversVLLM(t *testing.T) {
	for _, want := range []string{"ollama", "lmstudio", "vllm"} {
		if !slices.Contains(schedulerEngines, want) {
			t.Errorf("schedulerEngines is missing %q: %v", want, schedulerEngines)
		}
	}
}

// TestVLLMWorkloadsCountTowardTheNodeWideRanking proves vLLM work shares the
// node's queue depth with the other engines rather than being ranked
// separately: one GPU serves them all, so a node busy with vLLM must rank below
// an idle one for every engine, including Ollama.
func TestVLLMWorkloadsCountTowardTheNodeWideRanking(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"busy", "idle"},
		workload{ID: "1", Engine: "vllm", RunID: "v1", State: "running", OriginatedFrom: "x", ScheduledOn: "busy"},
		workload{ID: "2", Engine: "vllm", RunID: "v2", State: "queued", OriginatedFrom: "x", ScheduledOn: "busy"},
	)
	order, ranks := m.rank()
	if len(order) == 0 || order[len(order)-1] != "busy" {
		t.Fatalf("order = %v, want the vLLM-busy node last", order)
	}
	if pendingOf(ranks, "busy") != 2 {
		t.Fatalf("vLLM work was not counted node-wide: %+v", ranks)
	}

	m.recomputeAll(false)
	for _, engine := range schedulerEngines {
		got := rec.orders(engine)
		if len(got) != 1 {
			t.Fatalf("%s emissions = %d, want 1", engine, len(got))
		}
		if got[0][len(got[0])-1] != "busy" {
			t.Errorf("%s order = %v, want the busy node last", engine, got[0])
		}
	}
}
