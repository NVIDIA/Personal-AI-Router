// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed mpi_socket_adapter.py
var diagnosticMPIAdapterPython string

//go:embed mpi_socket_native.py
var diagnosticMPINativePython string

type diagnosticMPIHeader struct {
	SchemaVersion int    `json:"schemaVersion"`
	RecipeID      string `json:"recipeId"`
	OperationID   string `json:"operationId"`
	GroupID       string `json:"groupId"`
	OwnerNodeID   string `json:"ownerNodeId"`
	ProfileDigest string `json:"profileDigest"`
	PlanDigest    string `json:"planDigest"`
	CreatedAt     int64  `json:"createdAt"`
	ExpiresAt     int64  `json:"expiresAt"`
}

type diagnosticMPIResult struct {
	SchemaVersion          int    `json:"schemaVersion"`
	Action                 string `json:"action"`
	NodeID                 string `json:"nodeId"`
	OperationID            string `json:"operationId"`
	PlanDigest             string `json:"planDigest"`
	ProfileDigest          string `json:"profileDigest"`
	State                  string `json:"state"`
	Output                 string `json:"output"`
	Stderr                 string `json:"stderr,omitempty"`
	CleanupConfirmed       bool   `json:"cleanupConfirmed"`
	ErrorCode              string `json:"errorCode"`
	ExitCode               *int   `json:"exitCode"`
	UnitOwned              bool   `json:"unitOwned"`
	UnitProcessesGone      bool   `json:"unitProcessesGone"`
	UnitMetadataRemoved    bool   `json:"unitMetadataRemoved"`
	RankCleanupConfirmed   bool   `json:"rankCleanupConfirmed"`
	AuthorizationRemoved   bool   `json:"authorizationRemoved"`
	AgentGone              bool   `json:"agentGone"`
	PublicArtifactsRemoved bool   `json:"publicArtifactsRemoved"`
}

// This envelope is private to the authenticated participant control channel.
// The renderer cannot supply a profile, executable, key, or MPI plan.
type diagnosticBootstrapControl struct {
	Profile diagnosticProfile `json:"profile"`
	Plan    json.RawMessage   `json:"plan"`
}

func validateDiagnosticMPIBinding(p diagnosticProfile, raw json.RawMessage, r diagnosticParticipantRequest, live bool) (diagnosticMPIHeader, error) {
	var plan diagnosticMPIHeader
	if len(raw) == 0 || len(raw) > 96<<10 || json.Unmarshal(raw, &plan) != nil ||
		plan.SchemaVersion != 1 || diagnosticMPIRecipeRanks(plan.RecipeID) == 0 || diagnosticMPIRecipeRanks(plan.RecipeID) != len(p.Members) ||
		!diagnosticDigest.MatchString(plan.PlanDigest) || !diagnosticDigest.MatchString(plan.OperationID+plan.OperationID) ||
		plan.GroupID != p.GroupID || plan.OwnerNodeID != p.OwnerNodeID || plan.ProfileDigest != profileDigest(p) ||
		p.Bootstrap.OperationID != plan.OperationID ||
		r.GroupID != plan.GroupID || r.OperationID != plan.OperationID || r.ProfileDigest != plan.ProfileDigest ||
		r.BootstrapPlanDigest != plan.PlanDigest || r.ExpiresAt != plan.ExpiresAt ||
		plan.CreatedAt <= 0 || plan.ExpiresAt <= plan.CreatedAt || plan.ExpiresAt-plan.CreatedAt > diagnosticLease.Milliseconds() {
		return plan, errors.New("native MPI plan does not bind the original profile and rank lease")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return plan, errors.New("native MPI plan is not an object")
	}
	delete(body, "planDigest")
	canonical, err := json.Marshal(body)
	if err != nil {
		return plan, err
	}
	// Re-decode to sort nested objects too, matching the fixed Python producer.
	var value any
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return plan, errors.New("native MPI plan is malformed")
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return plan, errors.New("native MPI plan cannot be bound")
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}))
	if hex.EncodeToString(sum[:]) != plan.PlanDigest {
		return plan, errors.New("native MPI plan content changed")
	}
	// Old retained leases remain usable for cleanup, but cannot start workers.
	if r.ExecutionDeadlineAt != 0 && (r.ExecutionDeadlineAt <= plan.CreatedAt || r.ExecutionDeadlineAt > plan.ExpiresAt) {
		return plan, errors.New("native MPI execution deadline exceeds its original lease")
	}
	now := time.Now().UnixMilli()
	if live && (plan.CreatedAt > now || now >= plan.ExpiresAt || now >= r.ExecutionDeadlineAt) {
		return plan, errors.New("native MPI plan or execution deadline expired before participant admission")
	}
	if err := validateDiagnosticBootstrapProfile(p); err != nil {
		return plan, err
	}
	return plan, nil
}

