// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// This internal lease is minted only while reconcileLocked's caller owns
// engine.opMu. It is never serialized into a request, and is revoked before the
// synchronous cleanup returns. A remote HTTP request cannot inherit it.
type vllmHeldEngineLockKey struct{}
type vllmHeldEngineLock struct {
	engine *engineState
	active atomic.Bool
}

func vllmHeldEngineContext(ctx context.Context, engine *engineState) (context.Context, func()) {
	lease := &vllmHeldEngineLock{engine: engine}
	lease.active.Store(true)
	return context.WithValue(ctx, vllmHeldEngineLockKey{}, lease), func() { lease.active.Store(false) }
}
func vllmEngineLockHeld(ctx context.Context, engine *engineState) bool {
	lease, _ := ctx.Value(vllmHeldEngineLockKey{}).(*vllmHeldEngineLock)
	return lease != nil && lease.engine == engine && lease.active.Load()
}

type vllmClosedRank struct {
	RunID            string `json:"runId"`
	Generation       uint64 `json:"generation"`
	Rank             int    `json:"rank"`
	PlanDigest       string `json:"planDigest"`
	NodeID           string `json:"nodeId"`
	UID              int    `json:"uid"`
	Unit             string `json:"unit"`
	PlanHash         string `json:"planHash"`
	StartFenced      bool   `json:"startFenced"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
}

func vllmRankClosurePath(st *engineState, b vllmGroupBinding) (string, error) {
	if err := validateVLLMGroupPeerRequest(vllmGroupRequest(b, "reconcile")); err != nil {
		return "", err
	}
	return filepath.Join(st.installDir, "closed-ranks", fmt.Sprintf("%s-%d-%d.json", b.RunID, b.Generation, b.Rank)), nil
}
func readVLLMRankClosure(st *engineState, b vllmGroupBinding) (*vllmClosedRank, error) {
	path, err := vllmRankClosurePath(st, b)
	if err != nil {
		return nil, err
	}
	var record vllmClosedRank
	if err := readVLLMJSON(st.installDir, path, 4096, &record); err != nil {
		return nil, err
	}
	expectedUnit := fmt.Sprintf("nvpair-vllm-rank-%s-%d-%d.service", b.RunID, b.Generation, b.Rank)
	if record.RunID != b.RunID || record.Generation != b.Generation || record.Rank != b.Rank || record.PlanDigest != b.PlanDigest || record.NodeID != b.Plan.Members[b.Rank].NodeID || record.UID != os.Getuid() || record.Unit != expectedUnit || !onboardingSHA.MatchString(record.PlanHash) || !record.StartFenced || !record.CleanupConfirmed {
		return nil, errors.New("closed rank marker is not bound to this exact operation")
	}
	return &record, nil
}
func rejectVLLMClosedRank(st *engineState, b vllmGroupBinding) error {
	_, err := readVLLMRankClosure(st, b)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("this exact rank operation is durably closed; obtain a fresh group review")
}

func (p *vllmGroupPeer) closeMissingSystemRank(ctx context.Context, st *engineState, b vllmGroupBinding, action string, elevation *diagnosticPackageElevation) (vllmGroupPeerResult, error) {
	return closeMissingVLLMRank(ctx, st, b, action, func() (vllmRankSystemPlan, error) {
		// Closing-only absence fencing does not execute runtime content. Use the
		// stable owned install root so an old generation remains closable after a
		// runtime update; any retained root plan mismatch still fails closed.
		return buildVLLMSystemClosurePlan(st, b)
	}, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
		return runVLLMSystemRank(ctx, "reconcile", plan, nil, elevation)
	})
}

func vllmSystemPlanMatchesReceipt(plan vllmRankSystemPlan, receipt vllmRankReceipt) bool {
	return plan.RunID == receipt.RunID && plan.Generation == receipt.Generation && plan.Rank == receipt.Rank && plan.PlanDigest == receipt.PlanDigest
}

func vllmSystemPlanIsRequestPredecessor(plan vllmRankSystemPlan, b vllmGroupBinding) bool {
	if plan.Owner != vllmSystemRankOwner || plan.Generation >= b.Generation || plan.UID != os.Getuid() || b.Rank < 0 || b.Rank >= len(b.Plan.Members) || len(b.Plan.Members) == 0 {
		return false
	}
	member, coordinator := b.Plan.Members[b.Rank], b.Plan.Members[0]
	if member.Placement == nil || coordinator.Placement == nil {
		return false
	}
	return plan.NodeID == member.NodeID && plan.LocalAddress == member.Placement.Address && plan.CoordinatorAddress == coordinator.Placement.Address &&
		plan.Model == b.Plan.Model && plan.ModelDigest == member.ModelDigest && plan.GPUUUID == member.GPUUUID && plan.ConfigSHA256 == b.Plan.Topology.ConfigSHA256 &&
		plan.APIPort == member.Placement.APIPort && plan.MasterPort == member.Placement.MasterPort
}

// Reconcile only exact retained custody. The caller holds engine.opMu. One
// participant Reconcile action may use its fresh node-scoped administrator
// choice first for this exact predecessor prerequisite and then for the
// requested missing rank; both plans must bind the same local node and UID.
// The historical receipt keeps its own identity.
func reconcileRetainedVLLMRank(ctx context.Context, st *engineState, rank *vllmManagedRank, old *vllmRankReceipt, b vllmGroupBinding, fence func(vllmRankSystemPlan) (vllmSystemRankResult, error)) error {
	if old.SystemPlan == nil {
		return errors.New("retained unconfirmed rank lacks its original root custody plan")
	}
	plan := *old.SystemPlan
	if !vllmSystemPlanMatchesReceipt(plan, *old) || !vllmSystemPlanIsRequestPredecessor(plan, b) {
		return errors.New("retained root custody is not an exact predecessor of the requested local rank")
	}
	expectedHash, err := vllmSystemPlanHash(plan)
	if err != nil {
		return err
	}
	expectedUnit := fmt.Sprintf("nvpair-vllm-rank-%s-%d-%d.service", plan.RunID, plan.Generation, plan.Rank)
	if rank != nil {
		rank.mu.Lock()
		defer rank.mu.Unlock()
		if rank.receipt.SystemPlan == nil || !vllmSystemPlanMatchesReceipt(*rank.receipt.SystemPlan, rank.receipt) ||
			rank.receipt.RunID != old.RunID || rank.receipt.Generation != old.Generation || rank.receipt.Rank != old.Rank || rank.receipt.PlanDigest != old.PlanDigest {
			return errors.New("retained memory rank differs from its durable custody")
		}
		memoryHash, hashErr := vllmSystemPlanHash(*rank.receipt.SystemPlan)
		if hashErr != nil || memoryHash != expectedHash {
			return errors.New("retained memory rank plan differs from its durable custody")
		}
	}
	pending := *old
	pending.State, pending.CleanupConfirmed = "stopping", false
	path := filepath.Join(st.installDir, "serving-rank.json")
	if err := writeVLLMJSON(st.installDir, path, pending); err != nil {
		return err
	}
	if rank != nil {
		rank.receipt.State, rank.receipt.CleanupConfirmed = "stopping", false
	}
	st.mu.Lock()
	st.healthy, st.stopping = false, true
	st.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := fence(plan)
	if err != nil {
		return err
	}
	if !systemRankCleanupPassed(result) || result.PlanHash != expectedHash || result.Unit != expectedUnit {
		return errors.New("exact retained rank cleanup is unconfirmed")
	}
	cleaned := pending
	applyVLLMSystemCleanup(&cleaned, result)
	if err := writeVLLMJSON(st.installDir, path, cleaned); err != nil {
		return err
	}
	*old = cleaned
	if rank != nil {
		applyVLLMSystemCleanup(&rank.receipt, result)
	}
	st.mu.Lock()
	st.running, st.healthy, st.stopping = false, false, false
	st.mu.Unlock()
	return ctx.Err()
}

// A missing prepared rank is not proof of cleanup. Only this closing-only
// branch may create a distinct durable marker, after the existing approved root
// route fences the exact requested operation. It never replaces serving-rank.json.
// The fixed I/O seams let ordinary source fixtures avoid all native operations.
func closeMissingVLLMRank(ctx context.Context, st *engineState, b vllmGroupBinding, action string, makePlan func() (vllmRankSystemPlan, error), fence func(vllmRankSystemPlan) (vllmSystemRankResult, error)) (vllmGroupPeerResult, error) {
	request := vllmGroupRequest(b, action)
	if action != "stop" && action != "reconcile" {
		return vllmGroupPeerResult{}, errors.New("missing-rank recovery is closing-only")
	}
	if err := validateVLLMGroupPeerRequest(request); err != nil {
		return vllmGroupPeerResult{}, err
	}
	if ctx.Err() != nil {
		return vllmGroupPeerResult{}, ctx.Err()
	}
	if !vllmEngineLockHeld(ctx, st) {
		if !st.opMu.TryLock() {
			return vllmGroupPeerResult{}, errors.New("participant lifecycle is busy; closure remains unconfirmed")
		}
		defer st.opMu.Unlock()
	}
	st.mu.Lock()
	rank := st.vllmRank
	busy := st.running || st.proc != nil || st.adopted || st.stopPending != 0
	st.mu.Unlock()
	if busy {
		return vllmGroupPeerResult{}, errors.New("an active engine owner prevents missing-rank closure")
	}
	if rank != nil {
		rank.mu.Lock()
		matching := rank.matches(b)
		rank.mu.Unlock()
		if matching {
			return vllmGroupPeerResult{}, errors.New("prepared or unconfirmed rank requires its existing owner reconciliation")
		}
	}
	// Preserve and inspect the old on-disk receipt even when restart correctly
	// restored its closed state as nil. Never overwrite or delete that history.
	var old vllmRankReceipt
	err := readVLLMJSON(st.installDir, filepath.Join(st.installDir, "serving-rank.json"), vllmRankReceiptLimit, &old)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return vllmGroupPeerResult{}, err
	}
	if err == nil && (!onboardingID.MatchString(old.RunID) || old.Generation == 0 || !onboardingSHA.MatchString(old.PlanDigest) || old.Rank < 0 || old.Rank >= 3) {
		return vllmGroupPeerResult{}, errors.New("retained rank identity prevents missing-rank closure")
	}
	if _, err := readVLLMRankClosure(st, b); err == nil {
		return closedMissingVLLMResult(request), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return vllmGroupPeerResult{}, err
	}
	var plan vllmRankSystemPlan
	oldMatches := old.RunID == b.RunID && old.Generation == b.Generation && old.Rank == b.Rank && old.PlanDigest == b.PlanDigest
	if oldMatches && old.State == "stopped" && old.CleanupConfirmed && !old.StartFenced {
		// This exact rank ran and its own durable receipt recorded the root-confirmed
		// stop; only the group journal missed it before a manager restart. A unit
		// that ran leaves no pre-unit tombstone, so fencing it again cannot succeed.
		return closedMissingVLLMResult(request), nil
	}
	if err == nil && !oldMatches && (!old.CleanupConfirmed || old.State != "stopped") {
		if err := reconcileRetainedVLLMRank(ctx, st, rank, &old, b, fence); err != nil {
			return vllmGroupPeerResult{}, err
		}
	}
	if rank != nil {
		rank.mu.Lock()
		closed := rank.receipt.State == "stopped" && rank.receipt.CleanupConfirmed
		rank.mu.Unlock()
		if !closed {
			return vllmGroupPeerResult{}, errors.New("prepared or unconfirmed rank requires its existing owner reconciliation")
		}
	}
	if oldMatches {
		if old.SystemPlan == nil {
			return vllmGroupPeerResult{}, errors.New("requested old operation lacks its original root custody plan")
		}
		// Reuse the exact historical root hash across manager restarts. This is
		// also the crash-recovery path when the root tombstone landed before the
		// local receipt could record StartFenced/CleanupConfirmed.
		plan = *old.SystemPlan
	} else {
		plan, err = makePlan()
		if err != nil {
			return vllmGroupPeerResult{}, err
		}
	}
	if plan.RunID != b.RunID || plan.Generation != b.Generation || plan.Rank != b.Rank || plan.PlanDigest != b.PlanDigest || plan.NodeID != b.Plan.Members[b.Rank].NodeID || plan.UID != os.Getuid() {
		return vllmGroupPeerResult{}, errors.New("closing plan changed the requested operation or UID")
	}
	expectedHash, err := vllmSystemPlanHash(plan)
	if err != nil {
		return vllmGroupPeerResult{}, err
	}
	expectedUnit := fmt.Sprintf("nvpair-vllm-rank-%s-%d-%d.service", b.RunID, b.Generation, b.Rank)
	result, err := fence(plan)
	if err != nil {
		return vllmGroupPeerResult{}, err
	}
	if result.State != "closed" || !result.Tombstone || !result.StartFenced || !result.CleanupConfirmed || result.EffectsApplied || result.PlanHash != expectedHash || result.Unit != expectedUnit {
		return vllmGroupPeerResult{}, errors.New("exact root pre-unit fence and absence are unconfirmed")
	}
	if ctx.Err() != nil {
		return vllmGroupPeerResult{}, ctx.Err()
	}
	path, err := vllmRankClosurePath(st, b)
	if err != nil {
		return vllmGroupPeerResult{}, err
	}
	if err := validateVLLMOwnedPath(st.installDir, path); err != nil {
		return vllmGroupPeerResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return vllmGroupPeerResult{}, err
	}
	marker := vllmClosedRank{RunID: b.RunID, Generation: b.Generation, Rank: b.Rank, PlanDigest: b.PlanDigest, NodeID: plan.NodeID, UID: plan.UID, Unit: expectedUnit, PlanHash: expectedHash, StartFenced: true, CleanupConfirmed: true}
	if err := writeVLLMJSON(st.installDir, path, marker); err != nil {
		return vllmGroupPeerResult{}, err
	}
	return closedMissingVLLMResult(request), nil
}
func closedMissingVLLMResult(request vllmGroupPeerRequest) vllmGroupPeerResult {
	result := vllmGroupPeerRefusal(request)
	result.ActivationEnabled = true
	result.State = "stopped"
	result.Code = "ok"
	result.Reason = ""
	result.EffectsApplied = false
	result.CleanupConfirmed = true
	return result
}
