// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVLLMGroupCleanupFailurePersistsAndClearsOnlyForRetriedRanks(t *testing.T) {
	f := vllmResourceFixture(t)
	plan := vllmGroupTestPlan(3)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{
		RunID: strings.Repeat("d", 32), Generation: 13, PlanDigest: digest, Plan: plan,
		State: "cleanup-required", Failure: "original participant failure",
		Ranks: []vllmGroupRank{
			{NodeID: plan.Members[0].NodeID, Attempted: true, CleanupConfirmed: true},
			{NodeID: plan.Members[1].NodeID, Attempted: true},
			{NodeID: plan.Members[2].NodeID, Attempted: true},
		},
	}
	if err := os.MkdirAll(f.st.installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.st.installDir, vllmGroupJournalFile)
	if err := writeVLLMJSON(f.st.installDir, path, run); err != nil {
		t.Fatal(err)
	}
	f.st.opMu.Lock()
	g, err := newVLLMServingGroup(f.st, path)
	f.st.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	const password = "synthetic-admin-input"
	calls := [3]int{}
	g.call = func(ctx context.Context, binding vllmGroupBinding, action string) error {
		calls[binding.Rank]++
		if binding.Rank == 0 || action != "stop" {
			return errors.New("confirmed rank or non-cleanup action was retried")
		}
		elevation := groupRequestElevation(g.elevationContext(ctx), plan.Members[binding.Rank].NodeID)
		if elevation == nil {
			return errors.New("unresolved rank did not receive its operation-scoped administrator choice")
		}
		if elevation.NonInteractive {
			return classifyVLLMRankProcess(errors.New("exit status 1"), 0, []byte("sudo: a password is required"))
		}
		if elevation.ElevationPassword != password {
			return errors.New("password retry reached the wrong rank")
		}
		return nil
	}
	m := &Manager{exec: f.e}
	params := map[string]any{"runId": run.RunID, "generation": run.Generation}
	if reply := groupControlTestCall(t, m, "engine:vllm-group-reconcile", params); reply.Error == nil {
		t.Fatal("noninteractive sudo failures released the retained group")
	}
	held := g.status()
	if !g.reserved() || held.CleanupConfirmed || !held.Ranks[0].CleanupConfirmed || calls != [3]int{0, 1, 1} {
		t.Fatalf("noninteractive cleanup changed confirmed A or released the hold: calls=%v run=%+v", calls, held)
	}
	for _, rank := range held.Ranks[1:] {
		if rank.CleanupConfirmed || rank.CleanupFailure == nil || rank.CleanupFailure.StderrCode != "sudo_authentication_required" || !validVLLMRankStartFailure(rank.CleanupFailure) {
			t.Fatalf("unresolved rank lost its bounded sudo diagnosis: %+v", rank)
		}
	}
	persisted, err := readVLLMGroupStatus(f.st.installDir)
	if err != nil || !persisted.Reserved || persisted.Run == nil || persisted.Run.Ranks[1].CleanupFailure == nil || persisted.Run.Ranks[2].CleanupFailure == nil {
		t.Fatalf("bounded cleanup failures were not retained in status: %+v err=%v", persisted, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "password is required") || strings.Contains(string(raw), password) {
		t.Fatal("cleanup journal retained raw stderr or administrator input")
	}

	params["elevation"] = []diagnosticPackageElevation{
		{NodeID: plan.Members[1].NodeID, ElevationPassword: password},
		{NodeID: plan.Members[2].NodeID, ElevationPassword: password},
	}
	if reply := groupControlTestCall(t, m, "engine:vllm-group-reconcile", params); reply.Error != nil {
		t.Fatalf("bounded password retry did not close unresolved ranks: %+v", reply.Error)
	}
	closed := g.status()
	if g.reserved() || !closed.CleanupConfirmed || closed.State != "failed" || calls != [3]int{0, 2, 2} {
		t.Fatalf("retry repeated confirmed A or failed to close the group: calls=%v run=%+v", calls, closed)
	}
	for _, rank := range closed.Ranks {
		if !rank.CleanupConfirmed || rank.CleanupFailure != nil {
			t.Fatalf("successful exact-rank cleanup retained stale failure: %+v", rank)
		}
	}
	raw, err = json.Marshal(closed)
	if err != nil || strings.Contains(string(raw), "cleanupFailure") || strings.Contains(string(raw), password) {
		t.Fatal("terminal status retained stale cleanup failure or administrator input")
	}
}
