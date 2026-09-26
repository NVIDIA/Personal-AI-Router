// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const diagnosticRuntimeRecipe = "dgx-spark-nccl-cuda13-sm121-user-build-v2"

// Assigned only from shipped embedded source, never from an RPC request.
//
//go:embed diagnostic_runtime_build.py
var diagnosticRuntimePython string

//go:embed diagnostic_runtime_build_worker.py
var diagnosticRuntimeWorkerPython string

type diagnosticRuntimeTarget struct {
	NodeID           string               `json:"nodeId"`
	Principal        string               `json:"principal"`
	Local            bool                 `json:"local"`
	Address          string               `json:"address"`
	ClusterPinSHA256 string               `json:"clusterPinSha256,omitempty"`
	Candidate        *onboardingCandidate `json:"candidate,omitempty"`
	State            string               `json:"state"`
	Reason           string               `json:"reason,omitempty"`
	Review           json.RawMessage      `json:"review,omitempty"`
	Receipt          json.RawMessage      `json:"receipt,omitempty"`
	CleanupConfirmed bool                 `json:"cleanupConfirmed"`
	Attempt          int                  `json:"attempt,omitempty"`
}

type diagnosticRuntimeReview struct {
	ReviewID    string                    `json:"reviewId"`
	OperationID string                    `json:"operationId"`
	GroupID     string                    `json:"groupId"`
	ExpiresAt   int64                     `json:"expiresAt"`
	CanBuild    bool                      `json:"canBuild"`
	Targets     []diagnosticRuntimeTarget `json:"targets"`
}

type diagnosticRuntimeBinding struct {
	Review diagnosticRuntimeReview `json:"review"`
	diagnosticParticipantBinding
}

type diagnosticRuntimeOperation struct {
	OperationID       string                    `json:"operationId"`
	ReviewID          string                    `json:"reviewId"`
	GroupID           string                    `json:"groupId"`
	State             string                    `json:"state"`
	Stage             string                    `json:"stage"`
	Revision          uint64                    `json:"revision"`
	StartedAt         int64                     `json:"startedAt"`
	FinishedAt        int64                     `json:"finishedAt,omitempty"`
	Targets           []diagnosticRuntimeTarget `json:"targets"`
	CleanupConfirmed  bool                      `json:"cleanupConfirmed"`
	RuntimeValidated  bool                      `json:"runtimeValidated"`
	Adopted           bool                      `json:"adopted"`
	RetrySourceStatus string                    `json:"retrySourceStatus,omitempty"`
}

type diagnosticRuntimeRun struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Owner         string                     `json:"owner"`
	Public        diagnosticRuntimeOperation `json:"operation"`
	Binding       diagnosticRuntimeBinding   `json:"binding"`
	cancel        context.CancelFunc
	busy          bool
}

type diagnosticRuntimeAction struct {
	OperationID          string `json:"operationId,omitempty"`
	ReviewID             string `json:"reviewId,omitempty"`
	GroupID              string `json:"groupId,omitempty"`
	CloseUnstartedReview bool   `json:"closeUnstartedReview,omitempty"`
	ExpectedRevision     uint64 `json:"expectedRevision,omitempty"`
}

type diagnosticRuntimeStatus struct {
	Operation        *diagnosticRuntimeOperation `json:"operation"`
	RecoveryRequired bool                        `json:"recoveryRequired"`
	ReviewClosed     bool                        `json:"reviewClosed,omitempty"`
}

type diagnosticRuntimeResult struct {
	SchemaVersion       int             `json:"schemaVersion"`
	Action              string          `json:"action"`
	OperationID         string          `json:"operationId"`
	PlanDigest          string          `json:"planDigest"`
	State               string          `json:"state"`
	Attempt             int             `json:"attempt"`
	CanBuild            bool            `json:"canBuild"`
	EffectsApplied      bool            `json:"effectsApplied"`
	EffectsUnknown      bool            `json:"effectsUnknown"`
	CleanupConfirmed    bool            `json:"cleanupConfirmed"`
	RuntimeValidated    bool            `json:"runtimeValidated"`
	ManagerAdopted      bool            `json:"managerAdopted"`
	FreshReviewRequired bool            `json:"freshReviewRequired"`
	OperationClosed     bool            `json:"operationClosed"`
	ArtifactsValidated  bool            `json:"artifactsValidated"`
	ArtifactObservedAt  string          `json:"artifactObservedAt"`
	ErrorCode           string          `json:"errorCode"`
	Plan                json.RawMessage `json:"plan"`
	Registration        json.RawMessage `json:"registration"`
}

type diagnosticRuntimePlan struct {
	SchemaVersion int    `json:"schemaVersion"`
	RecipeID      string `json:"recipeId"`
	OperationID   string `json:"operationId"`
	PlanDigest    string `json:"planDigest"`
	WorkerSHA256  string `json:"workerSha256"`
	Identity      struct {
		NodeID    string `json:"nodeId"`
		Principal string `json:"principal"`
		UID       int    `json:"uid"`
		Home      string `json:"home"`
	} `json:"identity"`
	Sources map[string]struct {
		URL    string `json:"url"`
		Commit string `json:"commit"`
		Tag    string `json:"tag"`
	} `json:"sources"`
	MPIPackages []struct {
		Name         string `json:"name"`
		Version      string `json:"version"`
		Architecture string `json:"architecture"`
	} `json:"mpiPackages"`
	Limits struct {
		MaxAttempts     int `json:"maxAttempts"`
		MaxBuildSeconds int `json:"maxBuildSeconds"`
		ParallelJobs    int `json:"parallelJobs"`
	} `json:"limits"`
	GPUExecuted      bool `json:"gpuExecuted"`
	MPIExecuted      bool `json:"mpiExecuted"`
	ManagerAdopted   bool `json:"managerAdopted"`
	RuntimeValidated bool `json:"runtimeValidated"`
}

