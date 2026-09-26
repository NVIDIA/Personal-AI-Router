// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const diagnosticDeadline = 90 * time.Second
const diagnosticLease = 120 * time.Second

type diagnosticRequest struct {
	GroupID     string `json:"groupId"`
	Preset      string `json:"preset,omitempty"`
	OperationID string `json:"operationId,omitempty"`
}

type diagnosticParticipantRequest struct {
	GroupID             string `json:"groupId"`
	OperationID         string `json:"operationId"`
	ProfileDigest       string `json:"profileDigest"`
	ExpiresAt           int64  `json:"expiresAt"`
	BootstrapPlanDigest string `json:"bootstrapPlanDigest,omitempty"`
	ExecutionDeadlineAt int64  `json:"executionDeadlineAt,omitempty"`
}

type diagnosticLeaseRecord struct {
	Request diagnosticParticipantRequest `json:"request"`
	Member  diagnosticMember             `json:"member"`
}

type diagnosticParticipantResult struct {
	ExecutionError    string `json:"executionError,omitempty"`
	ActiveOperationID string `json:"activeOperationId,omitempty"`
	NodeID            string `json:"nodeId"`
	Prepared          bool   `json:"prepared"`
	CleanupConfirmed  bool   `json:"cleanupConfirmed"`
	Message           string `json:"message,omitempty"`
}

type diagnosticService struct {
	packageReviews         map[string]diagnosticPackageBinding
	packageRuns            map[string]*diagnosticPackageRun
	packageActive          string
	packageRecoveryFailed  bool
	packageAdmissionClosed bool
	packageTestRun         func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error)
	runtimeReviews         map[string]diagnosticRuntimeBinding
	runtimeRuns            map[string]*diagnosticRuntimeRun
	runtimeActive          string
	runtimeRecoveryFailed  bool
	runtimeTestRun         func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error)
	bootstrapTestRun       func(context.Context, io.Reader) ([]byte, error)
	// Replaces only exact participant cleanup in socket-free reconciliation tests.
	reconcileCancel func(context.Context, diagnosticParticipantRequest) (bool, error)
	mpiReviews      map[string]*diagnosticMPIReviewBinding
	m               *Manager
	mu              sync.Mutex
	operations      map[string]diagnosticOperation
	cancels         map[string]context.CancelFunc
	reservation     *diagnosticLeaseRecord
	workloadWait    map[string]chan bool
	// These seams replace process I/O in socket-free tests, never protocol truth.
	profiles       func() ([]diagnosticProfile, error)
	preflight      func(context.Context, diagnosticProfile, diagnosticMember, string) error
	run            func(context.Context, diagnosticProfile, string) ([]diagnosticSample, error)
	participant    func(context.Context, diagnosticProfile, diagnosticMember, string, diagnosticParticipantRequest) (diagnosticParticipantResult, error)
	ctx            context.Context
	recoveryFailed bool
}

func newDiagnosticService(m *Manager) *diagnosticService {
	d := &diagnosticService{m: m, operations: map[string]diagnosticOperation{}, cancels: map[string]context.CancelFunc{}, workloadWait: map[string]chan bool{}, ctx: context.Background()}
	d.packageReviews = map[string]diagnosticPackageBinding{}
	d.packageRuns = map[string]*diagnosticPackageRun{}
	d.runtimeReviews = map[string]diagnosticRuntimeBinding{}
	d.runtimeRuns = map[string]*diagnosticRuntimeRun{}
	d.loadPackageRuns()
	d.loadRuntimeRuns()
	d.profiles = func() ([]diagnosticProfile, error) { return loadDiagnosticProfiles(m.exec.baseDir) }
	d.preflight = d.nativePreflight
	d.run = d.nativeRun
	d.participant = d.callParticipant
	return d
}

func (d *diagnosticService) profile(id string) (diagnosticProfile, error) {
	profiles, err := d.profiles()
	if err != nil {
		return diagnosticProfile{}, err
	}
	for _, p := range profiles {
		if p.GroupID == id {
			return p, nil
		}
	}
	return diagnosticProfile{}, errors.New("diagnostic group is not configured on this node")
}

