// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type vllmGroupRouteStatus struct {
	RunID       string   `json:"runId"`
	Generation  uint64   `json:"generation"`
	Model       string   `json:"model"`
	Coordinator string   `json:"coordinator"`
	Role        string   `json:"role"`
	State       string   `json:"state"`
	Members     []string `json:"members"`
	Routing     bool     `json:"routing,omitempty"`
}

// The existing broker still owns advertisement and the existing proxy still
// owns Jobs. Mask its ordinary healthy signal until this exact coordinator is
// collectively ready; never manufacture health from a group state alone.
func (g *vllmServingGroup) routeStatus(nodeID string, status EngineStatus) EngineStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		return status
	}
	run := g.run
	role := "participant"
	if nodeID == run.Plan.Coordinator {
		role = "coordinator"
	}
	routing := role == "coordinator" && run.State == "starting" && g.routing
	route := &vllmGroupRouteStatus{RunID: run.RunID, Generation: run.Generation,
		Model: run.Plan.Model, Coordinator: run.Plan.Coordinator, Role: role, State: run.State, Routing: routing}
	for _, member := range run.Plan.Members {
		route.Members = append(route.Members, member.NodeID)
	}
	status.ServingGroup = route
	eligible := role == "coordinator" && (run.State == "ready" || routing) && g.ctx != nil && g.ctx.Err() == nil
	for _, rank := range run.Ranks {
		eligible = eligible && rank.Started && !rank.CleanupConfirmed
	}
	status.Healthy = status.Healthy && eligible
	return status
}

// Paired transport validates identity; this validates the bound receipt.
// A receipt is not a substitute for native model/process proof by its owner.
func validateVLLMGroupPeerResult(r vllmGroupPeerRequest, got vllmGroupPeerResult) error {
	if err := validateVLLMGroupPeerRequest(r); err != nil {
		return err
	}
	if got.Protocol != r.Protocol || got.Action != r.Action || got.RunID != r.RunID ||
		got.Generation != r.Generation || got.PlanDigest != r.PlanDigest || got.Rank != r.Rank ||
		got.Controller != r.Plan.Coordinator || got.NodeID != r.Plan.Members[r.Rank].NodeID {
		return errors.New("participant receipt does not bind the exact requested operation")
	}
	if got.StartFailure != nil && !validVLLMRankStartFailure(got.StartFailure) {
		return errors.New("invalid redacted start failure")
	}
	if got.State == "failed" {
		if r.Action == "prepare" {
			if got.Code != "prepare_failed" || got.Reason != "" || !got.ActivationEnabled || got.EffectsApplied || got.CleanupConfirmed || got.StartFailure == nil {
				return errors.New("invalid failed preparation receipt")
			}
			return nil
		}
		if r.Action != "start" || got.Code != "start_failed" || got.Reason != "" || !got.ActivationEnabled || got.CleanupConfirmed || got.StartFailure == nil {
			return errors.New("invalid failed start receipt")
		}
		return nil
	}
	if got == vllmGroupPeerRefusal(r) {
		return nil
	}
	if !got.ActivationEnabled || got.Code != "ok" || got.Reason != "" {
		return errors.New("participant has not admitted native activation")
	}
	switch got.State {
	case "available":
		if r.Action != "capability" || got.EffectsApplied || got.CleanupConfirmed {
			return errors.New("capability receipt cannot report native effects or cleanup")
		}
	case "prepared", "started", "ready":
		if !got.EffectsApplied || got.CleanupConfirmed {
			return errors.New("active participant receipt lacks retained ownership")
		}
	case "stopped":
		if !got.CleanupConfirmed {
			return errors.New("stopped receipt requires confirmed owned-child cleanup")
		}
	default:
		return errors.New("unknown participant state")
	}
	return nil
}

func vllmGroupActionOutcome(r vllmGroupPeerRequest, got vllmGroupPeerResult) error {
	if err := validateVLLMGroupPeerResult(r, got); err != nil {
		return err
	}
	if got.State == "failed" && got.StartFailure != nil {
		return &vllmRankStartError{cloneVLLMRankStartFailure(*got.StartFailure)}
	}
	if !got.ActivationEnabled {
		return errVLLMGroupNativeUnavailable
	}
	want := map[string]string{"capability": "available", "prepare": "prepared", "start": "started", "ready": "ready", "stop": "stopped", "reconcile": "stopped"}[r.Action]
	if r.Action == "status" {
		return nil
	}
	if got.State != want {
		return fmt.Errorf("participant %s has state %s after %s", got.NodeID, got.State, r.Action)
	}
	return nil
}

