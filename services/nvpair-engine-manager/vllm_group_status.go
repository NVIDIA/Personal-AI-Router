// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const (
	vllmGroupJournalFile     = "serving-group.json"
	vllmGroupJournalMaxBytes = 32 << 10
	vllmGroupHeldReason      = "retained serving group holds vLLM until every attempted rank confirms cleanup; fresh native admission required"
)

// Digests are compared byte-for-byte downstream, so only the writer's
// lowercase hex form is accepted.
var vllmGroupDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// vllmGroupStates is the closed set the retained journal writer emits. Any
// other value is an unknown owner generation and fails closed.
var vllmGroupStates = map[string]bool{"starting": true, "ready": true, "stopping": true, "stopped": true, "failed": true, "cleanup-required": true}

// vllmGroupCleanupRequest is an exact retained-run binding. It is intentionally
// smaller than the private journal: callers may identify the owner generation,
// but cannot supply commands, nodes or paths that could expand cleanup scope.
type vllmGroupCleanupRequest struct {
	RunID      string `json:"runId"`
	Generation uint64 `json:"generation"`
	PlanDigest string `json:"planDigest"`
}

// VLLMGroupStatus reads the retained journal from the stable owned vLLM
// directory. It deliberately does not resolve a runnable local platform: a
// Windows or macOS controller may still own remote Linux vLLM routing state,
// and local platform eligibility must not turn this ownership read into an
// unrelated global hold.
func (e *Executor) VLLMGroupStatus() (vllmGroupStatus, error) {
	return readVLLMGroupStatus(filepath.Join(e.baseDir, "vllm"))
}

// rejectVLLMGroupMutation is the owner-side effect fence. It deliberately
// reads the journal for every mutation instead of trusting a previously
// reported UI status: a retained run can appear after discovery but before an
// effect is requested. Missing or fully cleaned journals release ordinary
// single-engine operations; held, unreadable and invalid journals fail closed.
func (e *Executor) rejectVLLMGroupMutation(engine, operation string) error {
	if engine != "vllm" {
		return nil
	}
	status, err := readVLLMGroupStatus(filepath.Join(e.baseDir, "vllm"))
	if err != nil {
		return fmt.Errorf("cannot %s vLLM while serving-group ownership is unknown: %w", operation, err)
	}
	if status.Reserved {
		return fmt.Errorf("cannot %s vLLM: %s", operation, status.Reason)
	}
	e.mu.Lock()
	st := e.engines["vllm"]
	e.mu.Unlock()
	if st != nil {
		return rejectVLLMGroupOwnerMutation(st, operation)
	}
	return nil
}

// ReconcileVLLMGroupCleanup validates a cleanup-only request against a fresh
// journal read, then enters the same exact rank owner used by group Reconcile.
// It has no start/model/inference branch and supplies only the product-owned
// noninteractive fixed-helper route; any unresolved participant stays held.
func (e *Executor) ReconcileVLLMGroupCleanup(ctx context.Context, req vllmGroupCleanupRequest) (vllmGroupStatus, error) {
	if !onboardingHistoryID.MatchString(req.RunID) || req.Generation == 0 || !vllmGroupDigest.MatchString(req.PlanDigest) {
		return vllmGroupStatus{}, fmt.Errorf("cleanup requires exact runId, generation and planDigest binding")
	}
	status, err := readVLLMGroupStatus(filepath.Join(e.baseDir, "vllm"))
	if err != nil {
		return vllmGroupStatus{}, fmt.Errorf("cannot admit cleanup while serving-group ownership is unknown: %w", err)
	}
	if status.Run == nil {
		return status, fmt.Errorf("no retained serving group exists for cleanup")
	}
	if req.RunID != status.Run.RunID || req.Generation != status.Run.Generation || req.PlanDigest != status.Run.PlanDigest {
		return status, fmt.Errorf("cleanup binding does not match the retained serving group")
	}
	if !status.Reserved {
		return status, nil
	}
	g, err := e.groupOwner(false)
	if err != nil || g == nil {
		return status, errors.Join(errors.New("retained group cleanup owner is unavailable"), err)
	}
	g.engine.opMu.Lock()
	defer g.engine.opMu.Unlock()
	run := g.status()
	if run.RunID != req.RunID || run.Generation != req.Generation || run.PlanDigest != req.PlanDigest {
		return groupStatus(g), errors.New("cleanup binding changed before owner admission")
	}
	auth, err := newVLLMGroupElevation(run.Plan, nonInteractiveVLLMGroupElevation(run.Plan), false)
	if err != nil {
		return groupStatus(g), err
	}
	g.mu.Lock()
	g.elevation = auth
	g.mu.Unlock()
	err = g.reconcileLocked(ctx, req.RunID, req.Generation)
	auth.close()
	return groupStatus(g), err
}

