// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"
)

// A busy participant is refused promptly rather than holding its caller for
// the engine action's full run.
const (
	fabricAdmissionWait = 5 * time.Second
	fabricAdmissionPoll = 25 * time.Millisecond
)

type fabricControlRequest struct {
	Method      string              `json:"method"`
	OperationID string              `json:"operationId,omitempty"`
	Target      fabricTarget        `json:"target"`
	Candidates  []fabricCandidateIP `json:"candidates,omitempty"`
}
type fabricControlResult struct {
	NodeID      string              `json:"nodeId"`
	Principal   string              `json:"principal"`
	OperationID string              `json:"operationId,omitempty"`
	Inventory   *fabricInventory    `json:"inventory,omitempty"`
	Reserved    bool                `json:"reserved"`
	Qualified   bool                `json:"qualified,omitempty"`
	Candidates  []fabricCandidateIP `json:"candidates,omitempty"`
	FailureCode string              `json:"failureCode,omitempty"`
}

type fabricQualificationError struct {
	Code  string
	cause error
}

func (e *fabricQualificationError) Error() string { return e.Code }
func (e *fabricQualificationError) Unwrap() error { return e.cause }

func validFabricQualificationFailureCode(code string) bool {
	switch code {
	case "route-unavailable", "gid-unavailable", "pinned-identity-unavailable", "provider-unavailable", "result-invalid", "deadline-exceeded", "cancelled":
		return true
	}
	return false
}

func fabricQualificationFailure(code string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.Canceled) {
		code = "cancelled"
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code = "deadline-exceeded"
	} else {
		var existing *fabricQualificationError
		if errors.As(cause, &existing) && validFabricQualificationFailureCode(existing.Code) {
			return existing
		}
		if !validFabricQualificationFailureCode(code) {
			code = "result-invalid"
		}
	}
	return &fabricQualificationError{Code: code, cause: cause}
}

func fabricQualificationFailureCode(err error, fallback string) string {
	var failure *fabricQualificationError
	wrapped := fabricQualificationFailure(fallback, err)
	if errors.As(wrapped, &failure) {
		return failure.Code
	}
	return "result-invalid"
}

func (s *fabricService) readInventory(ctx context.Context, nodeID string) (fabricInventory, error) {
	principal := ""
	if s.m.cableLocal != nil && nodeID == s.m.cableLocal.nodeID {
		principal = s.m.mesh.NodeUUID()
	} else if peer, ok := s.m.peers.lookup(nodeID); ok {
		principal = peer.clusterUUID
	}
	result, e := s.callControl(ctx, fabricTarget{NodeID: nodeID, Principal: principal}, fabricControlRequest{Method: "inventory", Target: fabricTarget{NodeID: nodeID, Principal: principal}})
	if e != nil {
		return fabricInventory{}, e
	}
	if result.Inventory == nil {
		return fabricInventory{}, errors.New("fabric inventory missing")
	}
	return *result.Inventory, nil
}
func (s *fabricService) callControl(ctx context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
	s.m.mesh.Refresh()
	if !s.m.mesh.Clustered() || !s.m.mesh.HasPin(target.Principal) {
		return fabricControlResult{}, errors.New("fabric participant is not currently pinned")
	}
	if s.m.cableLocal != nil && target.NodeID == s.m.cableLocal.nodeID {
		return s.localControl(ctx, s.m.mesh.NodeUUID(), request)
	}
	peer, ok := s.m.peers.lookup(target.NodeID)
	if !ok || peer.clusterUUID != target.Principal {
		return fabricControlResult{}, errors.New("fabric participant identity changed")
	}
	client, e := s.m.remoteClient(ctx, peer)
	if e != nil {
		return fabricControlResult{}, e
	}
	body, e := client.postJSON(ctx, fabricControlPath, "", request)
	if e != nil {
		return fabricControlResult{}, e
	}
	var result fabricControlResult
	if onboardingDecode(body, &result) != nil || result.NodeID != target.NodeID || result.Principal != target.Principal || result.OperationID != request.OperationID {
		return result, errors.New("fabric response binding mismatch")
	}
	return result, nil
}