func vllmGroupRequest(b vllmGroupBinding, action string) vllmGroupPeerRequest {
	return vllmGroupPeerRequest{Protocol: vllmGroupPeerProtocol, Action: action,
		RunID: b.RunID, Generation: b.Generation, PlanDigest: b.PlanDigest,
		Plan: cloneVLLMGroupPlan(b.Plan), Rank: b.Rank}
}

// ponytail: compose the existing owner and paired actions, not another run
// registry or remote protocol. Call before exposing an enabled group review.
func (p *vllmGroupPeer) attach(g *vllmServingGroup) error {
	if p == nil || p.native == nil || g == nil {
		return errVLLMGroupNativeUnavailable
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.call != nil {
		return errors.New("serving-group transport is already owned or reserved")
	}
	g.call = func(ctx context.Context, binding vllmGroupBinding, action string) error {
		return p.call(g.elevationContext(ctx), binding, action)
	}
	g.live = func(ctx context.Context, run vllmGroupRun) error {
		return p.live(g.elevationContext(ctx), run)
	}
	if p.m != nil && p.m.exec != nil && p.m.exec.fabric != nil {
		g.fabric = p.m.exec.fabric
	}
	if p.m != nil && p.m.codec != nil && p.m.groupRoute != nil {
		g.route = p.m.awaitVLLMGroupRoute
	}
	return nil
}

func (p *vllmGroupPeer) call(ctx context.Context, b vllmGroupBinding, action string) error {
	r := vllmGroupRequest(b, action)
	if action == "start" || action == "stop" {
		r.Elevation = groupRequestElevation(ctx, r.Plan.Members[r.Rank].NodeID)
	}
	if action == "ready" {
		return awaitVLLMGroupReady(ctx, r, p.control)
	}
	result, err := p.control(ctx, r)
	if err != nil {
		return err
	}
	return vllmGroupActionOutcome(r, result)
}

// Readiness is read-only. Poll only after an exact, successful native receipt
// says startup is still pending; transport errors never retry an uncertain POST.
func awaitVLLMGroupReady(ctx context.Context, r vllmGroupPeerRequest, control func(context.Context, vllmGroupPeerRequest) (vllmGroupPeerResult, error)) error {
	ctx, cancel := context.WithTimeout(ctx, vllmGroupReadinessBudget(r.Plan))
	defer cancel()
	for {
		result, err := control(ctx, r)
		if err == nil {
			err = vllmGroupActionOutcome(r, result)
		}
		if err == nil {
			return nil
		}
		if validateVLLMGroupPeerResult(r, result) != nil || !result.ActivationEnabled ||
			(result.State != "prepared" && result.State != "started") {
			return err
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Start is serial and each fixed Qwen root preflight may consume ten minutes.
// The first unit can therefore spend up to twenty-three minutes across the
// residual of its wrapper and two later 11-minute wrappers. A 32-minute
// readiness budget (including qualification)
// leaves five minutes in its one-hour lease. This is not timing proof.
func vllmGroupReadinessBudget(plan vllmGroupPlan) time.Duration {
	if isQwen38ProfileModel(plan.Model) && plan.Limits == vllmGroupLimitsForModel(plan.Model) {
		return 32 * time.Minute
	}
	return 5 * time.Minute
}

// Every participant must still own its selected rank. The coordinator's ready
// action must additionally check its exact model and managed HTTP child.
func (p *vllmGroupPeer) live(ctx context.Context, run vllmGroupRun) error {
	for rank := range run.Ranks {
		r := vllmGroupRequest(vllmGroupBinding{RunID: run.RunID, Generation: run.Generation,
			PlanDigest: run.PlanDigest, Plan: run.Plan, Rank: rank}, "status")
		result, err := p.control(ctx, r)
		if err != nil {
			return err
		}
		if err := vllmGroupActionOutcome(r, result); err != nil {
			return err
		}
		if result.State != "started" && result.State != "ready" {
			return errors.New("a serving-group participant no longer owns a live rank")
		}
	}
	r := vllmGroupRequest(vllmGroupBinding{RunID: run.RunID, Generation: run.Generation,
		PlanDigest: run.PlanDigest, Plan: run.Plan, Rank: 0}, "ready")
	result, err := p.control(ctx, r)
	if err != nil {
		return err
	}
	return vllmGroupActionOutcome(r, result)
}