func canonicalDiagnosticMPIPythonSources(adapter, native string) (string, string, error) {
	adapter = strings.ReplaceAll(adapter, "\r\n", "\n")
	native = strings.ReplaceAll(native, "\r\n", "\n")
	if adapter == "" || native == "" {
		return "", "", errors.New("fixed MPI native module is unavailable")
	}
	if strings.ContainsRune(adapter, '\r') || strings.ContainsRune(native, '\r') {
		return "", "", errors.New("fixed MPI native module contains an unsupported carriage return")
	}
	return adapter, native, nil
}

func diagnosticMPIProgram() (string, error) {
	adapter, native, err := canonicalDiagnosticMPIPythonSources(diagnosticMPIAdapterPython, diagnosticMPINativePython)
	if err != nil {
		return "", err
	}
	program := "import sys,types\n_a=types.ModuleType('mpi_socket_adapter')\n_adapterSource=" + strconv.Quote(adapter) +
		"\n_a.SHIPPED_SOURCE=_adapterSource.encode('utf-8')\nexec(_adapterSource,_a.__dict__)\nsys.modules['mpi_socket_adapter']=_a\n_n=types.ModuleType('mpi_socket_native')\n_nativeSource=" + strconv.Quote(native) +
		"\n_n.SHIPPED_SOURCE=_nativeSource.encode('utf-8')\nsys.modules['mpi_socket_native']=_n\nexec(_nativeSource,_n.__dict__)\n_n.main()\n"
	if len(program) > 120<<10 {
		return "", errors.New("fixed native MPI module exceeds the process argument budget")
	}
	return program, nil
}

func (d *diagnosticService) mpiNative(ctx context.Context, action string, p diagnosticProfile, raw json.RawMessage, r diagnosticParticipantRequest, privateKey []byte) (diagnosticMPIResult, error) {
	var result diagnosticMPIResult
	live := action == "prepare-peer" || action == "start-coordinator"
	if action != "prepare-peer" && action != "start-coordinator" && action != "status" && action != "cancel" && action != "cleanup" {
		return result, errors.New("unknown fixed MPI participant action")
	}
	if _, err := validateDiagnosticMPIBinding(p, raw, r, live); err != nil {
		return result, err
	}
	if (action == "start-coordinator" && (len(privateKey) == 0 || len(privateKey) > 16384)) || (action != "start-coordinator" && len(privateKey) != 0) {
		return result, errors.New("volatile MPI identity must belong only to the coordinator launch")
	}
	header, err := json.Marshal(struct {
		Action string          `json:"action"`
		Plan   json.RawMessage `json:"plan"`
	}{action, raw})
	if err != nil {
		return result, err
	}
	input := io.MultiReader(bytes.NewReader(append(header, '\n')), bytes.NewReader(privateKey))
	var output []byte
	if d.bootstrapTestRun != nil {
		output, err = d.bootstrapTestRun(ctx, input)
	} else {
		if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
			return result, errors.New("native MPI executes only on its Linux ARM64 participant")
		}
		program, sourceErr := diagnosticMPIProgram()
		if sourceErr != nil {
			return result, sourceErr
		}
		output, err = diagnosticProcessInput(ctx, "/usr/bin/python3", []string{"-I", "-c", program}, []string{"PATH=/usr/bin:/bin"}, input, nil)
	}
	if err != nil {
		return result, errors.New("fixed native MPI operation failed; participant cleanup must be reconciled")
	}
	self, selfErr := d.self(p)
	if selfErr != nil || len(output) > 2<<20 || json.Unmarshal(output, &result) != nil || result.SchemaVersion != 1 || result.Action != action || result.NodeID != self.NodeID || result.OperationID != r.OperationID || result.PlanDigest != r.BootstrapPlanDigest || result.ProfileDigest != r.ProfileDigest {
		return diagnosticMPIResult{}, errors.New("native MPI response did not match the admitted lease")
	}
	if result.ErrorCode != "" {
		return result, errors.New("native MPI participant could not complete the fixed operation: " + diagnosticPublicMessage(result.ErrorCode))
	}
	if result.CleanupConfirmed && !(result.UnitOwned && result.UnitProcessesGone && result.UnitMetadataRemoved && result.RankCleanupConfirmed && result.AuthorizationRemoved && result.AgentGone && result.PublicArtifactsRemoved) {
		return diagnosticMPIResult{}, errors.New("native MPI cleanup omitted an owned resource")
	}
	return result, nil
}

func writeDiagnosticBootstrapFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return syncManagedFile(path)
}

func (d *diagnosticService) prepareBootstrapParticipant(ctx context.Context, p diagnosticProfile, raw json.RawMessage, r diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
	member, err := d.self(p)
	result := diagnosticParticipantResult{NodeID: member.NodeID}
	if err != nil {
		return result, err
	}
	if _, err = validateDiagnosticMPIBinding(p, raw, r, true); err != nil {
		return result, err
	}
	if runtime.GOOS != "linux" && d.bootstrapTestRun == nil {
		return result, errors.New("managed NCCL participant requires Linux")
	}
	d.m.exec.diagnosticMu.Lock()
	defer d.m.exec.diagnosticMu.Unlock()
	if d.m.exec.fabric.held() {
		return result, errors.New("temporary fabric setup or owned-address cleanup is active")
	}
	d.mu.Lock()
	held := d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed || d.reservation != nil || d.recoveryFailed
	d.mu.Unlock()
	if held {
		return result, errors.New("participant setup, workload, or recovery ownership holds MPI admission")
	}
	if err = d.preflight(ctx, p, member, ""); err != nil {
		return result, err
	}
	dir := d.runDir(r.OperationID)
	unlock, err := diagnosticLock(ctx, dir)
	if err != nil {
		return result, err
	}
	if _, err = os.Lstat(filepath.Join(dir, "cancelled")); !errors.Is(err, os.ErrNotExist) {
		unlock()
		return result, errors.New("rank launch was already closed")
	}
	profileRaw, err := json.Marshal(p)
	if err == nil {
		err = writeDiagnosticBootstrapFile(filepath.Join(dir, "profile.json"), profileRaw)
	}
	if err == nil {
		err = writeDiagnosticBootstrapFile(filepath.Join(dir, "bootstrap-plan.json"), raw)
	}
	lease := diagnosticLeaseRecord{Request: r, Member: member}
	if err == nil {
		err = writeJSONAtomic(filepath.Join(dir, "lease.json"), lease)
	}
	if err == nil {
		err = syncManagedFile(filepath.Join(dir, "lease.json"))
	}
	unlock()
	if err != nil {
		return result, err
	}
	d.mu.Lock()
	d.reservation = &lease
	d.mu.Unlock()
	go d.watchParticipant(p, lease)
	if member.NodeID != p.OwnerNodeID {
		var native diagnosticMPIResult
		native, err = d.mpiNative(ctx, "prepare-peer", p, raw, r, nil)
		if err == nil && native.State != "prepared" {
			err = errors.New("native MPI peer did not confirm preparation")
		}
	}
	result.Prepared = err == nil
	return result, err
}

