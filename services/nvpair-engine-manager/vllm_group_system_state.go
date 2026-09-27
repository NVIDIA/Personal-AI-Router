// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"time"
)

func (p *vllmGroupPeer) bootstrapSystemOwner() {
	if runtime.GOOS == "linux" && os.Getuid() > 0 {
		p.native = p.systemNative
	}
}

func (r *vllmManagedRank) hasSystemOwner() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.receipt.SystemPlan != nil
}

func systemRankPolicyPassed(plan vllmRankSystemPlan, result vllmSystemRankResult) bool {
	p := result.Policy
	base := result.State == "started" && result.EffectsApplied && !result.CleanupConfirmed && result.Supervisor != nil &&
		p != nil && len(p.EffectivePrograms.Ingress) > 0 && len(p.EffectivePrograms.Egress) > 0 &&
		p.IngressAllowed && !p.ForbiddenIngressReceived && p.EgressDenied && !p.SocketPayloadFallback
	if !base {
		return false
	}
	if plan.Model == vllmQwen38ModelID {
		want := []string{}
		for _, lane := range plan.RDMALanes {
			want = append(want, lane.RDMADevice+":"+strconv.Itoa(lane.GIDPort))
		}
		return p.Transport == vllmQwen38Transport && !p.RDMADisabled && slices.Equal(p.HCAs, want) && p.NetGDRLevel != nil && *p.NetGDRLevel == 0 && p.NetGDRC2C != nil && *p.NetGDRC2C == 0 && p.NetGDRRead != nil && *p.NetGDRRead == 0 && p.NetPlugin == "none" && p.EnvPlugin == "none" && p.GINPlugin == "none" && p.MergeNICs != nil && *p.MergeNICs == 1 && p.SubnetAwareRouting == nil && p.SubnetPrefixLength == nil
	}
	if plan.DirectSocket != nil {
		return p.Transport == vllmGroupDirectSocketMode && p.RDMADisabled && len(p.HCAs) == 0 && p.SocketInterface == plan.DirectSocket.InterfaceName
	}
	if plan.RingSocket != nil {
		return p.Transport == vllmGroupRingSocketMode && p.RDMADisabled && len(p.HCAs) == 0 && p.SocketInterface == plan.RingSocket.InterfaceName
	}
	return p.Transport == "tcp-only" && p.RDMADisabled && len(p.HCAs) == 0 && p.SocketInterface == ""
}

func systemRankCleanupPassed(result vllmSystemRankResult) bool {
	return result.CleanupConfirmed && (result.State == "stopped" ||
		result.State == "closed" && result.Tombstone && result.StartFenced && !result.EffectsApplied)
}