func parseRuntimePlan(raw json.RawMessage, target diagnosticInspectionTarget, operationID string) (diagnosticRuntimePlan, error) {
	var plan diagnosticRuntimePlan
	if len(raw) > 96<<10 || json.Unmarshal(raw, &plan) != nil || plan.SchemaVersion != 1 || plan.RecipeID != diagnosticRuntimeRecipe || plan.OperationID != operationID || !diagnosticDigest.MatchString(plan.PlanDigest) || !diagnosticDigest.MatchString(plan.WorkerSHA256) || plan.Identity.NodeID != target.NodeID || plan.Identity.Principal != target.Principal || plan.Identity.UID <= 0 || !diagnosticPath.MatchString(plan.Identity.Home) || plan.Identity.Home == "/" || plan.GPUExecuted || plan.MPIExecuted || plan.ManagerAdopted || plan.RuntimeValidated || plan.Limits.MaxAttempts != 3 || plan.Limits.MaxBuildSeconds != 1800 || plan.Limits.ParallelJobs != 2 {
		return plan, errors.New("invalid fixed NCCL build plan or participant binding")
	}
	if len(plan.Sources) != 2 || plan.Sources["nccl"].URL != "https://github.com/NVIDIA/nccl.git" || plan.Sources["nccl"].Commit != "73cf112295c33aee2b895f329f592f2a9b4b0f97" || plan.Sources["nccl-tests"].URL != "https://github.com/NVIDIA/nccl-tests.git" || plan.Sources["nccl-tests"].Commit != "b4d5beebca8a76cf01335f724d154b9b9d394d96" || runtimeMPIVersion(plan) == "" {
		return plan, errors.New("NCCL build sources or MPI family differ from the fixed recipe")
	}
	return plan, nil
}

func runtimeMPIVersion(plan diagnosticRuntimePlan) string {
	family := make([]diagnosticInstalledMPIPackage, 0, len(plan.MPIPackages))
	for _, pkg := range plan.MPIPackages {
		family = append(family, diagnosticInstalledMPIPackage{Name: pkg.Name, Version: pkg.Version, Architecture: pkg.Architecture, Status: "install ok installed"})
	}
	return packageFamilyVersion(family)
}

func diagnosticRuntimeProgram() string {
	if diagnosticRuntimePython == "" || diagnosticRuntimeWorkerPython == "" {
		return ""
	}
	return "import sys,types\n_i=types.ModuleType('diagnostic_tools_remote')\nexec(" + strconv.Quote(diagnosticInspectPython) + ",_i.__dict__)\nsys.modules['diagnostic_tools_remote']=_i\n_w=types.ModuleType('diagnostic_runtime_build_worker')\n_workerSource=" + strconv.Quote(diagnosticRuntimeWorkerPython) + ".encode('utf-8')\nexec(_workerSource,_w.__dict__)\n_w.SHIPPED_SOURCE=_workerSource\nsys.modules['diagnostic_runtime_build_worker']=_w\n" + diagnosticRuntimePython
}

func (d *diagnosticService) runtimeNative(ctx context.Context, target diagnosticInspectionTarget, access onboardingPrivateTarget, request map[string]any) ([]byte, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	defer clear(input)
	if d.runtimeTestRun != nil {
		return d.runtimeTestRun(ctx, target, access, input)
	}
	program := diagnosticRuntimeProgram()
	if program == "" {
		return nil, errors.New("the shipped NCCL build helper is unavailable")
	}
	return d.participantProgram(ctx, target, access, program, input)
}

func runtimeResult(raw []byte, action, operationID, planDigest string) (diagnosticRuntimeResult, error) {
	var result diagnosticRuntimeResult
	if len(raw) > 192<<10 || json.Unmarshal(raw, &result) != nil || result.SchemaVersion != 1 || result.Action != action || result.RuntimeValidated || result.ManagerAdopted || operationID != "" && result.OperationID != operationID || planDigest != "" && result.PlanDigest != planDigest && !(result.State == "unknown" && result.EffectsUnknown && result.PlanDigest == "") {
		return result, errors.New("NCCL build reply does not match the owned action and plan")
	}
	if result.EffectsUnknown {
		result.CleanupConfirmed = false
	}
	return result, nil
}

// This is a derived compatibility hint, not a replacement for live retry
// admission or the native worker's complete approved-plan comparison.
func runtimeRetrySourceStatus(run *diagnosticRuntimeRun) string {
	if run == nil || diagnosticRuntimeWorkerPython == "" || !validDiagnosticParticipantBinding(run.Binding.diagnosticParticipantBinding) || len(run.Binding.Review.Targets) != len(run.Binding.Targets) || len(run.Public.Targets) != len(run.Binding.Targets) {
		return "unknown"
	}
	worker := sha256.Sum256([]byte(diagnosticRuntimeWorkerPython))
	current := hex.EncodeToString(worker[:])
	status := "current"
	for i := range run.Binding.Targets {
		plan, _, err := runtimeTargetPlan(run, i)
		if err != nil {
			return "unknown"
		}
		if plan.WorkerSHA256 != current {
			status = "changed"
		}
	}
	return status
}

func cloneRuntimeOperation(run *diagnosticRuntimeRun) diagnosticRuntimeOperation {
	op := run.Public
	op.Targets = append([]diagnosticRuntimeTarget(nil), op.Targets...)
	op.RetrySourceStatus = runtimeRetrySourceStatus(run)
	return op
}
func (d *diagnosticService) runtimeSnapshot(run *diagnosticRuntimeRun) diagnosticRuntimeOperation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneRuntimeOperation(run)
}
func (d *diagnosticService) runtimePath(id string) string {
	return filepath.Join(d.m.exec.baseDir, "diagnostic-runtime-operations", id+".json")
}
func (d *diagnosticService) saveRuntime(run *diagnosticRuntimeRun) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil || len(data) > 1<<20 {
		return errors.New("NCCL build ownership exceeds its durable size bound")
	}
	if err := os.MkdirAll(filepath.Dir(d.runtimePath(run.Public.OperationID)), 0700); err != nil {
		return err
	}
	return writeJSONAtomic(d.runtimePath(run.Public.OperationID), run)
}
func (d *diagnosticService) updateRuntime(run *diagnosticRuntimeRun, update func(*diagnosticRuntimeOperation)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	update(&run.Public)
	run.Public.CleanupConfirmed = true
	for _, target := range run.Public.Targets {
		run.Public.CleanupConfirmed = run.Public.CleanupConfirmed && target.CleanupConfirmed
	}
	run.Public.Revision++
	if err := d.saveRuntime(run); err != nil {
		d.runtimeRecoveryFailed = true
		return err
	}
	return nil
}