func (d *diagnosticService) loadBootstrapOperation(r diagnosticParticipantRequest) (diagnosticProfile, json.RawMessage, error) {
	p, err := loadDiagnosticBootstrapProfile(d.m.exec.baseDir, r.GroupID, r.OperationID, r.ProfileDigest)
	if err != nil {
		return p, nil, err
	}
	raw, err := readOnboardingFile(filepath.Join(d.runDir(r.OperationID), "bootstrap-plan.json"), 96<<10)
	if err != nil {
		return p, nil, err
	}
	_, err = validateDiagnosticMPIBinding(p, raw, r, false)
	return p, raw, err
}

func (d *diagnosticService) runBootstrap(ctx context.Context, p diagnosticProfile, r diagnosticParticipantRequest, privateKey []byte) ([]diagnosticSample, error) {
	defer clear(privateKey)
	stored, raw, err := d.loadBootstrapOperation(r)
	if err != nil {
		return nil, err
	}
	if profileDigest(stored) != profileDigest(p) {
		return nil, errors.New("managed MPI profile changed before launch")
	}
	self, err := d.self(p)
	if err != nil || self.NodeID != p.OwnerNodeID {
		return nil, errors.New("only the admitted coordinator may launch MPI")
	}
	result, err := d.mpiNative(ctx, "start-coordinator", p, raw, r, privateKey)
	if err != nil {
		return nil, err
	}
	if result.State != "completed" || result.ExitCode == nil || *result.ExitCode != 0 {
		return nil, errors.New("native MPI coordinator did not complete the fixed collective")
	}
	var plan diagnosticMPIHeader
	if json.Unmarshal(raw, &plan) != nil {
		return nil, errors.New("managed MPI result lost its original recipe")
	}
	return parseDiagnosticMPIOutput(result.Output, result.Stderr, plan.RecipeID)
}

func (d *diagnosticService) cleanupBootstrapParticipant(ctx context.Context, r diagnosticParticipantRequest) (bool, error) {
	p, raw, err := d.loadBootstrapOperation(r)
	if err != nil {
		return false, err
	}
	// Native cleanup begins with the same durable fence as cancel. Calling
	// cancel first only duplicates unit inspection and prevents cleanup from
	// consuming a retained unitCollected proof after system-tool updates.
	result, nativeErr := d.mpiNative(ctx, "cleanup", p, raw, r, nil)
	// The Go fence and exact PID/starttime check remain authoritative even if
	// the native bootstrap reports that its own unit and SSH artifacts are gone.
	rankClean, rankErr := diagnosticCancelRank(ctx, d.runDir(r.OperationID))
	return nativeErr == nil && rankErr == nil && result.CleanupConfirmed && rankClean, errors.Join(nativeErr, rankErr)
}

type diagnosticCoordinatorBootstrap struct {
	Owner   string                       `json:"owner"`
	Request diagnosticParticipantRequest `json:"request"`
	Profile diagnosticProfile            `json:"profile"`
	Plan    json.RawMessage              `json:"plan"`
}

func (d *diagnosticService) coordinatorBootstrapPath(id string) string {
	return d.operationPath(id) + ".bootstrap"
}

func (d *diagnosticService) loadCoordinatorBootstrap(id string) (diagnosticProfile, json.RawMessage, diagnosticParticipantRequest, error) {
	var saved diagnosticCoordinatorBootstrap
	if !onboardingID.MatchString(id) {
		return saved.Profile, nil, saved.Request, errors.New("invalid managed MPI operation selector")
	}
	filename := d.coordinatorBootstrapPath(id)
	if _, err := os.Lstat(filename); err != nil {
		return saved.Profile, nil, saved.Request, err
	}
	raw, err := readOnboardingFile(filename, 128<<10)
	if err != nil {
		return saved.Profile, nil, saved.Request, err
	}
	if strictDiagnosticJSON(raw, &saved) != nil || saved.Owner != "pair-mpi-coordinator-v1" || saved.Request.OperationID != id {
		return diagnosticProfile{}, nil, diagnosticParticipantRequest{}, errors.New("original managed MPI coordinator snapshot is invalid")
	}
	_, err = validateDiagnosticMPIBinding(saved.Profile, saved.Plan, saved.Request, false)
	return saved.Profile, saved.Plan, saved.Request, err
}

