// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"nvpair-shared/cableprobe"
)

func TestCablePrearmV4CapabilityAndLaunchAdmission(t *testing.T) {
	var capability map[string]any
	if json.Unmarshal([]byte(cableWorkerCapabilitiesJSON()), &capability) != nil || len(capability) != 3 || capability["protocol"] != "pair-cable-worker/4" || capability["maxSeconds"] != float64(20) || capability["maxPorts"] != float64(2) {
		t.Fatal("fixed capability producer does not advertise bounded v4")
	}
	for _, mode := range []string{"old-request", "v2-request", "v3-request", "missing-request", "unknown-request", "old-runtime", "v2-runtime", "v3-runtime", "unknown-runtime"} {
		t.Run(mode, func(t *testing.T) {
			plan, _, _ := launchTestRuntimePlan(t)
			request := launchTestRequest(plan)
			switch mode {
			case "old-request":
				request.Protocol = "pair-cable-worker/1"
			case "v2-request":
				request.Protocol = "pair-cable-worker/2"
			case "v3-request":
				request.Protocol = "pair-cable-worker/3"
			case "missing-request":
				request.Protocol = ""
			case "unknown-request":
				request.Protocol = "pair-cable-worker/5"
			case "old-runtime":
				plan.Runtime.Protocol = "pair-cable-worker/1"
			case "v2-runtime":
				plan.Runtime.Protocol = "pair-cable-worker/2"
			case "v3-runtime":
				plan.Runtime.Protocol = "pair-cable-worker/3"
			case "unknown-runtime":
				plan.Runtime.Protocol = "pair-cable-worker/5"
			}
			calls := 0
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { calls++; return nil, nil }}
			if worker, err := openCableWorker(context.Background(), client, plan, request, "synthetic-admin-input"); err == nil || worker != nil || calls != 0 {
				t.Fatal("mixed protocol reached session creation")
			}
		})
	}
}

func TestCablePrearmOldBindingStopsReviewBeforeDial(t *testing.T) {
	for _, protocol := range []string{"pair-cable-worker/1", "pair-cable-worker/2", "pair-cable-worker/3"} {
		t.Run(protocol, func(t *testing.T) {
			f := newCableProductFixture(t)
			binding := f.s.workerBinding
			f.s.workerBinding = func(ctx context.Context, target cableprobe.Target) (cableWorkerBinding, error) {
				value, err := binding(ctx, target)
				value.Protocol = protocol
				return value, err
			}
			var calls atomic.Int32
			dial := f.s.m.onboarding.dial
			f.s.m.onboarding.dial = func(ctx context.Context, candidate onboardingCandidate, access onboardingAccess) (*onboardingSSH, error) {
				calls.Add(1)
				return dial(ctx, candidate, access)
			}
			review := f.review()
			if review.Available || calls.Load() != 0 {
				t.Fatal("legacy runtime reached new-probe inspection or fallback")
			}
			for _, row := range review.Permission.Targets {
				if row.WorkerAvailable || !strings.Contains(row.Reason, "protocol v4") {
					t.Fatal("incompatibility was not explained")
				}
			}
		})
	}
}

func TestCablePrearmLegacyInspectionAndCleanupRemainSeparate(t *testing.T) {
	for _, protocol := range []string{"pair-cable-worker/1", "pair-cable-worker/2", "pair-cable-worker/3"} {
		t.Run(protocol, func(t *testing.T) {
			plan, mesh, found := launchTestRuntimePlan(t)
			plan.Runtime.Protocol, found.Protocol = protocol, protocol
			found.MaxSeconds = 10
			client := &onboardingSSH{testRun: func(_ context.Context, command string, input io.Reader) ([]byte, error) {
				if command != onboardingPython(cableRuntimeInspectScript) {
					t.Fatal("unexpected inspection command")
				}
				return json.Marshal(found)
			}}
			approved, err := inspectCableWorker(context.Background(), client, plan, mesh)
			if err != nil || approved.Runtime.Protocol != protocol {
				t.Fatal("legacy cleanup provenance inspection was normalized or rejected")
			}
			found.MaxSeconds = 20
			if _, err := inspectCableWorker(context.Background(), client, plan, mesh); err == nil {
				t.Fatal("legacy runtime incorrectly advertised the current probe window")
			}
			found.MaxSeconds = 10
			plan.WorkerOrigin = "controller-running-image:" + plan.WorkerSHA256
			if !cleanupHistoricalPlanSupported(plan) {
				t.Fatal("legacy original plan no longer supports recovery")
			}
			found.Protocol = cableWorkerProtocol
			if _, err := inspectCableWorker(context.Background(), client, plan, mesh); err == nil {
				t.Fatal("reported version differs from approved legacy binding")
			}
			if strings.Count(cableRootLaunchScript, "b['protocol']!='pair-cable-worker/4'") != 1 || strings.Contains(cableRootLaunchScript, "b['protocol'] not in") || strings.Count(cableCleanupLoaderScript(), "b['protocol'] not in ('pair-cable-worker/1','pair-cable-worker/2','pair-cable-worker/3','pair-cable-worker/4')") != 1 || strings.Contains(cableCleanupLoaderScript(), "'--cable-probe-once'") || strings.Count(cableCleanupLoaderScript(), "'--cable-cleanup-once'") != 1 {
				t.Fatal("probe and cleanup fixed protocol/mode checks overlap")
			}
		})
	}
}

func TestCablePrearmLegacyOriginalAndCompanionReload(t *testing.T) {
	for _, protocol := range []string{"pair-cable-worker/1", "pair-cable-worker/2", "pair-cable-worker/3"} {
		t.Run(protocol, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			stored := x.f.s.runs[x.run.RunID]
			for i := range stored.Plans {
				stored.Plans[i].Runtime.Protocol = protocol
			}
			if err := x.f.s.save(stored); err != nil {
				t.Fatal(err)
			}
			var err error
			x.original, err = os.ReadFile(x.f.s.file(x.run.RunID))
			if err != nil {
				t.Fatal(err)
			}
			binding := x.f.s.workerBinding
			x.f.s.workerBinding = func(ctx context.Context, target cableprobe.Target) (cableWorkerBinding, error) {
				value, err := binding(ctx, target)
				value.Protocol = protocol
				return value, err
			}
			x.authorize()
			review := x.review(t)
			if !review.Available {
				t.Fatal("legacy cleanup inspector was not reviewable")
			}
			if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true); err != nil {
				t.Fatal(err)
			}
			run := x.done(t)
			if run.CleanupConfirmed || run.CleanupRecovery == nil || !run.CleanupRecovery.HoldReleased {
				t.Fatal("separate legacy recovery outcome changed")
			}
			loaded := newCableProductService(x.f.s.m)
			if loaded.recoveryFailed || loaded.held() || !validCleanupRecord(loaded.cleanupRecords[run.RunID], loaded.runs[run.RunID]) {
				t.Fatal("legacy companion or original did not reload")
			}
			after, err := os.ReadFile(x.f.s.file(run.RunID))
			if err != nil || string(after) != string(x.original) {
				t.Fatal("recovery or reload rewrote the original")
			}
		})
	}
}
