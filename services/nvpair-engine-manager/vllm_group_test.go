// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func vllmGroupTestPlan(n int) vllmGroupPlan {
	model := "example/model@" + strings.Repeat("a", 40)
	p := vllmGroupPlan{Coordinator: "node-a", Model: model, Runtime: vllmManagedVersion, Limits: vllmGroupLimitsForModel(model)}
	p.Topology = vllmGroupTopology{TensorParallel: 2, PipelineParallel: 1, DataParallel: 1, ConfigSHA256: strings.Repeat("d", 64)}
	if n == 3 {
		p.Topology.TensorParallel = 1
		p.Topology.PipelineParallel = 3
	}
	for i := range n {
		p.Members = append(p.Members, vllmGroupMember{NodeID: fmt.Sprintf("node-%c", 'a'+i), PinSHA256: strings.Repeat(fmt.Sprint(i+1), 64), GPUUUID: fmt.Sprintf("GPU-%08x-0000-0000-0000-000000000001", i+1), ModelDigest: strings.Repeat("a", 64), RuntimeDigest: strings.Repeat("b", 64), RuntimeCompatibilitySHA256: strings.Repeat("e", 64)})
	}
	return p
}

func vllmGroupTestOwner(t *testing.T, call vllmGroupCall) *vllmServingGroup {
	t.Helper()
	st := &engineState{}
	g, err := newVLLMServingGroup(st, filepath.Join(t.TempDir(), "vllm-group.json"))
	if err != nil {
		t.Fatal(err)
	}
	g.call = call
	return g
}

func vllmGroupTestStart(t *testing.T, g *vllmServingGroup, ctx context.Context, n int) vllmGroupRun {
	t.Helper()
	review, err := g.reviewPlan(vllmGroupTestPlan(n))
	if err != nil {
		t.Fatal(err)
	}
	run, err := g.start(ctx, review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestVLLMGroupReviewBindsOrderDigestsAndStaysNativeUnavailable(t *testing.T) {
	g := vllmGroupTestOwner(t, nil)
	p := vllmGroupTestPlan(3)
	review, err := g.reviewPlan(p)
	if err != nil || review.ActivationEnabled || review.Reason != errVLLMGroupNativeUnavailable.Error() {
		t.Fatalf("native capability misreported: %+v %v", review, err)
	}
	p.Members[1].NodeID = "changed-input"
	review.Plan.Members[2].NodeID = "changed-return"
	if g.review.Plan.Members[1].NodeID != "node-b" || g.review.Plan.Members[2].NodeID != "node-c" {
		t.Fatal("review aliases caller memory")
	}
	if _, err := g.start(context.Background(), review.ReviewID); !errors.Is(err, errVLLMGroupNativeUnavailable) || g.reserved() {
		t.Fatalf("production start was not refused before reservation: %v", err)
	}
	if _, err := os.Stat(g.path); !os.IsNotExist(err) {
		t.Fatal("native refusal created a run journal")
	}
	for _, mutate := range []func(*vllmGroupPlan){
		func(p *vllmGroupPlan) { p.Members = p.Members[:1] },
		func(p *vllmGroupPlan) { p.Coordinator = "node-b" },
		func(p *vllmGroupPlan) { p.Members[1].PinSHA256 = p.Members[0].PinSHA256 },
		func(p *vllmGroupPlan) { p.Members[1].GPUUUID = p.Members[0].GPUUUID },
		func(p *vllmGroupPlan) { p.Members[1].ModelDigest = strings.Repeat("c", 64) },
		func(p *vllmGroupPlan) { p.Members[1].RuntimeCompatibilitySHA256 = strings.Repeat("c", 64) },
		func(p *vllmGroupPlan) { p.Runtime = "unknown" },
	} {
		invalid := vllmGroupTestPlan(3)
		mutate(&invalid)
		if _, err := g.reviewPlan(invalid); err == nil {
			t.Fatalf("accepted invalid plan: %+v", invalid)
		}
	}
	ordered := vllmGroupTestPlan(3)
	a, _ := vllmGroupPlanDigest(ordered)
	ordered.Members[1], ordered.Members[2] = ordered.Members[2], ordered.Members[1]
	b, _ := vllmGroupPlanDigest(ordered)
	if a == b {
		t.Fatal("rank order is not included in the plan digest")
	}
}

func TestVLLMGroupCoordinatorEligibilityAndImmediateCancellation(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var mu sync.Mutex
				starts, stops := 0, 0
				g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
					mu.Lock()
					defer mu.Unlock()
					if action == "start" {
						starts++
					}
					if action == "stop" {
						stops++
					}
					if action == "ready" && (starts != count || b.Rank != 0) {
						return errors.New("readiness before all launches or not coordinator")
					}
					return nil
				})
				run := vllmGroupTestStart(t, g, ctx, count)
				synctest.Wait()
				if !g.coordinatorEligible("node-a", run.RunID, run.Generation) || g.coordinatorEligible("node-b", run.RunID, run.Generation) || g.coordinatorEligible("node-a", run.RunID, run.Generation+1) {
					t.Fatal("coordinator-only generation eligibility failed")
				}
				cancel()
				if g.coordinatorEligible("node-a", run.RunID, run.Generation) {
					t.Fatal("canceled context remains eligible before cleanup is scheduled")
				}
				if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
					t.Fatal(err)
				}
				if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
					t.Fatal(err)
				}
				before, _ := os.ReadFile(g.path)
				if err := g.reconcile(context.Background(), run.RunID, run.Generation); err != nil {
					t.Fatal(err)
				}
				after, _ := os.ReadFile(g.path)
				if string(before) != string(after) || !validVLLMGroupRun(g.status()) {
					t.Fatal("repeat clean stop/reconcile changed valid retained ownership")
				}
				mu.Lock()
				defer mu.Unlock()
				if stops != count || g.reserved() {
					t.Fatalf("cleanup=%d held=%v", stops, g.reserved())
				}
			})
		})
	}
}