func (d *diagnosticService) self(p diagnosticProfile) (diagnosticMember, error) {
	principal := d.m.mesh.NodeUUID()
	for _, member := range p.Members {
		if member.Principal == principal {
			return member, nil
		}
	}
	return diagnosticMember{}, errors.New("this admitted identity is not a configured participant")
}

func (d *diagnosticService) groups(ctx context.Context) (any, error) {
	profiles, err := d.profiles()
	if err != nil {
		return nil, err
	}
	groups := []diagnosticGroup{}
	for _, p := range profiles {
		g := p.descriptor()
		if err := p.validate(); err != nil {
			g.Reason = diagnosticPublicMessage(err.Error())
		} else {
			owner, _ := p.member(p.OwnerNodeID)
			result, err := d.participant(ctx, p, owner, "capability", diagnosticParticipantRequest{GroupID: p.GroupID, ProfileDigest: profileDigest(p)})
			g.ActiveOperationID = result.ActiveOperationID
			if err == nil {
				for _, member := range p.Members {
					if member.NodeID != owner.NodeID {
						_, err = d.participant(ctx, p, member, "capability", diagnosticParticipantRequest{GroupID: p.GroupID, ProfileDigest: profileDigest(p)})
						if err != nil {
							break
						}
					}
				}
			}
			g.Available = err == nil
			if err != nil {
				g.Reason = diagnosticPublicMessage(err.Error())
			}
		}
		groups = append(groups, g)
	}
	return map[string]any{"groups": groups}, nil
}

func (d *diagnosticService) operationProfile(method string, request diagnosticRequest) (diagnosticProfile, error) {
	if (method == "engine:diagnostic-status" || method == "engine:diagnostic-cancel") && diagnosticDigest.MatchString(request.OperationID+request.OperationID) {
		_, statErr := os.Lstat(d.coordinatorBootstrapPath(request.OperationID))
		if statErr == nil {
			p, _, _, err := d.loadCoordinatorBootstrap(request.OperationID)
			if err != nil {
				return diagnosticProfile{}, errors.New("retained managed MPI operation is malformed; recovery is required")
			}
			if p.GroupID != request.GroupID {
				return diagnosticProfile{}, errors.New("managed MPI operation belongs to a different group")
			}
			return p, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return diagnosticProfile{}, errors.New("retained managed MPI operation is unavailable; recovery is required")
		}
	}
	return d.profile(request.GroupID)
}

