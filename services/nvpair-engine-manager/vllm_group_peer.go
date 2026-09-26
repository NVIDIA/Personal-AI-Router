// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"
)

const vllmGroupPeerPath = "/v1/vllm/group-participant"
const vllmGroupPeerProtocol = "pair-vllm-group/1"
const vllmGroupPeerMaxBytes = 16 << 10

// This transport owns no ranks. The managed owner supplies an admitted native
// action callback; no callback is installed in this source candidate. A paired
// reply alone is not model/GPU attestation or proof of process cleanup.
type vllmGroupPeer struct {
	m *Manager
	// Installed only by the managed native rank owner after platform admission.
	native func(context.Context, vllmGroupPeerRequest) (vllmGroupPeerResult, error)
}

type vllmGroupPeerRequest struct {
	Elevation  *diagnosticPackageElevation `json:"elevation,omitempty"`
	Protocol   string                      `json:"protocol"`
	Action     string                      `json:"action"`
	RunID      string                      `json:"runId"`
	Generation uint64                      `json:"generation"`
	PlanDigest string                      `json:"planDigest"`
	Plan       vllmGroupPlan               `json:"plan"`
	Rank       int                         `json:"rank"`
}

type vllmGroupPeerResult struct {
	StartFailure      *vllmRankStartFailure `json:"startFailure,omitempty"`
	Protocol          string                `json:"protocol"`
	Action            string                `json:"action"`
	NodeID            string                `json:"nodeId"`
	Controller        string                `json:"controller"`
	RunID             string                `json:"runId"`
	Generation        uint64                `json:"generation"`
	PlanDigest        string                `json:"planDigest"`
	Rank              int                   `json:"rank"`
	State             string                `json:"state"`
	Code              string                `json:"code"`
	Reason            string                `json:"reason"`
	ActivationEnabled bool                  `json:"activationEnabled"`
	EffectsApplied    bool                  `json:"effectsApplied"`
	CleanupConfirmed  bool                  `json:"cleanupConfirmed"`
}

func validateVLLMGroupPeerRequest(r vllmGroupPeerRequest) error {
	digest, err := vllmGroupPlanDigest(r.Plan)
	digestMatches := err == nil && r.PlanDigest == digest
	legacyCleanup := false
	if !digestMatches && (r.Action == "status" || r.Action == "stop" || r.Action == "reconcile") {
		legacyDigest, legacyErr := legacyVLLMGroupPlanDigest(r.Plan)
		legacyCleanup = legacyErr == nil && r.PlanDigest == legacyDigest
	}
	if (!digestMatches && !legacyCleanup) || r.Protocol != vllmGroupPeerProtocol || !onboardingID.MatchString(r.RunID) || r.Generation == 0 || r.Rank < 0 || r.Rank >= len(r.Plan.Members) {
		return errors.New("invalid serving-group operation, generation, plan or rank")
	}
	if r.Elevation != nil {
		if r.Action == "capability" || r.Action == "prepare" || r.Elevation.NodeID != r.Plan.Members[r.Rank].NodeID {
			return errors.New("administrator input belongs only to this exact participant action")
		}
		passwords, consent, err := packageElevation([]diagnosticPackageElevation{*r.Elevation}, []diagnosticInspectionTarget{{NodeID: r.Elevation.NodeID}})
		clear(passwords)
		if err != nil || !consent[r.Elevation.NodeID] {
			return errors.New("invalid operation-scoped administrator input")
		}
	}
	switch r.Action {
	case "prepare", "start", "ready":
		if legacyCleanup {
			return errors.New("legacy serving-group generations are cleanup-only")
		}
		return nil
	case "capability", "status", "stop", "reconcile":
		return nil
	default:
		return errors.New("unknown serving-group participant action")
	}
}

func vllmGroupPeerRefusal(r vllmGroupPeerRequest) vllmGroupPeerResult {
	return vllmGroupPeerResult{Protocol: vllmGroupPeerProtocol, Action: r.Action,
		NodeID: r.Plan.Members[r.Rank].NodeID, Controller: r.Plan.Coordinator,
		RunID: r.RunID, Generation: r.Generation, PlanDigest: r.PlanDigest, Rank: r.Rank,
		State: "unavailable", Code: "native_unavailable", Reason: errVLLMGroupNativeUnavailable.Error()}
}