func (s *fabricService) localControl(ctx context.Context, owner string, request fabricControlRequest) (fabricControlResult, error) {
	result := fabricControlResult{OperationID: request.OperationID}
	if s.m.cableLocal == nil {
		return result, errors.New("local fabric identity unavailable")
	}
	result.NodeID, result.Principal = s.m.cableLocal.nodeID, s.m.mesh.NodeUUID()
	if request.Target.NodeID != result.NodeID || request.Target.Principal != result.Principal {
		return result, errors.New("fabric target identity mismatch")
	}
	if request.Method == "inventory" {
		facts, e := readFabricInventory(ctx, s.m.cableLocal, s.m.mesh)
		result.Inventory = &facts
		return result, e
	}
	if !onboardingID.MatchString(request.OperationID) || !cableIdentifier(owner, 256) || len(request.Target.Interfaces) != 2 {
		return result, errors.New("invalid exact fabric reservation")
	}
	// Re-proof only reads and release only relaxes admission. Neither may wait
	// behind an engine action, which holds admission for its whole run.
	if request.Method == "requalify" {
		qualified, err := s.qualifyLocal(ctx, request.Target, request.Candidates)
		if err != nil {
			result.FailureCode = fabricQualificationFailureCode(err, "provider-unavailable")
			return result, nil
		}
		result.Qualified = true
		result.Candidates = qualified
		return result, nil
	}
	if request.Method == "release" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.reservation == nil {
			return result, nil
		}
		if s.reservation.OperationID != request.OperationID || s.reservation.OwnerPrincipal != owner || !fabricTargetsEqual(s.reservation.Target, request.Target) {
			return result, errors.New("only the exact fabric owner may release its reservation")
		}
		if e := os.Remove(s.reservationFile()); e != nil && !errors.Is(e, os.ErrNotExist) {
			return result, errors.New("fabric reservation cleanup could not be retained")
		}
		s.reservation = nil
		return result, nil
	}
	if request.Method != "reserve" && request.Method != "reserve-rollback" && request.Method != "qualify" {
		return result, errors.New("unknown fabric control method")
	}
	// The same lock protects managed starts and diagnostic reservations. A new
	// fabric hold is published while it is held; no check-then-act admission gap.
	if err := s.lockAdmission(ctx, fabricAdmissionWait); err != nil {
		return result, err
	}
	defer s.m.exec.diagnosticMu.Unlock()
	s.mu.Lock()
	existing := s.reservation
	s.mu.Unlock()
	if request.Method == "qualify" {
		if existing == nil || existing.OperationID != request.OperationID || existing.OwnerPrincipal != owner || !fabricTargetsEqual(existing.Target, request.Target) {
			return result, errors.New("exact active fabric reservation unavailable for qualification")
		}
		qualified, err := s.qualifyLocal(ctx, request.Target, request.Candidates)
		if err != nil {
			result.FailureCode = fabricQualificationFailureCode(err, "provider-unavailable")
			return result, nil
		}
		result.Qualified = true
		result.Candidates = qualified
		return result, nil
	}
	if existing != nil {
		if existing.OperationID == request.OperationID && existing.OwnerPrincipal == owner && fabricTargetsEqual(existing.Target, request.Target) {
			if request.Method == "reserve" {
				result.Reserved = true
				return result, nil
			}
		} else {
			return result, errors.New("another fabric reservation requires cleanup")
		}
	}
	if s.otherBusy() {
		return result, errors.New("diagnostic or cable operation has active or unconfirmed resources")
	}
	if d := s.m.exec.diagnostics; d != nil {
		if e := d.managedIdle(); e != nil {
			return result, e
		}
		if e := d.checkWorkloads(ctx); e != nil {
			return result, e
		}
	} else {
		return result, errors.New("workload admission unavailable")
	}
	facts, e := readFabricInventory(ctx, s.m.cableLocal, s.m.mesh)
	valid := fabricSameTarget(request.Target, facts)
	if request.Method == "reserve-rollback" {
		valid = fabricSameIdentity(request.Target, facts)
	}
	if e != nil || !valid {
		return result, errors.New("fabric inventory changed before reservation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Release does not wait for admission, so a caller that already gave up must
	// not have its hold published after its compensating release ran.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if s.shuttingDown || s.recoveryFailed {
		return result, errors.New("retained fabric ownership is unavailable")
	}
	for id, r := range s.runs {
		if id != request.OperationID && !r.Public.CleanupConfirmed && r.Public.State != "active" {
			return result, errors.New("another fabric operation requires cleanup")
		}
	}
	reservation := fabricReservation{OperationID: request.OperationID, OwnerPrincipal: owner, Target: request.Target}
	if e := writeJSONAtomic(s.reservationFile(), reservation); e != nil {
		return result, errors.New("fabric reservation could not be retained")
	}
	s.reservation = &reservation
	result.Reserved = true
	return result, nil
}

// lockAdmission takes the admission lock without joining the RWMutex writer
// queue: a queued writer stalls every later engine start behind the in-flight
// action, and it still runs after its caller gave up.
func (s *fabricService) lockAdmission(ctx context.Context, wait time.Duration) error {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(fabricAdmissionPoll)
	defer poll.Stop()
	for !s.m.exec.diagnosticMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("fabric admission is busy with an engine action")
		case <-poll.C:
		}
	}
	return nil
}

