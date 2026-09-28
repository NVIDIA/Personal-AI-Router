// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestMissingLocalCoordinatorReconcileUsesActualHeldLock(t *testing.T) {
	f, b, system := missingRankFixture(t)
	b.Rank = 0
	system.Rank = 0
	system.NodeID = b.Plan.Members[0].NodeID
	g, err := newVLLMServingGroup(f.st, filepath.Join(f.st.installDir, "test-group.json"))
	if err != nil {
		t.Fatal(err)
	}
	g.held = true
	g.run = vllmGroupRun{RunID: b.RunID, Generation: b.Generation, PlanDigest: b.PlanDigest, Plan: b.Plan, State: "cleanup-required", Ranks: []vllmGroupRank{{NodeID: b.Plan.Members[0].NodeID, Attempted: true}, {NodeID: b.Plan.Members[1].NodeID}}}
	var retained context.Context
	calls := 0
	g.call = func(ctx context.Context, binding vllmGroupBinding, action string) error {
		if action != "stop" || binding.Rank != 0 {
			t.Fatal("unexpected participant call")
		}
		forwarded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		retained = forwarded
		_, err := closeMissingVLLMRank(forwarded, f.st, binding, action, func() (vllmRankSystemPlan, error) { return system, nil }, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
			calls++
			if f.st.opMu.TryLock() {
				f.st.opMu.Unlock()
				t.Fatal("coordinator mutex was not held")
			}
			result := fenceResult(plan)
			result.Unit = fmt.Sprintf("nvpair-vllm-rank-%s-%d-%d.service", plan.RunID, plan.Generation, plan.Rank)
			return result, nil
		})
		return err
	}
	if err := g.reconcile(context.Background(), b.RunID, b.Generation); err != nil {
		t.Fatal("local explicit Reconcile deadlocked/rejected its own lock", err)
	}
	if calls != 1 || g.reserved() || !g.status().CleanupConfirmed {
		t.Fatal("local missing coordinator was not reconciled")
	}
	if retained == nil || vllmEngineLockHeld(context.WithoutCancel(retained), f.st) {
		t.Fatal("lock lease outlived synchronous cleanup")
	}
	if !f.st.opMu.TryLock() {
		t.Fatal("coordinator lock leaked")
	}
	f.st.opMu.Unlock()
}
func TestMissingRankLockLeaseCannotAuthorizeAnotherEngine(t *testing.T) {
	f, b, system := missingRankFixture(t)
	different := &engineState{}
	ctx, revoke := vllmHeldEngineContext(context.Background(), different)
	defer revoke()
	f.st.opMu.Lock()
	defer f.st.opMu.Unlock()
	_, err := closeMissingVLLMRank(ctx, f.st, b, "reconcile", func() (vllmRankSystemPlan, error) { t.Fatal("wrong-engine lease reached factory"); return system, nil }, func(vllmRankSystemPlan) (vllmSystemRankResult, error) {
		t.Fatal("wrong-engine lease reached native fence")
		return vllmSystemRankResult{}, nil
	})
	if err == nil {
		t.Fatal("wrong-engine lease bypassed mutex")
	}
}