// Takes ownership of privateKey on every return path. The immutable public
// recovery snapshot is durable before any remote prepare can acquire resources.
func (d *diagnosticService) startBootstrap(p diagnosticProfile, raw json.RawMessage, privateKey []byte) (diagnosticOperation, error) {
	transferred := false
	defer func() {
		if !transferred {
			clear(privateKey)
		}
	}()
	var plan diagnosticMPIHeader
	if json.Unmarshal(raw, &plan) != nil {
		return diagnosticOperation{}, errors.New("invalid managed MPI plan")
	}
	deadline := min(time.Now().Add(diagnosticDeadline).UnixMilli(), plan.ExpiresAt)
	if parent, ok := d.ctx.Deadline(); ok {
		deadline = min(deadline, parent.UnixMilli())
	}
	r := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: plan.OperationID, ProfileDigest: profileDigest(p), ExpiresAt: plan.ExpiresAt, BootstrapPlanDigest: plan.PlanDigest, ExecutionDeadlineAt: deadline}
	if _, err := validateDiagnosticMPIBinding(p, raw, r, true); err != nil {
		return diagnosticOperation{}, err
	}
	if len(privateKey) == 0 || len(privateKey) > 16384 {
		return diagnosticOperation{}, errors.New("a volatile operation identity is required")
	}
	d.m.mesh.Refresh()
	self, err := d.self(p)
	if err != nil || self.NodeID != p.OwnerNodeID || !d.m.mesh.Clustered() {
		return diagnosticOperation{}, errors.New("only the current managed MPI coordinator may start")
	}
	for _, member := range p.Members {
		pin, known := d.m.mesh.PinSHA256(member.Principal)
		if !known || pin != member.ClusterPinSHA256 {
			return diagnosticOperation{}, errors.New("managed MPI participant pin changed")
		}
	}
	d.m.exec.diagnosticMu.Lock()
	defer d.m.exec.diagnosticMu.Unlock()
	if d.m.exec.fabric.held() {
		return diagnosticOperation{}, errors.New("temporary fabric setup or owned-address cleanup is active")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed || d.recoveryFailed || d.reservation != nil || len(d.cancels) != 0 {
		return diagnosticOperation{}, errors.New("setup, recovery, or another operation holds managed MPI admission")
	}
	if _, exists := d.operations[r.OperationID]; exists {
		return diagnosticOperation{}, errors.New("managed MPI operation was already consumed")
	}
	snapshot := diagnosticCoordinatorBootstrap{Owner: "pair-mpi-coordinator-v1", Request: r, Profile: p, Plan: raw}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > 128<<10 {
		return diagnosticOperation{}, errors.New("managed MPI ownership exceeds its durable bound")
	}
	if err := os.MkdirAll(filepath.Dir(d.coordinatorBootstrapPath(r.OperationID)), 0700); err != nil {
		return diagnosticOperation{}, err
	}
	if err := writeDiagnosticBootstrapFile(d.coordinatorBootstrapPath(r.OperationID), encoded); err != nil {
		return diagnosticOperation{}, err
	}
	op := diagnosticOperation{OperationID: r.OperationID, GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, Preset: diagnosticPreset, RecipeID: plan.RecipeID, State: "preparing", StartedAt: time.Now().UnixMilli(), Message: "Checking the selected managed NCCL participants and reserving the test window", MemberNodeIDs: p.descriptor().MemberNodeIDs, profileDigest: r.ProfileDigest}
	if err := d.saveOperation(op); err != nil {
		return diagnosticOperation{}, err
	}
	// Persist and propagate the same deadline so native workers reserve result
	// retention time before this context can terminate their control process.
	ctx, cancel := context.WithDeadline(d.ctx, time.UnixMilli(r.ExecutionDeadlineAt))
	d.operations[op.OperationID], d.cancels[op.OperationID] = op, cancel
	transferred = true
	go d.executeBootstrap(ctx, p, raw, r, op, privateKey)
	return op, nil
}