// Host UUIDs and cluster certificate principals are distinct namespaces. Resolve
// them through the existing directory, never from request-supplied endpoints.
func (p *vllmGroupPeer) principals(r vllmGroupPeerRequest) ([]string, error) {
	if p == nil || p.m == nil || p.m.mesh == nil || p.m.peers == nil || p.m.cableLocal == nil {
		return nil, errors.New("paired serving-group identity is unavailable")
	}
	p.m.mesh.Refresh()
	if !p.m.mesh.Clustered() {
		return nil, errors.New("paired serving-group membership is unavailable")
	}
	principals := make([]string, len(r.Plan.Members))
	for i, member := range r.Plan.Members {
		if member.NodeID == p.m.cableLocal.nodeID {
			principals[i] = p.m.mesh.NodeUUID()
		} else {
			peer, ok := p.m.peers.lookup(member.NodeID)
			if !ok || peer.clusterUUID == "" {
				return nil, errors.New("serving-group member is not a current ec peer")
			}
			principals[i] = peer.clusterUUID
		}
		pin, ok := p.m.mesh.PinSHA256(principals[i])
		if !ok || pin != member.PinSHA256 {
			return nil, errors.New("serving-group member pin changed")
		}
	}
	return principals, nil
}

func (p *vllmGroupPeer) local(ctx context.Context, caller string, r vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
	if err := validateVLLMGroupPeerRequest(r); err != nil {
		return vllmGroupPeerResult{}, err
	}
	principals, err := p.principals(r)
	if err != nil {
		return vllmGroupPeerResult{}, err
	}
	if ctx.Err() != nil {
		return vllmGroupPeerResult{}, ctx.Err()
	}
	if caller != principals[0] || r.Plan.Members[r.Rank].NodeID != p.m.cableLocal.nodeID || principals[r.Rank] != p.m.mesh.NodeUUID() {
		return vllmGroupPeerResult{}, errors.New("only the paired coordinator may address this exact participant rank")
	}
	if p.native == nil {
		return vllmGroupPeerRefusal(r), nil
	}
	result, err := p.native(ctx, r)
	if err != nil {
		if r.Action == "prepare" {
			return failedVLLMGroupPrepare(r, err), nil
		}
		if r.Action == "start" {
			return failedVLLMGroupStart(r, rankStartError("admission", "unclassified", err, 0)), nil
		}
		return result, err
	}
	err = validateVLLMGroupPeerResult(r, result)
	if err != nil && r.Action == "prepare" {
		return failedVLLMGroupPrepare(r, rankStartError("peer-return", "binding_mismatch", err, 0)), nil
	}
	if err != nil && r.Action == "start" {
		return failedVLLMGroupStart(r, rankStartError("peer-return", "binding_mismatch", err, 0)), nil
	}
	return result, err
}

// All replies remain bound to current membership and the exact operation.
// An absent native owner remains a typed refusal for execution and cleanup.
func (p *vllmGroupPeer) control(ctx context.Context, r vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
	if err := validateVLLMGroupPeerRequest(r); err != nil {
		return vllmGroupPeerResult{}, err
	}
	if isQwen38ProfileModel(r.Plan.Model) && r.Action != "stop" && r.Action != "reconcile" {
		if p == nil || p.m == nil || p.m.exec == nil {
			return vllmGroupPeerResult{}, errors.New("Qwen3.8 fabric owner is unavailable")
		}
		if err := currentVLLMGroupTransport(p.m.exec.fabric, r.Plan); err != nil {
			return vllmGroupPeerResult{}, err
		}
		lease := fabricConsumerLease{Owner: fabricLeaseOwnerServingGroup, RunID: r.RunID, Generation: r.Generation, PlanDigest: r.PlanDigest}
		if (r.Action == "prepare" || r.Action == "start") && !p.m.exec.fabric.consumerLeaseHeld(r.Plan.Transport.OperationID, lease) {
			return vllmGroupPeerResult{}, errors.New("the Qwen3.8 fabric consumer lease must be held before any rank starts")
		}
	}
	if r.Plan.DirectSocket != nil && r.Action != "stop" && r.Action != "reconcile" {
		if p == nil || p.m == nil || p.m.exec == nil || p.m.exec.fabric == nil {
			return vllmGroupPeerResult{}, errors.New("the direct fabric owner is unavailable")
		}
		if err := currentVLLMGroupDirectSocket(p.m.exec.fabric, r.Plan); err != nil {
			return vllmGroupPeerResult{}, err
		}
		lease := fabricConsumerLease{Owner: fabricLeaseOwnerServingGroup, RunID: r.RunID, Generation: r.Generation, PlanDigest: r.PlanDigest}
		if (r.Action == "prepare" || r.Action == "start") && !p.m.exec.fabric.consumerLeaseHeld(r.Plan.DirectSocket.OperationID, lease) {
			return vllmGroupPeerResult{}, errors.New("the direct fabric consumer lease must be held before any rank starts")
		}
	}
	timeout := vllmGroupPeerActionBudget(r)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	before, err := p.principals(r)
	if err != nil {
		return vllmGroupPeerResult{}, err
	}
	if before[0] != p.m.mesh.NodeUUID() || r.Plan.Coordinator != p.m.cableLocal.nodeID {
		return vllmGroupPeerResult{}, errors.New("serving-group control belongs to the local coordinator")
	}
	var result vllmGroupPeerResult
	if r.Plan.Members[r.Rank].NodeID == p.m.cableLocal.nodeID {
		result, err = p.local(ctx, before[0], r)
	} else {
		peer, ok := p.m.peers.lookup(r.Plan.Members[r.Rank].NodeID)
		if !ok || peer.clusterUUID != before[r.Rank] {
			return result, errors.New("serving-group participant identity changed")
		}
		var client *remoteClient
		client, err = p.m.remoteClient(ctx, peer)
		if err == nil {
			result, err = client.vllmGroupParticipant(ctx, r)
		}
	}
	if err != nil {
		return vllmGroupPeerResult{}, rankStartError("peer-transport", "request_failed", err, 0)
	}
	after, err := p.principals(r)
	if err != nil || !slices.Equal(before, after) || ctx.Err() != nil || validateVLLMGroupPeerResult(r, result) != nil {
		return vllmGroupPeerResult{}, rankStartError("peer-return", "membership_changed", err, 0)
	}
	if r.Action == "capability" || r.Action == "status" {
		return result, nil
	}
	return result, vllmGroupActionOutcome(r, result)
}

