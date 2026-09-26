// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type diagnosticMPIRecoveryReference struct {
	ReviewID         string   `json:"reviewId"`
	BuildOperationID string   `json:"buildOperationId"`
	OperationID      string   `json:"operationId"`
	GroupID          string   `json:"groupId"`
	OwnerNodeID      string   `json:"ownerNodeId"`
	MemberNodeIDs    []string `json:"memberNodeIds"`
	RecipeID         string   `json:"recipeId,omitempty"`
}
type diagnosticMPIRecovery struct {
	Reference        *diagnosticMPIRecoveryReference `json:"reference"`
	Operation        *diagnosticOperation            `json:"operation"`
	RecoveryRequired bool                            `json:"recoveryRequired"`
}
type diagnosticMPIRecoveryMember struct {
	NodeID           string `json:"nodeId"`
	Principal        string `json:"principal"`
	ClusterPinSHA256 string `json:"clusterPinSha256"`
}
type diagnosticMPIRecoveryControl struct {
	RegisteredMemberCount int                           `json:"registeredMemberCount,omitempty"`
	BuildOperationID      string                        `json:"buildOperationId"`
	Members               []diagnosticMPIRecoveryMember `json:"members"`
}

func (d *diagnosticService) recoverMPI(ctx context.Context, request diagnosticMPIRequest) (diagnosticMPIRecovery, error) {
	var result diagnosticMPIRecovery
	if !onboardingID.MatchString(request.BuildOperationID) || request.OwnerNodeID != "" || request.ReviewID != "" || request.OperationID != "" || request.GroupID != "" || request.Network != "" || request.DedicatedTestWindow || request.CloseUnstartedReview {
		return result, errors.New("MPI recovery requires only the registered build identity")
	}
	d.mu.Lock()
	run := d.runtimeRuns[request.BuildOperationID]
	if run == nil {
		d.mu.Unlock()
		return result, errors.New("registered build is unavailable for MPI recovery")
	}
	record, err := d.readManaged(run, d.managedIO())
	binding := run.Binding
	d.mu.Unlock()
	if err != nil || record == nil {
		return result, errors.New("verified registered roster is unavailable for MPI recovery")
	}
	registeredCount := len(record.Targets)
	selected, participantBinding, err := selectDiagnosticMPIRecord(*record, binding.diagnosticParticipantBinding, request.MemberNodeIDs)
	if err != nil {
		return result, err
	}
	record = &selected
	binding.diagnosticParticipantBinding = participantBinding
	if err = d.participantPinsCurrent(binding.diagnosticParticipantBinding); err != nil {
		return result, err
	}
	control := diagnosticMPIRecoveryControl{BuildOperationID: record.OperationID, RegisteredMemberCount: registeredCount}
	for _, target := range record.Targets {
		control.Members = append(control.Members, diagnosticMPIRecoveryMember{NodeID: target.NodeID, Principal: target.Principal, ClusterPinSHA256: target.ClusterPinSHA256})
	}
	owner := record.Targets[0].NodeID
	if owner == d.m.mesh.NodeUUID() {
		result, err = d.recoverMPIAsCoordinator(ctx, d.m.mesh.NodeUUID(), control)
	} else {
		client, clientErr := d.mpiPeer(ctx, owner)
		if clientErr != nil {
			return result, clientErr
		}
		raw, callErr := client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: "mpi-recover", MPIRecovery: &control})
		if callErr != nil {
			return result, callErr
		}
		result, err = parseMPIRecovery(raw, control)
	}
	if err != nil {
		return diagnosticMPIRecovery{}, err
	}
	if err = validateMPIRecovery(result, control); err != nil {
		return diagnosticMPIRecovery{}, err
	}
	if err = d.participantPinsCurrent(binding.diagnosticParticipantBinding); err != nil {
		return diagnosticMPIRecovery{}, err
	}
	return result, nil
}

func parseMPIRecovery(raw []byte, control diagnosticMPIRecoveryControl) (diagnosticMPIRecovery, error) {
	var result diagnosticMPIRecovery
	var fields map[string]json.RawMessage
	if strictDiagnosticJSON(raw, &result) != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 3 || fields["reference"] == nil || fields["operation"] == nil || fields["recoveryRequired"] == nil {
		return result, errors.New("MPI recovery response is incomplete")
	}
	flag := bytes.TrimSpace(fields["recoveryRequired"])
	if !bytes.Equal(flag, []byte("true")) && !bytes.Equal(flag, []byte("false")) {
		return result, errors.New("MPI recovery certainty is not explicit")
	}
	return result, validateMPIRecovery(result, control)
}

