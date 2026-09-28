// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"
)

func TestHistoricalManagedUpgradeWithoutRetentionLoadsReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name, state, stage, phase string
		wantRecovery              bool
	}{
		{"completed", "completed", "paired", "retired", false},
		{"cancelled", "cancelled", "cancelled", "cancelled-before-stop", false},
		{"resumable", "failed", "verification-failed", "retired", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, id := managedOnboardingPredecessor(t)
			plan.ExistingInstallation.Retention = nil
			summary := plan.ExistingInstallation.Summary()
			plan.Review.ExistingInstallation = &summary
			plan.UpgradePhase = tc.phase
			plan.Receipt.CleanupConfirmed = true
			if tc.state == "completed" {
				plan.Receipt.Installed = true
				plan.Receipt.ServiceInstalled = true
				plan.Receipt.ServiceStarted = true
			}
			e := NewExecutor(nil, NewReporter(nil), nil, t.TempDir())
			s := &onboardingService{m: &Manager{exec: e}, ctx: context.Background(), operations: map[string]*onboardingRun{}, targets: map[string]*onboardingPrivateTarget{}}
			target := onboardingTargetState{CandidateID: plan.Candidate.CandidateID, Stage: tc.stage, NodeID: plan.ExistingInstallation.NodeID, CleanupConfirmed: true}
			if tc.state != "completed" {
				target.CanRetry = true
				target.CanCancel = true
			}
			run := &onboardingRun{Public: onboardingOperation{OperationID: id, ReviewID: newOpID(), Revision: 1, State: tc.state, StartedAt: time.Now().Add(-time.Minute).UnixMilli(), FinishedAt: time.Now().UnixMilli(), Targets: []onboardingTargetState{target}}, Plans: map[string]onboardingPlan{plan.Candidate.CandidateID: plan}}
			if err := s.saveRun(run); err != nil {
				t.Fatal(err)
			}
			loaded := newOnboardingService(s.m)
			if loaded.recoveryRequired != tc.wantRecovery {
				t.Fatalf("recoveryRequired=%t, want %t", loaded.recoveryRequired, tc.wantRecovery)
			}
			history := loaded.operations[id]
			if history == nil || history.historyOnly == tc.wantRecovery {
				t.Fatalf("historyOnly=%t recovery=%t", history != nil && history.historyOnly, loaded.recoveryRequired)
			}
			candidate := loaded.targets[plan.Candidate.CandidateID]
			if candidate == nil ||
				candidate.candidate.BootstrapState != "ssh-ready" ||
				candidate.candidate.BootstrapSource != "retained-operation" {
				t.Fatalf("retained candidate bootstrap state = %+v", candidate)
			}
			if history.historyOnly {
				for _, method := range []string{"engine:onboarding-retry", "engine:onboarding-cancel"} {
					if _, err := loaded.operationRequest(context.Background(), method, onboardingOperationRequest{OperationID: id}); err == nil {
						t.Fatalf("%s admitted read-only history", method)
					}
				}
			}
		})
	}
}
