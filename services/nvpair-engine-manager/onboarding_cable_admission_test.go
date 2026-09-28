// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOnboardingFinalAdmissionRechecksCableHold(t *testing.T) {
	f := newCableProductFixture(t)
	f.holdResult = true
	cableReview := f.ready()
	s := f.s.m.onboarding
	target := s.targets[f.ids["host-peer"]]
	file, artifact := writeOnboardingFixture(t, onboardingFixtureEntries(183))
	archiveRoot, archiveBytes, err := verifyOnboardingArchive(file, artifact)
	if err != nil {
		t.Fatal(err)
	}
	reviewID := strings.Repeat("d", 32)
	row := onboardingReviewTarget{CandidateID: target.candidate.CandidateID, Label: target.candidate.Label, Address: target.candidate.Address, Port: target.candidate.Port, AccessID: target.candidate.AccessID, AccessLabel: target.candidate.AccessLabel, HostKeySHA256: target.candidate.HostKeySHA256, StartupLifetime: target.lifetime, Status: "ready", Artifact: &artifact}
	s.reviews[reviewID] = onboardingReviewBinding{
		review:   onboardingReview{ReviewID: reviewID, ControllerNodeID: "host-owner", TargetClusterID: "synthetic-cluster", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), CanApprove: true, Targets: []onboardingReviewTarget{row}},
		info:     map[string]onboardingPlatformInfo{row.CandidateID: {OS: "Linux", Arch: "aarch64", Home: "/home/synthetic-user", UID: 1000}},
		packages: map[string]onboardingPackage{row.CandidateID: {source: onboardingArtifactSource{onboardingArtifact: artifact, File: file}, file: file, archiveRoot: archiveRoot, bytes: archiveBytes}},
	}
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.testCluster = func(ctx context.Context, method string, _ any) (json.RawMessage, error) {
		if method != "cluster:get-node-id" {
			return nil, errors.New("unexpected fixture cluster request")
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return json.RawMessage(`{"nodeUuid":"host-owner","clusterId":"synthetic-cluster"}`), nil
	}
	var saves atomic.Int32
	s.testSave = func(*onboardingRun) error {
		saves.Add(1)
		// Stop the pre-fix counterexample before any onboarding execution.
		return errors.New("fixture stops unexpected onboarding admission before effects")
	}
	finished := make(chan error, 1)
	go func() { _, err := s.approve(ctx, reviewID); finished <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("onboarding did not reach its unlocked identity read")
	}
	run := f.start(cableReview)
	f.awaitCount("started:", 2)
	if !f.s.held() {
		t.Fatal("actual cable admission did not hold the shared owner")
	}
	close(release)
	select {
	case err := <-finished:
		if saves.Load() != 0 {
			t.Fatalf("onboarding reached %d save/admission attempt after cable took the owner", saves.Load())
		}
		if err == nil || !strings.Contains(err.Error(), "cable operation") {
			t.Fatalf("late cable hold did not reject onboarding: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("final onboarding admission did not settle")
	}
	s.mu.Lock()
	active, operations := s.active, len(s.operations)
	s.mu.Unlock()
	if active != "" || operations != 0 {
		t.Fatal("onboarding published a concurrent operation")
	}
	if _, err := f.s.cancelRun(run.RunID); err != nil {
		t.Fatal(err)
	}
	if !f.done(run).CleanupConfirmed {
		t.Fatal("fixture cable cleanup was not confirmed")
	}
}
