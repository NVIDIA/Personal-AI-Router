// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math/rand"
	"testing"
)

// A/B for the one routing rule mlx-proxy does not inherit from the proxy it was
// cloned from: an owner that already holds the requested model resident is
// preferred over one that merely has it on disk.
//
//	arm A  NVPAIR_MLX_ROUTING unset   residency preferred (shipped default)
//	arm B  NVPAIR_MLX_ROUTING=any     rank by load only -- what ollama-proxy and
//	                                  lmstudio-proxy do today (the control)
//
// Both arms run in one invocation over the same nodes and the same request
// sequence, because a number saved from an earlier run is not a control.
//
// What is measured is the routing DECISION, not inference: resolveCandidates is
// the whole policy, and driving it directly keeps multi-second generation
// variance out of a question that is really about counting. The physical cost of
// the reload this avoids is measured separately (bench/reload_cost.py); the two
// multiply:
//
//	latency saved per request = (miss-rate delta) x (measured reload seconds)
//
// Run it with:  go test -run TestRoutingPolicyAB -v .

var abModels = []string{
	"mlx-community/Llama-3.2-1B-Instruct-4bit",
	"mlx-community/Qwen3-4B-4bit",
	"mlx-community/Mistral-7B-Instruct-v0.3-4bit",
}

// abNode is a node holding every model on disk and exactly one resident, which
// is what mlx_lm.server can actually be in: one model in memory at a time.
func abNode(id string, resident string) Node {
	n := prNode(id)
	n.Models = append([]string(nil), abModels...)
	n.Loaded = []string{resident}
	return n
}

func runArm(t *testing.T, sequence []string, residentOf map[string]string) int {
	t.Helper()
	disc := NewDiscovery()
	for id, resident := range residentOf {
		disc.AddManual(abNode(id, resident))
	}
	p := testProxy(disc, 1235)

	hits := 0
	for _, model := range sequence {
		cands := p.resolveCandidates(model)
		if len(cands) == 0 {
			t.Fatalf("no candidate for %q: a node holding the model on disk must always be routable", model)
		}
		// Only the first candidate matters: the rest are the failover chain,
		// and a healthy node never falls through to them.
		if residentOf[cands[0].id] == model {
			hits++
		}
	}
	return hits
}

func TestRoutingPolicyAB(t *testing.T) {
	const requests = 600
	residentOf := map[string]string{}
	for i, m := range abModels {
		residentOf[fmt.Sprintf("node-%d", i)] = m
	}

	rng := rand.New(rand.NewSource(7))
	sequence := make([]string, requests)
	for i := range sequence {
		sequence[i] = abModels[rng.Intn(len(abModels))]
	}

	t.Setenv(routingPolicyEnv, "")
	armA := runArm(t, sequence, residentOf)

	t.Setenv(routingPolicyEnv, "any")
	armB := runArm(t, sequence, residentOf)

	rate := func(h int) float64 { return float64(h) / float64(requests) * 100 }
	t.Logf("%d requests, %d nodes, %d models", requests, len(residentOf), len(abModels))
	t.Logf("  A residency-preferred (default): %3d/%d resident hits (%.1f%%)", armA, requests, rate(armA))
	t.Logf("  B load-only (control)          : %3d/%d resident hits (%.1f%%)", armB, requests, rate(armB))
	t.Logf("  delta                          : %+.1f pp", rate(armA)-rate(armB))
	t.Logf("  multiply the delta by the measured reload seconds (bench/reload_cost.py)")
	t.Logf("  for the per-request latency the policy saves")

	// The arms have to actually differ, or the flag is not wired and the whole
	// comparison is measuring one policy twice.
	if armA <= armB {
		t.Errorf("residency preference did not beat the control (%d vs %d); is the arm switch wired?", armA, armB)
	}
	// Every request has a resident owner available here, so arm A should find
	// one every time. Anything less means ranking is overriding residency.
	if armA != requests {
		t.Errorf("arm A resident hits = %d, want %d: a resident owner existed for every request", armA, requests)
	}
}