func (d *diagnosticService) reviewRuntime(ctx context.Context, request diagnosticInspectionRequest) (diagnosticRuntimeReview, error) {
	targets, err := d.inspectionTargets(request.diagnosticSetupRequest)
	if err != nil {
		return diagnosticRuntimeReview{}, err
	}
	d.mu.Lock()
	held := d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed
	d.mu.Unlock()
	if held {
		return diagnosticRuntimeReview{}, errors.New("another setup operation or unresolved cleanup owns this lane")
	}
	accepted := map[string]string{}
	for _, key := range request.AcceptedHostKeys {
		if accepted[key.CandidateID] != "" {
			return diagnosticRuntimeReview{}, errors.New("duplicate fingerprint consent")
		}
		accepted[key.CandidateID] = key.SHA256
	}
	review := diagnosticRuntimeReview{ReviewID: newOpID(), OperationID: newOpID(), GroupID: request.GroupID, ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(), CanBuild: true, Targets: []diagnosticRuntimeTarget{}}
	binding := diagnosticRuntimeBinding{Review: review, diagnosticParticipantBinding: diagnosticParticipantBinding{Pair: request.diagnosticSetupRequest, Controller: d.m.mesh.NodeUUID(), Pins: map[string]string{}, Targets: targets.Targets, Generations: map[string]string{}}}
	binding.ControllerPin, _ = d.m.mesh.PinSHA256(binding.Controller)
	versions := map[string]bool{}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(targets.Targets))*75*time.Second+10*time.Second)
	defer cancel()
	for i, target := range targets.Targets {
		row := diagnosticRuntimeTarget{NodeID: target.NodeID, Principal: target.Principal, Local: target.Local, Address: target.Address, Candidate: target.Candidate, State: "blocked", CleanupConfirmed: true}
		binding.Pins[target.Principal], _ = d.m.mesh.PinSHA256(target.Principal)
		row.ClusterPinSHA256 = binding.Pins[target.Principal]
		access, readErr := d.packageAccess(target, accepted)
		if readErr == nil {
			binding.Generations[target.NodeID] = access.accessGeneration
			if !target.Local {
				candidate := access.candidate
				row.Candidate = &candidate
				binding.Targets[i].Candidate = &candidate
			}
			readCtx, stop := context.WithTimeout(ctx, 75*time.Second)
			raw, callErr := d.runtimeNative(readCtx, target, access, map[string]any{"action": "review", "operationId": review.OperationID, "nodeId": target.NodeID, "principal": target.Principal})
			stop()
			readErr = callErr
			if readErr == nil {
				result, decodeErr := runtimeResult(raw, "review", "", "")
				readErr = decodeErr
				if readErr == nil && (result.EffectsApplied || result.EffectsUnknown) {
					readErr = errors.New("build review unexpectedly reported effects")
				}
				if readErr == nil && result.State == "reviewed" && result.CanBuild {
					plan, err := parseRuntimePlan(result.Plan, target, review.OperationID)
					readErr = err
					if err == nil {
						versions[runtimeMPIVersion(plan)] = true
						row.State = "reviewed"
					}
				}
				if readErr == nil {
					row.Review = append(json.RawMessage(nil), raw...)
					if row.State != "reviewed" {
						row.Reason = diagnosticPublicMessage(result.ErrorCode)
					}
				}
			}
		}
		if readErr != nil {
			row.Reason = diagnosticPublicMessage(readErr.Error())
		}
		if row.State != "reviewed" {
			review.CanBuild = false
		}
		review.Targets = append(review.Targets, row)
	}
	if len(versions) > 1 {
		review.CanBuild = false
		for i := range review.Targets {
			review.Targets[i].State = "blocked"
			review.Targets[i].Reason = "The selected devices have different OpenMPI versions"
		}
	}
	binding.Review = review
	if err := d.runtimeBindingCurrent(binding); err != nil {
		return review, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed {
		return review, errors.New("shutdown or another setup operation holds admission")
	}
	if len(d.runtimeReviews) >= 16 {
		for id := range d.runtimeReviews {
			delete(d.runtimeReviews, id)
			break
		}
	}
	d.runtimeReviews[review.ReviewID] = binding
	return review, nil
}