// Status refreshes briefly own the same lifecycle lock as rank preparation.
// Wait for that owner under the participant request deadline instead of turning
// an ordinary read collision into a failed serving-group generation.
func lockVLLMGroupOwner(ctx context.Context, st *engineState) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return false
		}
		if st.opMu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (p *vllmGroupPeer) systemNative(ctx context.Context, request vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
	if runtime.GOOS != "linux" || os.Getuid() <= 0 {
		return vllmGroupPeerRefusal(request), nil
	}
	for _, path := range []string{"/run/systemd/system", "/sys/fs/cgroup/cgroup.controllers"} {
		if _, err := os.Stat(path); err != nil {
			return vllmGroupPeerRefusal(request), nil
		}
	}
	b := vllmGroupBinding{RunID: request.RunID, Generation: request.Generation, PlanDigest: request.PlanDigest, Plan: cloneVLLMGroupPlan(request.Plan), Rank: request.Rank}
	st, err := p.m.exec.state("vllm")
	if err != nil {
		return vllmGroupPeerResult{}, err
	}
	if request.Action == "capability" {
		result := vllmGroupPeerRefusal(request)
		if !lockVLLMGroupOwner(ctx, st) {
			return result, nil
		}
		st.opMu.Unlock()
		result.ActivationEnabled, result.State, result.Code, result.Reason = true, "available", "ok", ""
		return result, nil // permission and effective kernel enforcement are tested at Start
	}
	if request.Action == "prepare" {
		if !lockVLLMGroupOwner(ctx, st) {
			return vllmGroupPeerResult{}, rankStartError("admission", "prepare_owner_busy", ctx.Err(), 0)
		}
		defer st.opMu.Unlock()
		member, coordinator := b.Plan.Members[b.Rank], b.Plan.Members[0]
		if member.Placement == nil || coordinator.Placement == nil {
			return vllmGroupPeerResult{}, errors.New("reviewed placement missing")
		}
		placement := vllmRankPlacement{LocalNode: member.NodeID, LocalAddress: member.Placement.Address,
			CoordinatorAddress: coordinator.Placement.Address, ModelPath: member.Placement.ModelPath,
			APIPort: member.Placement.APIPort, MasterPort: member.Placement.MasterPort}
		rank, err := p.m.exec.prepareVLLMRank(ctx, st, b, placement)
		if err != nil {
			return vllmGroupPeerResult{}, err
		}
		record, err := readVLLMRuntimeRecord(st)
		if err != nil {
			return vllmGroupPeerResult{}, err
		}
		_, _, receipt, err := validateVLLMRuntimeBinaries(st, record.Active)
		if err != nil {
			return vllmGroupPeerResult{}, err
		}
		plan, err := buildVLLMSystemRankPlan(b, receipt)
		if err != nil {
			return vllmGroupPeerResult{}, err
		}
		rank.mu.Lock()
		rank.receipt.SystemPlan = &plan
		err = rank.save()
		rank.mu.Unlock()
		if err != nil {
			return vllmGroupPeerResult{}, err
		}
	}
	st.mu.Lock()
	rank := st.vllmRank
	st.mu.Unlock()
	if request.Action == "stop" || request.Action == "reconcile" {
		matches := false
		if rank != nil {
			rank.mu.Lock()
			matches = rank.matches(b)
			if matches && len(rank.binding.Plan.Members) == 0 {
				rank.binding.Plan = cloneVLLMGroupPlan(b.Plan)
			}
			rank.mu.Unlock()
		}
		if !matches {
			return p.closeMissingSystemRank(ctx, st, b, request.Action, request.Elevation)
		}
	}
	if rank == nil {
		return vllmGroupPeerResult{}, errors.New("exact prepared participant is unavailable")
	}
	return rank.systemAction(ctx, b, request.Action, request.Elevation)
}

