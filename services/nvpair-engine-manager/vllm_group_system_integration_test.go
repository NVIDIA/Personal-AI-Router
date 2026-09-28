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
	"time"
)

func TestVLLMGroupPrepareBeginsAfterStartUnlock(t *testing.T) {
	prepared := make(chan bool, 1)
	var group *vllmServingGroup
	group = vllmGroupTestOwner(t, func(_ context.Context, binding vllmGroupBinding, action string) error {
		if action == "prepare" && binding.Rank == 0 {
			available := group.engine.opMu.TryLock()
			if available {
				group.engine.opMu.Unlock()
			}
			prepared <- available
		}
		return nil
	})
	run := vllmGroupTestStart(t, group, context.Background(), 2)
	select {
	case available := <-prepared:
		if !available {
			t.Fatal("local Prepare raced its own Start operation lock")
		}
	case <-time.After(time.Second):
		t.Fatal("Prepare did not begin")
	}
	if err := group.stop(context.Background(), run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
}

func TestVLLMGroupAdmissionWaitsForTransientLifecycleOwner(t *testing.T) {
	st := &engineState{}
	st.opMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	acquired := make(chan bool, 1)
	go func() {
		ok := lockVLLMGroupOwner(ctx, st)
		if ok {
			st.opMu.Unlock()
		}
		acquired <- ok
	}()
	select {
	case <-acquired:
		t.Fatal("serving-group admission did not wait for the transient lifecycle owner")
	case <-time.After(40 * time.Millisecond):
	}
	st.opMu.Unlock()
	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("serving-group admission failed after the transient lifecycle owner released")
		}
	case <-time.After(time.Second):
		t.Fatal("serving-group admission did not resume after lifecycle release")
	}
}