// readVLLMGroupStatus performs zero effects: it never creates, rewrites or
// removes the journal. A missing journal is inactive; anything unreadable,
// redirected or invalid is an error so callers keep vLLM mutations held.
func readVLLMGroupStatus(installDir string) (vllmGroupStatus, error) {
	var run vllmGroupRun
	err := readManagedVLLMJSON(installDir, filepath.Join(installDir, vllmGroupJournalFile), vllmGroupJournalMaxBytes, &run)
	if os.IsNotExist(err) {
		return vllmGroupStatus{}, nil
	}
	if err != nil {
		return vllmGroupStatus{}, fmt.Errorf("retained serving-group journal is unreadable; preserve it for recovery: %w", err)
	}
	if err := validateVLLMGroupRun(run); err != nil {
		return vllmGroupStatus{}, fmt.Errorf("retained serving-group journal is invalid; preserve it for recovery: %w", err)
	}
	status := vllmGroupStatus{Reserved: !run.CleanupConfirmed, Run: &run}
	if status.Reserved {
		status.Reason = vllmGroupHeldReason
	}
	return status, nil
}

// validateVLLMGroupRun enforces the writer's own invariants. The plan digest
// is format-checked only: recomputing it needs the writer's canonical plan
// schema, which belongs to the admission slice, not this read path.
func validateVLLMGroupRun(r vllmGroupRun) error {
	if !onboardingHistoryID.MatchString(r.RunID) || r.Generation == 0 || !vllmGroupDigest.MatchString(r.PlanDigest) {
		return fmt.Errorf("run identity is incomplete")
	}
	if !vllmGroupStates[r.State] {
		return fmt.Errorf("state %q is unknown", r.State)
	}
	p := r.Plan
	if len(p.Members) == 0 || p.Coordinator != p.Members[0].NodeID || p.Model == "" || p.Runtime == "" {
		return fmt.Errorf("plan needs the coordinator first, a model and a runtime")
	}
	if p.Topology.TensorParallel <= 0 || p.Topology.PipelineParallel <= 0 || p.Topology.DataParallel <= 0 || !vllmGroupDigest.MatchString(p.Topology.ConfigSHA256) {
		return fmt.Errorf("topology is incomplete")
	}
	if isQwen38ProfileModel(p.Model) {
		digest, planErr := vllmGroupPlanDigest(p)
		if planErr != nil {
			digest, planErr = historicalQwenGroupPlanDigest(p)
			if planErr != nil || !r.CleanupConfirmed || r.State != "stopped" && r.State != "failed" {
				return fmt.Errorf("Qwen serving-group plan is not current or cleanup-only history")
			}
		}
		if digest != r.PlanDigest {
			return fmt.Errorf("Qwen serving-group plan digest changed")
		}
	}
	if len(r.Ranks) != len(p.Members) {
		return fmt.Errorf("ranks do not correlate with members")
	}
	nodes, pins := make(map[string]bool, len(p.Members)), make(map[string]bool, len(p.Members))
	clean := true
	for i, m := range p.Members {
		if m.NodeID == "" || nodes[m.NodeID] || pins[m.PinSHA256] || m.GPUUUID == "" || !vllmGroupDigest.MatchString(m.PinSHA256) || !vllmGroupDigest.MatchString(m.ModelDigest) || !vllmGroupDigest.MatchString(m.RuntimeDigest) || !vllmGroupDigest.MatchString(m.RuntimeCompatibilitySHA256) {
			return fmt.Errorf("member %d is incomplete or duplicates another member", i)
		}
		nodes[m.NodeID], pins[m.PinSHA256] = true, true
		rank := r.Ranks[i]
		if rank.NodeID != m.NodeID || rank.Started && !rank.Attempted || rank.CleanupConfirmed && !rank.Attempted ||
			(rank.CleanupFailure != nil && (!validVLLMRankStartFailure(rank.CleanupFailure) || !rank.Attempted || rank.CleanupConfirmed)) ||
			(rank.StartFailure != nil && !validVLLMRankStartFailure(rank.StartFailure)) {
			return fmt.Errorf("rank %d does not correlate with its member", i)
		}
		clean = clean && (!rank.Attempted || rank.CleanupConfirmed)
	}
	if r.CleanupConfirmed && (!clean || r.State != "stopped" && r.State != "failed") {
		return fmt.Errorf("cleanup evidence is inconsistent")
	}
	return nil
}