func TestVLLMGroupStopCancelsPendingPrepareWithoutLaunchingRanks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered := make(chan struct{})
		starts, stops := 0, 0
		g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
			switch action {
			case "prepare":
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			case "start":
				starts++
			case "stop":
				stops++
			}
			return nil
		})
		run := vllmGroupTestStart(t, g, ctx, 3)
		<-entered
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		if starts != 0 || stops != 1 || g.reserved() || g.status().State != "stopped" {
			t.Fatalf("pending cancellation lost ownership: starts=%d stops=%d %+v", starts, stops, g.status())
		}
	})
}

func TestVLLMGroupStopWithdrawsBeforeTimedOutJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		releaseCleanup := make(chan struct{})
		g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
			if action == "stop" {
				select {
				case <-releaseCleanup:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		run := vllmGroupTestStart(t, g, ctx, 2)
		synctest.Wait()
		joinCtx, stopJoin := context.WithCancel(context.Background())
		stopJoin()
		if err := g.stop(joinCtx, run.RunID, run.Generation); !errors.Is(err, context.Canceled) {
			t.Fatalf("join should remain incomplete: %v", err)
		}
		if g.coordinatorEligible("node-a", run.RunID, run.Generation) || g.status().State != "stopping" || !g.reserved() {
			t.Fatal("accepted stop kept eligibility or released its reservation before cleanup")
		}
		close(releaseCleanup)
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
	})
}

func TestVLLMGroupFailedStartCleansEveryAttemptedRankAndRetainsUncertainty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var stopped []int
		g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
			if action == "start" && b.Rank == 1 {
				return errors.New("lost start acknowledgement")
			}
			if action == "stop" {
				stopped = append(stopped, b.Rank)
				if b.Rank == 1 {
					return errors.New("cleanup unconfirmed")
				}
			}
			return nil
		})
		run := vllmGroupTestStart(t, g, ctx, 3)
		synctest.Wait()
		if !reflect.DeepEqual(stopped, []int{2, 1, 0}) || !g.reserved() || g.status().State != "cleanup-required" {
			t.Fatalf("lost partial ownership: stopped=%v %+v", stopped, g.status())
		}
		retainedFailure := g.status().Failure
		if _, err := g.reviewPlan(vllmGroupTestPlan(2)); err == nil {
			t.Fatal("uncertain cleanup admitted another plan")
		}
		restarted, err := newVLLMServingGroup(&engineState{}, g.path)
		if err != nil || !restarted.reserved() || restarted.coordinatorEligible("node-a", run.RunID, run.Generation) {
			t.Fatalf("restart invented readiness/cleanup: %v", err)
		}
		if restarted.status().Failure != retainedFailure || restarted.status().Failure == "" {
			t.Fatalf("restart replaced retained failure history: before=%q after=%q", retainedFailure, restarted.status().Failure)
		}
		if err := restarted.reconcile(context.Background(), run.RunID, run.Generation); err == nil || !restarted.reserved() {
			t.Fatal("unavailable native recovery released ownership")
		}
		restarted.call = func(ctx context.Context, b vllmGroupBinding, action string) error {
			if action == "stop" {
				var journal vllmGroupRun
				if err := readVLLMJSON(filepath.Dir(g.path), g.path, 32<<10, &journal); err != nil || !validVLLMGroupRun(journal) {
					return errors.New("invalid intermediate journal")
				}
			}
			return nil
		}
		if err := restarted.reconcile(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		next := vllmGroupTestStart(t, restarted, ctx, 2)
		synctest.Wait()
		if next.Generation != run.Generation+1 || next.RunID == run.RunID {
			t.Fatal("restart reused the old generation")
		}
		if err := restarted.stop(context.Background(), run.RunID, run.Generation); err == nil || !restarted.coordinatorEligible("node-a", next.RunID, next.Generation) {
			t.Fatal("stale stop reached a new generation")
		}
		if err := restarted.stop(context.Background(), next.RunID, next.Generation); err != nil {
			t.Fatal(err)
		}
	})
}

