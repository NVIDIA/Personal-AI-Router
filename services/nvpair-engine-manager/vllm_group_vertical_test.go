// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func verticalRequest(action string) vllmGroupPeerRequest {
	plan := vllmGroupTestPlan(2)
	digest, _ := vllmGroupPlanDigest(plan)
	return vllmGroupRequest(vllmGroupBinding{RunID: "0123456789abcdef0123456789abcdef", Generation: 1, PlanDigest: digest, Plan: plan, Rank: 1}, action)
}

func verticalResult(r vllmGroupPeerRequest, state string) vllmGroupPeerResult {
	result := vllmGroupPeerRefusal(r)
	result.ActivationEnabled, result.Code, result.Reason, result.State = true, "ok", "", state
	result.EffectsApplied = state != "available"
	result.CleanupConfirmed = state == "stopped"
	return result
}

func TestVLLMGroupVerticalOrdinaryActionReceipts(t *testing.T) {
	for action, state := range map[string]string{"capability": "available", "prepare": "prepared", "start": "started", "ready": "ready", "stop": "stopped", "reconcile": "stopped", "status": "started"} {
		r := verticalRequest(action)
		if err := vllmGroupActionOutcome(r, verticalResult(r, state)); err != nil {
			t.Fatalf("%s ordinary receipt: %v", action, err)
		}
		if err := vllmGroupActionOutcome(r, vllmGroupPeerRefusal(r)); !errors.Is(err, errVLLMGroupNativeUnavailable) {
			t.Fatalf("%s unavailable owner became success: %v", action, err)
		}
	}
	r := verticalRequest("ready")
	if err := vllmGroupActionOutcome(r, verticalResult(r, "started")); err == nil {
		t.Fatal("launch acknowledgement substituted for collective readiness")
	}
	r = verticalRequest("stop")
	result := verticalResult(r, "stopped")
	result.CleanupConfirmed = false
	if err := vllmGroupActionOutcome(r, result); err == nil {
		t.Fatal("stop acknowledgement substituted for confirmed cleanup")
	}
}

func TestVLLMGroupVerticalParticipantLossWithdrawsAndCleans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stops := 0
		var g *vllmServingGroup
		g = vllmGroupTestOwner(t, func(_ context.Context, b vllmGroupBinding, action string) error {
			if action == "stop" {
				stops++
				if g.coordinatorEligible(b.Plan.Coordinator, b.RunID, b.Generation) {
					t.Fatal("coordinator remained eligible when participant cleanup began")
				}
			}
			return nil
		})
		g.live = func(context.Context, vllmGroupRun) error { return errors.New("participant stopped") }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := vllmGroupTestStart(t, g, ctx, 2)
		synctest.Wait()
		if !g.coordinatorEligible("node-a", run.RunID, run.Generation) {
			t.Fatal("all started fixture ranks failed initial coordinator readiness")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if g.coordinatorEligible("node-a", run.RunID, run.Generation) || stops != 2 || !g.status().CleanupConfirmed || g.status().State != "failed" {
			t.Fatalf("lost participant left group eligible or unclean: %+v stops=%d", g.status(), stops)
		}
	})
}

func TestVLLMGroupVerticalAbsentNativeOwnerCannotAttach(t *testing.T) {
	g := vllmGroupTestOwner(t, nil)
	if err := (&vllmGroupPeer{}).attach(g); !errors.Is(err, errVLLMGroupNativeUnavailable) || g.call != nil || g.live != nil {
		t.Fatalf("absent native owner enabled transport: %v", err)
	}
}

func TestVLLMGroupVerticalRoutingRequiresHealthyCoordinator(t *testing.T) {
	g := vllmGroupTestOwner(t, nil)
	g.held = true
	g.ctx = context.Background()
	g.run = vllmGroupRun{RunID: "0123456789abcdef0123456789abcdef", Generation: 1,
		Plan: vllmGroupTestPlan(2), State: "ready", Ranks: []vllmGroupRank{{Started: true}, {Started: true}}}
	healthy := EngineStatus{Engine: "vllm", Running: true, Healthy: true, Port: 8001}
	if got := g.routeStatus("node-a", healthy); !got.Healthy || got.ServingGroup.Role != "coordinator" {
		t.Fatalf("live coordinator unavailable: %+v", got)
	}
	if got := g.routeStatus("node-b", healthy); got.Healthy || got.ServingGroup.Role != "participant" {
		t.Fatalf("headless participant advertised: %+v", got)
	}
	unhealthy := healthy
	unhealthy.Healthy = false
	if got := g.routeStatus("node-a", unhealthy); got.Healthy {
		t.Fatal("group receipt manufactured engine HTTP health")
	}
	g.run.State = "stopping"
	if got := g.routeStatus("node-a", healthy); got.Healthy {
		t.Fatal("stopping coordinator remains routable")
	}
	g.held = false
	if got := g.routeStatus("node-a", healthy); got.ServingGroup != nil || !got.Healthy {
		t.Fatal("completed group interferes with later standalone serving")
	}
}

func TestVLLMGroupVerticalStartupPollsConfirmedPendingOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		r := verticalRequest("ready")
		err := awaitVLLMGroupReady(context.Background(), r, func(_ context.Context, r vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
			calls++
			state := "started"
			if calls == 3 {
				state = "ready"
			}
			got := verticalResult(r, state)
			return got, vllmGroupActionOutcome(r, got)
		})
		if err != nil || calls != 3 {
			t.Fatalf("normal pending-to-ready: calls=%d err=%v", calls, err)
		}
		calls = 0
		err = awaitVLLMGroupReady(context.Background(), r, func(context.Context, vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
			calls++
			return vllmGroupPeerResult{}, errors.New("response unavailable")
		})
		if err == nil || calls != 1 {
			t.Fatalf("uncertain transport retried: calls=%d err=%v", calls, err)
		}
	})
}

func TestVLLMGroupReadinessBudgetPreservesQwenQualificationLease(t *testing.T) {
	ordinary := vllmGroupTestPlan(2)
	ready := verticalRequest("ready")
	if got := vllmGroupPeerActionBudget(ready); got != 90*time.Second {
		t.Fatalf("native readiness action budget changed: %s", got)
	}
	if got := vllmGroupReadinessBudget(ordinary); got != 5*time.Minute {
		t.Fatalf("ordinary readiness budget changed: %s", got)
	}
	if vllmGroupQualificationBudget != 3*time.Minute || vllmGroupQualificationBudget >= vllmGroupReadinessBudget(ordinary) || vllmGroupQualificationBudget >= time.Duration(ordinary.Limits.RuntimeSeconds)*time.Second {
		t.Fatalf("ordinary first-inference qualification is not bounded inside readiness and residency: %s", vllmGroupQualificationBudget)
	}
	qwen := ordinary
	qwen.Model = vllmQwen38ModelID
	qwen.Limits = vllmGroupLimitsForModel(qwen.Model)
	got := vllmGroupReadinessBudget(qwen)
	if got != 32*time.Minute || 23*time.Minute+got != time.Duration(qwen.Limits.RuntimeSeconds)*time.Second-5*time.Minute {
		t.Fatalf("Qwen readiness budget does not preserve its one-token qualification lease: %s", got)
	}
}