func (d *diagnosticService) retryBootstrapCleanup(ctx context.Context, p diagnosticProfile, op diagnosticOperation) {
	stored, raw, r, loadErr := d.loadCoordinatorBootstrap(op.OperationID)
	clean := loadErr == nil && profileDigest(stored) == profileDigest(p)
	if clean {
		clean = cleanupDiagnosticMPIMembers(ctx, stored.Members, func(ctx context.Context, member diagnosticMember) (diagnosticParticipantResult, error) {
			return d.callBootstrapParticipant(ctx, stored, raw, member, "bootstrap-cancel", r)
		})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	op.State, op.Message = "failed", "The interrupted NCCL test did not pass; owned MPI cleanup is confirmed"
	if !clean {
		op.Message = "Original managed MPI resources remain unconfirmed; recovery is required"
	}
	op.CleanupConfirmed, op.FinishedAt = clean, time.Now().UnixMilli()
	if err := d.saveOperation(op); err != nil {
		op.CleanupConfirmed = false
		op.Message = "Managed MPI cleanup receipt could not be retained"
		clean = false
	}
	if !clean {
		d.recoveryFailed = true
	}
	d.operations[op.OperationID] = op
	if cancel := d.cancels[op.OperationID]; cancel != nil {
		cancel()
		delete(d.cancels, op.OperationID)
	}
}

// Each node gets the same cleanup deadline. Serial waits could consume the
// entire 25-second window before the third participant is asked to stop.
func cleanupDiagnosticMPIMembers(ctx context.Context, members []diagnosticMember, cancel func(context.Context, diagnosticMember) (diagnosticParticipantResult, error)) bool {
	if len(members) < 1 || len(members) > 3 || cancel == nil {
		return false
	}
	seen := map[string]bool{}
	for _, member := range members {
		if member.NodeID == "" || seen[member.NodeID] {
			return false
		}
		seen[member.NodeID] = true
	}
	results := make(chan bool, len(members))
	for _, member := range members {
		go func() {
			result, err := cancel(ctx, member)
			results <- err == nil && result.NodeID == member.NodeID && result.CleanupConfirmed
		}()
	}
	clean := true
	for range members {
		select {
		case confirmed := <-results:
			clean = clean && confirmed
		case <-ctx.Done():
			return false
		}
	}
	return clean && ctx.Err() == nil
}

func (d *diagnosticService) callBootstrapParticipant(ctx context.Context, p diagnosticProfile, raw json.RawMessage, member diagnosticMember, action string, r diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
	if action != "bootstrap-prepare" && action != "bootstrap-cancel" {
		return diagnosticParticipantResult{}, errors.New("unsupported managed MPI participant action")
	}
	if _, err := validateDiagnosticMPIBinding(p, raw, r, action == "bootstrap-prepare"); err != nil {
		return diagnosticParticipantResult{}, err
	}
	d.m.mesh.Refresh()
	owner, known := p.member(p.OwnerNodeID)
	if !known || owner.Principal != d.m.mesh.NodeUUID() || !d.m.mesh.Clustered() || !d.m.mesh.HasPin(member.Principal) {
		return diagnosticParticipantResult{}, errors.New("managed MPI coordinator or participant admission changed")
	}
	if action == "bootstrap-prepare" {
		pin, ok := d.m.mesh.PinSHA256(member.Principal)
		ownerPin, ownerOK := d.m.mesh.PinSHA256(owner.Principal)
		if !ok || !ownerOK || pin != member.ClusterPinSHA256 || ownerPin != owner.ClusterPinSHA256 {
			return diagnosticParticipantResult{}, errors.New("reviewed MPI certificate generation changed")
		}
	}
	if member.Principal == d.m.mesh.NodeUUID() {
		if action == "bootstrap-prepare" {
			return d.prepareBootstrapParticipant(ctx, p, raw, r)
		}
		clean, err := d.cancelParticipant(ctx, r)
		return diagnosticParticipantResult{NodeID: member.NodeID, CleanupConfirmed: clean}, err
	}
	client, err := d.client(ctx, member)
	if err != nil {
		return diagnosticParticipantResult{}, err
	}
	output, err := client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{
		Method: action, Request: diagnosticRequest{GroupID: p.GroupID}, Participant: &r, Bootstrap: &diagnosticBootstrapControl{Profile: p, Plan: raw},
	})
	if err != nil {
		return diagnosticParticipantResult{}, err
	}
	var result diagnosticParticipantResult
	if strictDiagnosticJSON(output, &result) != nil || result.NodeID != member.NodeID {
		return diagnosticParticipantResult{}, errors.New("managed MPI participant reply changed identity")
	}
	return result, nil
}