func (d *diagnosticService) approveRuntime(request diagnosticRuntimeAction) (diagnosticRuntimeOperation, error) {
	d.mu.Lock()
	binding, ok := d.runtimeReviews[request.ReviewID]
	for _, run := range d.runtimeRuns {
		if run.Public.ReviewID == request.ReviewID {
			op := cloneRuntimeOperation(run)
			d.mu.Unlock()
			return op, nil
		}
	}
	d.mu.Unlock()
	if !ok || !binding.Review.CanBuild || binding.Review.ExpiresAt <= time.Now().UnixMilli() {
		return diagnosticRuntimeOperation{}, errors.New("NCCL build review expired or is blocked")
	}
	if err := d.runtimeBindingCurrent(binding); err != nil {
		return diagnosticRuntimeOperation{}, err
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
	defer d.mu.Unlock()
	for _, run := range d.runtimeRuns {
		if run.Public.ReviewID == request.ReviewID {
			return cloneRuntimeOperation(run), nil
		}
	}
	if fabricHeld {
		return diagnosticRuntimeOperation{}, errors.New("fabric setup or recovery holds runtime admission")
	}
	if current, ok := d.runtimeReviews[request.ReviewID]; !ok || current.Review.ExpiresAt <= time.Now().UnixMilli() {
		return diagnosticRuntimeOperation{}, errors.New("NCCL build review was closed before admission")
	}
	if d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed {
		return diagnosticRuntimeOperation{}, errors.New("shutdown or another setup operation holds admission")
	}
	run := &diagnosticRuntimeRun{SchemaVersion: 1, Owner: "pair-nccl-build-controller-v1", Binding: binding, Public: diagnosticRuntimeOperation{OperationID: binding.Review.OperationID, ReviewID: request.ReviewID, GroupID: binding.Review.GroupID, State: "running", Stage: "starting-builds", Revision: 1, StartedAt: time.Now().UnixMilli(), Targets: append([]diagnosticRuntimeTarget(nil), binding.Review.Targets...)}}
	if err := d.saveRuntime(run); err != nil {
		return diagnosticRuntimeOperation{}, err
	}
	ctx, cancel := context.WithTimeout(d.ctx, 35*time.Minute)
	run.cancel = cancel
	run.busy = true
	d.runtimeRuns[run.Public.OperationID] = run
	d.runtimeActive = run.Public.OperationID
	d.m.exec.diagnosticMu.Unlock()
	admissionLocked = false
	go d.executeRuntime(ctx, run, "build")
	return cloneRuntimeOperation(run), nil
}

func (d *diagnosticService) runtimeBindingCurrent(binding diagnosticRuntimeBinding) error {
	if len(binding.Review.Targets) != len(binding.Targets) || binding.Review.GroupID != binding.Pair.GroupID {
		return errors.New("reviewed runtime participant roster changed")
	}
	for i, target := range binding.Targets {
		reviewed := binding.Review.Targets[i]
		if target.NodeID != reviewed.NodeID || target.Principal != reviewed.Principal || target.Address != reviewed.Address || target.Local != reviewed.Local {
			return errors.New("reviewed runtime participant identity changed")
		}
	}
	return d.participantBindingCurrent(binding.diagnosticParticipantBinding)
}

func runtimeTargetPlan(run *diagnosticRuntimeRun, index int) (diagnosticRuntimePlan, json.RawMessage, error) {
	if run == nil || !validDiagnosticParticipantCount(len(run.Binding.Targets)) || len(run.Binding.Review.Targets) != len(run.Binding.Targets) || index < 0 || index >= len(run.Binding.Targets) {
		return diagnosticRuntimePlan{}, nil, errors.New("original runtime participant roster is incomplete")
	}
	result, err := runtimeResult(run.Binding.Review.Targets[index].Review, "review", "", "")
	if err != nil {
		return diagnosticRuntimePlan{}, nil, err
	}
	plan, err := parseRuntimePlan(result.Plan, run.Binding.Targets[index], run.Public.OperationID)
	return plan, result.Plan, err
}

func validRuntimeRegistration(result diagnosticRuntimeResult, plan diagnosticRuntimePlan) bool {
	var registration struct {
		SchemaVersion int             `json:"schemaVersion"`
		Kind          string          `json:"kind"`
		RecipeID      string          `json:"recipeId"`
		OperationID   string          `json:"operationId"`
		PlanDigest    string          `json:"planDigest"`
		Identity      json.RawMessage `json:"identity"`
		Sources       json.RawMessage `json:"sources"`
		Binary        struct {
			Path        string `json:"path"`
			SHA256      string `json:"sha256"`
			Size        uint64 `json:"size"`
			Interpreter string `json:"interpreter"`
		} `json:"binary"`
		Library struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Size   uint64 `json:"size"`
		} `json:"ncclLibrary"`
		LinkValidation   string `json:"linkValidation"`
		ManagerAdopted   *bool  `json:"managerAdopted"`
		RuntimeValidated *bool  `json:"runtimeValidated"`
		GPUExecuted      *bool  `json:"gpuExecuted"`
		MPIExecuted      *bool  `json:"mpiExecuted"`
	}
	if json.Unmarshal(result.Registration, &registration) != nil || registration.SchemaVersion != 1 || registration.Kind != "pair-nccl-runtime-candidate-v1" || registration.RecipeID != plan.RecipeID || registration.OperationID != plan.OperationID || registration.PlanDigest != plan.PlanDigest || registration.LinkValidation != "static-elf-resolution-only" {
		return false
	}
	var binding diagnosticRuntimePlan
	if json.Unmarshal(result.Registration, &binding) != nil || binding.Identity != plan.Identity || !reflect.DeepEqual(binding.Sources, plan.Sources) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, result.ArtifactObservedAt); err != nil || !result.ArtifactsValidated {
		return false
	}
	for _, flag := range []*bool{registration.ManagerAdopted, registration.RuntimeValidated, registration.GPUExecuted, registration.MPIExecuted} {
		if flag == nil || *flag {
			return false
		}
	}
	if registration.Binary.Interpreter != "/lib/ld-linux-aarch64.so.1" || !diagnosticDigest.MatchString(registration.Binary.SHA256) || !diagnosticDigest.MatchString(registration.Library.SHA256) || registration.Binary.Size == 0 || registration.Binary.Size > 2<<30 || registration.Library.Size == 0 || registration.Library.Size > 2<<30 {
		return false
	}
	prefix := plan.Identity.Home + "/.local/share/pair-nccl-build-v1/" + plan.OperationID + "/attempt-000" + strconv.Itoa(result.Attempt) + "/runtime/"
	return registration.Binary.Path == prefix+"bin/all_reduce_perf" && registration.Library.Path == prefix+"lib/libnccl.so.2"
}

