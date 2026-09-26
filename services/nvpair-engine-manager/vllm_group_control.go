// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"
)

type vllmGroupStatus struct {
	ActivationEnabled bool          `json:"activationEnabled"`
	Reserved          bool          `json:"reserved"`
	Reason            string        `json:"reason"`
	Run               *vllmGroupRun `json:"run,omitempty"`
}

func (e *Executor) groupOwner(create bool) (*vllmServingGroup, error) {
	if e.usesVLLMWSL("vllm") {
		return nil, errors.New("distributed serving-group control is unavailable through the WSL child")
	}
	st, err := e.state("vllm")
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	g := st.vllmGroup
	st.mu.Unlock()
	if g != nil {
		g.mu.Lock()
		attached := g.call != nil && g.live != nil
		g.mu.Unlock()
		if attached {
			// Observing an attached owner must not compete with Prepare's TryLock.
			_ = g.releaseSettledFabricLease()
			return g, nil
		}
	}
	if !create && !st.opMu.TryLock() {
		// Status and Stop must remain reachable while a lifecycle operation owns
		// opMu. An idle restored owner is attached on the next control lookup.
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.vllmGroup, nil
	}
	if create {
		st.opMu.Lock()
	}
	defer st.opMu.Unlock()
	st.mu.Lock()
	g = st.vllmGroup
	st.mu.Unlock()
	if g == nil {
		g, err = newVLLMServingGroup(st, filepath.Join(st.installDir, "serving-group.json"))
	}
	if g != nil {
		st.mu.Lock()
		rank := st.vllmRank
		st.mu.Unlock()
		if rank == nil {
			restored := e.restoreVLLMRankHold(st)
			if restored != nil {
				run := g.status()
				restored.mu.Lock()
				if restored.receipt.RunID == run.RunID &&
					restored.receipt.Generation == run.Generation &&
					restored.receipt.PlanDigest == run.PlanDigest {
					restored.binding.Plan = cloneVLLMGroupPlan(run.Plan)
				}
				restored.mu.Unlock()
				st.mu.Lock()
				if st.vllmRank == nil {
					st.vllmRank = restored
				}
				st.mu.Unlock()
			}
		}
	}
	if g != nil && e.groupPeer != nil && e.groupPeer.native != nil {
		g.mu.Lock()
		unattached := g.call == nil && g.live == nil
		g.mu.Unlock()
		if unattached {
			// Restored owners need the same transport as new owners. Attachment
			// never clears a retained hold, rewrites its journal, or starts work.
			err = errors.Join(err, e.groupPeer.attach(g))
		}
	}
	if g != nil {
		err = errors.Join(err, g.releaseSettledFabricLease())
	}
	return g, err
}

func groupStatus(g *vllmServingGroup) vllmGroupStatus {
	result := vllmGroupStatus{Reason: errVLLMGroupNativeUnavailable.Error()}
	if g != nil {
		g.mu.Lock()
		result.ActivationEnabled = g.call != nil && g.live != nil
		g.mu.Unlock()
		if result.ActivationEnabled {
			result.Reason = ""
		}
		result.Reserved = g.reserved()
		run := g.status()
		if run.RunID != "" || result.Reserved {
			result.Run = &run
		}
	}
	return result
}