func TestVLLMGroupRestartPreservesExactHeldFailureWithoutRewritingJournal(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "vllm-group.json")
	plan := vllmGroupTestPlan(3)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{
		RunID:      "facfa61ad839f1108ea0972f3f389b3c",
		Generation: 13,
		PlanDigest: digest,
		Plan:       plan,
		State:      "cleanup-required",
		Failure:    "a participant action failed; all owned ranks require cleanup",
	}
	for _, member := range plan.Members {
		run.Ranks = append(run.Ranks, vllmGroupRank{NodeID: member.NodeID, Attempted: true})
	}
	if err := writeVLLMJSON(dir, filename, run); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	g, err := newVLLMServingGroup(&engineState{}, filename)
	if err != nil {
		t.Fatal(err)
	}
	observed := g.status()
	if observed.RunID != run.RunID || observed.Generation != run.Generation || observed.PlanDigest != run.PlanDigest || observed.State != "cleanup-required" || observed.Failure != run.Failure || !g.reserved() {
		t.Fatalf("held operation changed across rehydration: %+v", observed)
	}
	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rehydration rewrote the retained journal")
	}
	g.call = func(_ context.Context, _ vllmGroupBinding, action string) error {
		if action != "stop" {
			t.Fatalf("unexpected recovery action %q", action)
		}
		return nil
	}
	if err := g.reconcile(context.Background(), run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
	closed := g.status()
	if closed.State != "failed" || !closed.CleanupConfirmed || closed.Failure != run.Failure {
		t.Fatalf("reconcile did not preserve terminal failure history: %+v", closed)
	}
}

func TestVLLMGroupRestartSynthesizesFailureOnlyWhenRetainedFailureIsEmpty(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "vllm-group.json")
	plan := vllmGroupTestPlan(2)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{RunID: strings.Repeat("a", 32), Generation: 2, PlanDigest: digest, Plan: plan, State: "stopping"}
	for _, member := range plan.Members {
		run.Ranks = append(run.Ranks, vllmGroupRank{NodeID: member.NodeID, Attempted: true})
	}
	if err := writeVLLMJSON(dir, filename, run); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filename)
	g, err := newVLLMServingGroup(&engineState{}, filename)
	if err != nil {
		t.Fatal(err)
	}
	if g.status().Failure != "prior owner ended; reconcile every attempted rank before another start" {
		t.Fatalf("empty retained failure did not receive the recovery explanation: %+v", g.status())
	}
	after, _ := os.ReadFile(filename)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failure synthesis rewrote the retained journal during load")
	}
}

func TestVLLMGroupReadinessFailureAndJournalFailureFailClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stops := 0
		g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
			if action == "ready" {
				return errors.New("collective readiness failed")
			}
			if action == "stop" {
				stops++
			}
			return nil
		})
		run := vllmGroupTestStart(t, g, ctx, 2)
		synctest.Wait()
		if stops != 2 || g.status().State != "failed" || g.reserved() || g.coordinatorEligible("node-a", run.RunID, run.Generation) {
			t.Fatal("readiness failure was published or leaked ranks")
		}
	})
	calls := 0
	g := vllmGroupTestOwner(t, func(context.Context, vllmGroupBinding, string) error { calls++; return nil })
	g.path = t.TempDir() // Atomic replacement of a directory must fail on Windows and Linux.
	review, err := g.reviewPlan(vllmGroupTestPlan(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.start(context.Background(), review.ReviewID); err == nil || calls != 0 || !g.reserved() {
		t.Fatal("journal failure permitted effects or lost its hold")
	}
}

func TestVLLMGroupReservationBlocksExistingMutationsWithoutEffects(t *testing.T) {
	f := vllmResourceFixture(t)
	g, err := newVLLMServingGroup(f.st, filepath.Join(t.TempDir(), "group.json"))
	if err != nil {
		t.Fatal(err)
	}
	g.held = true // Models an unresolved persisted participant; no fake engine runs.
	beforePort := f.st.port
	for name, invoke := range map[string]func() error{
		"uninstall": func() error { return f.e.Uninstall(context.Background(), "vllm") },
		"port":      func() error { _, err := f.e.SetPort(context.Background(), "vllm", 8123); return err },
		"resources": func() error {
			_, err := f.e.Action(context.Background(), "vllm", "set_resource_settings", []byte(`{"max_model_len":1024}`))
			return err
		},
		"start": func() error { return f.e.Start(context.Background(), "vllm") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := invoke(); err == nil || !strings.Contains(err.Error(), "serving-group reservation") {
				t.Fatalf("reservation did not reject mutation: %v", err)
			}
		})
	}
	if len(f.commands) != 0 || f.st.port != beforePort {
		t.Fatal("reserved mutation reached commands or settings")
	}
}

