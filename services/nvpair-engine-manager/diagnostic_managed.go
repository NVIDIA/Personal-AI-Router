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
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"
)

const diagnosticManagedOwner = "pair-managed-nccl-registry-v1"
const diagnosticManagedDiskOwner = "pair-managed-nccl-record-v1"
const diagnosticManagedLimit = 64

type diagnosticManagedTarget struct {
	NodeID             string          `json:"nodeId"`
	Principal          string          `json:"principal"`
	Local              bool            `json:"local"`
	Address            string          `json:"address"`
	ClusterPinSHA256   string          `json:"clusterPinSha256"`
	CandidateID        string          `json:"candidateId,omitempty"`
	SSHHostKeySHA256   string          `json:"sshHostKeySha256,omitempty"`
	PlanDigest         string          `json:"planDigest"`
	Attempt            int             `json:"attempt"`
	ArtifactObservedAt string          `json:"artifactObservedAt"`
	Registration       json.RawMessage `json:"registration"`
}

// This is adoption authority, never evidence that the GPU or MPI loader ran.
type diagnosticManagedRecord struct {
	SchemaVersion    int                       `json:"schemaVersion"`
	Owner            string                    `json:"owner"`
	OperationID      string                    `json:"operationId"`
	ReviewID         string                    `json:"reviewId"`
	GroupID          string                    `json:"groupId"`
	ApprovedRevision uint64                    `json:"approvedRevision"`
	AdoptedAt        int64                     `json:"adoptedAt"`
	Adopted          bool                      `json:"adopted"`
	RuntimeValidated bool                      `json:"runtimeValidated"`
	RunAvailable     bool                      `json:"runAvailable"`
	Targets          []diagnosticManagedTarget `json:"targets"`
}

type diagnosticManagedDisk struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Owner         string                   `json:"owner"`
	Record        diagnosticManagedRecord  `json:"record"`
	Binding       diagnosticRuntimeBinding `json:"binding"`
}

type diagnosticManagedAction struct {
	OperationID      string `json:"operationId"`
	ExpectedRevision uint64 `json:"expectedRevision"`
}
type diagnosticManagedAdoption struct {
	Operation diagnosticRuntimeOperation `json:"operation"`
	Record    *diagnosticManagedRecord   `json:"record"`
}
type diagnosticManagedLookup struct {
	Record *diagnosticManagedRecord `json:"record"`
}
type diagnosticManagedInventory struct {
	Records          []diagnosticManagedRecord `json:"records"`
	RecoveryRequired bool                      `json:"recoveryRequired"`
}

// Tests replace only local persistence, never native proof or publication policy.
type diagnosticManagedIO struct {
	read    func(string) ([]byte, error)
	write   func(string, any) error
	sync    func(string) error
	saveRun func(*diagnosticRuntimeRun) error
}

