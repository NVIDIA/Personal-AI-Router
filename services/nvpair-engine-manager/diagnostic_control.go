// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

const diagnosticControlPath = "/v1/diagnostics/nccl"

type diagnosticControlRequest struct {
	Method       string                           `json:"method"`
	Request      diagnosticRequest                `json:"request"`
	Participant  *diagnosticParticipantRequest    `json:"participant,omitempty"`
	Bootstrap    *diagnosticBootstrapControl      `json:"bootstrap,omitempty"`
	MPISelection *diagnosticMPIInterfaceSelection `json:"mpiSelection,omitempty"`
	MPIReview    *diagnosticMPIReviewControl      `json:"mpiReview,omitempty"`
	MPIFacts     *diagnosticMPIFactsRequest       `json:"mpiFacts,omitempty"`
	MPIReviewID  string                           `json:"mpiReviewId,omitempty"`
	MPIRecovery  *diagnosticMPIRecoveryControl    `json:"mpiRecovery,omitempty"`
	MPIReconcile *diagnosticMPIReconcileControl   `json:"mpiReconcile,omitempty"`
	MPIFabric    *diagnosticMPIFabricCheck        `json:"mpiFabric,omitempty"`
}

func strictDiagnosticJSON(raw []byte, out any) error {
	if len(raw) > 128<<10 {
		return errors.New("diagnostic request exceeds size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("diagnostic request must contain exactly one JSON value")
	}
	return nil
}

func profileDigest(p diagnosticProfile) string {
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (d *diagnosticService) client(ctx context.Context, member diagnosticMember) (*remoteClient, error) {
	peer, ok := d.m.peers.lookup(member.NodeID)
	if !ok || peer.clusterUUID != member.Principal || !slices.Contains(peer.addresses, member.Host) {
		return nil, errors.New("configured participant is not the currently discovered pinned ec identity")
	}
	return d.m.remoteClient(ctx, peer)
}

func (d *diagnosticService) remoteOperation(ctx context.Context, p diagnosticProfile, owner diagnosticMember, method string, request diagnosticRequest) (any, error) {
	client, err := d.client(ctx, owner)
	if err != nil {
		return nil, err
	}
	raw, err := client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: method, Request: request})
	if err != nil {
		return nil, err
	}
	var op diagnosticOperation
	if err := strictDiagnosticJSON(raw, &op); err != nil {
		return nil, err
	}
	if op.OwnerNodeID != owner.NodeID || op.GroupID != p.GroupID || (request.OperationID != "" && op.OperationID != request.OperationID) {
		return nil, errors.New("diagnostic result owner, group, or operation did not match the request")
	}
	return op, nil
}

func (d *diagnosticService) callParticipant(ctx context.Context, p diagnosticProfile, member diagnosticMember, action string, request diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	d.m.mesh.Refresh()
	if !d.m.mesh.Clustered() || !d.m.mesh.HasPin(member.Principal) {
		return diagnosticParticipantResult{}, errors.New("diagnostic participant is not a currently admitted pinned identity")
	}
	if member.Principal == d.m.mesh.NodeUUID() {
		return d.localParticipant(ctx, p, action, request)
	}
	client, err := d.client(ctx, member)
	if err != nil {
		return diagnosticParticipantResult{}, err
	}
	raw, err := client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: action, Request: diagnosticRequest{GroupID: p.GroupID}, Participant: &request})
	if err != nil {
		return diagnosticParticipantResult{}, err
	}
	var result diagnosticParticipantResult
	if err := strictDiagnosticJSON(raw, &result); err != nil {
		return result, err
	}
	if result.NodeID != member.NodeID {
		return result, errors.New("diagnostic participant reply has the wrong identity")
	}
	return result, nil
}