func TestVLLMGroupExpiredReviewAndInvalidRetainedOwnerStayBlocked(t *testing.T) {
	g := vllmGroupTestOwner(t, func(context.Context, vllmGroupBinding, string) error {
		t.Error("expired review reached participant")
		return nil
	})
	r, err := g.reviewPlan(vllmGroupTestPlan(2))
	if err != nil {
		t.Fatal(err)
	}
	g.review.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if _, err := g.start(context.Background(), r.ReviewID); err == nil || g.reserved() {
		t.Fatal("expired review admitted an operation")
	}
	if err := os.WriteFile(g.path, []byte(`{"state":"ready"}`), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := newVLLMServingGroup(&engineState{}, g.path)
	if err == nil || !restored.reserved() {
		t.Fatal("invalid owner was silently dropped")
	}
	if err := restored.reconcile(context.Background(), "", 0); err == nil || !restored.reserved() {
		t.Fatal("invalid retained operation was repaired by guessing")
	}
}

func TestVLLMGroupNormalStopAndShutdownOwnAllRanks(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := vllmResourceFixture(t)
				g, err := newVLLMServingGroup(f.st, filepath.Join(t.TempDir(), "group.json"))
				if err != nil {
					t.Fatal(err)
				}
				stops := 0
				g.call = func(_ context.Context, _ vllmGroupBinding, action string) error {
					if action == "stop" {
						stops++
					}
					return nil
				}
				run := vllmGroupTestStart(t, g, context.Background(), 3)
				synctest.Wait()
				if err := f.e.Restart(context.Background(), "vllm"); err == nil || !g.coordinatorEligible("node-a", run.RunID, run.Generation) {
					t.Fatal("ordinary restart changed a reserved group")
				}
				if shutdown {
					err = f.e.stopAll()
				} else {
					err = f.e.Stop("vllm")
				}
				if err != nil || stops != 3 || g.reserved() || g.coordinatorEligible("node-a", run.RunID, run.Generation) || !g.status().CleanupConfirmed {
					t.Fatalf("group cleanup not owned: stops=%d status=%+v err=%v", stops, g.status(), err)
				}
				if shutdown {
					if _, err := g.reviewPlan(vllmGroupTestPlan(2)); err == nil {
						t.Fatal("shutdown reopened review")
					}
				}
				if len(f.commands) != 0 {
					t.Fatal("group Stop reached standalone engine command")
				}
				before, _ := os.ReadFile(g.path)
				f.e.StopAll()
				after, _ := os.ReadFile(g.path)
				if string(before) != string(after) || !g.status().CleanupConfirmed {
					t.Fatal("repeat shutdown rewrote completed cleanup")
				}
			})
		})
	}
}

func TestVLLMGroupShutdownReportsUnconfirmedCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := vllmResourceFixture(t)
		g, err := newVLLMServingGroup(f.st, filepath.Join(t.TempDir(), "group.json"))
		if err != nil {
			t.Fatal(err)
		}
		g.call = func(_ context.Context, _ vllmGroupBinding, action string) error {
			if action == "stop" {
				return errors.New("owned cleanup unconfirmed")
			}
			return nil
		}
		run := vllmGroupTestStart(t, g, context.Background(), 2)
		synctest.Wait()
		if err := f.e.stopAll(); err == nil || !g.reserved() || g.status().CleanupConfirmed || g.coordinatorEligible("node-a", run.RunID, run.Generation) {
			t.Fatal("shutdown hid uncertain rank cleanup")
		}
	})
}
