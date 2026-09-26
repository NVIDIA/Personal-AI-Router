// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// A closed Qwen receipt retains its full native plan, so it outgrows 4 KiB.
const vllmRankReceiptLimit = 16 << 10

// A rank receipt cannot establish post-crash process absence. Retain a hold
// before publishing engine state; no PID/name kill or synthesized cleanup.
func (e *Executor) restoreVLLMRankHold(st *engineState) *vllmManagedRank {
	path := filepath.Join(st.installDir, "serving-rank.json")
	var raw json.RawMessage
	err := readVLLMJSON(st.installDir, path, vllmRankReceiptLimit, &raw)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	r := &vllmManagedRank{e: e, st: st, path: path}
	if err == nil && strictDiagnosticJSON(raw, &r.receipt) == nil &&
		onboardingID.MatchString(r.receipt.RunID) && r.receipt.Generation > 0 &&
		onboardingSHA.MatchString(r.receipt.PlanDigest) && r.receipt.Rank >= 0 && r.receipt.Rank < 3 &&
		r.receipt.State == "stopped" && r.receipt.CleanupConfirmed {
		return nil
	}
	r.receipt.State = "cleanup-required"
	r.receipt.CleanupConfirmed = false
	if r.receipt.SystemPlan != nil {
		p := r.receipt.SystemPlan
		r.binding = vllmGroupBinding{RunID: p.RunID, Generation: p.Generation, PlanDigest: p.PlanDigest, Rank: p.Rank}
		r.placement = vllmRankPlacement{LocalNode: p.NodeID, LocalAddress: p.LocalAddress, CoordinatorAddress: p.CoordinatorAddress, ModelPath: p.ModelPath, APIPort: p.APIPort, MasterPort: p.MasterPort}
	}
	return r
}

func (r *vllmManagedRank) routeStatus(status EngineStatus) EngineStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.receipt.CleanupConfirmed {
		return status
	}
	role := "participant"
	if r.binding.Rank == 0 && r.binding.Plan.Coordinator != "" {
		role = "coordinator"
	}
	route := &vllmGroupRouteStatus{RunID: r.receipt.RunID, Generation: r.receipt.Generation,
		Model: r.binding.Plan.Model, Coordinator: r.binding.Plan.Coordinator, Role: role, State: r.receipt.State}
	for _, member := range r.binding.Plan.Members {
		route.Members = append(route.Members, member.NodeID)
	}
	status.ServingGroup = route
	// Only the coordinator qualifies model readiness. A launched headless rank
	// remains "started" while serving the group; it is running, not a model owner.
	status.Starting = r.receipt.State == "prepared" || r.receipt.State == "starting" || r.receipt.State == "started" && r.binding.Rank == 0
	status.Healthy = status.Healthy && role == "coordinator" && r.receipt.State == "ready"
	return status
}

// The admitted native dispatcher can use this A-to-C adapter. It is deliberately
// not installed in groupPeer.native: rank-local model binding, live resource and
// network admission, and crash containment still gate production activation.
func (r *vllmManagedRank) peerAction(ctx context.Context, b vllmGroupBinding, action string) (vllmGroupPeerResult, error) {
	request := vllmGroupRequest(b, action)
	if err := validateVLLMGroupPeerRequest(request); err != nil {
		return vllmGroupPeerResult{}, err
	}
	var err error
	switch action {
	case "ready":
		_, err = r.readiness(ctx, b) // confirmed pending is a state, not a transport error
	case "prepare":
		// The dispatcher already prepared this exact owner under engine.opMu.
	case "reconcile":
		err = r.stop(b)
	default:
		err = r.call(ctx, b, action)
	}
	if err != nil {
		return vllmGroupPeerResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.matches(b) || action == "prepare" && r.receipt.State != "prepared" {
		return vllmGroupPeerResult{}, errors.New("participant owner changed before its receipt")
	}
	result := vllmGroupPeerRefusal(request)
	result.ActivationEnabled, result.Code, result.Reason = true, "ok", ""
	result.State, result.EffectsApplied, result.CleanupConfirmed = r.receipt.State, true, r.receipt.CleanupConfirmed
	return result, validateVLLMGroupPeerResult(request, result)
}