func (d *diagnosticService) dispatch(ctx context.Context, method string, request diagnosticRequest) (any, error) {
	if method == "engine:diagnostic-groups" {
		return d.groups(ctx)
	}
	if method == "engine:diagnostic-start" && strings.HasPrefix(request.GroupID, "pair-recipe/") {
		return nil, errors.New("PAIR recipe inspection is not an admitted diagnostic execution profile")
	}
	p, err := d.operationProfile(method, request)
	if err != nil {
		return nil, err
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	owner, _ := p.member(p.OwnerNodeID)
	if owner.Principal != d.m.mesh.NodeUUID() {
		return d.remoteOperation(ctx, p, owner, method, request)
	}
	if method == "engine:diagnostic-start" {
		if request.Preset != diagnosticPreset || request.OperationID != "" {
			return nil, errors.New("only the fixed nccl-smoke preset is supported")
		}
		return d.start(p)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	op, ok := d.operations[request.OperationID]
	if !ok && diagnosticDigest.MatchString(request.OperationID+request.OperationID) {
		if readDiagnosticJSON(d.operationPath(request.OperationID), &op) == nil && op.OperationID == request.OperationID {
			ok = true
		}
	}
	if !ok || op.GroupID != p.GroupID {
		return nil, errors.New("diagnostic operation is unknown on its configured owner")
	}
	if method == "engine:diagnostic-cancel" {
		if cancel := d.cancels[op.OperationID]; cancel != nil {
			op.State, op.Message = "cancelling", "Cancelling owned ranks and confirming cleanup"
			d.operations[op.OperationID] = op
			cancel()
		} else if !op.CleanupConfirmed {
			if p.Bootstrap.OperationID == "" {
				pin, err := os.Open(d.operationPath(op.OperationID) + ".profile")
				if err != nil {
					return nil, errors.New("original diagnostic profile binding is unavailable; cleanup requires operator recovery")
				}
				digest, readErr := io.ReadAll(io.LimitReader(pin, 65))
				_ = pin.Close()
				if readErr != nil || string(digest) != profileDigest(p) {
					return nil, errors.New("configured diagnostic profile changed; do not clean a different participant group")
				}
			}
			op.State, op.Message = "cancelling", "Rechecking the original participants and confirming owned rank cleanup"
			d.operations[op.OperationID] = op
			cleanupCtx, cancel := context.WithTimeout(d.ctx, 20*time.Second)
			d.cancels[op.OperationID] = cancel
			go d.retryCleanup(cleanupCtx, p, op)
		}
	} else if method != "engine:diagnostic-status" {
		return nil, errors.New("unknown diagnostic operation")
	}
	return op, nil
}

func (d *diagnosticService) start(p diagnosticProfile) (diagnosticOperation, error) {
	d.m.exec.diagnosticMu.Lock()
	defer d.m.exec.diagnosticMu.Unlock()
	if d.m.exec.fabric != nil && d.m.exec.fabric.held() {
		return diagnosticOperation{}, errors.New("temporary fabric setup or owned-address cleanup is active")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.packageAdmissionClosed || d.ctx.Err() != nil || d.packageActive != "" || d.runtimeActive != "" || d.packageRecoveryFailed || d.runtimeRecoveryFailed {
		return diagnosticOperation{}, errors.New("setup ownership, recovery or shutdown holds diagnostic admission")
	}
	if len(d.cancels) != 0 {
		return diagnosticOperation{}, errors.New("a diagnostic operation is already active on this coordinator")
	}
	if len(d.operations) >= 32 {
		for id := range d.operations {
			delete(d.operations, id)
			break
		}
	}
	op := diagnosticOperation{OperationID: newOpID(), GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, Preset: diagnosticPreset, State: "preparing", StartedAt: time.Now().UnixMilli(), Message: "Checking configured participants and reserving the test window", MemberNodeIDs: p.descriptor().MemberNodeIDs}
	op.profileDigest = profileDigest(p)
	if err := d.saveOperation(op); err != nil {
		return diagnosticOperation{}, fmt.Errorf("cannot retain diagnostic operation before starting: %w", err)
	}
	ctx, cancel := context.WithTimeout(d.ctx, diagnosticDeadline)
	d.operations[op.OperationID], d.cancels[op.OperationID] = op, cancel
	go d.execute(ctx, p, op)
	return op, nil
}

func (d *diagnosticService) execute(ctx context.Context, p diagnosticProfile, op diagnosticOperation) {
	request := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: op.OperationID, ProfileDigest: profileDigest(p), ExpiresAt: time.Now().Add(diagnosticLease).UnixMilli()}
	// Include uncertain prepare acknowledgements in cleanup: a timed-out reply
	// does not prove that the remote participant never reserved or started.
	attempted := []diagnosticMember{}
	var runErr error
	var samples []diagnosticSample
	for _, member := range p.Members {
		attempted = append(attempted, member)
		var result diagnosticParticipantResult
		result, runErr = d.participant(ctx, p, member, "prepare", request)
		if runErr == nil && (!result.Prepared || result.NodeID != member.NodeID) {
			runErr = errors.New("participant did not confirm preparation for its configured identity")
		}
		if runErr != nil {
			break
		}
	}
	if runErr == nil {
		d.mu.Lock()
		current := d.operations[op.OperationID]
		if current.State != "cancelling" {
			current.State, current.Message = "running", "Running the bounded NCCL correctness and communications test"
			d.operations[op.OperationID] = current
		}
		d.mu.Unlock()
		samples, runErr = d.run(ctx, p, op.OperationID)
		if runErr == nil {
			if len(samples) != 21 {
				runErr = errors.New("NCCL runner did not return the complete fixed-preset results")
			}
			for _, sample := range samples {
				if sample.Wrong != 0 {
					runErr = errors.New("NCCL correctness check failed")
				}
			}
		}
	}
	clean := true
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cleanupCancel()
	for _, member := range attempted {
		result, err := d.participant(cleanupCtx, p, member, "cancel", request)
		if err != nil || result.NodeID != member.NodeID || !result.CleanupConfirmed {
			clean = false
			runErr = errors.Join(runErr, fmt.Errorf("owned rank cleanup was not confirmed for %s: %v", member.NodeID, err))
		}
		if result.ExecutionError != "" {
			runErr = errors.Join(runErr, fmt.Errorf("rank on %s: %s", member.NodeID, result.ExecutionError))
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cancelled := d.operations[op.OperationID].State == "cancelling"
	op.State, op.Message = "passed", "NCCL correctness and communications test passed; owned ranks cleaned up"
	if runErr != nil {
		op.State, op.Message = "failed", runErr.Error()
	}
	if cancelled && clean {
		op.State, op.Message = "cancelled", "Diagnostic cancelled; owned ranks cleaned up"
	}
	op.Message = diagnosticPublicMessage(op.Message)
	op.CleanupConfirmed, op.FinishedAt, op.Samples = clean, time.Now().UnixMilli(), samples
	if err := d.saveOperation(op); err != nil {
		op.State = "failed"
		op.Message = diagnosticPublicMessage(op.Message + "; operation receipt could not be saved: " + err.Error())
	}
	d.operations[op.OperationID] = op
	if cancel := d.cancels[op.OperationID]; cancel != nil {
		cancel()
		delete(d.cancels, op.OperationID)
	}
}

func (d *diagnosticService) reserved() bool {
	if d.m.exec.fabric != nil && d.m.exec.fabric.held() {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reservation != nil || d.recoveryFailed // Expiry releases only after rank cleanup is confirmed.
}

func (d *diagnosticService) localParticipant(ctx context.Context, p diagnosticProfile, action string, r diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
	member, err := d.self(p)
	if err != nil {
		return diagnosticParticipantResult{}, err
	}
	result := diagnosticParticipantResult{NodeID: member.NodeID}
	if profileDigest(p) != r.ProfileDigest {
		return result, errors.New("participant diagnostic profile does not match coordinator")
	}
	if action == "capability" {
		d.mu.Lock()
		for id := range d.cancels {
			if d.operations[id].GroupID == p.GroupID {
				result.ActiveOperationID = id
				break
			}
		}
		d.mu.Unlock()
		if result.ActiveOperationID != "" {
			return result, nil
		}
		return result, d.preflight(ctx, p, member, "")
	}
	if !diagnosticDigest.MatchString(r.OperationID + r.OperationID) {
		return result, errors.New("invalid diagnostic operation ID")
	}
	if action == "cancel" {
		result.CleanupConfirmed, err = d.cancelParticipant(ctx, r)
		if result.CleanupConfirmed {
			var rank diagnosticRankRecord
			if readDiagnosticJSON(filepath.Join(d.runDir(r.OperationID), "rank.json"), &rank) == nil {
				result.ExecutionError = rank.Error
			}
		}
		return result, err
	}
	if action != "prepare" {
		return result, errors.New("unknown participant diagnostic action")
	}
	if runtime.GOOS != "linux" {
		return result, errors.New("NCCL rank execution is supported only on Linux")
	}
	if remaining := time.Until(time.UnixMilli(r.ExpiresAt)); remaining <= 0 || remaining > diagnosticLease+time.Second {
		return result, errors.New("invalid or expired diagnostic reservation")
	}
	d.m.exec.diagnosticMu.Lock()
	defer d.m.exec.diagnosticMu.Unlock()
	if d.reserved() {
		return result, errors.New("participant already has a diagnostic reservation")
	}
	if err := d.preflight(ctx, p, member, ""); err != nil {
		return result, err
	}
	lease := diagnosticLeaseRecord{Request: r, Member: member}
	dir := d.runDir(r.OperationID)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return result, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return result, err
	}
	if err := writeJSONAtomic(filepath.Join(dir, "lease.json"), lease); err != nil {
		return result, err
	}
	d.mu.Lock()
	d.reservation = &lease
	d.mu.Unlock()
	result.Prepared = true
	go d.watchParticipant(p, lease)
	return result, nil
}

func (d *diagnosticService) runDir(id string) string {
	return filepath.Join(d.m.exec.baseDir, "diagnostic-runs", id)
}

func (d *diagnosticService) watchParticipant(p diagnosticProfile, lease diagnosticLeaseRecord) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-d.ctx.Done():
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := d.preflight(ctx, p, lease.Member, lease.Request.OperationID)
		cancel()
		if err != nil || time.Now().UnixMilli() >= lease.Request.ExpiresAt || d.ctx.Err() != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = d.cancelParticipant(ctx, lease.Request)
			cancel()
			return
		}
		d.mu.Lock()
		live := d.reservation != nil && d.reservation.Request.OperationID == lease.Request.OperationID
		d.mu.Unlock()
		if !live {
			return
		}
	}
}

func (d *diagnosticService) shutdown() {
	d.mu.Lock()
	d.closePackageAdmissionLocked()
	for _, cancel := range d.cancels {
		cancel()
	}
	lease := d.reservation
	d.mu.Unlock()
	if lease != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		_, _ = d.cancelParticipant(ctx, lease.Request)
	}
}