func TestVLLMSystemCleanupRequiresFreshCredentialWithoutNormalSupervisor(t *testing.T) {
	f := vllmResourceFixture(t)
	rank := &vllmManagedRank{e: f.e, st: f.st, path: filepath.Join(t.TempDir(), "rank.json"),
		receipt: vllmRankReceipt{State: "cleanup-required", SystemPlan: &vllmRankSystemPlan{NodeID: "node-a"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rank.stopSystem(ctx, nil); err == nil {
		t.Fatal("unconfirmed native cleanup was accepted")
	}
	if rank.receipt.CleanupConfirmed {
		t.Fatal("failed cleanup claimed clean")
	}
}

func TestVLLMSystemCleanupUsesRootReconcileForDeadSupervisor(t *testing.T) {
	dead := &vllmRankSystemIdentity{PID: 2147483647, StartTicks: "historical", UID: os.Getuid()}
	action, supervisor := vllmSystemCleanupInvocation(dead)
	if action != "reconcile" || supervisor != nil {
		t.Fatalf("dead historical supervisor selected %q with %+v", action, supervisor)
	}
	action, supervisor = vllmSystemCleanupInvocation(nil)
	if action != "reconcile" || supervisor != nil {
		t.Fatalf("missing historical supervisor selected %q with %+v", action, supervisor)
	}
}

func TestVLLMSystemPreunitClosureRequiresBothProofFlags(t *testing.T) {
	good := vllmSystemRankResult{State: "closed", Tombstone: true, StartFenced: true, CleanupConfirmed: true}
	if !systemRankCleanupPassed(good) {
		t.Fatal("exact closed preunit outcome rejected")
	}
	for _, change := range []func(*vllmSystemRankResult){
		func(r *vllmSystemRankResult) { r.Tombstone = false },
		func(r *vllmSystemRankResult) { r.StartFenced = false },
		func(r *vllmSystemRankResult) { r.EffectsApplied = true },
		func(r *vllmSystemRankResult) { r.CleanupConfirmed = false },
	} {
		value := good
		change(&value)
		if systemRankCleanupPassed(value) {
			t.Fatal("incomplete closure accepted")
		}
	}
	b, placement := managedRankFixture(t, 1)
	f := vllmResourceFixture(t)
	rank := &vllmManagedRank{e: f.e, st: f.st, binding: b, placement: placement, receipt: vllmRankReceipt{RunID: b.RunID, Generation: b.Generation, PlanDigest: b.PlanDigest, Rank: b.Rank,
		State: "stopped", CleanupConfirmed: true, StartFenced: true, SystemEffectsApplied: false,
		SystemPlan: &vllmRankSystemPlan{RunID: b.RunID, Generation: b.Generation, Rank: b.Rank, PlanDigest: b.PlanDigest}}}
	result, err := rank.systemAction(context.Background(), b, "stop", nil)
	if err != nil || result.State != "stopped" || !result.CleanupConfirmed || result.EffectsApplied {
		t.Fatalf("preunit effect truth lost: %+v %v", result, err)
	}
}

func TestVLLMGroupElevationIsScopedAndNotDurable(t *testing.T) {
	plan := vllmGroupTestPlan(2)
	secret := "fixture-only-private-value"
	entries := []diagnosticPackageElevation{{NodeID: plan.Members[0].NodeID, ElevationPassword: secret}, {NodeID: plan.Members[1].NodeID, NonInteractive: true}}
	auth, err := newVLLMGroupElevation(plan, entries, true)
	if err != nil {
		t.Fatal(err)
	}
	if auth.takeForNode("unselected") != nil {
		t.Fatal("unselected node obtained elevation")
	}
	request := vllmGroupRequest(vllmGroupBinding{RunID: strings.Repeat("a", 32), Generation: 1, PlanDigest: strings.Repeat("b", 64), Plan: plan, Rank: 0}, "start")
	request.PlanDigest, _ = vllmGroupPlanDigest(plan)
	request.Elevation = auth.takeForNode(plan.Members[1].NodeID)
	if validateVLLMGroupPeerRequest(request) == nil {
		t.Fatal("another participant's administrator input accepted")
	}
	for _, value := range []any{vllmGroupReview{Plan: plan}, vllmGroupRun{Plan: plan}, vllmRankReceipt{State: "prepared"}} {
		body, _ := json.Marshal(value)
		if strings.Contains(string(body), secret) {
			t.Fatal("credential entered durable projection")
		}
	}
	first := auth.takeForNode(plan.Members[0].NodeID)
	if first == nil || auth.takeForNode(plan.Members[0].NodeID) != nil {
		t.Fatal("credential was not consumed exactly once")
	}
	auth.close()
	if auth.takeForNode(plan.Members[0].NodeID) != nil {
		t.Fatal("closed operation retained credential")
	}
}

func TestVLLMSystemInvocationKeepsAdministratorInputOutOfArguments(t *testing.T) {
	secret := "fixture-only-secret"
	plan := vllmRankSystemPlan{NodeID: "node-a"}
	argv, input, err := vllmSystemRankInvocation("start", plan, nil, &diagnosticPackageElevation{NodeID: "node-a", ElevationPassword: secret})
	defer clear(input)
	if err != nil || strings.Contains(strings.Join(argv, " "), secret) || !strings.HasPrefix(string(input), secret+"\n") {
		t.Fatalf("credential transport was not isolated: %v", err)
	}
	if _, _, err := vllmSystemRankInvocation("start", plan, nil, nil); err == nil {
		t.Fatal("missing admin choice became authority")
	}
}

func TestVLLMSystemRankDoesNotExposeStandaloneSelection(t *testing.T) {
	b, placement := managedRankFixture(t, 1)
	rank := &vllmManagedRank{binding: b, placement: placement, receipt: vllmRankReceipt{State: "started", SystemPlan: &vllmRankSystemPlan{Model: b.Plan.Model}}}
	data, err := rank.currentSystemModels(context.Background())
	if err != nil || string(data) != `{"data":[],"object":"list"}` {
		t.Fatalf("headless rank became a model owner: %s %v", data, err)
	}
	wrong := b
	wrong.Generation++
	if err := rank.stop(wrong); err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("wrong generation reached native Stop")
	}
}