// Called only after the managed registry and current participant observations
// have produced the fixed public plan. The private identity is never retained.
func (d *diagnosticService) executeBootstrap(ctx context.Context, p diagnosticProfile, raw json.RawMessage, r diagnosticParticipantRequest, op diagnosticOperation, privateKey []byte) {
	defer clear(privateKey)
	attempted := []diagnosticMember{}
	var runErr error
	var samples []diagnosticSample
	for _, member := range p.Members {
		attempted = append(attempted, member)
		result, err := d.callBootstrapParticipant(ctx, p, raw, member, "bootstrap-prepare", r)
		if err != nil || !result.Prepared {
			runErr = errors.Join(err, errors.New("managed MPI participant did not confirm preparation"))
			break
		}
	}
	if runErr == nil {
		d.mu.Lock()
		current := d.operations[op.OperationID]
		if current.State != "cancelling" {
			current.State, current.Message = "running", "Running the bounded socket NCCL test on the selected participant interfaces"
			d.operations[op.OperationID] = current
		}
		d.mu.Unlock()
		samples, runErr = d.runBootstrap(ctx, p, r, privateKey)
	}
	cleanupCtx, stop := context.WithTimeout(context.Background(), 25*time.Second)
	defer stop()
	clean := cleanupDiagnosticMPIMembers(cleanupCtx, attempted, func(ctx context.Context, member diagnosticMember) (diagnosticParticipantResult, error) {
		return d.callBootstrapParticipant(ctx, p, raw, member, "bootstrap-cancel", r)
	})
	if !clean {
		runErr = errors.Join(runErr, errors.New("managed MPI cleanup remains unconfirmed for an original participant"))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cancelled := d.operations[op.OperationID].State == "cancelling"
	op.State, op.Message = "passed", "Bounded NCCL correctness and communications test passed; owned MPI resources cleaned up"
	if op.RecipeID == diagnosticMPIQuickRecipe || op.RecipeID == diagnosticMPITripleRecipe {
		op.Message = "Small NCCL correctness smoke passed for 14 sizes from 8 B to 64 KiB in both modes; owned MPI resources cleaned up. Throughput qualification is separate."
	}
	if runErr != nil {
		op.State, op.Message = "failed", diagnosticPublicMessage(runErr.Error())
	}
	if cancelled && clean {
		op.State, op.Message = "cancelled", "NCCL test cancelled; owned MPI resources cleaned up"
	}
	op.CleanupConfirmed, op.FinishedAt, op.Samples = clean, time.Now().UnixMilli(), samples
	if !clean {
		d.recoveryFailed = true
	}
	if err := d.saveOperation(op); err != nil {
		op.State, op.Message = "failed", "Managed MPI result could not be retained"
		d.recoveryFailed = true
	}
	d.operations[op.OperationID] = op
	if cancel := d.cancels[op.OperationID]; cancel != nil {
		cancel()
		delete(d.cancels, op.OperationID)
	}
}