// A restarted owner may not forget an independently running MPI rank. Recover
// only its own bounded lease directory and cancel old ranks before announcing
// readiness; unknown cleanup keeps managed starts fenced.
func (d *diagnosticService) recover(ctx context.Context) {
	defer d.recoverOperations()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nativeRoot, nativeErr := diagnosticNativeBootstrapRoot()
	if nativeErr != nil || d.checkBootstrapRoots(ctx, nativeRoot) != nil {
		d.recoveryFailed = true
	}
	directory, err := os.Open(filepath.Join(d.m.exec.baseDir, "diagnostic-runs"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		d.recoveryFailed = true
		return
	}
	defer directory.Close()
	for {
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			d.recoveryFailed = true
			return
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				d.recoveryFailed = true
				return
			}
			if !entry.IsDir() || !diagnosticDigest.MatchString(entry.Name()+entry.Name()) {
				continue
			}
			var lease diagnosticLeaseRecord
			dir := d.runDir(entry.Name())
			var rank diagnosticRankRecord
			rankErr := readDiagnosticJSON(filepath.Join(dir, "rank.json"), &rank)
			if leaseErr := readDiagnosticJSON(filepath.Join(dir, "lease.json"), &lease); leaseErr != nil {
				// A cancelled prepare that never acquired a lease has no rank to
				// forget. Any other missing/corrupt lease is unknown, not idle.
				_, cancelErr := os.Stat(filepath.Join(dir, "cancelled"))
				present, artifactErr := d.bootstrapArtifactsPresent(entry.Name())
				if !errors.Is(leaseErr, os.ErrNotExist) || !errors.Is(rankErr, os.ErrNotExist) || cancelErr != nil || present || artifactErr != nil {
					d.recoveryFailed = true
				}
				continue
			}
			if lease.Request.BootstrapPlanDigest == "" {
				present, artifactErr := d.bootstrapArtifactsPresent(entry.Name())
				if present || artifactErr != nil {
					d.recoveryFailed = true
					continue
				}
			}
			if lease.Request.BootstrapPlanDigest == "" && rankErr == nil && rank.Done && rank.Clean {
				if time.Now().UnixMilli() > lease.Request.ExpiresAt+60000 {
					_ = pruneDiagnosticRun(dir)
				}
				continue
			}
			if lease.Request.OperationID != entry.Name() {
				d.recoveryFailed = true
				continue
			}
			cleanupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			clean, _ := d.cancelParticipant(cleanupCtx, lease.Request)
			cancel()
			if !clean {
				d.recoveryFailed = true
			}
		}
		if readErr == io.EOF {
			return
		}
	}
}