func (r *vllmManagedRank) systemAction(ctx context.Context, b vllmGroupBinding, action string, elevation *diagnosticPackageElevation) (vllmGroupPeerResult, error) {
	r.mu.Lock()
	if !r.matches(b) || r.receipt.SystemPlan == nil {
		r.mu.Unlock()
		return vllmGroupPeerResult{}, errors.New("native rank generation changed")
	}
	r.mu.Unlock()
	var err error
	switch action {
	case "prepare":
	case "start":
		err = r.startSystem(ctx, elevation)
	case "ready":
		err = r.readySystem(ctx)
	case "status":
		_, err = r.observeSystem(ctx)
	case "stop", "reconcile":
		err = r.stopSystem(ctx, elevation)
	default:
		err = errors.New("unknown native rank action")
	}
	if err != nil {
		if action == "start" {
			failure := rankStartError("native-return", "unclassified", err, 0)
			r.mu.Lock()
			if validVLLMRankStartFailure(r.receipt.StartFailure) {
				failure = &vllmRankStartError{cloneVLLMRankStartFailure(*r.receipt.StartFailure)}
			} else {
				r.receipt.StartFailure = &failure.Failure
			}
			_ = r.save()
			r.mu.Unlock()
			return failedVLLMGroupStart(vllmGroupRequest(b, action), failure), nil
		}
		return vllmGroupPeerResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := vllmGroupPeerRefusal(vllmGroupRequest(b, action))
	result.ActivationEnabled, result.Code, result.Reason = true, "ok", ""
	result.State, result.EffectsApplied, result.CleanupConfirmed = r.receipt.State, true, r.receipt.CleanupConfirmed
	result.StartFailure = r.receipt.StartFailure
	if r.receipt.State == "stopped" {
		result.EffectsApplied = r.receipt.SystemEffectsApplied
	}
	return result, validateVLLMGroupPeerResult(vllmGroupRequest(b, action), result)
}

func (r *vllmManagedRank) startSystem(ctx context.Context, elevation *diagnosticPackageElevation) error {
	// Register before the rank unlock defer so every terminal snapshot is emitted
	// after locks are released, including a successful native Start acknowledgement.
	defer r.e.emitState("vllm")
	r.mu.Lock()
	if r.receipt.State != "prepared" || r.receipt.SystemPlan == nil || ctx.Err() != nil {
		r.mu.Unlock()
		return rankStartError("admission", "owner_not_prepared", ctx.Err(), 0)
	}
	record, err := readVLLMRuntimeRecord(r.st)
	if err != nil {
		r.mu.Unlock()
		return rankStartError("admission", "runtime_unavailable", err, 0)
	}
	_, _, runtimeReceipt, err := validateVLLMRuntimeBinaries(r.st, record.Active)
	if err == nil {
		err = r.e.validateVLLMReviewedRuntime(
			ctx,
			r.st,
			runtimeReceipt,
			r.binding.Plan.Members[r.binding.Rank],
		)
	}
	if err != nil {
		r.mu.Unlock()
		return rankStartError("admission", "runtime_changed", err, 0)
	}
	if isQwen38ProfileModel(r.binding.Plan.Model) {
		member := r.binding.Plan.Members[r.binding.Rank]
		facts, factsErr := r.e.currentQwen38GroupFacts(ctx, r.st, runtimeReceipt.Environment, runtimeReceipt, member.ModelDigest, member.RuntimeCompatibilitySHA256, len(r.binding.Plan.Members))
		if factsErr != nil || validateQwen38GroupFacts(facts, len(r.binding.Plan.Members)) != nil {
			r.mu.Unlock()
			return rankStartError("admission", "runtime_changed", errors.New("Qwen3.8 provider qualification changed before native start"), 0)
		}
	}
	r.receipt.State = "starting"
	if err = r.save(); err != nil {
		r.mu.Unlock()
		return rankStartError("journal", "write_failed", err, 0)
	}
	plan := *r.receipt.SystemPlan
	r.mu.Unlock()
	r.e.emitState("vllm") // Startup is observable before native admission returns.
	result, err := runVLLMSystemRank(ctx, "start", plan, nil, elevation)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.receipt.State != "starting" {
		return rankStartError("native-return", "start_withdrawn", nil, 0)
	}
	if err != nil {
		r.receipt.State = "cleanup-required"
		_ = r.save()
		return err
	}
	if !systemRankPolicyPassed(plan, result) || result.Supervisor.UID != r.receipt.SystemPlan.UID {
		return rankStartError("native-return", "policy_unconfirmed", nil, result.stdoutBytes)
	}
	policy := *result.Policy
	policy.EffectivePrograms.Ingress = slices.Clone(result.Policy.EffectivePrograms.Ingress)
	policy.EffectivePrograms.Egress = slices.Clone(result.Policy.EffectivePrograms.Egress)
	policy.HCAs = slices.Clone(result.Policy.HCAs)
	r.receipt.Supervisor, r.receipt.SystemPolicy, r.receipt.State = result.Supervisor, &policy, "started"
	r.receipt.SystemEffectsApplied = true
	if err := r.save(); err != nil {
		return rankStartError("journal", "write_failed", err, 0)
	}
	r.st.mu.Lock()
	r.st.proc, r.st.running, r.st.healthy, r.st.stopping, r.st.adopted = nil, true, false, false, false
	r.st.servingModel, r.st.version, r.st.port = r.binding.Plan.Model, r.binding.Plan.Runtime, r.placement.APIPort
	r.st.gen++
	r.st.mu.Unlock()
	return nil
}

func (r *vllmManagedRank) refreshSystemStatus(ctx context.Context) {
	_, err := r.observeSystem(ctx)
	healthy := false
	if err == nil {
		r.mu.Lock()
		ready := r.receipt.State == "ready" && r.binding.Rank == 0
		r.mu.Unlock()
		if ready {
			healthy = r.probeSystem(ctx)
		}
	}
	r.st.mu.Lock()
	r.st.healthy = healthy
	// Failed observation is not proof of exit; preserve running until Stop or
	// an exact native cleanup receipt confirms the cgroup is empty.
	if err == nil {
		r.st.running = true
	}
	r.st.mu.Unlock()
}

func (r *vllmManagedRank) currentSystemModels(ctx context.Context) (json.RawMessage, error) {
	r.mu.Lock()
	participant := r.binding.Rank != 0
	stopped := r.receipt.CleanupConfirmed
	r.mu.Unlock()
	data := []map[string]string{}
	if participant || stopped {
		return json.Marshal(map[string]any{"object": "list", "data": data})
	}
	if !r.e.snapshot("vllm", r.st).Healthy || !r.probeSystem(ctx) {
		return nil, errors.New("native coordinator residency is unconfirmed")
	}
	r.mu.Lock()
	model := r.receipt.SystemPlan.Model
	r.mu.Unlock()
	data = append(data, map[string]string{"id": model, "owned_by": "vllm"})
	return json.Marshal(map[string]any{"object": "list", "data": data})
}

func (r *vllmManagedRank) observeSystem(ctx context.Context) (vllmSystemRankResult, error) {
	r.mu.Lock()
	if r.receipt.SystemPlan == nil {
		r.mu.Unlock()
		return vllmSystemRankResult{}, errors.New("native owner absent")
	}
	plan, policy, supervisor := *r.receipt.SystemPlan, r.receipt.SystemPolicy, r.receipt.Supervisor
	r.mu.Unlock()
	if policy == nil || supervisor == nil {
		return vllmSystemRankResult{}, errors.New("native owner policy or supervisor is unavailable")
	}
	result, err := runVLLMSystemRank(ctx, "normal-status", plan, supervisor, nil)
	if err != nil {
		return result, err
	}
	wantTransport := "tcp-only"
	if plan.Model == vllmQwen38ModelID {
		wantTransport = vllmQwen38Transport
	} else if plan.DirectSocket != nil {
		wantTransport = vllmGroupDirectSocketMode
	} else if plan.RingSocket != nil {
		wantTransport = vllmGroupRingSocketMode
	}
	if result.State != "running" || result.CleanupConfirmed || result.MainPID != supervisor.PID ||
		!slices.Contains(result.OwnedPIDs, supervisor.PID) || result.Transport != wantTransport ||
		vllmSystemProcessTicks(supervisor.PID) != supervisor.StartTicks {
		return result, errors.New("current native cgroup owner is not live")
	}
	if !systemRankPolicyPassed(plan, vllmSystemRankResult{State: "started", EffectsApplied: true, Supervisor: supervisor, Policy: policy}) {
		return result, errors.New("current native Qwen3.8 RoCE policy differs from the reviewed plan")
	}
	return result, nil
}

func (r *vllmManagedRank) probeSystem(ctx context.Context) bool {
	status, err := r.observeSystem(ctx)
	if err != nil {
		return false
	}
	r.mu.Lock()
	plan := *r.receipt.SystemPlan
	state := r.receipt.State
	r.mu.Unlock()
	if plan.Rank != 0 || (state != "started" && state != "ready") {
		return false
	}
	pid, _, found := pidOnPort(plan.APIPort)
	if !found || !slices.Contains(status.OwnedPIDs, pid) {
		return false
	}
	ticks := vllmSystemProcessTicks(pid)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var version struct {
		Version string `json:"version"`
	}
	var models struct {
		Object string `json:"object"`
		Data   []struct {
			ID    string `json:"id"`
			Owner string `json:"owned_by"`
		} `json:"data"`
	}
	if r.e.vllmHTTP(ctx, plan.APIPort, http.MethodGet, "/version", nil, &version) != nil || version.Version != r.binding.Plan.Runtime ||
		r.e.vllmHTTP(ctx, plan.APIPort, http.MethodGet, "/health", nil, nil) != nil ||
		r.e.vllmHTTP(ctx, plan.APIPort, http.MethodGet, "/v1/models", nil, &models) != nil || models.Object != "list" || len(models.Data) != 1 || models.Data[0].ID != plan.Model || models.Data[0].Owner != "vllm" {
		return false
	}
	after, err := r.observeSystem(ctx)
	return err == nil && ticks != "" && ticks == vllmSystemProcessTicks(pid) && slices.Contains(after.OwnedPIDs, pid)
}

func (r *vllmManagedRank) readySystem(ctx context.Context) error {
	r.mu.Lock()
	wasReady := r.receipt.State == "ready"
	supervisor := r.receipt.Supervisor
	r.mu.Unlock()
	if supervisor == nil || supervisor.StartTicks == "" || vllmSystemProcessTicks(supervisor.PID) != supervisor.StartTicks {
		return errors.New("native coordinator supervisor is unavailable")
	}
	if !r.probeSystem(ctx) {
		if wasReady {
			return errors.New("qualified native coordinator lost model health")
		}
		// Startup is still pending. The outer readiness loop is bounded and the
		// exact supervisor identity above prevents a dead owner from waiting here.
		return nil
	}
	r.mu.Lock()
	if r.receipt.State == "ready" {
		r.mu.Unlock()
		return nil
	}
	if r.receipt.State != "started" {
		r.mu.Unlock()
		return errors.New("native coordinator generation changed before readiness")
	}
	r.receipt.State = "ready"
	if err := r.save(); err != nil {
		r.receipt.State = "failed"
		r.qualificationErr = err
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	r.st.mu.Lock()
	r.st.healthy = true
	r.st.mu.Unlock()
	r.e.emitState("vllm")
	return nil
}

func vllmSystemCleanupInvocation(supervisor *vllmRankSystemIdentity) (string, *vllmRankSystemIdentity) {
	if supervisor == nil || supervisor.PID <= 1 || supervisor.StartTicks == "" ||
		vllmSystemProcessTicks(supervisor.PID) != supervisor.StartTicks {
		return "reconcile", nil
	}
	return "normal-stop", supervisor
}

func applyVLLMSystemCleanup(receipt *vllmRankReceipt, result vllmSystemRankResult) {
	receipt.State, receipt.CleanupConfirmed = "stopped", true
	receipt.StartFenced = result.State == "closed" && result.StartFenced && result.Tombstone
	receipt.SystemEffectsApplied = !receipt.StartFenced
}

func (r *vllmManagedRank) stopSystem(ctx context.Context, elevation *diagnosticPackageElevation) error {
	// Publish after credential cleanup and both locks, including closed/error paths.
	defer r.e.emitState("vllm")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.receipt.SystemPlan == nil {
		return errors.New("native owner is unavailable")
	}
	if r.receipt.CleanupConfirmed {
		return nil
	}
	if r.qualificationCancel != nil {
		r.qualificationCancel()
	}
	r.receipt.State = "stopping"
	_ = r.save()
	r.st.mu.Lock()
	r.st.healthy, r.st.stopping = false, true
	r.st.mu.Unlock()
	action, supervisor := vllmSystemCleanupInvocation(r.receipt.Supervisor)
	if action == "reconcile" {
		if elevation == nil {
			return errors.New("native rank has no live normal-UID supervisor; provide fresh administrator access to reconcile")
		}
	}
	if action == "normal-stop" {
		elevation = nil
	}
	result, err := runVLLMSystemRank(ctx, action, *r.receipt.SystemPlan, supervisor, elevation)
	if err != nil || !systemRankCleanupPassed(result) {
		return errors.New("native rank cleanup is unconfirmed; reconcile this same operation in PAIR")
	}
	applyVLLMSystemCleanup(&r.receipt, result)
	if err := r.save(); err != nil {
		r.receipt.CleanupConfirmed = false
		return err
	}
	r.st.mu.Lock()
	r.st.running, r.st.healthy, r.st.stopping = false, false, false
	r.st.mu.Unlock()
	return nil
}
