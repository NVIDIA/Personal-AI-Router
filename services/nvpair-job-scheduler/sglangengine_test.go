// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// TestSGLangWorkloadsCountTowardTheNodeWideRanking proves SGLang work shares the
// node's queue depth with the other engines rather than being ranked separately:
// one GPU serves them all, so a node busy with SGLang must rank below an idle
// one for every engine, Ollama included. It is the same guarantee the vLLM case
// makes, asserted for the engine tag SGLang workloads actually carry.
func TestSGLangWorkloadsCountTowardTheNodeWideRanking(t *testing.T) {
	rec := &capRW{}
	m := mgrWith(rec, []string{"busy", "idle"},
		workload{ID: "1", Engine: "sglang", RunID: "s1", State: "running", OriginatedFrom: "x", ScheduledOn: "busy"},
		workload{ID: "2", Engine: "sglang", RunID: "s2", State: "queued", OriginatedFrom: "x", ScheduledOn: "busy"},
	)
	order, ranks := m.rank()
	if len(order) == 0 || order[len(order)-1] != "busy" {
		t.Fatalf("order = %v, want the SGLang-busy node last", order)
	}
	if pendingOf(ranks, "busy") != 2 {
		t.Fatalf("SGLang work was not counted node-wide: %+v", ranks)
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