func (d *diagnosticService) operationPath(id string) string {
	return filepath.Join(d.m.exec.baseDir, "diagnostic-operations", id+".json")
}

func (d *diagnosticService) saveOperation(op diagnosticOperation) error {
	if err := os.MkdirAll(filepath.Dir(d.operationPath(op.OperationID)), 0700); err != nil {
		return err
	}
	if op.profileDigest != "" {
		if err := os.WriteFile(d.operationPath(op.OperationID)+".profile", []byte(op.profileDigest), 0600); err != nil {
			return err
		}
	}
	return writeJSONAtomic(d.operationPath(op.OperationID), op)
}

func (d *diagnosticService) retryCleanup(ctx context.Context, p diagnosticProfile, op diagnosticOperation) {
	if p.Bootstrap.OperationID != "" {
		d.retryBootstrapCleanup(ctx, p, op)
		return
	}
	request := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: op.OperationID, ProfileDigest: profileDigest(p)}
	clean := true
	var cleanupErr error
	for _, member := range p.Members {
		result, err := d.participant(ctx, p, member, "cancel", request)
		if err != nil || result.NodeID != member.NodeID || !result.CleanupConfirmed {
			clean = false
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup is not confirmed for %s: %v", member.NodeID, err))
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	op.State, op.Message = "failed", "The interrupted collective did not pass; owned rank cleanup is now confirmed"
	if !clean {
		op.Message = diagnosticPublicMessage(cleanupErr.Error())
	}
	op.CleanupConfirmed, op.FinishedAt = clean, time.Now().UnixMilli()
	if err := d.saveOperation(op); err != nil {
		op.Message = diagnosticPublicMessage(op.Message + "; receipt save failed: " + err.Error())
	}
	d.operations[op.OperationID] = op
	if cancel := d.cancels[op.OperationID]; cancel != nil {
		cancel()
		delete(d.cancels, op.OperationID)
	}
}

// A coordinator restart is not a successful collective and local rank recovery
// cannot prove remote cleanup. Retain that failed terminal result so a desktop
// holding the prior operation ID does not remain permanently "running".
func (d *diagnosticService) recoverOperations() {
	directory, err := os.Open(filepath.Join(d.m.exec.baseDir, "diagnostic-operations"))
	if err != nil {
		return
	}
	defer directory.Close()
	for {
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || len(name) != 37 || name[32:] != ".json" || !diagnosticDigest.MatchString(name[:32]+name[:32]) {
				continue
			}
			var op diagnosticOperation
			if readDiagnosticJSON(d.operationPath(name[:32]), &op) != nil || op.OperationID != name[:32] {
				continue
			}
			if op.State != "passed" && op.State != "failed" && op.State != "cancelled" {
				op.State = "failed"
				op.FinishedAt = time.Now().UnixMilli()
				op.CleanupConfirmed = false
				op.Message = "Coordinator restarted before the test completed; remote rank cleanup is not confirmed. Participant leases expire independently."
				_ = d.saveOperation(op)
			}
			if len(d.operations) < 32 {
				d.operations[op.OperationID] = op
			}
		}
		if readErr == io.EOF {
			return
		}
	}
}

// Remove only the fixed artifacts of a validated, expired, clean operation.
// Unknown files keep the directory in place; no recursive deletion is used.
func pruneDiagnosticRun(dir string) error {
	for _, name := range []string{"lease.json", "rank.json", "cancelled", "lock", "mpi.app"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Remove(dir)
}

func decodeDiagnosticRequest(raw json.RawMessage) (diagnosticRequest, error) {
	var r diagnosticRequest
	err := strictDiagnosticJSON(raw, &r)
	return r, err
}
