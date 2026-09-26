// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	diagnosticMPIReconcileCompleted = "completed"
	diagnosticMPIReconcileRequired  = "recovery-required"
	diagnosticMPICleanupConfirmed   = "cleanup-confirmed"
)

type diagnosticMPIReconcileOperation struct {
	NodeID           string `json:"nodeId"`
	OperationID      string `json:"operationId,omitempty"`
	OwnerNodeID      string `json:"ownerNodeId,omitempty"`
	State            string `json:"state"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
	Code             string `json:"code,omitempty"`
	Message          string `json:"message"`
}

type diagnosticMPIReconcileResult struct {
	State            string                            `json:"state"`
	RecoveryRequired bool                              `json:"recoveryRequired"`
	Operations       []diagnosticMPIReconcileOperation `json:"operations"`
}

type diagnosticMPIReconcileControl struct {
	ControllerNodeID  string                      `json:"controllerNodeId"`
	Target            diagnosticMPIRecoveryMember `json:"target"`
	BuildOperationIDs []string                    `json:"buildOperationIds"`
}

func (d *diagnosticService) recoveryRequired() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.recoveryFailed
}

func diagnosticMPIReconcileFailure(node, operation, owner, code string) diagnosticMPIReconcileOperation {
	message := "PAIR could not confirm complete cleanup for this retained diagnostic operation."
	if operation == "" {
		message = "PAIR could not confirm the complete retained diagnostic inventory on this node."
	}
	if code == "participant-unavailable" {
		message = "The paired participant did not return a confirmed diagnostic cleanup result."
	}
	return diagnosticMPIReconcileOperation{NodeID: node, OperationID: operation, OwnerNodeID: owner, State: diagnosticMPIReconcileRequired, Code: code, Message: message}
}

func diagnosticMPIReconcileSuccess(node, operation, owner string) diagnosticMPIReconcileOperation {
	return diagnosticMPIReconcileOperation{NodeID: node, OperationID: operation, OwnerNodeID: owner, State: diagnosticMPICleanupConfirmed, CleanupConfirmed: true, Message: "Owned diagnostic resources are cleaned up."}
}

func finishDiagnosticMPIReconcile(operations []diagnosticMPIReconcileOperation) diagnosticMPIReconcileResult {
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].NodeID != operations[j].NodeID {
			return operations[i].NodeID < operations[j].NodeID
		}
		return operations[i].OperationID < operations[j].OperationID
	})
	result := diagnosticMPIReconcileResult{State: diagnosticMPIReconcileCompleted, Operations: operations}
	for _, operation := range operations {
		if !operation.CleanupConfirmed {
			result.State, result.RecoveryRequired = diagnosticMPIReconcileRequired, true
			break
		}
	}
	return result
}

func validDiagnosticMPIReconcileResult(result diagnosticMPIReconcileResult, node string) bool {
	if result.State != diagnosticMPIReconcileCompleted && result.State != diagnosticMPIReconcileRequired || result.RecoveryRequired != (result.State == diagnosticMPIReconcileRequired) || result.Operations == nil {
		return false
	}
	seen := map[string]bool{}
	recoveryRequired := false
	for _, operation := range result.Operations {
		if operation.NodeID != node || !diagnosticMPIReconcileMessage(operation) || operation.CleanupConfirmed != (operation.State == diagnosticMPICleanupConfirmed) {
			return false
		}
		if operation.OperationID != "" && !onboardingID.MatchString(operation.OperationID) || operation.OwnerNodeID != "" && !diagnosticToken.MatchString(operation.OwnerNodeID) {
			return false
		}
		if operation.CleanupConfirmed {
			if operation.Code != "" || operation.OperationID == "" || operation.OwnerNodeID == "" {
				return false
			}
		} else if operation.State != diagnosticMPIReconcileRequired || operation.Code != "cleanup-unconfirmed" && operation.Code != "binding-unavailable" && operation.Code != "inventory-unconfirmed" && operation.Code != "participant-unavailable" {
			return false
		}
		recoveryRequired = recoveryRequired || !operation.CleanupConfirmed
		if operation.OperationID != "" {
			key := operation.NodeID + "\x00" + operation.OperationID
			if seen[key] {
				return false
			}
			seen[key] = true
		}
	}
	return recoveryRequired == result.RecoveryRequired
}

func diagnosticMPIReconcileMessage(operation diagnosticMPIReconcileOperation) bool {
	if operation.CleanupConfirmed {
		return operation.Message == "Owned diagnostic resources are cleaned up."
	}
	if operation.Code == "participant-unavailable" {
		return operation.Message == "The paired participant did not return a confirmed diagnostic cleanup result."
	}
	if operation.OperationID == "" {
		return operation.Message == "PAIR could not confirm the complete retained diagnostic inventory on this node."
	}
	return operation.Message == "PAIR could not confirm complete cleanup for this retained diagnostic operation."
}

func validDiagnosticMPIReconcileControl(control diagnosticMPIReconcileControl, caller, self string, allowEmptyBuilds bool) bool {
	if !diagnosticToken.MatchString(caller) || control.ControllerNodeID != caller || control.Target.NodeID != self || control.Target.Principal != self || !diagnosticDigest.MatchString(control.Target.ClusterPinSHA256) || len(control.BuildOperationIDs) > diagnosticManagedLimit || !allowEmptyBuilds && len(control.BuildOperationIDs) == 0 {
		return false
	}
	for i, id := range control.BuildOperationIDs {
		if !onboardingID.MatchString(id) || i > 0 && control.BuildOperationIDs[i-1] >= id {
			return false
		}
	}
	return true
}

// reconcileMPI coordinates one bounded cleanup pass over every participant in
// the retained managed-runtime registry. The older recover RPC remains a
// read-only selector lookup.
func (d *diagnosticService) reconcileMPI(ctx context.Context, request diagnosticMPIRequest) (diagnosticMPIReconcileResult, error) {
	if request.BuildOperationID != "" || request.OwnerNodeID != "" || request.ReviewID != "" || request.OperationID != "" || request.GroupID != "" || request.Network != "" || request.DedicatedTestWindow || request.CloseUnstartedReview || request.MemberNodeIDs != nil {
		return diagnosticMPIReconcileResult{}, errors.New("MPI reconciliation does not accept an operation selector")
	}
	if d.m.mesh == nil {
		return diagnosticMPIReconcileResult{}, errors.New("paired identity is unavailable for MPI reconciliation")
	}
	d.m.mesh.Refresh()
	self := d.m.mesh.NodeUUID()
	selfPin, selfPinned := d.m.mesh.PinSHA256(self)
	if !selfPinned || !diagnosticToken.MatchString(self) {
		return diagnosticMPIReconcileResult{}, errors.New("current paired controller identity is required")
	}
	type target struct {
		member diagnosticMPIRecoveryMember
		builds map[string]bool
	}
	targets := map[string]*target{self: {member: diagnosticMPIRecoveryMember{NodeID: self, Principal: self, ClusterPinSHA256: selfPin}, builds: map[string]bool{}}}
	inventory := d.managedRuntimeInventory()
	if err := d.diagnosticMPIReconcileRegistry(inventory.Records); err != nil {
		return diagnosticMPIReconcileResult{}, err
	}
	d.mu.Lock()
	bindings := make(map[string]diagnosticParticipantBinding, len(inventory.Records))
	for _, record := range inventory.Records {
		run := d.runtimeRuns[record.OperationID]
		if managedRunShape(run) != nil {
			d.mu.Unlock()
			return diagnosticMPIReconcileResult{}, errors.New("retained managed runtime binding is incomplete")
		}
		bindings[record.OperationID] = run.Binding.diagnosticParticipantBinding
	}
	d.mu.Unlock()
	for _, record := range inventory.Records {
		if !record.Adopted || !onboardingID.MatchString(record.OperationID) {
			return diagnosticMPIReconcileResult{}, errors.New("retained managed runtime registry is incomplete")
		}
		if err := d.participantPinsCurrent(bindings[record.OperationID]); err != nil {
			return diagnosticMPIReconcileResult{}, err
		}
		for _, registered := range record.Targets {
			pin, ok := d.m.mesh.PinSHA256(registered.Principal)
			if !ok || registered.NodeID != registered.Principal || pin != registered.ClusterPinSHA256 {
				return diagnosticMPIReconcileResult{}, errors.New("registered diagnostic participant trust changed")
			}
			current := targets[registered.NodeID]
			if current == nil {
				current = &target{member: diagnosticMPIRecoveryMember{NodeID: registered.NodeID, Principal: registered.Principal, ClusterPinSHA256: pin}, builds: map[string]bool{}}
				targets[registered.NodeID] = current
			}
			current.builds[record.OperationID] = true
		}
	}
	ordered := make([]string, 0, len(targets))
	for node := range targets {
		ordered = append(ordered, node)
	}
	sort.Strings(ordered)
	if len(ordered) > 3 {
		return diagnosticMPIReconcileResult{}, errors.New("retained diagnostic participant inventory exceeds the supported bound")
	}
	controls := make(map[string]diagnosticMPIReconcileControl, len(ordered))
	for _, node := range ordered {
		selected := targets[node]
		builds := make([]string, 0, len(selected.builds))
		for id := range selected.builds {
			builds = append(builds, id)
		}
		sort.Strings(builds)
		controls[node] = diagnosticMPIReconcileControl{ControllerNodeID: self, Target: selected.member, BuildOperationIDs: builds}
	}
	return coordinateDiagnosticMPIReconcile(ctx, ordered, func(ctx context.Context, node string) diagnosticMPIReconcileResult {
		control := controls[node]
		if node == self {
			result, err := d.reconcileRetainedMPI(ctx, self, control)
			if err == nil {
				return result
			}
		} else if client, err := d.mpiPeer(ctx, node); err == nil {
			raw, callErr := client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: "mpi-reconcile", MPIReconcile: &control})
			var result diagnosticMPIReconcileResult
			if callErr == nil && strictDiagnosticJSON(raw, &result) == nil && validDiagnosticMPIReconcileResult(result, node) {
				return result
			}
		}
		return finishDiagnosticMPIReconcile([]diagnosticMPIReconcileOperation{diagnosticMPIReconcileFailure(node, "", "", "participant-unavailable")})
	}), nil
}

func (d *diagnosticService) diagnosticMPIReconcileRegistry(records []diagnosticManagedRecord) error {
	expected := make(map[string]bool, len(records))
	for _, record := range records {
		expected[record.OperationID] = true
	}
	entries, err := mpiRecoveryEntries(filepath.Dir(d.managedPath("unused")), diagnosticManagedLimit)
	if err != nil {
		return errors.New("retained managed runtime registry is unavailable")
	}
	for _, entry := range entries {
		name := entry.Name()
		id := name
		if len(name) > 5 && name[len(name)-5:] == ".json" {
			id = name[:len(name)-5]
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || name != id+".json" || !onboardingID.MatchString(id) || !expected[id] {
			return errors.New("retained managed runtime registry is incomplete")
		}
		delete(expected, id)
	}
	if len(expected) != 0 {
		return errors.New("retained managed runtime registry is incomplete")
	}
	return nil
}

func coordinateDiagnosticMPIReconcile(ctx context.Context, nodes []string, call func(context.Context, string) diagnosticMPIReconcileResult) diagnosticMPIReconcileResult {
	type answer struct {
		node   string
		result diagnosticMPIReconcileResult
	}
	answers := make(chan answer, len(nodes))
	for _, node := range nodes {
		go func(node string) { answers <- answer{node: node, result: call(ctx, node)} }(node)
	}
	operations := []diagnosticMPIReconcileOperation{}
	pending := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		pending[node] = true
	}
	for range nodes {
		select {
		case answer := <-answers:
			if !pending[answer.node] {
				continue
			}
			delete(pending, answer.node)
			if !validDiagnosticMPIReconcileResult(answer.result, answer.node) {
				operations = append(operations, diagnosticMPIReconcileFailure(answer.node, "", "", "participant-unavailable"))
				continue
			}
			operations = append(operations, answer.result.Operations...)
		case <-ctx.Done():
			for node := range pending {
				operations = append(operations, diagnosticMPIReconcileFailure(node, "", "", "participant-unavailable"))
			}
			return finishDiagnosticMPIReconcile(operations)
		}
	}
	for node := range pending {
		operations = append(operations, diagnosticMPIReconcileFailure(node, "", "", "participant-unavailable"))
	}
	return finishDiagnosticMPIReconcile(operations)
}

func (d *diagnosticService) reconcileRetainedMPI(ctx context.Context, caller string, control diagnosticMPIReconcileControl) (diagnosticMPIReconcileResult, error) {
	root, err := diagnosticNativeBootstrapRoot()
	if err != nil {
		return diagnosticMPIReconcileResult{}, errors.New("native MPI recovery inventory is unavailable")
	}
	return d.reconcileRetainedMPIAt(ctx, caller, control, root)
}

func (d *diagnosticService) reconcileRetainedMPIAt(ctx context.Context, caller string, control diagnosticMPIReconcileControl, nativeRoot string) (diagnosticMPIReconcileResult, error) {
	self := d.m.mesh.NodeUUID()
	pin, pinned := d.m.mesh.PinSHA256(self)
	_, callerPinned := d.m.mesh.PinSHA256(caller)
	allowUnregisteredLocal := caller == self && len(control.BuildOperationIDs) == 0
	if !pinned || !callerPinned || pin != control.Target.ClusterPinSHA256 || !d.m.mesh.Clustered() || !validDiagnosticMPIReconcileControl(control, caller, self, allowUnregisteredLocal) {
		return diagnosticMPIReconcileResult{}, errors.New("current pinned diagnostic participant binding is required")
	}
	allowed := map[string]bool{}
	for _, id := range control.BuildOperationIDs {
		allowed[id] = true
	}
	d.m.exec.diagnosticMu.Lock()
	defer d.m.exec.diagnosticMu.Unlock()
	d.mu.Lock()
	busy := d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.reservation != nil || len(d.cancels) != 0
	d.mu.Unlock()
	if busy || ctx.Err() != nil {
		return finishDiagnosticMPIReconcile([]diagnosticMPIReconcileOperation{diagnosticMPIReconcileFailure(self, "", "", "inventory-unconfirmed")}), nil
	}
	operations := []diagnosticMPIReconcileOperation{}
	allClean := true
	inventoryFailure := false
	if err := d.strictDiagnosticMPINativeRoots(ctx, nativeRoot); err != nil {
		allClean = false
		inventoryFailure = true
		operations = append(operations, diagnosticMPIReconcileFailure(self, "", "", "inventory-unconfirmed"))
	}
	entries, err := mpiRecoveryEntries(filepath.Join(d.m.exec.baseDir, "diagnostic-runs"), 128)
	if err != nil {
		allClean = false
		operations = append(operations, diagnosticMPIReconcileFailure(self, "", "", "inventory-unconfirmed"))
	}
	for _, entry := range entries {
		id := entry.Name()
		if entry.IsDir() == false || entry.Type()&os.ModeSymlink != 0 || !onboardingID.MatchString(id) {
			allClean = false
			operations = append(operations, diagnosticMPIReconcileFailure(self, "", "", "inventory-unconfirmed"))
			continue
		}
		var lease diagnosticLeaseRecord
		if readDiagnosticJSON(filepath.Join(d.runDir(id), "lease.json"), &lease) != nil || lease.Request.OperationID != id {
			allClean = false
			operations = append(operations, diagnosticMPIReconcileFailure(self, id, "", "binding-unavailable"))
			continue
		}
		owner := ""
		if lease.Request.BootstrapPlanDigest == "" {
			allClean = false
			operations = append(operations, diagnosticMPIReconcileFailure(self, id, owner, "binding-unavailable"))
			continue
		}
		profile, _, loadErr := d.loadBootstrapOperation(lease.Request)
		if loadErr == nil {
			owner = profile.OwnerNodeID
			for _, member := range profile.Members {
				if !allowed[member.Runtime.BuildOperationID] && !allowUnregisteredLocal {
					loadErr = errors.New("retained MPI build is not in the coordinator registry")
					break
				}
			}
		}
		if loadErr != nil {
			allClean = false
			operations = append(operations, diagnosticMPIReconcileFailure(self, id, owner, "binding-unavailable"))
			continue
		}
		operationCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cleanup := d.cancelParticipant
		if d.reconcileCancel != nil {
			cleanup = d.reconcileCancel
		}
		clean, cleanupErr := cleanup(operationCtx, lease.Request)
		cancel()
		if cleanupErr != nil || !clean {
			allClean = false
			operations = append(operations, diagnosticMPIReconcileFailure(self, id, owner, "cleanup-unconfirmed"))
		} else {
			operations = append(operations, diagnosticMPIReconcileSuccess(self, id, owner))
		}
	}
	if err := d.strictDiagnosticMPINativeRoots(ctx, nativeRoot); err != nil {
		allClean = false
		if !inventoryFailure {
			operations = append(operations, diagnosticMPIReconcileFailure(self, "", "", "inventory-unconfirmed"))
		}
	}
	if !allClean && len(operations) == 0 {
		operations = append(operations, diagnosticMPIReconcileFailure(self, "", "", "inventory-unconfirmed"))
	}
	d.mu.Lock()
	if allClean && d.reservation == nil && len(d.cancels) == 0 {
		d.recoveryFailed = false
	} else {
		d.recoveryFailed = true
	}
	d.mu.Unlock()
	return finishDiagnosticMPIReconcile(operations), nil
}

// The restart reader historically ignored unknown names. An explicit release
// cannot: every native root must have one exact managed lease before the
// process-wide recovery fence can be lowered.
func (d *diagnosticService) strictDiagnosticMPINativeRoots(ctx context.Context, nativeRoot string) error {
	entries, err := mpiRecoveryEntries(nativeRoot, 128)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id := entry.Name()
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !onboardingID.MatchString(id) {
			return errors.New("native MPI recovery inventory contains foreign ownership")
		}
		var lease diagnosticLeaseRecord
		if readDiagnosticJSON(filepath.Join(d.runDir(id), "lease.json"), &lease) != nil || lease.Request.OperationID != id || lease.Request.BootstrapPlanDigest == "" {
			return errors.New("native MPI recovery inventory lost its managed lease")
		}
		if _, err := d.participantCancellationBinding(lease.Request, ""); err != nil {
			return err
		}
	}
	return nil
}