// Local stdio is the normal account control boundary. Plans are proposals;
// current pins are checked here, but native attestation/activation is not implied.
func (m *Manager) handleVLLMGroup(ctx context.Context, msg *Message) {
	if msg.Method == "engine:vllm-group-review" {
		var request struct {
			Selection vllmGroupSelection `json:"selection"`
		}
		if decodeActionParams(msg.Params, &request) != nil {
			m.codec.RespondError(msg.ID, -32602, "group review accepts only selected node IDs and exact model")
			return
		}
		plan, err := m.buildVLLMGroupPlan(ctx, request.Selection)
		digest := ""
		if err == nil {
			digest, err = vllmGroupPlanDigest(plan)
		}
		if err == nil && (m.exec.shuttingDown.Load() || m.exec.groupPeer == nil || m.cableLocal == nil || plan.Coordinator != m.cableLocal.nodeID) {
			err = errors.New("group review requires the current local coordinator and an open owner")
		}
		if err == nil {
			_, err = m.exec.groupPeer.principals(vllmGroupPeerRequest{Plan: plan, PlanDigest: digest})
		}
		if err != nil {
			m.respondOrErr(msg, nil, err)
			return
		}
		g, err := m.exec.groupOwner(true)
		if err != nil {
			m.respondOrErr(msg, nil, err)
			return
		}
		review, err := g.reviewPlan(plan)
		m.respondOrErr(msg, review, err)
		return
	}
	if msg.Method == "engine:vllm-group-status" {
		var empty struct{}
		if decodeActionParams(msg.Params, &empty) != nil {
			m.codec.RespondError(msg.ID, -32602, "group status accepts no parameters")
			return
		}
		g, err := m.exec.groupOwner(false)
		if err != nil {
			// A controller without a local runnable vLLM platform may still own
			// remote Linux routing state. Preserve the selected target-aware read.
			status, readErr := m.exec.VLLMGroupStatus()
			m.respondOrErr(msg, status, readErr)
			return
		}
		m.respondOrErr(msg, groupStatus(g), nil)
		return
	}
	if msg.Method == "engine:vllm-group-check" || msg.Method == "engine:vllm-group-start" {
		var request struct {
			ReviewID  string                       `json:"reviewId"`
			Elevation []diagnosticPackageElevation `json:"elevation,omitempty"`
		}
		if decodeActionParams(msg.Params, &request) != nil || !onboardingID.MatchString(request.ReviewID) {
			m.codec.RespondError(msg.ID, -32602, "an exact group reviewId is required")
			return
		}
		g, err := m.exec.groupOwner(false)
		if err != nil || g == nil || m.exec.shuttingDown.Load() {
			m.respondOrErr(msg, nil, errors.New("current group review is unavailable"))
			return
		}
		g.mu.Lock()
		var review vllmGroupReview
		valid := g.review != nil && g.review.ReviewID == request.ReviewID && time.Now().UnixMilli() < g.review.ExpiresAt && !g.held
		if valid {
			review = *g.review
			review.Plan = cloneVLLMGroupPlan(g.review.Plan)
		}
		g.mu.Unlock()
		if !valid {
			m.respondOrErr(msg, nil, errors.New("current unexpired unconsumed group review is required"))
			return
		}
		if msg.Method == "engine:vllm-group-check" && len(request.Elevation) != 0 {
			m.respondOrErr(msg, nil, errors.New("capability checks do not accept administrator credentials"))
			return
		}
		if msg.Method == "engine:vllm-group-start" {
			if m.exec.groupPeer == nil {
				m.respondOrErr(msg, nil, errVLLMGroupNativeUnavailable)
				return
			}
			if _, err := m.exec.groupPeer.principals(vllmGroupPeerRequest{Plan: review.Plan}); err != nil {
				m.respondOrErr(msg, nil, err)
				return
			}
			// Only a bootstrap-installed managed native owner can attach g.call.
			if len(request.Elevation) == 0 {
				// The renderer never carries administrator secrets. The fixed rank
				// helper first uses the product-owned noninteractive sudo route.
				request.Elevation = nonInteractiveVLLMGroupElevation(review.Plan)
			}
			auth, err := newVLLMGroupElevation(review.Plan, request.Elevation, true)
			if err != nil {
				m.respondOrErr(msg, nil, err)
				return
			}
			run, err := g.startWithElevation(ctx, request.ReviewID, auth)
			if err != nil {
				auth.close()
			}
			m.respondOrErr(msg, run, err)
			return
		}
		if m.exec.groupPeer == nil {
			m.respondOrErr(msg, nil, errVLLMGroupNativeUnavailable)
			return
		}
		results := make([]vllmGroupPeerResult, 0, len(review.Plan.Members))
		for rank := range review.Plan.Members {
			result, err := m.exec.groupPeer.control(ctx, vllmGroupPeerRequest{Protocol: vllmGroupPeerProtocol, Action: "capability", RunID: review.ReviewID, Generation: 1, PlanDigest: review.PlanDigest, Plan: review.Plan, Rank: rank})
			if err != nil {
				m.respondOrErr(msg, nil, err)
				return
			}
			results = append(results, result)
		}
		enabled := len(results) == len(review.Plan.Members)
		for _, result := range results {
			enabled = enabled && result.ActivationEnabled && result.State == "available"
		}
		m.codec.Respond(msg.ID, map[string]any{"reviewId": review.ReviewID, "activationEnabled": enabled, "participants": results})
		return
	}
	var request struct {
		RunID      string                       `json:"runId"`
		Generation uint64                       `json:"generation"`
		Elevation  []diagnosticPackageElevation `json:"elevation,omitempty"`
	}
	if decodeActionParams(msg.Params, &request) != nil || !onboardingID.MatchString(request.RunID) || request.Generation == 0 {
		m.codec.RespondError(msg.ID, -32602, "exact group runId and generation are required")
		return
	}
	g, err := m.exec.groupOwner(false)
	if err != nil || g == nil {
		m.respondOrErr(msg, nil, errors.New("retained group operation is unavailable"))
		return
	}
	if msg.Method == "engine:vllm-group-stop" {
		if len(request.Elevation) != 0 {
			m.respondOrErr(msg, nil, errors.New("ordinary group Stop accepts no administrator input"))
			return
		}
		// Serialize the generation check with start; a stale request must never
		// become an unqualified normal Stop against a replacement generation.
		g.engine.opMu.Lock()
		run := g.status()
		if run.RunID != request.RunID || run.Generation != request.Generation {
			err = errors.New("group operation generation changed")
		} else if !g.reserved() && run.CleanupConfirmed {
			// A consumed group Stop cannot stop later independent engine work or
			// rewrite its ON/OFF intent; preserve the completed group receipt.
			err = nil
		} else {
			err = errors.Join(m.exec.doStop(g.engine, "vllm"), m.exec.setDesiredEnabled("vllm", false))
		}
		g.engine.opMu.Unlock()
	} else {
		g.engine.opMu.Lock()
		run := g.status()
		if run.RunID != request.RunID || run.Generation != request.Generation {
			err = errors.New("group operation generation changed")
		} else {
			if len(request.Elevation) == 0 {
				request.Elevation = nonInteractiveVLLMGroupElevation(run.Plan)
			}
			var auth *vllmGroupElevation
			auth, err = newVLLMGroupElevation(run.Plan, request.Elevation, false)
			if err == nil {
				g.mu.Lock()
				active := false
				if g.done != nil {
					select {
					case <-g.done:
					default:
						active = true
					}
				}
				if !active {
					g.elevation = auth
				}
				g.mu.Unlock()
				if active {
					err = errors.New("cancel and join the active operation before reconciliation")
				} else {
					err = g.reconcileLocked(ctx, request.RunID, request.Generation)
				}
				auth.close()
			}
		}
		g.engine.opMu.Unlock()
	}
	m.respondOrErr(msg, groupStatus(g), err)
}