func (s *controlServer) handleDiagnostic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.exec.diagnostics == nil {
		http.Error(w, "diagnostics unavailable", 503)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (128<<10)+1))
	var body diagnosticControlRequest
	if err == nil {
		err = strictDiagnosticJSON(raw, &body)
	}
	if err != nil {
		http.Error(w, "invalid diagnostic request", 400)
		return
	}
	d := s.exec.diagnostics
	if body.MPIFabric != nil {
		// Only a plan member may ask the fabric owner to revalidate its fabric.
		caller, pinned := s.mesh.VerifyClientPin(r)
		if !pinned || body.Method != "mpi-fabric" || body.MPIReconcile != nil || body.MPIRecovery != nil || body.MPIReview != nil || body.MPIFacts != nil || body.MPIReviewID != "" || body.Participant != nil || body.Bootstrap != nil || body.MPISelection != nil || body.Request != (diagnosticRequest{}) || !slices.Contains(body.MPIFabric.Principals, caller) {
			http.Error(w, "invalid MPI fabric revalidation envelope", 400)
			return
		}
		if err := d.currentMPIFabric(r.Context(), *body.MPIFabric); err != nil {
			http.Error(w, diagnosticPublicMessage(err.Error()), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body.MPIFabric.Fabric)
		return
	}
	if body.MPIReconcile != nil {
		caller, pinned := s.mesh.VerifyClientPin(r)
		if !pinned || body.Method != "mpi-reconcile" || body.MPIRecovery != nil || body.MPIReview != nil || body.MPIFacts != nil || body.MPIReviewID != "" || body.Participant != nil || body.Bootstrap != nil || body.MPISelection != nil || body.Request != (diagnosticRequest{}) {
			http.Error(w, "invalid MPI reconciliation envelope", 400)
			return
		}
		result, reconcileErr := d.reconcileRetainedMPI(r.Context(), caller, *body.MPIReconcile)
		if reconcileErr != nil {
			http.Error(w, diagnosticPublicMessage(reconcileErr.Error()), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if body.MPIRecovery != nil {
		caller, pinned := s.mesh.VerifyClientPin(r)
		if !pinned || body.Method != "mpi-recover" || body.MPIReconcile != nil || body.MPIReview != nil || body.MPIFacts != nil || body.MPIReviewID != "" || body.Participant != nil || body.Bootstrap != nil || body.MPISelection != nil || body.Request != (diagnosticRequest{}) {
			http.Error(w, "invalid MPI recovery envelope", 400)
			return
		}
		result, recoverErr := d.recoverMPIAsCoordinator(r.Context(), caller, *body.MPIRecovery)
		if recoverErr != nil {
			http.Error(w, diagnosticPublicMessage(recoverErr.Error()), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if body.MPIReview != nil || body.MPIFacts != nil || body.MPIReviewID != "" {
		caller, pinned := s.mesh.VerifyClientPin(r)
		if !pinned || body.Participant != nil || body.Bootstrap != nil || body.MPISelection != nil || body.Request != (diagnosticRequest{}) {
			http.Error(w, "invalid authenticated MPI control envelope", 400)
			return
		}
		var value any
		var controlErr error
		switch {
		case body.Method == "mpi-review" && body.MPIReview != nil && body.MPIFacts == nil && body.MPIReviewID == "":
			value, controlErr = d.reviewMPIAsCoordinator(r.Context(), caller, *body.MPIReview)
		case body.Method == "mpi-facts" && body.MPIFacts != nil && body.MPIReview == nil && body.MPIReviewID == "":
			value, controlErr = d.localMPIFacts(r.Context(), *body.MPIFacts)
		case body.Method == "mpi-approve" && onboardingID.MatchString(body.MPIReviewID) && body.MPIReview == nil && body.MPIFacts == nil:
			value, controlErr = d.approveMPI(caller, body.MPIReviewID)
		case body.Method == "mpi-close-review" && onboardingID.MatchString(body.MPIReviewID) && body.MPIReview == nil && body.MPIFacts == nil:
			value, controlErr = d.closeUnstartedMPIReview(caller, body.MPIReviewID)
		default:
			http.Error(w, "invalid fixed MPI control action", 400)
			return
		}
		if controlErr != nil {
			http.Error(w, diagnosticPublicMessage(controlErr.Error()), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
		return
	}
	if body.MPISelection != nil {
		_, pinned := s.mesh.VerifyClientPin(r)
		if !pinned || body.Method != "mpi-address" || body.Bootstrap != nil || body.Participant != nil {
			http.Error(w, "a pinned caller may request only current selected interface facts", 403)
			return
		}
		result, addressErr := d.localMPIAddress(r.Context(), *body.MPISelection)
		if addressErr != nil {
			http.Error(w, diagnosticPublicMessage(addressErr.Error()), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if body.Bootstrap != nil {
		p := body.Bootstrap.Profile
		caller, ok := s.mesh.VerifyClientPin(r)
		owner, known := p.member(p.OwnerNodeID)
		if !ok || !known || caller != owner.Principal || body.Participant == nil || (body.Method != "bootstrap-prepare" && body.Method != "bootstrap-cancel") || body.Request.GroupID != p.GroupID {
			http.Error(w, "only the admitted coordinator may prepare its managed MPI participants", 403)
			return
		}
		if err := p.validate(); err != nil {
			http.Error(w, "invalid managed MPI profile", 400)
			return
		}
		var result diagnosticParticipantResult
		var prepareErr error
		if body.Method == "bootstrap-prepare" {
			result, prepareErr = d.prepareBootstrapParticipant(r.Context(), p, body.Bootstrap.Plan, *body.Participant)
		} else {
			_, prepareErr = validateDiagnosticMPIBinding(p, body.Bootstrap.Plan, *body.Participant, false)
			self, selfErr := d.self(p)
			result.NodeID = self.NodeID
			if prepareErr == nil {
				prepareErr = selfErr
			}
			if prepareErr == nil {
				_, prepareErr = d.participantCancellationBinding(*body.Participant, caller)
			}
			if prepareErr == nil {
				result.CleanupConfirmed, prepareErr = d.cancelParticipant(r.Context(), *body.Participant)
			}
		}
		if prepareErr != nil {
			http.Error(w, diagnosticPublicMessage(prepareErr.Error()), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if body.Participant != nil && body.Participant.BootstrapPlanDigest != "" && body.Method == "cancel" {
		p, _, loadErr := d.loadBootstrapOperation(*body.Participant)
		caller, ok := s.mesh.VerifyClientPin(r)
		owner, known := p.member(p.OwnerNodeID)
		if loadErr != nil || !ok || !known || caller != owner.Principal || body.Request.GroupID != p.GroupID {
			http.Error(w, "original managed MPI owner or profile is unavailable", 409)
			return
		}
		self, selfErr := d.self(p)
		if selfErr != nil {
			http.Error(w, "participant identity changed", 409)
			return
		}
		if _, bindErr := d.participantCancellationBinding(*body.Participant, caller); bindErr != nil {
			http.Error(w, diagnosticPublicMessage(bindErr.Error()), 409)
			return
		}
		clean, cancelErr := d.cancelParticipant(r.Context(), *body.Participant)
		if cancelErr != nil {
			http.Error(w, diagnosticPublicMessage(cancelErr.Error()), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diagnosticParticipantResult{NodeID: self.NodeID, CleanupConfirmed: clean})
		return
	}
	p, err := d.operationProfile(body.Method, body.Request)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if err := p.validate(); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	var result any
	if body.Participant != nil {
		caller, ok := s.mesh.VerifyClientPin(r)
		owner, _ := p.member(p.OwnerNodeID)
		// Any pinned controller may request a capability read; participant
		// mutations are restricted to the profile's exact coordinator.
		if !ok || (body.Method != "capability" && caller != owner.Principal) {
			http.Error(w, "only the configured coordinator may prepare or cancel ranks", 403)
			return
		}
		if body.Participant.GroupID != p.GroupID {
			http.Error(w, "group mismatch", 400)
			return
		}
		if body.Method == "cancel" {
			if _, bindErr := d.participantCancellationBinding(*body.Participant, caller); bindErr != nil {
				http.Error(w, diagnosticPublicMessage(bindErr.Error()), 409)
				return
			}
		}
		result, err = d.localParticipant(r.Context(), p, body.Method, *body.Participant)
	} else {
		owner, _ := p.member(p.OwnerNodeID)
		if owner.Principal != s.mesh.NodeUUID() {
			http.Error(w, "this node is not the configured diagnostic coordinator", 409)
			return
		}
		if body.Method != "engine:diagnostic-start" && body.Method != "engine:diagnostic-status" && body.Method != "engine:diagnostic-cancel" {
			http.Error(w, "unknown diagnostic method", 400)
			return
		}
		result, err = d.dispatch(r.Context(), body.Method, body.Request)
	}
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (m *Manager) handleDiagnostic(ctx context.Context, msg *Message) {
	request, err := decodeDiagnosticRequest(msg.Params)
	if err != nil {
		m.codec.RespondError(msg.ID, -32602, err.Error())
		return
	}
	result, err := m.exec.diagnostics.dispatch(ctx, msg.Method, request)
	m.respondOrErr(msg, result, err)
}

// A request-correlated broker snapshot is required at preparation and while
// running. Silence is unknown, never an idle workload count.
func (d *diagnosticService) checkWorkloads(ctx context.Context) error {
	id := newOpID()
	ch := make(chan bool, 1)
	d.mu.Lock()
	d.workloadWait[id] = ch
	d.mu.Unlock()
	defer func() { d.mu.Lock(); delete(d.workloadWait, id); d.mu.Unlock() }()
	if err := d.m.codec.Notify("engine:diagnostic-workload-check", map[string]string{"requestId": id}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	select {
	case idle := <-ch:
		if !idle {
			return errors.New("active PAIR workloads prevent a collective test")
		}
		return nil
	case <-ctx.Done():
		return errors.New("fresh PAIR workload availability could not be confirmed")
	}
}

func (d *diagnosticService) receiveWorkloads(raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Idle      *bool  `json:"idle"`
	}
	if strictDiagnosticJSON(raw, &p) != nil || p.Idle == nil {
		return
	}
	d.mu.Lock()
	ch := d.workloadWait[p.RequestID]
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- *p.Idle:
		default:
		}
	}
}

func (d *diagnosticService) managedIdle() error {
	d.m.exec.desired.mu.Lock()
	state, err := d.m.exec.desired.load()
	d.m.exec.desired.mu.Unlock()
	if err != nil {
		return err
	}
	for engine, enabled := range state.Engines {
		if enabled {
			return fmt.Errorf("disable managed %s before running a collective test", engine)
		}
	}
	d.m.exec.mu.Lock()
	defer d.m.exec.mu.Unlock()
	for engine, st := range d.m.exec.engines {
		st.mu.Lock()
		running, starting := st.running, st.startCancel != nil
		st.mu.Unlock()
		if running || starting {
			return fmt.Errorf("managed %s is running or starting", engine)
		}
	}
	return nil
}
