// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVLLMGroupReconcilePreservesConfirmedRanksWithRemainingOnlyElevation(t *testing.T) {
	a := vllmResourceFixture(t)
	b, binding, system := missingRankFixture(t)
	if err := os.MkdirAll(a.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	aReceiptPath := filepath.Join(a.st.installDir, "serving-rank.json")
	closed := vllmRankReceipt{RunID: binding.RunID, Generation: binding.Generation, Rank: 0, PlanDigest: binding.PlanDigest, State: "stopped", CleanupConfirmed: true}
	if err := writeVLLMJSON(a.st.installDir, aReceiptPath, closed); err != nil {
		t.Fatal(err)
	}
	aReceipt, err := os.ReadFile(aReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{RunID: binding.RunID, Generation: binding.Generation, PlanDigest: binding.PlanDigest, Plan: binding.Plan, State: "cleanup-required", Ranks: []vllmGroupRank{
		{NodeID: binding.Plan.Members[0].NodeID, Attempted: true, CleanupConfirmed: true},
		{NodeID: binding.Plan.Members[1].NodeID, Attempted: true},
	}}
	groupPath := filepath.Join(a.st.installDir, "serving-group.json")
	if err := writeVLLMJSON(a.st.installDir, groupPath, run); err != nil {
		t.Fatal(err)
	}
	a.st.opMu.Lock()
	g, err := newVLLMServingGroup(a.st, groupPath)
	a.st.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	originalFailure := g.status().Failure
	aCalls, bCalls, fences := 0, 0, 0
	boundFence := false
	g.call = func(ctx context.Context, got vllmGroupBinding, action string) error {
		if got.Rank == 0 {
			aCalls++
			return errors.New("confirmed A must not need another administrator request")
		}
		bCalls++
		if !reflect.DeepEqual(got, binding) || action != "stop" {
			t.Fatal("cleanup changed the retained operation or invoked new work")
		}
		authCtx := g.elevationContext(ctx)
		elevation := groupRequestElevation(authCtx, binding.Plan.Members[1].NodeID)
		if elevation == nil || !elevation.NonInteractive || groupRequestElevation(authCtx, binding.Plan.Members[0].NodeID) != nil {
			t.Fatal("Reconcile did not preserve B-only administrator scope")
		}
		result, err := closeMissingVLLMRank(ctx, b.st, got, action, func() (vllmRankSystemPlan, error) { return system, nil }, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
			fences++
			result := fenceResult(plan)
			if !boundFence {
				result.PlanHash = strings.Repeat("f", 64)
			}
			return result, nil
		})
		if err != nil {
			return err
		}
		return vllmGroupActionOutcome(vllmGroupRequest(got, action), result)
	}
	m := &Manager{exec: a.e}
	params := map[string]any{"runId": binding.RunID, "generation": binding.Generation, "elevation": []diagnosticPackageElevation{{NodeID: binding.Plan.Members[1].NodeID, NonInteractive: true}}}
	for _, valid := range []bool{false, true} {
		boundFence = valid
		reply := groupControlTestCall(t, m, "engine:vllm-group-reconcile", params)
		current := g.status()
		if (reply.Error == nil) != valid || current.CleanupConfirmed != valid || g.reserved() == valid || current.Ranks[1].CleanupConfirmed != valid {
			t.Fatal("unresolved B bypassed its exact fence or valid cleanup stayed held")
		}
		if aCalls != 0 || !current.Ranks[0].CleanupConfirmed || current.Failure != originalFailure {
			t.Fatal("Reconcile revisited confirmed A or erased retained failure")
		}
		if !valid {
			if _, err := readVLLMRankClosure(b.st, binding); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unbound B proof created a closure marker")
			}
		}
	}
	if bCalls != 2 || fences != 2 || rejectVLLMClosedRank(b.st, binding) == nil {
		t.Fatal("B cleanup did not use its real fence path or block late Prepare")
	}
	if reply := groupControlTestCall(t, m, "engine:vllm-group-reconcile", params); reply.Error != nil || aCalls != 0 || bCalls != 2 || fences != 2 {
		t.Fatal("completed Reconcile repeated participant work")
	}
	after, err := os.ReadFile(aReceiptPath)
	if err != nil || !bytes.Equal(aReceipt, after) {
		t.Fatal("confirmed A receipt was changed")
	}
}
