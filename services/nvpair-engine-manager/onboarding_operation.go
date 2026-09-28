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
	"reflect"
	"strings"
	"time"
)

type onboardingPlan struct {
	AccessGeneration        string                          `json:"-"`
	Candidate               onboardingCandidate             `json:"candidate"`
	Review                  onboardingReviewTarget          `json:"review"`
	Info                    onboardingPlatformInfo          `json:"info"`
	Artifact                onboardingArtifactSource        `json:"artifact"`
	PackageFile             string                          `json:"packageFile"`
	ArchiveRoot             string                          `json:"archiveRoot"`
	ArchiveBytes            int64                           `json:"archiveBytes"`
	Username                string                          `json:"username"`
	AuthKind                string                          `json:"authKind"`
	Receipt                 onboardingInstallReceipt        `json:"receipt"`
	InviteID                string                          `json:"inviteId,omitempty"`
	InviteRequestKey        string                          `json:"inviteRequestKey,omitempty"`
	InviteExpectedClusterID string                          `json:"inviteExpectedClusterId"`
	InviteNodeID            string                          `json:"inviteNodeId,omitempty"`
	InviteHistory           []onboardingSettledInvite       `json:"inviteHistory,omitempty"`
	LingerChanged           bool                            `json:"lingerChanged"`
	LingerRequested         bool                            `json:"lingerRequested"`
	ExistingInstallation    *onboardingExistingInstallation `json:"existingInstallation,omitempty"`
	UpgradePhase            string                          `json:"upgradePhase,omitempty"`
}
type onboardingRun struct {
	targetCancels    map[string]context.CancelFunc
	Public           onboardingOperation       `json:"operation"`
	ControllerNodeID string                    `json:"controllerNodeId"`
	Deadline         int64                     `json:"deadline"`
	Plans            map[string]onboardingPlan `json:"plans"`
	cancel           context.CancelFunc
	cancelTarget     map[string]bool
	historyOnly      bool
}