func validateMPIRecovery(result diagnosticMPIRecovery, control diagnosticMPIRecoveryControl) error {
	if result.Reference == nil {
		if result.Operation != nil {
			return errors.New("MPI recovery operation has no original reference")
		}
		return nil
	}
	ref := result.Reference
	if _, _, err := diagnosticMPIRecipeArgs(diagnosticMPIStoredRecipe(ref.RecipeID)); err != nil {
		return err
	}
	if !validDiagnosticParticipantCount(len(control.Members)) || diagnosticMPIRecipeRanks(diagnosticMPIStoredRecipe(ref.RecipeID)) != len(control.Members) || !onboardingID.MatchString(ref.ReviewID) || !onboardingID.MatchString(ref.OperationID) || ref.BuildOperationID != control.BuildOperationID || ref.OwnerNodeID != control.Members[0].NodeID || ref.GroupID != "pair-smoke-"+ref.OperationID || !slices.Equal(ref.MemberNodeIDs, diagnosticMPIRecoveryMemberIDs(control.Members)) {
		return errors.New("MPI recovery reference changed its registered owner or selected roster")
	}
	if op := result.Operation; op != nil && (diagnosticMPIStoredRecipe(op.RecipeID) != diagnosticMPIStoredRecipe(ref.RecipeID) || op.OperationID != ref.OperationID || op.GroupID != ref.GroupID || op.OwnerNodeID != ref.OwnerNodeID || !slices.Equal(op.MemberNodeIDs, ref.MemberNodeIDs)) {
		return errors.New("MPI recovery operation changed its original reference")
	}
	return nil
}

func mpiRecoveryEntries(directory string, maximum int) ([]os.DirEntry, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("MPI recovery namespace is not regular storage")
	}
	f, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maximum + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > maximum {
		return nil, errors.New("MPI recovery namespace exceeds its retained bound")
	}
	return entries, nil
}