func (d *diagnosticService) observeRuntimeTarget(ctx context.Context, run *diagnosticRuntimeRun, index int, action string, preserveFailureReason bool) error {
	target := run.Binding.Targets[index]
	if err := d.participantPinsCurrent(run.Binding.diagnosticParticipantBinding); err != nil {
		return err
	}
	accepted := map[string]string{}
	if target.Candidate != nil {
		accepted[target.Candidate.CandidateID] = target.Candidate.HostKeySHA256
	}
	access, err := d.packageAccess(target, accepted)
	if err != nil {
		return err
	}
	plan, rawPlan, err := runtimeTargetPlan(run, index)
	if err != nil {
		return err
	}
	request := map[string]any{"action": action, "operationId": run.Public.OperationID, "nodeId": target.NodeID, "principal": target.Principal}
	if action == "build" || action == "retry" {
		request["approvedPlan"] = rawPlan
		if action == "retry" {
			request["expectedAttempt"] = d.runtimeSnapshot(run).Targets[index].Attempt
		}
	} else {
		request["expectedPlanDigest"] = plan.PlanDigest
	}
	readCtx, stop := context.WithTimeout(ctx, 75*time.Second)
	defer stop()
	raw, callErr := d.runtimeNative(readCtx, target, access, request)
	result, decodeErr := runtimeResult(raw, action, run.Public.OperationID, plan.PlanDigest)
	if callErr != nil {
		return callErr
	}
	if decodeErr != nil {
		return decodeErr
	}
	if result.Attempt < 0 || result.Attempt > 3 {
		return errors.New("invalid retained build attempt")
	}
	if result.State == "built" && (!result.CleanupConfirmed || result.Attempt == 0 || !validRuntimeRegistration(result, plan)) {
		return errors.New("NCCL build artifact receipt failed its identity or static verification binding")
	}
	return d.updateRuntime(run, func(op *diagnosticRuntimeOperation) {
		row := &op.Targets[index]
		row.State = result.State
		row.Receipt = append(json.RawMessage(nil), raw...)
		row.CleanupConfirmed = result.CleanupConfirmed
		if result.Attempt > 0 {
			row.Attempt = result.Attempt
		}
		if !preserveFailureReason || result.ErrorCode != "" || row.Reason == "" || result.State == "built" || result.State == "building" {
			row.Reason = diagnosticPublicMessage(result.ErrorCode)
		}
	})
}

func (d *diagnosticService) finishRuntimeLocked(run *diagnosticRuntimeRun, cancelled bool) {
	clean, built, pending := true, true, false
	for _, target := range run.Public.Targets {
		clean = clean && target.CleanupConfirmed
		built = built && target.State == "built"
		pending = pending || target.State == "building" || target.State == "starting"
	}
	run.Public.CleanupConfirmed = clean
	run.Public.FinishedAt = time.Now().UnixMilli()
	run.Public.State = "failed"
	run.Public.Stage = "build-failed"
	if built && clean {
		run.Public.State = "completed"
		run.Public.Stage = "artifacts-built"
	}
	if cancelled {
		run.Public.State = "cancelled"
		run.Public.Stage = "cancelled"
	}
	if pending && !cancelled {
		run.Public.State = "running"
		run.Public.Stage = "building"
		run.Public.FinishedAt = 0
	}
	if !clean {
		run.Public.Stage = "cleanup-unconfirmed"
	}
	run.Public.Revision++
	run.busy = false
	if run.cancel != nil {
		run.cancel()
		run.cancel = nil
	}
	if clean && d.runtimeActive == run.Public.OperationID {
		d.runtimeActive = ""
	} else if !clean && d.runtimeActive == "" {
		d.runtimeActive = run.Public.OperationID
	}
	if d.saveRuntime(run) != nil {
		d.runtimeRecoveryFailed = true
	}
}