func (s *onboardingService) operationPath(id string) string {
	return filepath.Join(s.m.exec.baseDir, "onboarding-operations", id+".json")
}
func (s *onboardingService) saveRun(run *onboardingRun) error {
	if s.testSave != nil {
		return s.testSave(run)
	}
	if err := os.MkdirAll(filepath.Dir(s.operationPath(run.Public.OperationID)), 0700); err != nil {
		return err
	}
	return writeJSONAtomic(s.operationPath(run.Public.OperationID), run)
}
func cloneOnboardingOperation(op onboardingOperation) onboardingOperation {
	op.Targets = append([]onboardingTargetState(nil), op.Targets...)
	return op
}
func (s *onboardingService) loadOperations() {
	dir, err := os.Open(filepath.Join(s.m.exec.baseDir, "onboarding-operations"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.recoveryRequired = true
		return
	}
	defer dir.Close()
	count := 0
	for {
		entries, readErr := dir.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			s.recoveryRequired = true
			return
		}
		for _, entry := range entries {
			count++
			if count > 1024 {
				s.recoveryRequired = true
				return
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if entry.IsDir() || !onboardingID.MatchString(id) {
				continue
			}
			data, e := readOnboardingFile(s.operationPath(id), 256<<10)
			if e != nil {
				s.recoveryRequired = true
				continue
			}
			var run onboardingRun
			if json.Unmarshal(data, &run) != nil || run.Public.OperationID != id || len(run.Public.Targets) < 1 || len(run.Public.Targets) > 4 || len(run.Plans) != len(run.Public.Targets) || run.Public.Revision < 1 {
				s.recoveryRequired = true
				continue
			}
			for _, target := range run.Public.Targets {
				plan, ok := run.Plans[target.CandidateID]
				upgradeValid := ok && validOnboardingUpgradePlan(plan)
				legacyHistory := ok && !upgradeValid && validHistoricalOnboardingUpgradePlan(run.Public, target, plan)
				if !ok || plan.Candidate.CandidateID != target.CandidateID || plan.Candidate.Address == "" || plan.Info.UID <= 0 || !validOnboardingInvitationPlan(plan) || !upgradeValid && !legacyHistory {
					s.recoveryRequired = true
				}
				run.historyOnly = run.historyOnly || legacyHistory
			}
			run.cancelTarget = map[string]bool{}
			run.targetCancels = map[string]context.CancelFunc{}
			if run.Public.State == "running" {
				run.Public.State = "interrupted"
				run.Public.Revision++
				for i := range run.Public.Targets {
					t := &run.Public.Targets[i]
					if t.Stage != "paired" {
						t.Stage = "verification-failed"
						t.Message = "PAIR restarted; authorize access again and retry the same owned operation"
						t.CanRetry = true
						t.CanCancel = true
						if phase := run.Plans[t.CandidateID].UpgradePhase; phase == "retiring" || phase == "retired" {
							t.CanCancel = false
						}
						t.CleanupConfirmed = false
					}
				}
				if s.saveRun(&run) != nil {
					s.recoveryRequired = true
				}
			}
			s.operations[id] = &run
			for cid, plan := range run.Plans {
				if s.targets[cid] == nil {
					c := plan.Candidate
					c.AccessAvailable = false
					c.HostKeyTrusted = false
					if c.BootstrapState == "" {
						c.BootstrapSource = "retained-operation"
						c.BootstrapState = "ssh-ready"
					}
					c.Reason = "Authorize device account access again to resume or clean the retained operation"
					s.targets[cid] = &onboardingPrivateTarget{candidate: c, access: onboardingAccess{user: plan.Username}, lifetime: plan.Review.StartupLifetime}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
	}
}
func (s *onboardingService) stopOperations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.operations {
		if run.cancel != nil {
			run.cancel()
		}
	}
}
func (s *onboardingService) approve(ctx context.Context, reviewID string) (onboardingOperation, error) {
	s.expireAccess(time.Now())
	s.mu.Lock()
	if s.recoveryRequired {
		s.mu.Unlock()
		return onboardingOperation{}, errors.New("onboarding ownership recovery is required before any new approval")
	}
	if s.m.cables != nil && s.m.cables.held() {
		s.mu.Unlock()
		return onboardingOperation{}, errors.New("an active or cleanup-unconfirmed cable operation blocks device setup")
	}
	for _, run := range s.operations {
		if run.Public.ReviewID == reviewID {
			op := cloneOnboardingOperation(run.Public)
			s.mu.Unlock()
			return op, nil
		}
	}
	binding, ok := s.reviews[reviewID]
	s.mu.Unlock()
	if !ok || !binding.review.CanApprove || time.Now().UnixMilli() > binding.review.ExpiresAt {
		return onboardingOperation{}, errors.New("review is expired, unavailable or has blocked targets; inspect again")
	}
	rejectDuplicateOnboardingUpgradePeers(&binding.review, binding.upgrades)
	if !binding.review.CanApprove {
		return onboardingOperation{}, errors.New("selected SSH aliases resolve to the same enrolled peer; inspect distinct peers")
	}
	identity, err := s.localIdentity(ctx)
	if err != nil || identity.NodeUUID != binding.review.ControllerNodeID || identity.ClusterID != binding.review.TargetClusterID {
		return onboardingOperation{}, errors.New("PAIR controller or target cluster changed since review")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m.cables != nil && s.m.cables.held() {
		return onboardingOperation{}, errors.New("an active or cleanup-unconfirmed cable operation blocks device setup")
	}
	if s.active != "" {
		return onboardingOperation{}, errors.New("another approved onboarding operation is active")
	}
	for _, run := range s.operations {
		if run.Public.ReviewID == reviewID {
			return cloneOnboardingOperation(run.Public), nil
		}
	}
	run := &onboardingRun{Public: onboardingOperation{OperationID: newOpID(), ReviewID: reviewID, TargetClusterID: identity.ClusterID, Revision: 1, State: "running", StartedAt: time.Now().UnixMilli(), Targets: []onboardingTargetState{}}, ControllerNodeID: identity.NodeUUID, Deadline: time.Now().Add(20 * time.Minute).UnixMilli(), Plans: map[string]onboardingPlan{}, cancelTarget: map[string]bool{}, targetCancels: map[string]context.CancelFunc{}}
	for _, row := range binding.review.Targets {
		target := s.targets[row.CandidateID]
		if target == nil || target.changedKey || !target.candidate.AccessAvailable || target.candidate.AccessID != row.AccessID || target.lifetime != row.StartupLifetime || target.candidate.Address != row.Address || target.candidate.Port != row.Port || target.candidate.HostKeySHA256 != row.HostKeySHA256 {
			return onboardingOperation{}, errors.New("reviewed device access or identity changed")
		}
		if !target.expiresAt.IsZero() && target.expiresAt.UnixMilli() < run.Deadline {
			run.Deadline = target.expiresAt.UnixMilli()
		}
		pkg := binding.packages[row.CandidateID]
		var existing *onboardingExistingInstallation
		if row.Action == "upgrade" {
			value, found := binding.upgrades[row.CandidateID]
			if !found || row.ExistingInstallation == nil {
				return onboardingOperation{}, errors.New("reviewed existing installation binding is missing")
			}
			if !reflect.DeepEqual(*row.ExistingInstallation, value.Summary()) {
				return onboardingOperation{}, errors.New("reviewed existing installation summary changed")
			}
			if err := validateOnboardingExisting(value, binding.info[row.CandidateID]); err != nil {
				return onboardingOperation{}, fmt.Errorf("reviewed existing installation is no longer valid: %w", err)
			}
			existing = &value
		}
		kind := "password"
		if target.access.keyPath != "" || target.access.signer != nil {
			kind = "existing-key"
		}
		run.Plans[row.CandidateID] = onboardingPlan{AccessGeneration: target.accessGeneration, Candidate: target.candidate, Review: row, Info: binding.info[row.CandidateID], Artifact: pkg.source, PackageFile: pkg.file, ArchiveRoot: pkg.archiveRoot, ArchiveBytes: pkg.bytes, Username: target.access.user, AuthKind: kind, ExistingInstallation: existing}
		run.Public.Targets = append(run.Public.Targets, onboardingTargetState{CandidateID: row.CandidateID, Stage: "access-authorized", CanCancel: true, CleanupConfirmed: true})
	}
	if err = s.saveRun(run); err != nil {
		return onboardingOperation{}, errors.New("approved operation could not be retained before effects")
	}
	s.operations[run.Public.OperationID] = run
	s.active = run.Public.OperationID
	opctx, cancel := context.WithDeadline(s.ctx, time.UnixMilli(run.Deadline))
	run.cancel = cancel
	go s.executeOperation(opctx, run, nil)
	return cloneOnboardingOperation(run.Public), nil
}

type onboardingNodeIdentity struct {
	NodeUUID  string `json:"nodeUuid"`
	ClusterID string `json:"clusterId"`
}

func (s *onboardingService) localIdentity(ctx context.Context) (onboardingNodeIdentity, error) {
	var identity onboardingNodeIdentity
	raw, err := s.cluster(ctx, "cluster:get-node-id", map[string]any{})
	if err != nil {
		return identity, err
	}
	if json.Unmarshal(raw, &identity) != nil || identity.NodeUUID == "" {
		return identity, errors.New("PAIR identity is unavailable")
	}
	return identity, nil
}
func (s *onboardingService) updateTarget(run *onboardingRun, id, stage, message string, receipt *onboardingInstallReceipt, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range run.Public.Targets {
		t := &run.Public.Targets[i]
		if t.CandidateID != id {
			continue
		}
		t.Stage, t.Message = stage, message
		if stage == "installing" || stage == "starting" || stage == "invitation-sent" || stage == "pairing" || stage == "verifying" {
			t.CleanupConfirmed = false
		}
		if nodeID != "" {
			t.NodeID = nodeID
		}
		t.CanRetry = stage == "verification-failed" || stage == "blocked" || stage == "cancelled"
		t.CanCancel = stage != "paired"
		if phase := run.Plans[id].UpgradePhase; phase == "retiring" || phase == "retired" {
			t.CanCancel = false
		}
		if stage == "paired" {
			t.CleanupConfirmed = true
			t.CanRetry = false
		}
		if receipt != nil {
			plan := run.Plans[id]
			plan.Receipt = *receipt
			run.Plans[id] = plan
			t.CleanupConfirmed = receipt.CleanupConfirmed
		}
	}
	run.Public.Revision++
	err := s.saveRun(run)
	if err != nil {
		s.recoveryRequired = true
	}
	return err
}
func (s *onboardingService) operationRequest(ctx context.Context, method string, request onboardingOperationRequest) (any, error) {
	s.expireAccess(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryRequired && request.OperationID == "" {
		return nil, errors.New("onboarding recovery is required; unknown retained effects must not be treated as absent")
	}
	var run *onboardingRun
	if request.OperationID != "" {
		run = s.operations[request.OperationID]
	} else {
		for _, candidate := range s.operations {
			if request.ReviewID != "" && candidate.Public.ReviewID != request.ReviewID {
				continue
			}
			if run == nil || candidate.Public.StartedAt > run.Public.StartedAt {
				run = candidate
			}
		}
	}
	if run == nil {
		if method == "engine:onboarding-status" {
			return nil, nil
		}
		return nil, errors.New("onboarding operation is unknown")
	}
	if method == "engine:onboarding-status" {
		return cloneOnboardingOperation(run.Public), nil
	}
	if run.historyOnly {
		return nil, errors.New("historical onboarding operation is retained read-only and cannot be retried or cancelled")
	}
	if request.OperationID == "" {
		return nil, errors.New("operation ID is required")
	}
	selected := []string{}
	for _, t := range run.Public.Targets {
		if request.CandidateID != "" && t.CandidateID != request.CandidateID {
			continue
		}
		if t.Stage != "paired" {
			if method == "engine:onboarding-cancel" && (run.Plans[t.CandidateID].UpgradePhase == "retiring" || run.Plans[t.CandidateID].UpgradePhase == "retired") {
				if request.CandidateID != "" {
					return nil, errors.New("upgrade is already committed; retry its remaining verification or retirement")
				}
				continue // A batch cancel cannot undo another target's committed replacement.
			}
			selected = append(selected, t.CandidateID)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("no unfinished selected target")
	}
	var launchCtx context.Context
	if method == "engine:onboarding-cancel" {
		if s.active != "" && s.active != run.Public.OperationID {
			return nil, errors.New("another onboarding operation is active")
		}
		for _, id := range selected {
			run.cancelTarget[id] = true
			if cancel := run.targetCancels[id]; cancel != nil {
				cancel()
			}
		}
		if run.Public.State != "running" {
			budget := 320 * time.Second
			for _, id := range selected {
				target, plan := s.targets[id], run.Plans[id]
				if target == nil || !target.candidate.AccessAvailable || target.access.user != plan.Username || target.candidate.HostKeySHA256 != plan.Review.HostKeySHA256 {
					return nil, errors.New("authorize the same device account and host identity before cancellation cleanup")
				}
				plan.AccessGeneration = target.accessGeneration
				run.Plans[id] = plan
				if run.Plans[id].ExistingInstallation != nil {
					budget = onboardingUpgradePhaseBudget + 45*time.Second
				}
			}
			opctx, cancel := context.WithTimeout(s.ctx, budget)
			run.cancel = cancel
			run.Public.State = "running"
			s.active = run.Public.OperationID
			launchCtx = opctx
		}
	} else {
		if s.active != "" {
			return nil, errors.New("onboarding work is already running")
		}
		committedOnly := len(selected) != 0
		for _, id := range selected {
			phase := run.Plans[id].UpgradePhase
			committedOnly = committedOnly && (phase == "retiring" || phase == "retired")
		}
		if time.Now().UnixMilli() > run.Deadline && !committedOnly {
			return nil, errors.New("approved setup session expired; retained effects require explicit cleanup before a new setup")
		}
		for _, id := range selected {
			target := s.targets[id]
			plan := run.Plans[id]
			if target == nil || !target.candidate.AccessAvailable || target.access.user != plan.Username || target.candidate.HostKeySHA256 != plan.Review.HostKeySHA256 {
				return nil, errors.New("authorize the same device account and host identity before retry")
			}
			delete(run.cancelTarget, id)
			plan.AccessGeneration = target.accessGeneration
			run.Plans[id] = plan
		}
		opctx, cancel := context.WithDeadline(s.ctx, time.UnixMilli(run.Deadline))
		if committedOnly && time.Now().UnixMilli() > run.Deadline {
			cancel()
			opctx, cancel = context.WithTimeout(s.ctx, onboardingUpgradePhaseBudget)
		}
		run.cancel = cancel
		run.Public.State = "running"
		run.Public.FinishedAt = 0
		s.active = run.Public.OperationID
		launchCtx = opctx
	}
	run.Public.Revision++
	if err := s.saveRun(run); err != nil {
		s.recoveryRequired = true
		if launchCtx != nil {
			run.cancel()
			run.cancel = nil
			run.Public.State = "interrupted"
			s.active = ""
		}
		return nil, errors.New("operation checkpoint could not be retained; no new execution was started")
	}
	if launchCtx != nil {
		go s.executeOperation(launchCtx, run, selected)
	}
	return cloneOnboardingOperation(run.Public), nil
}
func (s *onboardingService) executeOperation(ctx context.Context, run *onboardingRun, only []string) {
	s.mu.Lock()
	targets := append([]onboardingTargetState(nil), run.Public.Targets...)
	s.mu.Unlock()
	for _, target := range targets {
		if target.Stage == "paired" {
			continue
		}
		if only != nil {
			found := false
			for _, id := range only {
				if id == target.CandidateID {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		s.executeTarget(ctx, run, target.CandidateID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	allPaired, allCancelled := true, true
	for _, t := range run.Public.Targets {
		allPaired = allPaired && t.Stage == "paired"
		allCancelled = allCancelled && t.Stage == "cancelled"
	}
	run.Public.State = "failed"
	if allPaired {
		run.Public.State = "completed"
	} else if allCancelled {
		run.Public.State = "cancelled"
	}
	run.Public.FinishedAt = time.Now().UnixMilli()
	run.Public.Revision++
	if run.cancel != nil {
		run.cancel()
		run.cancel = nil
	}
	for id, plan := range run.Plans {
		if target := s.targets[id]; target != nil && target.accessGeneration == plan.AccessGeneration {
			target.access = onboardingAccess{}
			target.candidate.AccessAvailable = false
			target.candidate.Reason = "Setup access was released; authorize access again for any retained recovery"
		}
	}
	if s.active == run.Public.OperationID {
		s.active = ""
	}
	if s.saveRun(run) != nil {
		s.recoveryRequired = true
		run.Public.State = "interrupted"
	}
}
func (s *onboardingService) executeTarget(ctx context.Context, run *onboardingRun, id string) {
	ctx, cancelTarget := context.WithCancel(ctx)
	defer cancelTarget()
	s.mu.Lock()
	if run.targetCancels == nil {
		run.targetCancels = map[string]context.CancelFunc{}
	}
	run.targetCancels[id] = cancelTarget
	plan := run.Plans[id]
	target := s.targets[id]
	cancelled := run.cancelTarget[id]
	var access onboardingAccess
	if target != nil && target.accessGeneration == plan.AccessGeneration {
		access = target.access
	}
	expectedCluster := run.Public.TargetClusterID
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(run.targetCancels, id); s.mu.Unlock() }()
	if access.user != plan.Username || access.user == "" {
		s.updateTarget(run, id, "blocked", "Authorize the same device access to continue the retained operation", nil, "")
		return
	}
	if plan.ExistingInstallation != nil {
		access = access.forPurpose("enrolled-peer-upgrade")
	}
	connectionCandidate := plan.Candidate
	connectionCandidate.HostKeyTrusted = true
	connectionCandidate.HostKeySHA256 = plan.Review.HostKeySHA256
	connectionCtx := ctx
	if cancelled || ctx.Err() != nil {
		var cancel context.CancelFunc
		budget := 320 * time.Second
		if plan.ExistingInstallation != nil {
			budget = onboardingUpgradePhaseBudget + 45*time.Second
		}
		connectionCtx, cancel = context.WithTimeout(context.Background(), budget)
		defer cancel()
	}
	client, err := s.dial(connectionCtx, connectionCandidate, access)
	if err != nil {
		s.updateTarget(run, id, "verification-failed", err.Error(), nil, "")
		return
	}
	defer client.close()
	fresh, accountErr := readOnboardingPlatform(connectionCtx, client)
	freshOS, freshArch, _ := onboardingPlatform(fresh.OS, fresh.Arch)
	if accountErr != nil || fresh.UID != plan.Info.UID || fresh.Home != plan.Info.Home || freshOS != plan.Review.Platform || freshArch != plan.Review.Arch {
		s.updateTarget(run, id, "blocked", "Authenticated device account or native platform changed; no new effects attempted", nil, "")
		return
	}
	if plan.ExistingInstallation != nil {
		s.executeUpgradeTarget(ctx, client, run, id, plan, access, cancelled)
		return
	}
	receipt := plan.Receipt
	if cancelled || ctx.Err() != nil {
		if e := s.cancelOnboardingInvitation(connectionCtx, run, id); e != nil {
			receipt.CleanupConfirmed = false
			s.updateTarget(run, id, "verification-failed", e.Error(), &receipt, "")
			return
		}
		clean, e := cleanupOnboardingTarget(connectionCtx, client, receipt)
		receipt.CleanupConfirmed = clean
		message := "Onboarding cancelled; installed bundles and identities were retained"
		if e != nil {
			message = "Cancellation cleanup is unconfirmed; retained effects were not guessed away"
		}
		stage := "cancelled"
		if !clean || e != nil {
			stage = "verification-failed"
		}
		s.updateTarget(run, id, stage, message, &receipt, "")
		return
	}
	if e := s.reconcileOnboardingInvite(ctx, run, id); e != nil {
		s.failTarget(ctx, run, id, client, receipt, e)
		return
	}
	s.mu.Lock()
	expectedCluster = run.Public.TargetClusterID
	s.mu.Unlock()
	identity, err := s.localIdentity(ctx)
	if err != nil || identity.NodeUUID != run.ControllerNodeID || identity.ClusterID != expectedCluster {
		s.updateTarget(run, id, "blocked", "Controller cluster changed; no new onboarding effects were attempted", nil, "")
		return
	}
	pkg := onboardingPackage{source: plan.Artifact, file: plan.PackageFile, archiveRoot: plan.ArchiveRoot, bytes: plan.ArchiveBytes}
	if _, _, err = verifyOnboardingArchive(pkg.file, pkg.source.onboardingArtifact); err != nil {
		s.updateTarget(run, id, "verification-failed", err.Error(), nil, "")
		return
	}
	if plan.Review.NeedsLinger && !plan.LingerChanged {
		if access.elevationPassword == "" {
			s.updateTarget(run, id, "blocked", "Enter the separate approved elevation password for persistent startup", nil, "")
			return
		}
		if strings.ContainsAny(access.elevationPassword, "\r\n\x00") {
			s.updateTarget(run, id, "blocked", "Elevation input cannot contain line separators", nil, "")
			return
		}
		s.mu.Lock()
		current := run.Plans[id]
		current.LingerRequested = true
		run.Plans[id] = current
		err = s.saveRun(run)
		s.mu.Unlock()
		if err != nil {
			s.updateTarget(run, id, "verification-failed", "Persistent-startup checkpoint failed; privilege action was not attempted", nil, "")
			return
		}
		_, err = client.run(ctx, "/usr/bin/sudo -S -p '' -- /usr/bin/loginctl enable-linger "+fmt.Sprint(plan.Info.UID), strings.NewReader(access.elevationPassword+"\n"))
		if err != nil {
			s.updateTarget(run, id, "verification-failed", "The approved own-account persistent-startup action failed; no credential details were logged", nil, "")
			return
		}
		confirmed, readErr := readOnboardingPlatform(ctx, client)
		if readErr != nil || !confirmed.Linger {
			s.updateTarget(run, id, "verification-failed", "Persistent-startup readback is unconfirmed; prior action is retained for recovery", nil, "")
			return
		}
		s.mu.Lock()
		current = run.Plans[id]
		current.LingerChanged = true
		run.Plans[id] = current
		err = s.saveRun(run)
		s.mu.Unlock()
		if err != nil {
			s.updateTarget(run, id, "verification-failed", "Persistent-startup checkpoint failed after the approved action; no transfer was attempted", nil, "")
			return
		}
	}
	var checkpointErr error
	progress := func(stage, message string) {
		if e := s.updateTarget(run, id, stage, message, nil, ""); e != nil {
			checkpointErr = e
			cancelTarget()
		}
	}
	// The receiver refuses foreign identities and supports the same owned bundle
	// receipt. Pairing remains a separate backend-native transaction below.
	resume := receipt.StagePath != ""
	if !resume {
		receipt, err = onboardingInstallPaths(plan.Info, pkg, run.Public.OperationID)
		if err == nil {
			receipt.StartupLifetime = plan.Review.StartupLifetime
			receipt.ManifestSHA256, err = onboardingPackageManifestHash(pkg)
		}
		if err != nil {
			s.failTarget(ctx, run, id, client, receipt, err)
			return
		}
		s.mu.Lock()
		current := run.Plans[id]
		current.Receipt = receipt
		run.Plans[id] = current
		err = s.saveRun(run)
		s.mu.Unlock()
		if err != nil {
			s.updateTarget(run, id, "verification-failed", "Could not retain exact owned installation paths before transfer", nil, "")
			return
		}
	}
	if resume {
		if existing, e := onboardingRemoteIdentity(ctx, client, receipt); e == nil && existing.ClusterID != "" {
			if existing.ClusterID != expectedCluster || existing.NodeUUID == run.ControllerNodeID || (receipt.NodeID != "" && existing.NodeUUID != receipt.NodeID) {
				s.failTarget(ctx, run, id, client, receipt, errors.New("retained PAIR membership differs from the approved operation"))
				return
			}
			localRoster, e := s.cluster(ctx, "nodes:get-initial", map[string]any{})
			remoteRoster, e2 := runOnboardingControl(ctx, client, receipt, "nodes:get-initial", map[string]any{})
			if e == nil && e2 == nil && onboardingRosterMember(localRoster, existing.NodeUUID) && onboardingRosterMember(remoteRoster, run.ControllerNodeID) {
				clean, e := finishOnboardingTarget(ctx, client, receipt)
				receipt.CleanupConfirmed = clean
				if e == nil && clean {
					s.updateTarget(run, id, "paired", "Retained reciprocal membership and owned staging cleanup verified", &receipt, existing.NodeUUID)
					return
				}
			}
			s.failTarget(ctx, run, id, client, receipt, errors.New("retained membership could not be reconciled"))
			return
		}
		receipt, err = resumeOnboardingTarget(ctx, client, plan.Info, pkg, receipt, progress)
	} else {
		receipt, err = installOnboardingTarget(ctx, client, plan.Info, pkg, run.Public.OperationID, plan.Review.StartupLifetime, progress)
	}
	if e := s.updateTarget(run, id, "starting", "Verifying native headless PAIR readiness", &receipt, ""); e != nil {
		checkpointErr = e
	}
	if checkpointErr != nil {
		err = errors.New("onboarding checkpoint could not be retained; no further pairing effects were attempted")
	}
	if err != nil {
		s.failTarget(ctx, run, id, client, receipt, err)
		return
	}
	remote, err := onboardingRemoteIdentity(ctx, client, receipt)
	if err != nil {
		s.failTarget(ctx, run, id, client, receipt, err)
		return
	}
	if remote.ClusterID != "" {
		if remote.ClusterID != expectedCluster {
			s.failTarget(ctx, run, id, client, receipt, errors.New("target already belongs to a different cluster"))
			return
		}
	} else {
		invite, e := s.obtainOnboardingInvite(ctx, run, id, remote.NodeUUID)
		if e != nil {
			invite.Pin = ""
			s.mu.Lock()
			checkpointFailed := s.recoveryRequired
			s.mu.Unlock()
			if checkpointFailed {
				cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = s.cancelOnboardingInvitation(cancelCtx, run, id)
				cancel()
			}
			s.failTarget(ctx, run, id, client, receipt, e)
			return
		}
		afterInvite, identityErr := s.localIdentity(ctx)
		if identityErr != nil || afterInvite.NodeUUID != run.ControllerNodeID || afterInvite.ClusterID == "" || afterInvite.ClusterID != invite.ClusterID {
			invite.Pin = ""
			s.failTarget(ctx, run, id, client, receipt, errors.New("PAIR cluster identity changed while creating the invitation"))
			return
		}
		s.updateTarget(run, id, "invitation-sent", "Invitation sent only to the approved device", nil, remote.NodeUUID)
		if e := s.updateTarget(run, id, "pairing", "Completing the normal authenticated pairing handshake", nil, remote.NodeUUID); e != nil {
			invite.Pin = ""
			cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = s.cancelOnboardingInvitation(cancelCtx, run, id)
			cancel()
			s.failTarget(ctx, run, id, client, receipt, errors.New("pairing checkpoint failed; completion was not sent"))
			return
		}
		if invite.State != "paired" {
			_, e = runOnboardingControl(ctx, client, receipt, "cluster:respond-to-invite", map[string]any{"inviteId": invite.InviteID, "accept": true, "pin": invite.Pin})
		}
		invite.Pin = ""
		if e != nil {
			s.failTarget(ctx, run, id, client, receipt, e)
			return
		}
	}
	s.updateTarget(run, id, "verifying", "Waiting for reciprocal PAIR membership and identity readback", nil, remote.NodeUUID)
	s.mu.Lock()
	expectedCluster = run.Public.TargetClusterID
	s.mu.Unlock()
	local, remote, e := s.waitOnboardingMembership(ctx, client, receipt, run.ControllerNodeID, remote.NodeUUID, expectedCluster)
	if e != nil {
		s.failTarget(ctx, run, id, client, receipt, e)
		return
	}
	s.mu.Lock()
	run.Public.TargetClusterID = local.ClusterID
	s.mu.Unlock()
	receipt.CleanupConfirmed, err = finishOnboardingTarget(ctx, client, receipt)
	if err != nil || !receipt.CleanupConfirmed {
		s.failTarget(ctx, run, id, client, receipt, errors.New("membership committed but owned staging cleanup is unconfirmed; retry verification without re-pairing"))
		return
	}
	s.updateTarget(run, id, "paired", "Installed PAIR and reciprocal membership verified; account startup state is retained", &receipt, remote.NodeUUID)
}
func (s *onboardingService) failTarget(ctx context.Context, run *onboardingRun, id string, client *onboardingSSH, receipt onboardingInstallReceipt, cause error) {
	s.mu.Lock()
	cancelled := run.cancelTarget[id]
	plan := run.Plans[id]
	target := s.targets[id]
	var access onboardingAccess
	if target != nil && target.accessGeneration == plan.AccessGeneration {
		access = target.access
	}
	s.mu.Unlock()
	if cancelled || ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 320*time.Second)
		defer cancel()
		if err := s.cancelOnboardingInvitation(cleanupCtx, run, id); err != nil {
			receipt.CleanupConfirmed = false
			s.updateTarget(run, id, "verification-failed", err.Error(), &receipt, "")
			return
		}
		candidate := plan.Candidate
		candidate.HostKeySHA256 = plan.Review.HostKeySHA256
		candidate.HostKeyTrusted = true
		if plan.ExistingInstallation != nil {
			access = access.forPurpose("enrolled-peer-upgrade")
		}
		cleaner, err := s.dial(cleanupCtx, candidate, access)
		if err == nil {
			defer cleaner.close()
			clean, e := cleanupOnboardingTarget(cleanupCtx, cleaner, receipt)
			receipt.CleanupConfirmed = clean
			if e == nil && clean {
				stage, message := "verification-failed", "Operation interrupted; owned cleanup confirmed and identity retained"
				if cancelled {
					stage, message = "cancelled", "Cancellation confirmed; installed bundles and identity were retained"
				}
				s.updateTarget(run, id, stage, message, &receipt, "")
				return
			}
		}
		cause = errors.New("Cancellation was requested but owned cleanup is unconfirmed; authorize access and cancel this same operation again")
	}
	message := cause.Error()
	if len(message) > 1024 {
		message = message[:1024]
	}
	s.updateTarget(run, id, "verification-failed", message, &receipt, "")
}
func (s *onboardingService) waitOnboardingMembership(ctx context.Context, client *onboardingSSH, receipt onboardingInstallReceipt, localID, remoteID, clusterID string) (onboardingNodeIdentity, onboardingNodeIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		local, e := s.localIdentity(ctx)
		remote, e2 := onboardingRemoteIdentity(ctx, client, receipt)
		if e == nil && e2 == nil && local.NodeUUID == localID && remote.NodeUUID == remoteID && local.ClusterID == clusterID && clusterID != "" && local.ClusterID == remote.ClusterID {
			left, e := s.cluster(ctx, "nodes:get-initial", map[string]any{})
			right, e2 := runOnboardingControl(ctx, client, receipt, "nodes:get-initial", map[string]any{})
			if e == nil && e2 == nil && onboardingRosterMember(left, remoteID) && onboardingRosterMember(right, localID) {
				return local, remote, nil
			}
		}
		if e == nil && local.NodeUUID != "" && local.NodeUUID != localID || e2 == nil && remote.NodeUUID != "" && remote.NodeUUID != remoteID {
			return local, remote, errors.New("PAIR identity changed during verification")
		}
		select {
		case <-ctx.Done():
			return local, remote, errors.New("reciprocal PAIR membership did not converge before the verification deadline; no duplicate invitation was sent")
		case <-ticker.C:
		}
	}
}
func onboardingRemoteIdentity(ctx context.Context, client *onboardingSSH, receipt onboardingInstallReceipt) (onboardingNodeIdentity, error) {
	var identity onboardingNodeIdentity
	raw, err := runOnboardingControl(ctx, client, receipt, "cluster:get-node-id", map[string]any{})
	if err != nil {
		return identity, err
	}
	if json.Unmarshal(raw, &identity) != nil || identity.NodeUUID == "" {
		return identity, errors.New("target PAIR identity unavailable")
	}
	return identity, nil
}
func onboardingRosterMember(raw json.RawMessage, id string) bool {
	var roster struct {
		Nodes []struct {
			NodeUUID string `json:"nodeUuid"`
			State    string `json:"state"`
		} `json:"nodes"`
	}
	if json.Unmarshal(raw, &roster) != nil || len(roster.Nodes) > 128 {
		return false
	}
	for _, node := range roster.Nodes {
		if node.NodeUUID == id && node.State == "member" {
			return true
		}
	}
	return false
}