// Read-only discovery of existing selectors. It neither mints a review nor
// starts, retries, closes, or cancels any operation.
func (d *diagnosticService) recoverMPIAsCoordinator(ctx context.Context, caller string, control diagnosticMPIRecoveryControl) (diagnosticMPIRecovery, error) {
	var result diagnosticMPIRecovery
	d.m.mesh.Refresh()
	pin, pinned := d.m.mesh.PinSHA256(caller)
	registeredCount := control.RegisteredMemberCount
	if registeredCount == 0 { // Original two-node callers did not send this field.
		registeredCount = 2
	}
	if !pinned || !d.m.mesh.Clustered() || !onboardingID.MatchString(control.BuildOperationID) || !validDiagnosticParticipantCount(registeredCount) || !validDiagnosticParticipantCount(len(control.Members)) || len(control.Members) > registeredCount || control.Members[0].NodeID != d.m.mesh.NodeUUID() {
		return result, errors.New("current registered MPI coordinator binding is required")
	}
	for i, member := range control.Members {
		current, ok := d.m.mesh.PinSHA256(member.Principal)
		if member.NodeID != member.Principal || !diagnosticToken.MatchString(member.NodeID) || !ok || current != member.ClusterPinSHA256 || (i > 0 && control.Members[i-1].NodeID >= member.NodeID) {
			return result, errors.New("registered MPI participant identity changed")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entries, err := mpiRecoveryEntries(filepath.Dir(d.mpiReviewMarkerPath("unused")), diagnosticMPIReviewMarkerLimit)
	if err != nil {
		return result, err
	}
	knownOperations := map[string]bool{}
	var chosen *diagnosticMPIReviewMarker
	var chosenOperation *diagnosticOperation
	chosenPriority := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Name() != id+".json" || !onboardingID.MatchString(id) {
			return result, errors.New("MPI review inventory contains unconfirmed ownership")
		}
		marker, err := d.readMPIReviewMarker(id, mpiReviewStorage())
		if err != nil {
			return result, err
		}
		if knownOperations[marker.OperationID] {
			return result, errors.New("multiple MPI reviews claim the same operation")
		}
		op, err := d.mpiReviewOperationLocked(marker)
		if err != nil {
			return result, err
		}
		// A marker only explains retained ownership after it agrees with its
		// own snapshot. Check this before filtering out another build, including
		// the gap after snapshot publication but before the public result exists.
		p, snapshotPlan, request, snapshotErr := d.loadCoordinatorBootstrap(marker.OperationID)
		hasSnapshot := snapshotErr == nil
		if snapshotErr != nil && !errors.Is(snapshotErr, os.ErrNotExist) {
			return result, snapshotErr
		}
		if op != nil && !hasSnapshot {
			return result, errors.New("retained MPI operation profile is unconfirmed")
		}
		if hasSnapshot {
			var header diagnosticMPIHeader
			if json.Unmarshal(snapshotPlan, &header) != nil || header.RecipeID != diagnosticMPIStoredRecipe(marker.RecipeID) {
				return result, errors.New("retained MPI recipe contradicts its review marker")
			}
			if marker.State != "consumed" {
				return result, errors.New("retained MPI snapshot lacks a consumed review")
			}
			if request.GroupID != marker.GroupID || p.OwnerNodeID != marker.OwnerNodeID || request.ProfileDigest != marker.ProfileDigest || request.BootstrapPlanDigest != marker.BootstrapPlanDigest || len(p.Members) != diagnosticMPIRecipeRanks(header.RecipeID) || marker.MemberSetDigest != "" && diagnosticMPIMemberDigest(diagnosticMPIProfileMemberIDs(p)) != marker.MemberSetDigest {
				return result, errors.New("retained MPI operation profile is unconfirmed")
			}
			for _, member := range p.Members {
				if member.Runtime.BuildOperationID != marker.BuildOperationID {
					return result, errors.New("MPI review build contradicts its retained snapshot")
				}
			}
			if op != nil && !slices.Equal(op.MemberNodeIDs, diagnosticMPIProfileMemberIDs(p)) {
				return result, errors.New("retained MPI operation members contradict its snapshot")
			}
		}
		knownOperations[marker.OperationID] = true
		if marker.BuildOperationID != control.BuildOperationID {
			continue
		}
		if err := d.mpiReviewCaller(marker, caller, pin, pinned); err != nil {
			return result, err
		}
		memberDigest := marker.MemberSetDigest
		if memberDigest == "" && hasSnapshot {
			memberDigest = diagnosticMPIMemberDigest(diagnosticMPIProfileMemberIDs(p))
		}
		if memberDigest == "" {
			if registeredCount != 2 || len(control.Members) != 2 {
				return result, errors.New("legacy unstarted MPI review cannot establish a three-node build subset")
			}
		} else if memberDigest != diagnosticMPIMemberDigest(diagnosticMPIRecoveryMemberIDs(control.Members)) {
			continue // The original ownership is valid, but belongs to another subset.
		}
		if hasSnapshot {
			if len(p.Members) != len(control.Members) {
				return result, errors.New("retained MPI participant count differs from the selected roster")
			}
			for i, member := range p.Members {
				if member.NodeID != control.Members[i].NodeID || member.Principal != control.Members[i].Principal || member.ClusterPinSHA256 != control.Members[i].ClusterPinSHA256 || member.Runtime.BuildOperationID != control.BuildOperationID {
					return result, errors.New("retained MPI operation differs from the registered build")
				}
			}
		}
		if marker.State == "closed" {
			if op != nil {
				return result, errors.New("closed review contradicts a retained MPI operation")
			}
			continue
		}
		priority := 1
		if op == nil {
			priority = 2
		}
		if marker.State == "consumed" && (op == nil || !op.CleanupConfirmed || op.State == "preparing" || op.State == "running" || op.State == "cancelling") {
			priority = 3
		}
		if chosen != nil && priority == 3 && chosenPriority == 3 {
			return result, errors.New("multiple unresolved MPI operations require reconciliation")
		}
		if chosen == nil || priority > chosenPriority || (priority == chosenPriority && (marker.ExpiresAt > chosen.ExpiresAt || marker.ExpiresAt == chosen.ExpiresAt && marker.ReviewID > chosen.ReviewID)) {
			copy := marker
			chosen = &copy
			chosenOperation = op
			chosenPriority = priority
		}
	}
	// Losing the marker directory must not hide an existing coordinator record.
	operations, err := mpiRecoveryEntries(filepath.Join(d.m.exec.baseDir, "diagnostic-operations"), 256)
	if err != nil {
		return result, err
	}
	for _, entry := range operations {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if strings.HasSuffix(entry.Name(), ".json") {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if !onboardingID.MatchString(id) {
				return result, errors.New("MPI operation receipt inventory is malformed")
			}
			var op diagnosticOperation
			if readDiagnosticJSON(d.operationPath(id), &op) != nil || op.OperationID != id {
				return result, errors.New("retained operation receipt is unreadable")
			}
			if op.GroupID == "pair-smoke-"+id && !knownOperations[id] {
				return result, errors.New("retained MPI result lost its original review reference")
			}
			continue
		}
		if strings.HasSuffix(entry.Name(), ".json.profile") {
			id := strings.TrimSuffix(entry.Name(), ".json.profile")
			if !onboardingID.MatchString(id) {
				return result, errors.New("MPI profile receipt inventory is malformed")
			}
			if !knownOperations[id] {
				var op diagnosticOperation
				if readDiagnosticJSON(d.operationPath(id), &op) != nil || op.OperationID != id || op.GroupID == "pair-smoke-"+id {
					return result, errors.New("retained MPI profile reference is orphaned")
				}
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json.bootstrap") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json.bootstrap")
		if entry.IsDir() || !onboardingID.MatchString(id) {
			return result, errors.New("MPI coordinator inventory is malformed")
		}
		p, _, _, err := d.loadCoordinatorBootstrap(id)
		if err != nil {
			return result, err
		}
		for _, member := range p.Members {
			if member.Runtime.BuildOperationID == control.BuildOperationID && !knownOperations[id] {
				return result, errors.New("retained MPI operation has lost its original review reference")
			}
		}
	}
	for id, op := range d.operations {
		if op.GroupID == "pair-smoke-"+id && !knownOperations[id] {
			return result, errors.New("in-memory MPI operation lost its durable review reference")
		}
	}
	for id := range d.cancels {
		if !knownOperations[id] {
			op, ok := d.operations[id]
			if !ok || op.GroupID == "pair-smoke-"+id {
				return result, errors.New("admitted MPI operation has unconfirmed recovery identity")
			}
		}
	}
	if d.reservation != nil && d.reservation.Request.BootstrapPlanDigest != "" && !knownOperations[d.reservation.Request.OperationID] {
		return result, errors.New("MPI reservation lost its durable review reference")
	}
	runs, err := mpiRecoveryEntries(filepath.Join(d.m.exec.baseDir, "diagnostic-runs"), 128)
	if err != nil {
		return result, err
	}
	for _, entry := range runs {
		id := entry.Name()
		if !onboardingID.MatchString(id) || knownOperations[id] {
			continue
		}
		var lease diagnosticLeaseRecord
		leaseErr := readDiagnosticJSON(filepath.Join(d.runDir(id), "lease.json"), &lease)
		if leaseErr == nil && lease.Request.BootstrapPlanDigest != "" {
			return result, errors.New("MPI rank lease lost its original review reference")
		}
		for _, name := range []string{"profile.json", "bootstrap-plan.json"} {
			if _, err := os.Lstat(filepath.Join(d.runDir(id), name)); !errors.Is(err, os.ErrNotExist) {
				return result, errors.New("MPI rank ownership has no confirmed recovery reference")
			}
		}
	}
	d.m.mesh.Refresh()
	currentPin, currentCaller := d.m.mesh.PinSHA256(caller)
	if !d.m.mesh.Clustered() || !currentCaller || currentPin != pin {
		return result, errors.New("MPI recovery controller changed during lookup")
	}
	for _, member := range control.Members {
		current, ok := d.m.mesh.PinSHA256(member.Principal)
		if !ok || current != member.ClusterPinSHA256 {
			return result, errors.New("MPI recovery participant changed during lookup")
		}
	}
	if chosen == nil {
		// Startup may retain unknown native ownership after a lost lease.
		// Only an unfenced empty inventory proves there is nothing to recover.
		result.RecoveryRequired = d.recoveryFailed
		return result, nil
	}
	result.Reference = &diagnosticMPIRecoveryReference{ReviewID: chosen.ReviewID, BuildOperationID: chosen.BuildOperationID, OperationID: chosen.OperationID, GroupID: chosen.GroupID, OwnerNodeID: chosen.OwnerNodeID, MemberNodeIDs: diagnosticMPIRecoveryMemberIDs(control.Members), RecipeID: chosen.RecipeID}
	result.Operation = chosenOperation
	return result, validateMPIRecovery(result, control)
}