func (d *diagnosticService) executeRuntime(ctx context.Context, run *diagnosticRuntimeRun, action string) {
	failed := false
	for i := range run.Binding.Targets {
		if ctx.Err() != nil {
			failed = true
			break
		}
		current := d.runtimeSnapshot(run).Targets[i]
		targetAction := action
		if action == "retry" {
			if current.State == "built" && current.CleanupConfirmed {
				continue
			}
			if current.Attempt == 0 {
				targetAction = "build"
			}
		}
		if err := d.updateRuntime(run, func(op *diagnosticRuntimeOperation) {
			op.Targets[i].CleanupConfirmed = false
			op.Targets[i].State = "starting"
		}); err != nil {
			failed = true
			break
		}
		if err := d.observeRuntimeTarget(ctx, run, i, targetAction, false); err != nil {
			_ = d.updateRuntime(run, func(op *diagnosticRuntimeOperation) {
				op.Targets[i].State = "unknown"
				op.Targets[i].Reason = diagnosticPublicMessage(err.Error())
			})
			failed = true
			break
		}
	}
	for !failed && ctx.Err() == nil {
		pending := false
		for i := range run.Binding.Targets {
			row := d.runtimeSnapshot(run).Targets[i]
			if row.State == "built" && row.CleanupConfirmed {
				continue
			}
			var statusErr error
			firstStatusError, lastStatusError := "", ""
			// Native status may settle cleanup; retry only when no command started.
		statusAttempts:
			for attempt := 0; attempt < 3; attempt++ {
				statusErr = d.observeRuntimeTarget(ctx, run, i, "status", false)
				if statusErr == nil {
					break
				}
				message := diagnosticPublicMessage(statusErr.Error())
				if attempt == 0 {
					firstStatusError = message
				}
				lastStatusError = message
				if attempt == 2 || !errors.Is(statusErr, errBeforeRemoteCommand) {
					break
				}
				select {
				case <-ctx.Done():
					break statusAttempts
				case <-time.After(600 * time.Millisecond):
				}
			}
			if ctx.Err() != nil {
				break
			}
			if statusErr != nil {
				reason := firstStatusError
				if lastStatusError != firstStatusError {
					bound := func(message string) string {
						if len(message) > 960 {
							return strings.ToValidUTF8(message[:957], "") + "..."
						}
						return message
					}
					reason = "first status error: " + bound(firstStatusError) + "; last status error: " + bound(lastStatusError)
				}
				_ = d.updateRuntime(run, func(op *diagnosticRuntimeOperation) {
					op.Targets[i].State = "unknown"
					op.Targets[i].Reason = reason
					op.Targets[i].CleanupConfirmed = false
				})
				failed = true
				break
			}
			row = d.runtimeSnapshot(run).Targets[i]
			if row.State == "failed" || row.State == "cancelled" || row.State == "unknown" {
				failed = true
				break
			}
			pending = pending || row.State != "built" || !row.CleanupConfirmed
		}
		if failed || !pending {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	if failed || ctx.Err() != nil {
		for i := range run.Binding.Targets {
			if d.runtimeSnapshot(run).Targets[i].CleanupConfirmed {
				continue
			}
			cleanupCtx, stop := context.WithTimeout(context.Background(), 75*time.Second)
			// A clean cancel receipt confirms cleanup, not the cause of the failed build.
			preserveFailureReason := d.runtimeSnapshot(run).State != "cancelling"
			if err := d.observeRuntimeTarget(cleanupCtx, run, i, "cancel", preserveFailureReason); err != nil {
				_ = d.updateRuntime(run, func(op *diagnosticRuntimeOperation) {
					op.Targets[i].Reason = diagnosticPublicMessage(err.Error())
					op.Targets[i].CleanupConfirmed = false
				})
			}
			stop()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.finishRuntimeLocked(run, ctx.Err() != nil)
}

func (d *diagnosticService) runtimeStatus(ctx context.Context, request diagnosticRuntimeAction) (diagnosticRuntimeStatus, error) {
	if request.ExpectedRevision != 0 {
		return diagnosticRuntimeStatus{}, errors.New("status does not accept a retry revision")
	}
	if request.CloseUnstartedReview && (!onboardingID.MatchString(request.ReviewID) || request.OperationID != "" || request.GroupID != "") {
		return diagnosticRuntimeStatus{}, errors.New("review closure requires only its review identifier")
	}
	d.mu.Lock()
	var selected *diagnosticRuntimeRun
	for _, run := range d.runtimeRuns {
		if request.OperationID != "" && run.Public.OperationID != request.OperationID || request.ReviewID != "" && run.Public.ReviewID != request.ReviewID || request.GroupID != "" && run.Public.GroupID != request.GroupID {
			continue
		}
		if selected == nil || selected.Public.CleanupConfirmed && !run.Public.CleanupConfirmed || selected.Public.CleanupConfirmed == run.Public.CleanupConfirmed && selected.Public.StartedAt < run.Public.StartedAt {
			selected = run
		}
	}
	result := diagnosticRuntimeStatus{RecoveryRequired: d.runtimeRecoveryFailed}
	if selected == nil {
		if request.CloseUnstartedReview && !result.RecoveryRequired {
			delete(d.runtimeReviews, request.ReviewID)
			result.ReviewClosed = true
		}
		d.mu.Unlock()
		return result, nil
	}
	refresh := !selected.busy && selected.cancel == nil
	if refresh {
		selected.busy = true
	}
	d.mu.Unlock()
	if refresh {
		for i := range selected.Binding.Targets {
			beforeOperation := d.runtimeSnapshot(selected)
			before := beforeOperation.Targets[i]
			if before.State == "reviewed" && before.Attempt == 0 && before.CleanupConfirmed {
				continue
			}
			if err := d.observeRuntimeTarget(ctx, selected, i, "status", beforeOperation.State == "failed"); err != nil {
				_ = d.updateRuntime(selected, func(op *diagnosticRuntimeOperation) {
					op.Targets[i].Reason = diagnosticPublicMessage(err.Error())
					op.Targets[i].State = "unknown"
					op.Targets[i].CleanupConfirmed = before.CleanupConfirmed
				})
			}
		}
		d.mu.Lock()
		d.finishRuntimeLocked(selected, selected.Public.State == "cancelled" || selected.Public.State == "cancelling")
		d.mu.Unlock()
	}
	op := d.runtimeSnapshot(selected)
	result.Operation = &op
	d.mu.Lock()
	result.RecoveryRequired = d.runtimeRecoveryFailed
	d.mu.Unlock()
	return result, nil
}

func (d *diagnosticService) runtimeAction(ctx context.Context, method string, request diagnosticRuntimeAction) (diagnosticRuntimeOperation, error) {
	if !onboardingID.MatchString(request.OperationID) || request.CloseUnstartedReview || request.ReviewID != "" || request.GroupID != "" {
		return diagnosticRuntimeOperation{}, errors.New("only the retained NCCL build operation may be controlled")
	}
	admissionLocked := method == "engine:diagnostic-runtime-retry"
	if admissionLocked {
		d.m.exec.diagnosticMu.Lock()
		defer func() {
			if admissionLocked {
				d.m.exec.diagnosticMu.Unlock()
			}
		}()
		if d.m.exec.fabric.held() {
			return diagnosticRuntimeOperation{}, errors.New("fabric setup or recovery holds runtime retry admission")
		}
	}
	d.mu.Lock()
	run := d.runtimeRuns[request.OperationID]
	if run == nil {
		d.mu.Unlock()
		return diagnosticRuntimeOperation{}, errors.New("NCCL build operation is unknown")
	}
	if method == "engine:diagnostic-runtime-retry" && (request.ExpectedRevision == 0 || request.ExpectedRevision != run.Public.Revision) {
		d.mu.Unlock()
		return diagnosticRuntimeOperation{}, errors.New("NCCL build changed since this explicit retry was requested; refresh its status")
	}
	if method == "engine:diagnostic-runtime-retry" {
		if runtimeRetrySourceStatus(run) != "current" {
			op := cloneRuntimeOperation(run)
			d.mu.Unlock()
			return op, errors.New("the approved NCCL build worker is changed or unconfirmed; obtain a fresh build review")
		}
		// Adoption binds one immutable build attempt. Even an uncertain record
		// left after rename owns that operation ID until recovery settles it.
		// A fresh build must use a new review/operation rather than mutate the
		// artifacts underneath an already-published runtime descriptor.
		_, registryErr := os.Lstat(d.managedPath(request.OperationID))
		if run.Public.Adopted || !errors.Is(registryErr, os.ErrNotExist) {
			d.mu.Unlock()
			return diagnosticRuntimeOperation{}, errors.New("registered or registry-owned NCCL builds cannot be retried; obtain a new build review")
		}
	}
	if method == "engine:diagnostic-runtime-cancel" && request.ExpectedRevision != 0 {
		d.mu.Unlock()
		return diagnosticRuntimeOperation{}, errors.New("cancellation does not accept a retry revision")
	}
	if method == "engine:diagnostic-runtime-cancel" && run.cancel != nil {
		run.Public.State = "cancelling"
		if run.Public.Stage == "adopting-runtime" || run.Public.Stage == "cancelling-adoption" {
			run.Public.Stage = "cancelling-adoption"
		} else {
			run.Public.Stage = "cancelling"
		}
		run.Public.Revision++
		err := d.saveRuntime(run)
		if err != nil {
			d.runtimeRecoveryFailed = true
		}
		run.cancel()
		op := cloneRuntimeOperation(run)
		d.mu.Unlock()
		return op, err
	}
	if method == "engine:diagnostic-runtime-cancel" && run.Public.CleanupConfirmed && !run.busy {
		op := cloneRuntimeOperation(run)
		d.mu.Unlock()
		return op, nil
	}
	if d.runtimeActive != "" && d.runtimeActive != request.OperationID {
		d.mu.Unlock()
		return diagnosticRuntimeOperation{}, errors.New("another NCCL build operation owns the lane")
	}
	if run.busy || run.cancel != nil {
		op := cloneRuntimeOperation(run)
		d.mu.Unlock()
		return op, errors.New("NCCL build operation is already being reconciled")
	}
	if method == "engine:diagnostic-runtime-retry" && (d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed || d.runtimeActive != "" && d.runtimeActive != request.OperationID || run.Public.State == "completed") {
		d.mu.Unlock()
		return diagnosticRuntimeOperation{}, errors.New("NCCL build retry is held")
	}
	run.busy = true
	var retryCancel context.CancelFunc
	if method == "engine:diagnostic-runtime-retry" {
		run.Public.Revision++
		run.Public.Stage = "reconciling-before-retry"
		if err := d.saveRuntime(run); err != nil {
			d.runtimeRecoveryFailed = true
			run.busy = false
			d.mu.Unlock()
			return diagnosticRuntimeOperation{}, err
		}
		ctx, retryCancel = context.WithCancel(ctx)
		run.cancel = retryCancel
	}
	d.runtimeActive = request.OperationID
	d.mu.Unlock()
	if admissionLocked {
		d.m.exec.diagnosticMu.Unlock()
		admissionLocked = false
	}
	started := false
	defer func() {
		if !started {
			d.mu.Lock()
			if retryCancel != nil && ctx.Err() != nil {
				d.mu.Unlock()
				go d.executeRuntime(ctx, run, "status")
				return
			}
			run.busy = false
			if retryCancel != nil {
				run.cancel = nil
			}
			if run.Public.CleanupConfirmed && d.runtimeActive == request.OperationID {
				d.runtimeActive = ""
			}
			d.mu.Unlock()
		}
		if retryCancel != nil {
			retryCancel()
		}
	}()
	if err := d.participantPinsCurrent(run.Binding.diagnosticParticipantBinding); err != nil {
		return d.runtimeSnapshot(run), err
	}
	if method == "engine:diagnostic-runtime-cancel" {
		for i := range run.Binding.Targets {
			if d.runtimeSnapshot(run).Targets[i].CleanupConfirmed {
				continue
			}
			if err := d.observeRuntimeTarget(ctx, run, i, "cancel", false); err != nil {
				return d.runtimeSnapshot(run), err
			}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.finishRuntimeLocked(run, true)
		return cloneRuntimeOperation(run), nil
	}
	for i := range run.Binding.Targets {
		before := d.runtimeSnapshot(run).Targets[i]
		if before.State == "reviewed" && before.Attempt == 0 && before.CleanupConfirmed {
			continue
		}
		if err := d.observeRuntimeTarget(ctx, run, i, "status", false); err != nil {
			return d.runtimeSnapshot(run), err
		}
		row := d.runtimeSnapshot(run).Targets[i]
		if !row.CleanupConfirmed {
			return d.runtimeSnapshot(run), errors.New("existing build work or cleanup must finish before retry")
		}
		var result diagnosticRuntimeResult
		if json.Unmarshal(row.Receipt, &result) != nil || result.OperationClosed {
			return d.runtimeSnapshot(run), errors.New("the original build operation was closed; obtain a new review")
		}
		if row.State != "built" && (row.Attempt < 1 || row.Attempt >= 3) {
			return d.runtimeSnapshot(run), errors.New("the original build reached its attempt limit")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.packageAdmissionClosed || d.ctx.Err() != nil || ctx.Err() != nil {
		return cloneRuntimeOperation(run), errors.New("NCCL build retry admission closed before publication")
	}
	run.Public.State = "running"
	run.Public.Stage = "retrying-builds"
	run.Public.FinishedAt = 0
	run.Public.CleanupConfirmed = false
	run.Public.Revision++
	if err := d.saveRuntime(run); err != nil {
		d.runtimeRecoveryFailed = true
		return diagnosticRuntimeOperation{}, err
	}
	opCtx, cancel := context.WithTimeout(d.ctx, 35*time.Minute)
	run.cancel = cancel
	started = true
	go d.executeRuntime(opCtx, run, "retry")
	return cloneRuntimeOperation(run), nil
}

func (d *diagnosticService) loadRuntimeRuns() {
	directory, err := os.Open(filepath.Join(d.m.exec.baseDir, "diagnostic-runtime-operations"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		d.runtimeRecoveryFailed = true
		return
	}
	defer directory.Close()
	for {
		entries, readErr := directory.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			d.runtimeRecoveryFailed = true
			return
		}
		for _, entry := range entries {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if entry.IsDir() || !onboardingID.MatchString(id) {
				continue
			}
			raw, err := readOnboardingFile(d.runtimePath(id), 1<<20)
			var run diagnosticRuntimeRun
			if err != nil || json.Unmarshal(raw, &run) != nil || run.SchemaVersion != 1 || run.Owner != "pair-nccl-build-controller-v1" || run.Public.OperationID != id || run.Public.OperationID != run.Binding.Review.OperationID || !onboardingID.MatchString(run.Public.ReviewID) || run.Public.ReviewID != run.Binding.Review.ReviewID || run.Public.GroupID != run.Binding.Review.GroupID || run.Public.GroupID != run.Binding.Pair.GroupID || run.Public.RuntimeValidated || !run.Binding.Review.CanBuild || !validDiagnosticParticipantBinding(run.Binding.diagnosticParticipantBinding) || len(run.Public.Targets) != len(run.Binding.Targets) || len(run.Binding.Review.Targets) != len(run.Binding.Targets) {
				d.runtimeRecoveryFailed = true
				continue
			}
			valid := true
			seen := map[string]bool{}
			for i, target := range run.Binding.Targets {
				row, reviewed := run.Public.Targets[i], run.Binding.Review.Targets[i]
				if seen[target.NodeID] || target.NodeID != row.NodeID || target.Principal != row.Principal || target.Address != row.Address || target.Local != row.Local || target.NodeID != reviewed.NodeID || target.Principal != reviewed.Principal || target.Address != reviewed.Address || target.Local != reviewed.Local {
					valid = false
				}
				seen[target.NodeID] = true
				if !target.Local && (target.Candidate == nil || !onboardingID.MatchString(target.Candidate.CandidateID) || target.Candidate.Address != target.Address || target.Candidate.HostKeySHA256 == "") {
					valid = false
				}
				if run.Public.CleanupConfirmed && !run.Public.Targets[i].CleanupConfirmed {
					valid = false
				}
				if _, _, err := runtimeTargetPlan(&run, i); err != nil {
					valid = false
				}
			}
			if !valid {
				d.runtimeRecoveryFailed = true
				continue
			}
			interruptedAdoption := run.Public.Stage == "adopting-runtime" || run.Public.Stage == "cancelling-adoption"
			adopted, adoptionErr := d.managedRuntimeAdopted(&run)
			if adoptionErr != nil || run.Public.Adopted && !adopted {
				d.runtimeRecoveryFailed = true
				continue
			}
			run.Public.Adopted = adopted
			if adopted {
				run.Public.Stage = "runtime-adopted"
			} else if interruptedAdoption {
				run.Public.Stage = "adoption-not-published"
				run.Public.Revision++
				if run.Public.State == "cancelling" {
					run.Public.State = "completed"
					for _, target := range run.Public.Targets {
						if target.State != "built" || !target.CleanupConfirmed {
							run.Public.State = "failed"
						}
					}
				}
			}
			if interruptedAdoption && !adopted {
				run.Public.CleanupConfirmed = true
				for _, target := range run.Public.Targets {
					run.Public.CleanupConfirmed = run.Public.CleanupConfirmed && target.CleanupConfirmed
				}
			}
			switch run.Public.State {
			case "running", "cancelling", "completed", "failed", "cancelled", "interrupted":
			default:
				d.runtimeRecoveryFailed = true
				continue
			}
			if run.Public.State == "cancelling" {
				run.Public.State = "cancelled"
				run.Public.Stage = "cancelled-cleanup-required"
				run.Public.CleanupConfirmed = false
			}
			if run.Public.State == "running" {
				run.Public.State = "interrupted"
				run.Public.Stage = "reconcile-builds"
				run.Public.CleanupConfirmed = false
			}
			if !run.Public.CleanupConfirmed {
				d.runtimeActive = id
			}
			for i, target := range run.Binding.Targets {
				run.Public.Targets[i].ClusterPinSHA256 = run.Binding.Pins[target.Principal]
				if target.Local {
					continue
				}
				candidate := *target.Candidate
				candidate.AccessAvailable = false
				candidate.HostKeyTrusted = false
				candidate.Reason = "Reauthorize account access for this retained NCCL build"
				run.Public.Targets[i].Candidate = &candidate
				if d.m.onboarding.targets[candidate.CandidateID] == nil {
					d.m.onboarding.targets[candidate.CandidateID] = &onboardingPrivateTarget{candidate: candidate, lifetime: "session"}
				}
			}
			d.runtimeRuns[id] = &run
			if d.saveRuntime(&run) != nil {
				d.runtimeRecoveryFailed = true
			}
		}
		if readErr == io.EOF {
			break
		}
	}
}

func (m *Manager) handleDiagnosticRuntime(ctx context.Context, msg *Message) {
	var result any
	var err error
	if msg.Method == "engine:diagnostic-runtime-adopt" {
		var request diagnosticManagedAction
		if onboardingDecode(msg.Params, &request) != nil {
			err = errors.New("invalid managed runtime adoption request")
		} else {
			result, err = m.exec.diagnostics.adoptRuntime(ctx, request)
		}
	} else if msg.Method == "engine:diagnostic-managed-runtime" {
		var request struct {
			OperationID string `json:"operationId"`
		}
		if onboardingDecode(msg.Params, &request) != nil {
			err = errors.New("invalid managed runtime lookup")
		} else {
			result, err = m.exec.diagnostics.getManagedRuntime(request.OperationID)
		}
	} else if msg.Method == "engine:diagnostic-managed-runtimes" {
		if onboardingDecode(msg.Params, &struct{}{}) != nil {
			err = errors.New("managed inventory takes no parameters")
		} else {
			result = m.exec.diagnostics.managedRuntimeInventory()
		}
	} else if msg.Method == "engine:diagnostic-runtime-review" {
		var request diagnosticInspectionRequest
		if onboardingDecode(msg.Params, &request) != nil {
			err = errors.New("invalid NCCL build review request")
		} else {
			result, err = m.exec.diagnostics.reviewRuntime(ctx, request)
		}
	} else {
		var request diagnosticRuntimeAction
		if onboardingDecode(msg.Params, &request) != nil {
			err = errors.New("invalid NCCL build control request")
		} else {
			switch msg.Method {
			case "engine:diagnostic-runtime-approve":
				if request.OperationID != "" || request.GroupID != "" || request.CloseUnstartedReview || request.ExpectedRevision != 0 {
					err = errors.New("build approval accepts only its review identifier")
				} else {
					result, err = m.exec.diagnostics.approveRuntime(request)
				}
			case "engine:diagnostic-runtime-status":
				result, err = m.exec.diagnostics.runtimeStatus(ctx, request)
			case "engine:diagnostic-runtime-cancel", "engine:diagnostic-runtime-retry":
				result, err = m.exec.diagnostics.runtimeAction(ctx, msg.Method, request)
			default:
				err = errors.New("unsupported NCCL build action")
			}
		}
	}
	m.respondOrErr(msg, result, err)
}
