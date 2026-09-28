// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"nvpair-shared/cableprobe"
)

func TestFabricCableProtocolCompositionAcceptsV4RuntimeWithFabricV3(t *testing.T) {
	if cableWorkerProtocol != "pair-cable-worker/4" || fabricWorkerProtocol != "pair-fabric-address/4" {
		t.Fatal("composition does not contain the intended independent protocol generations")
	}
	// This existing fixture performs the real fabric review, including both
	// cable runtime inspection and the mocked fabric /3 capability exchanges.
	s, review, controls := fabricRefusalFixture(t)
	s.mu.Lock()
	plans := s.reviews[review.ReviewID].access.plans
	s.mu.Unlock()
	if !review.Executable || review.State != "ready" || len(plans) != 2 {
		t.Fatal("cable /4 plus fabric /3 did not produce an executable fabric review")
	}
	for _, plan := range plans {
		if plan.Runtime == nil || plan.Runtime.Protocol != "pair-cable-worker/4" {
			t.Fatal("fabric review lost the exact current cable runtime binding")
		}
	}
	if controls.Load() != 0 || len(s.runs) != 0 || len(s.m.cables.runs) != 0 || s.held() {
		t.Fatal("read-only composition review acquired execution or cleanup ownership")
	}
	// Static inspection complements the actual review. The Python is not run.
	launcher := fabricRootLaunchScript()
	if strings.Count(launcher, "b['protocol']!='pair-cable-worker/4'") != 1 || !strings.Contains(launcher, "'--fabric-address-once'") || strings.Contains(launcher, "'--cable-probe-once'") {
		t.Fatal("derived fabric launcher does not preserve current runtime binding and purpose")
	}
}

func TestFabricCableProtocolCompositionRejectsV3BeforeFabricCapability(t *testing.T) {
	s, _, controls := fabricRefusalFixture(t)
	binding := s.m.cables.workerBinding
	s.m.cables.workerBinding = func(ctx context.Context, target cableprobe.Target) (cableWorkerBinding, error) {
		value, err := binding(ctx, target)
		value.Protocol = "pair-cable-worker/3"
		return value, err
	}
	var dials atomic.Int32
	dial := s.m.onboarding.dial
	s.m.onboarding.dial = func(ctx context.Context, candidate onboardingCandidate, access onboardingAccess) (*onboardingSSH, error) {
		dials.Add(1)
		return dial(ctx, candidate, access)
	}
	review, err := s.review(context.Background(), cableProductReviewRequest{ReviewRequest: cableTestSelection()})
	if err != nil || review.Executable || review.State != "blocked" || review.Permission == nil || len(review.Permission.Targets) != 2 {
		t.Fatalf("old cable runtime was not blocked by the real fabric review: %+v %v", review, err)
	}
	for _, target := range review.Permission.Targets {
		if target.WorkerAvailable || !strings.Contains(target.Reason, "protocol v4") {
			t.Fatal("old runtime rejection did not preserve its exact capability reason")
		}
	}
	if dials.Load() != 0 || controls.Load() != 0 || len(s.runs) != 0 || len(s.m.cables.runs) != 0 || s.held() {
		t.Fatal("old runtime reached fabric capability/launch or acquired an operation hold")
	}
}