func (s *fabricService) qualifyLocal(ctx context.Context, target fabricTarget, candidates []fabricCandidateIP) ([]fabricCandidateIP, error) {
	if len(candidates) != 2 || target.NodeID != s.m.cableLocal.nodeID {
		return nil, fabricQualificationFailure("result-invalid", errors.New("exact local fabric candidates are required"))
	}
	qualified := slices.Clone(candidates)
	seenInterfaces := map[int]bool{}
	for i, candidate := range qualified {
		if candidate.NodeID != target.NodeID || seenInterfaces[candidate.InterfaceIndex] {
			return nil, fabricQualificationFailure("result-invalid", errors.New("fabric candidate binding is ambiguous"))
		}
		seenInterfaces[candidate.InterfaceIndex] = true
		var iface fabricInterface
		matched := false
		for _, selected := range target.Interfaces {
			prefix, err := fabricNativePrefix(selected)
			if err == nil && selected.Index == candidate.InterfaceIndex && selected.Name == candidate.InterfaceName && selected.MAC == candidate.MAC && selected.PhysicalPort.SwitchID == candidate.SwitchID && selected.PhysicalPort.PortName == candidate.PortName && len(selected.RDMADevices) == 1 && selected.RDMADevices[0] == candidate.RDMADevice && prefix.Addr().String() == candidate.Address {
				iface, matched = selected, true
			}
		}
		if !matched {
			return nil, fabricQualificationFailure("result-invalid", errors.New("fabric candidate does not match the owned local endpoint"))
		}
		binding, err := fabricNativeRouteQualified(ctx, iface, candidate.PeerAddress)
		if err != nil {
			return nil, fabricQualificationFailure("route-unavailable", err)
		}
		qualified[i].RDMAPort, qualified[i].GIDIndex, qualified[i].GIDType = binding.Port, binding.GIDIndex, binding.GIDType
		peer, ok := s.m.peers.lookup(candidate.PeerNodeID)
		if !ok || peer.clusterUUID != candidate.PeerPrincipal || peer.port < 1 || peer.port > 65535 {
			return nil, fabricQualificationFailure("pinned-identity-unavailable", errors.New("fabric peer identity or engine-control service is unavailable"))
		}
		if err := s.pinnedFabricIdentityAt(ctx, peer, candidate.PeerAddress); err != nil {
			return nil, fabricQualificationFailure("pinned-identity-unavailable", err)
		}
	}
	return qualified, nil
}

// pinnedFabricIdentityAt uses the existing cluster-mTLS pool and existing
// fabric control route without teaching the shared address chooser about an IP
// until every endpoint check succeeds.
func (s *fabricService) pinnedFabricIdentityAt(ctx context.Context, peer ecPeer, address string) error {
	if net.ParseIP(address) == nil {
		return errors.New("invalid fabric peer address")
	}
	s.m.mesh.Refresh()
	if !s.m.mesh.Clustered() || !s.m.mesh.HasPin(peer.clusterUUID) {
		return errors.New("fabric peer certificate pin is unavailable")
	}
	client, ok := s.m.remoteHTTP.Client(peer.clusterUUID)
	if !ok {
		return errors.New("fabric peer certificate-pinned client is unavailable")
	}
	request := fabricControlRequest{Method: "inventory", Target: fabricTarget{NodeID: peer.nodeID, Principal: peer.clusterUUID}}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(callCtx, http.MethodPost, "https://"+net.JoinHostPort(address, strconv.Itoa(peer.port))+fabricControlPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := remoteDo(client, httpRequest)
	if err != nil {
		return errors.New("certificate-pinned fabric peer read failed")
	}
	defer response.Body.Close()
	raw, err := readRemoteBody(response.Body, maxRemoteResponseBytes)
	var result fabricControlResult
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || onboardingDecode(raw, &result) != nil || result.NodeID != peer.nodeID || result.Principal != peer.clusterUUID || result.Inventory == nil || result.Inventory.NodeID != peer.nodeID || result.Inventory.Principal != peer.clusterUUID {
		return errors.New("certificate-pinned fabric peer identity did not match")
	}
	return nil
}

func (s *controlServer) handleFabricControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.exec == nil || s.exec.fabric == nil {
		http.Error(w, "fabric setup unavailable", 503)
		return
	}
	owner, ok := s.mesh.VerifyClientPin(r)
	if !ok {
		http.Error(w, "paired caller required", 403)
		return
	}
	body, e := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	var request fabricControlRequest
	if e != nil || len(body) > 64<<10 || onboardingDecode(body, &request) != nil {
		http.Error(w, "invalid fabric control request", 400)
		return
	}
	result, e := s.exec.fabric.localControl(r.Context(), owner, request)
	if e != nil {
		http.Error(w, "fabric identity, inventory or admission unavailable", 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