func vllmGroupPeerActionBudget(r vllmGroupPeerRequest) time.Duration {
	timeout := 10 * time.Second
	if r.Action == "ready" {
		// Native coordinator readiness may perform two exact root-owned status
		// reads while the API listener is still coming up.
		timeout = 90 * time.Second
	}
	if r.Action == "start" || r.Action == "prepare" {
		timeout = 100 * time.Second
	}
	if (r.Action == "prepare" || r.Action == "start") && isQwen38ProfileModel(r.Plan.Model) {
		// The root helper gets ten minutes for the 132.7 GB content preflight;
		// this wrapper leaves one minute for pinned transport and policy overhead.
		timeout = 11 * time.Minute
	}
	if r.Action == "stop" || r.Action == "reconcile" {
		timeout = vllmGroupParticipantCleanupBudget
	}
	return timeout
}

func (c *remoteClient) vllmGroupParticipant(ctx context.Context, r vllmGroupPeerRequest) (vllmGroupPeerResult, error) {
	var result vllmGroupPeerResult
	if err := validateVLLMGroupPeerRequest(r); err != nil {
		return result, err
	}
	client := c.vllmGroupParticipantClient(r)
	var raw json.RawMessage
	var err error
	if client != c {
		raw, err = client.postJSONWithTimeout(ctx, vllmGroupPeerPath, "vllm", r, vllmGroupPeerActionBudget(r))
	} else {
		raw, err = client.postJSON(ctx, vllmGroupPeerPath, "vllm", r)
	}
	if err != nil {
		return result, rankStartError("peer-transport", "request_failed", err, 0)
	}
	if len(raw) > vllmGroupPeerMaxBytes || strictDiagnosticJSON(raw, &result) != nil {
		return vllmGroupPeerResult{}, rankStartError("peer-return", "invalid_response", nil, len(raw))
	}
	if validateVLLMGroupPeerResult(r, result) != nil {
		return vllmGroupPeerResult{}, rankStartError("peer-return", "binding_mismatch", nil, len(raw))
	}
	return result, nil
}

func (c *remoteClient) vllmGroupParticipantClient(r vllmGroupPeerRequest) *remoteClient {
	if c != nil && c.readyHTTP != nil && isQwen38ProfileModel(r.Plan.Model) && (r.Action == "prepare" || r.Action == "start") {
		copy := *c
		copy.http = c.readyHTTP
		return &copy
	}
	return c
}

// Register only on the existing ec mTLS mux. Authentication is also checked
// here, so accidentally omitting a wrapper cannot admit plaintext callers.
func (p *vllmGroupPeer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if p == nil || p.m == nil || p.m.mesh == nil {
		http.Error(w, "serving-group participant unavailable", http.StatusServiceUnavailable)
		return
	}
	p.m.mesh.Refresh()
	caller, ok := p.m.mesh.VerifyClientPin(r)
	if !ok {
		http.Error(w, "paired coordinator required", http.StatusForbidden)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	raw, err := io.ReadAll(io.LimitReader(r.Body, vllmGroupPeerMaxBytes+1))
	var request vllmGroupPeerRequest
	if err != nil || len(raw) > vllmGroupPeerMaxBytes || strictDiagnosticJSON(raw, &request) != nil {
		http.Error(w, "invalid serving-group participant request", http.StatusBadRequest)
		return
	}
	result, err := p.local(r.Context(), caller, request)
	if err != nil {
		http.Error(w, "serving-group controller, rank or plan is not admitted", http.StatusConflict)
		return
	}
	// local re-reads membership; bind the same authenticated certificate to that
	// refreshed view too, rather than retaining only its principal across rekeys.
	currentCaller, pinned := p.m.mesh.VerifyClientPin(r)
	if !pinned || currentCaller != caller {
		http.Error(w, "paired coordinator changed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