func (d *diagnosticService) managedIO() diagnosticManagedIO {
	return diagnosticManagedIO{read: readManagedFile, write: writeManagedFile, sync: syncManagedFile, saveRun: d.saveRuntime}
}
func (d *diagnosticService) managedPath(id string) string {
	return filepath.Join(d.m.exec.baseDir, "diagnostic-managed-runtimes", id+".json")
}
func readManagedFile(path string) ([]byte, error) {
	if parent, err := os.Lstat(filepath.Dir(path)); err != nil {
		return nil, err
	} else if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("managed runtime registry directory is not regular storage")
	}
	if _, err := os.Lstat(path); err != nil {
		return nil, err
	}
	return readOnboardingFile(path, 512<<10)
}
func writeManagedFile(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed runtime registry directory is not owned regular storage")
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(diagnosticManagedLimit + 1)
	closeDirectoryErr := directory.Close()
	if (readErr != nil && readErr != io.EOF) || closeDirectoryErr != nil || len(entries) >= diagnosticManagedLimit {
		return errors.New("managed runtime registry requires recovery or reached its record limit")
	}
	// writeJSONAtomic uses this fixed temporary sibling; never follow an existing
	// temporary file or replace an unreviewed registry entry.
	for _, entry := range []string{path, path + ".tmp"} {
		if _, err := os.Lstat(entry); !errors.Is(err, os.ErrNotExist) {
			return errors.New("managed runtime registry entry already exists or cannot be inspected")
		}
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(raw) > 512<<10 {
		return errors.New("managed runtime registry exceeds its size bound")
	}
	if err := writeJSONAtomic(path, value); err != nil {
		return err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	return syncManagedFile(path)
}

func syncManagedFile(path string) error {
	if _, err := readManagedFile(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Windows does not offer directory fsync through os.File. The Linux owner
	// syncs the directory after the standard atomic rename before adoption.
	if runtime.GOOS != "windows" {
		parent, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = parent.Sync()
		closeErr = parent.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func sameManagedJSON(left, right json.RawMessage) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}

func managedRunShape(run *diagnosticRuntimeRun) error {
	if run == nil || !onboardingID.MatchString(run.Public.OperationID) || run.Public.OperationID != run.Binding.Review.OperationID || run.Public.ReviewID != run.Binding.Review.ReviewID || run.Public.GroupID != run.Binding.Review.GroupID || run.Public.GroupID != run.Binding.Pair.GroupID || !validDiagnosticParticipantBinding(run.Binding.diagnosticParticipantBinding) || len(run.Public.Targets) != len(run.Binding.Targets) || len(run.Binding.Review.Targets) != len(run.Binding.Targets) || !run.Binding.Review.CanBuild {
		return errors.New("managed adoption requires the complete original reviewed roster")
	}
	for i, target := range run.Binding.Targets {
		row, reviewed, member := run.Public.Targets[i], run.Binding.Review.Targets[i], run.Binding.Pair.Members[i]
		if !diagnosticToken.MatchString(target.NodeID) || target.Principal != target.NodeID || (i > 0 && run.Binding.Targets[i-1].NodeID >= target.NodeID) || row.NodeID != target.NodeID || row.Principal != target.Principal || row.Address != target.Address || row.Local != target.Local || reviewed.NodeID != target.NodeID || reviewed.Principal != target.Principal || reviewed.Address != target.Address || reviewed.Local != target.Local || member.NodeID != target.NodeID || member.Principal != target.Principal || target.Local != (target.Candidate == nil) || !diagnosticDigest.MatchString(run.Binding.Pins[target.Principal]) {
			return errors.New("managed adoption participant binding changed")
		}
		if !target.Local && (!onboardingID.MatchString(target.Candidate.CandidateID) || target.Candidate.Address != target.Address || target.Candidate.HostKeySHA256 == "") {
			return errors.New("managed adoption SSH identity is incomplete")
		}
	}
	return nil
}

// Validate the entire native account identity and source binding, not merely a
// name/path. The retained and fresh registrations must describe identical bytes.
func managedDescriptor(result diagnosticRuntimeResult, plan diagnosticRuntimePlan, rawPlan json.RawMessage, target diagnosticInspectionTarget, pin string) (diagnosticManagedTarget, error) {
	worker := sha256.Sum256([]byte(diagnosticRuntimeWorkerPython))
	return managedDescriptorForWorker(result, plan, rawPlan, target, pin, hex.EncodeToString(worker[:]))
}

func managedDescriptorForWorker(result diagnosticRuntimeResult, plan diagnosticRuntimePlan, rawPlan json.RawMessage, target diagnosticInspectionTarget, pin, workerSHA256 string) (diagnosticManagedTarget, error) {
	var descriptor diagnosticManagedTarget
	if result.State != "built" || !result.CleanupConfirmed || result.EffectsUnknown || result.OperationClosed || result.Attempt < 1 || result.Attempt > 3 || !validRuntimeRegistration(result, plan) {
		return descriptor, errors.New("managed adoption requires fresh completed static artifact evidence")
	}
	var sourcePlan, registration map[string]json.RawMessage
	if json.Unmarshal(rawPlan, &sourcePlan) != nil || json.Unmarshal(result.Registration, &registration) != nil || !sameManagedJSON(sourcePlan["identity"], registration["identity"]) || !sameManagedJSON(sourcePlan["sources"], registration["sources"]) {
		return descriptor, errors.New("managed runtime account or source pins changed")
	}
	var identity struct {
		User                 string `json:"user"`
		PublicIdentitySHA256 string `json:"publicIdentitySha256"`
	}
	if json.Unmarshal(sourcePlan["identity"], &identity) != nil || !diagnosticToken.MatchString(identity.User) || !diagnosticDigest.MatchString(identity.PublicIdentitySHA256) || plan.Sources["nccl"].Tag != "v2.30.7-1" || plan.Sources["nccl-tests"].Tag != "v2.20.0" {
		return descriptor, errors.New("managed runtime public account identity or source tags are incomplete")
	}
	if plan.WorkerSHA256 != workerSHA256 {
		return descriptor, errors.New("managed runtime worker differs from this shipped recipe")
	}
	descriptor = diagnosticManagedTarget{NodeID: target.NodeID, Principal: target.Principal, Local: target.Local, Address: target.Address, ClusterPinSHA256: pin, PlanDigest: plan.PlanDigest, Attempt: result.Attempt, ArtifactObservedAt: result.ArtifactObservedAt, Registration: append(json.RawMessage(nil), result.Registration...)}
	if target.Candidate != nil {
		descriptor.CandidateID = target.Candidate.CandidateID
		descriptor.SSHHostKeySHA256 = target.Candidate.HostKeySHA256
	}
	return descriptor, nil
}

func validateManagedRecord(disk diagnosticManagedDisk, run *diagnosticRuntimeRun) error {
	if err := managedRunShape(run); err != nil {
		return err
	}
	r := disk.Record
	if disk.SchemaVersion != 1 || disk.Owner != diagnosticManagedDiskOwner || r.SchemaVersion != 1 || r.Owner != diagnosticManagedOwner || r.OperationID != run.Public.OperationID || r.ReviewID != run.Public.ReviewID || r.GroupID != run.Public.GroupID || r.ApprovedRevision == 0 || r.ApprovedRevision >= run.Public.Revision || r.AdoptedAt <= 0 || !r.Adopted || r.RuntimeValidated || r.RunAvailable || len(r.Targets) != len(run.Binding.Targets) {
		return errors.New("managed runtime registry ownership is invalid")
	}
	a, _ := json.Marshal(disk.Binding)
	b, _ := json.Marshal(run.Binding)
	if !bytes.Equal(a, b) {
		return errors.New("managed runtime registry differs from original pair approval")
	}
	version := ""
	for i, target := range run.Binding.Targets {
		plan, raw, err := runtimeTargetPlan(run, i)
		if err != nil {
			return err
		}
		if version != "" && version != runtimeMPIVersion(plan) {
			return errors.New("managed runtime MPI family differs between participants")
		}
		version = runtimeMPIVersion(plan)
		stored := r.Targets[i]
		result := diagnosticRuntimeResult{State: "built", CleanupConfirmed: true, Attempt: stored.Attempt, ArtifactsValidated: true, ArtifactObservedAt: stored.ArtifactObservedAt, Registration: stored.Registration}
		// A durable adopted record remains cleanup and roster authority across
		// PAIR upgrades. Fresh adoption separately requires today's worker.
		expected, err := managedDescriptorForWorker(result, plan, raw, target, run.Binding.Pins[target.Principal], plan.WorkerSHA256)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(expected, stored) {
			return errors.New("managed runtime descriptor binding changed")
		}
		var prior diagnosticRuntimeResult
		if json.Unmarshal(run.Public.Targets[i].Receipt, &prior) != nil || !sameManagedJSON(prior.Registration, stored.Registration) {
			return errors.New("managed runtime registry artifacts differ from the retained build")
		}
	}
	return nil
}

func (d *diagnosticService) readManaged(run *diagnosticRuntimeRun, storage diagnosticManagedIO) (*diagnosticManagedRecord, error) {
	if err := managedRunShape(run); err != nil {
		return nil, err
	}
	raw, err := storage.read(d.managedPath(run.Public.OperationID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var disk diagnosticManagedDisk
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 512<<10 || decoder.Decode(&disk) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("managed runtime registry record is malformed")
	}
	if err := validateManagedRecord(disk, run); err != nil {
		return nil, err
	}
	// A complete JSON record can remain after rename even when synchronization
	// failed. Every recovery reader must establish durability before publishing
	// its authority; record presence or Public.Adopted is not a commit receipt.
	if storage.sync == nil || storage.sync(d.managedPath(run.Public.OperationID)) != nil {
		d.runtimeRecoveryFailed = true
		return nil, errors.New("managed runtime registry durability is unconfirmed")
	}
	confirmed, err := storage.read(d.managedPath(run.Public.OperationID))
	if err != nil || !bytes.Equal(raw, confirmed) {
		d.runtimeRecoveryFailed = true
		return nil, errors.New("managed runtime registry changed during durability confirmation")
	}
	return &disk.Record, nil
}

// Called by the runtime loader before accepting or reconciling Public.Adopted.
// No registry is created and no participant is contacted. Recovery re-syncs the
// verified record without changing its bytes or permissions.
func (d *diagnosticService) managedRuntimeAdopted(run *diagnosticRuntimeRun) (bool, error) {
	return d.managedRuntimeAdoptedWithIO(run, d.managedIO())
}
func (d *diagnosticService) managedRuntimeAdoptedWithIO(run *diagnosticRuntimeRun, storage diagnosticManagedIO) (bool, error) {
	record, err := d.readManaged(run, storage)
	return record != nil && err == nil, err
}

func (d *diagnosticService) getManagedRuntime(operationID string) (diagnosticManagedLookup, error) {
	return d.getManagedRuntimeWithIO(operationID, d.managedIO())
}
func (d *diagnosticService) getManagedRuntimeWithIO(operationID string, storage diagnosticManagedIO) (diagnosticManagedLookup, error) {
	if !onboardingID.MatchString(operationID) {
		return diagnosticManagedLookup{}, errors.New("a retained runtime operation is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	run := d.runtimeRuns[operationID]
	if run == nil {
		return diagnosticManagedLookup{}, errors.New("runtime operation is unknown")
	}
	record, err := d.readManaged(run, storage)
	return diagnosticManagedLookup{Record: record}, err
}

func (d *diagnosticService) managedRuntimeInventory() diagnosticManagedInventory {
	return d.managedRuntimeInventoryWithIO(d.managedIO())
}
func (d *diagnosticService) managedRuntimeInventoryWithIO(storage diagnosticManagedIO) diagnosticManagedInventory {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := diagnosticManagedInventory{Records: []diagnosticManagedRecord{}, RecoveryRequired: d.runtimeRecoveryFailed || d.packageRecoveryFailed || d.recoveryFailed}
	directoryPath := filepath.Dir(d.managedPath("unused"))
	info, err := os.Lstat(directoryPath)
	if errors.Is(err, os.ErrNotExist) {
		return result
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		result.RecoveryRequired = true
		return result
	}
	dir, err := os.Open(directoryPath)
	if errors.Is(err, os.ErrNotExist) {
		return result
	}
	if err != nil {
		result.RecoveryRequired = true
		return result
	}
	defer dir.Close()
	entries, err := dir.ReadDir(diagnosticManagedLimit + 1)
	if err != nil && err != io.EOF || len(entries) > diagnosticManagedLimit {
		result.RecoveryRequired = true
		return result
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Name() != id+".json" || !onboardingID.MatchString(id) {
			result.RecoveryRequired = true
			continue
		}
		run := d.runtimeRuns[id]
		if run == nil {
			result.RecoveryRequired = true
			continue
		}
		record, err := d.readManaged(run, storage)
		if err != nil || record == nil {
			result.RecoveryRequired = true
			continue
		}
		result.Records = append(result.Records, *record)
	}
	sort.Slice(result.Records, func(i, j int) bool { return result.Records[i].OperationID < result.Records[j].OperationID })
	return result
}

func (d *diagnosticService) managedAccess(binding diagnosticParticipantBinding) ([]onboardingPrivateTarget, error) {
	if err := d.participantPinsCurrent(binding); err != nil {
		return nil, err
	}
	current, err := d.inspectionTargets(binding.Pair)
	if err != nil {
		return nil, err
	}
	if len(current.Targets) != len(binding.Targets) {
		return nil, errors.New("managed runtime participant roster changed")
	}
	accesses := make([]onboardingPrivateTarget, len(binding.Targets))
	for i, target := range binding.Targets {
		now := current.Targets[i]
		if now.NodeID != target.NodeID || now.Principal != target.Principal || now.Address != target.Address || now.Local != target.Local {
			return nil, errors.New("managed runtime participant changed")
		}
		accepted := map[string]string{}
		if target.Candidate != nil {
			accepted[target.Candidate.CandidateID] = target.Candidate.HostKeySHA256
		}
		accesses[i], err = d.packageAccess(target, accepted)
		if err != nil {
			return nil, err
		}
	}
	return accesses, nil
}

func (d *diagnosticService) adoptRuntime(ctx context.Context, request diagnosticManagedAction) (diagnosticManagedAdoption, error) {
	return d.adoptRuntimeWithIO(ctx, request, d.managedIO())
}
func (d *diagnosticService) adoptRuntimeWithIO(ctx context.Context, request diagnosticManagedAction, storage diagnosticManagedIO) (out diagnosticManagedAdoption, err error) {
	if !onboardingID.MatchString(request.OperationID) || request.ExpectedRevision == 0 {
		return out, errors.New("managed adoption requires the current operation and revision")
	}
	d.m.exec.diagnosticMu.Lock()
	admissionLocked := true
	defer func() {
		if admissionLocked {
			d.m.exec.diagnosticMu.Unlock()
		}
	}()
	fabricHeld := d.m.exec.fabric.held()
	d.mu.Lock()
	run := d.runtimeRuns[request.OperationID]
	if err = managedRunShape(run); err != nil {
		d.mu.Unlock()
		return out, err
	}
	if run.busy || run.cancel != nil || d.runtimeActive != "" || d.packageActive != "" || d.runtimeRecoveryFailed || d.packageRecoveryFailed || d.recoveryFailed || d.packageAdmissionClosed || d.ctx.Err() != nil || ctx.Err() != nil || len(d.cancels) != 0 || d.reservation != nil {
		d.mu.Unlock()
		return out, errors.New("another operation, shutdown or unresolved recovery holds managed adoption")
	}
	record, readErr := d.readManaged(run, storage)
	if readErr != nil {
		d.mu.Unlock()
		return out, readErr
	}
	if record != nil {
		if record.ApprovedRevision != request.ExpectedRevision {
			d.mu.Unlock()
			return out, errors.New("managed adoption was approved at a different revision")
		}
		if err = d.participantPinsCurrent(run.Binding.diagnosticParticipantBinding); err != nil {
			d.mu.Unlock()
			return out, err
		}
		if !run.Public.Adopted {
			run.Public.Adopted = true
			run.Public.Stage = "runtime-adopted"
			run.Public.Revision++
			if err = storage.saveRun(run); err != nil {
				d.runtimeRecoveryFailed = true
			}
		}
		out = diagnosticManagedAdoption{Operation: cloneRuntimeOperation(run), Record: record}
		d.mu.Unlock()
		return out, err
	}
	if runtimeRetrySourceStatus(run) != "current" {
		d.mu.Unlock()
		return out, errors.New("managed adoption requires the current shipped runtime worker")
	}
	if fabricHeld {
		d.mu.Unlock()
		return out, errors.New("fabric setup or recovery holds managed adoption")
	}
	if run.Public.Adopted || run.Public.Revision != request.ExpectedRevision || run.Public.State != "completed" || !run.Public.CleanupConfirmed || run.Public.RuntimeValidated {
		d.mu.Unlock()
		return out, errors.New("only a current completed unadopted build may be adopted")
	}
	for _, target := range run.Public.Targets {
		if target.State != "built" || !target.CleanupConfirmed {
			d.mu.Unlock()
			return out, errors.New("all participants require completed clean builds")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(run.Binding.Targets))*75*time.Second+10*time.Second)
	run.busy = true
	run.cancel = cancel
	d.runtimeActive = request.OperationID
	run.Public.Stage = "adopting-runtime"
	run.Public.Revision++
	if err = storage.saveRun(run); err != nil {
		d.runtimeRecoveryFailed = true
		run.busy = false
		run.cancel = nil
		d.runtimeActive = ""
		cancel()
		d.mu.Unlock()
		return out, err
	}
	d.mu.Unlock()
	d.m.exec.diagnosticMu.Unlock()
	admissionLocked = false
	published, publicationUnknown := false, false
	defer func() {
		cancel()
		d.mu.Lock()
		defer d.mu.Unlock()
		run.busy = false
		run.cancel = nil
		if d.runtimeActive == request.OperationID && run.Public.CleanupConfirmed {
			d.runtimeActive = ""
		}
		if !published {
			run.Public.Stage = "adoption-not-published"
			if publicationUnknown {
				run.Public.Stage = "adoption-publication-unknown"
			}
			run.Public.Revision++
			if run.Public.State == "cancelling" {
				run.Public.State = "completed"
			}
			if saveErr := storage.saveRun(run); saveErr != nil {
				d.runtimeRecoveryFailed = true
				err = errors.Join(err, saveErr)
			}
		}
		out.Operation = cloneRuntimeOperation(run)
	}()
	accesses, err := d.managedAccess(run.Binding.diagnosticParticipantBinding)
	if err != nil {
		return out, err
	}
	record = &diagnosticManagedRecord{SchemaVersion: 1, Owner: diagnosticManagedOwner, OperationID: run.Public.OperationID, ReviewID: run.Public.ReviewID, GroupID: run.Public.GroupID, ApprovedRevision: request.ExpectedRevision, AdoptedAt: time.Now().UnixMilli(), Adopted: true, Targets: []diagnosticManagedTarget{}}
	fresh := make([]json.RawMessage, len(run.Binding.Targets))
	for i, target := range run.Binding.Targets {
		plan, rawPlan, planErr := runtimeTargetPlan(run, i)
		if planErr != nil {
			return out, planErr
		}
		var identity struct {
			User string `json:"user"`
		}
		var planFields map[string]json.RawMessage
		_ = json.Unmarshal(rawPlan, &planFields)
		_ = json.Unmarshal(planFields["identity"], &identity)
		if !target.Local && accesses[i].access.user != identity.User {
			return out, errors.New("managed adoption requires the original device account")
		}
		readCtx, stop := context.WithTimeout(ctx, 75*time.Second)
		raw, callErr := d.runtimeNative(readCtx, target, accesses[i], map[string]any{"action": "status", "operationId": request.OperationID, "nodeId": target.NodeID, "principal": target.Principal, "expectedPlanDigest": plan.PlanDigest})
		stop()
		if callErr != nil {
			return out, callErr
		}
		result, decodeErr := runtimeResult(raw, "status", request.OperationID, plan.PlanDigest)
		if decodeErr != nil {
			return out, decodeErr
		}
		if !result.CleanupConfirmed {
			d.mu.Lock()
			row := &run.Public.Targets[i]
			row.State = "unknown"
			row.Receipt = append(json.RawMessage(nil), raw...)
			row.CleanupConfirmed = false
			row.Reason = "Fresh native status did not confirm owned build cleanup"
			run.Public.CleanupConfirmed = false
			run.Public.State = "failed"
			d.mu.Unlock()
			return out, errors.New("owned build cleanup must be reconciled before adoption")
		}
		descriptor, verifyErr := managedDescriptor(result, plan, rawPlan, target, run.Binding.Pins[target.Principal])
		if verifyErr != nil {
			return out, verifyErr
		}
		var prior diagnosticRuntimeResult
		if json.Unmarshal(run.Public.Targets[i].Receipt, &prior) != nil || run.Public.Targets[i].Attempt != result.Attempt || !sameManagedJSON(prior.Registration, result.Registration) {
			return out, errors.New("managed runtime artifacts changed since the completed build")
		}
		record.Targets = append(record.Targets, descriptor)
		fresh[i] = append(json.RawMessage(nil), raw...)
	}
	current, err := d.managedAccess(run.Binding.diagnosticParticipantBinding)
	if err != nil {
		return out, err
	}
	for i, target := range run.Binding.Targets {
		if !target.Local && (current[i].accessGeneration != accesses[i].accessGeneration || current[i].access.user != accesses[i].access.user) {
			return out, errors.New("managed adoption account access changed during verification")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil || d.ctx.Err() != nil || d.packageAdmissionClosed || d.runtimeActive != request.OperationID || d.packageActive != "" || run.Public.State != "completed" {
		return out, errors.New("managed adoption admission closed before publication")
	}
	record.AdoptedAt = time.Now().UnixMilli()
	disk := diagnosticManagedDisk{SchemaVersion: 1, Owner: diagnosticManagedDiskOwner, Record: *record, Binding: run.Binding}
	if err = validateManagedRecord(disk, run); err != nil {
		return out, err
	}
	if err = storage.write(d.managedPath(request.OperationID), disk); err != nil {
		// A write can fail after rename. Re-read the exact record and confirm
		// its synchronization before deciding whether publication occurred.
		existing, readErr := d.readManaged(run, storage)
		if readErr != nil || existing != nil {
			publicationUnknown = true
			storedJSON, _ := json.Marshal(existing)
			expectedJSON, _ := json.Marshal(record)
			if readErr == nil && sameManagedJSON(storedJSON, expectedJSON) {
				err = nil
				publicationUnknown = false
			} else {
				d.runtimeRecoveryFailed = true
				return out, err
			}
		} else {
			return out, err
		}
	}
	// The registry now owns adoption even if the secondary operation checkpoint
	// fails or its response is lost. The loader reconciles using this same record.
	published = true
	out.Record = record
	run.Public.Adopted = true
	run.Public.Stage = "runtime-adopted"
	run.Public.Revision++
	for i := range run.Public.Targets {
		run.Public.Targets[i].Receipt = fresh[i]
	}
	if err = storage.saveRun(run); err != nil {
		d.runtimeRecoveryFailed = true
	}
	return out, err
}
