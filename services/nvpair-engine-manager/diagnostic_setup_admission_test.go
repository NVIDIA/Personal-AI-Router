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
)

// All participant I/O uses the existing in-process fixtures and test seams.
func setupFabricAdmissionFixture(t *testing.T, mode string) (*diagnosticService, func() error) {
	t.Helper()
	switch mode {
	case "package-approve", "package-retry":
		d, run := retainedPackageFixture(t)
		if mode == "package-approve" {
			delete(d.packageRuns, run.Public.OperationID)
			d.packageActive = ""
			return d, func() error {
				_, err := d.approvePackages(context.Background(), packageApproval(run.Public.ReviewID))
				return err
			}
		}
		return d, func() error {
			_, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: run.Public.OperationID, Elevation: packageApproval(run.Public.ReviewID).Elevation})
			return err
		}
	case "runtime-approve":
		d, review := runtimeFixture(t)
		return d, func() error {
			_, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: review.ReviewID})
			return err
		}
	case "runtime-retry":
		d, run := retainedRuntimeFixture(t)
		return d, func() error {
			_, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", diagnosticRuntimeAction{OperationID: run.Public.OperationID, ExpectedRevision: run.Public.Revision})
			return err
		}
	case "runtime-adopt":
		d, _, request := managedFixture(t)
		return d, func() error {
			_, err := d.adoptRuntime(context.Background(), request)
			return err
		}
	default:
		t.Fatalf("unknown setup fixture %q", mode)
		return nil, nil
	}
}

func TestDiagnosticSetupFabricHoldPreventsClaims(t *testing.T) {
	for _, mode := range []string{"package-approve", "package-retry", "runtime-approve", "runtime-retry", "runtime-adopt"} {
		t.Run(mode, func(t *testing.T) {
			d, invoke := setupFabricAdmissionFixture(t, mode)
			d.m.exec.fabric = &fabricService{reservation: &fabricReservation{OperationID: newOpID()}}
			var calls atomic.Int32
			spy := func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
				calls.Add(1)
				return nil, errors.New("unexpected participant I/O")
			}
			d.packageTestRun, d.runtimeTestRun = spy, spy
			before, err := json.Marshal([]any{d.packageRuns, d.runtimeRuns, d.packageActive, d.runtimeActive})
			if err != nil {
				t.Fatal(err)
			}
			err = invoke()
			after, marshalErr := json.Marshal([]any{d.packageRuns, d.runtimeRuns, d.packageActive, d.runtimeActive})
			if err == nil || !strings.Contains(err.Error(), "fabric") || calls.Load() != 0 || marshalErr != nil || string(before) != string(after) {
				t.Fatalf("fabric hold changed setup ownership or reached I/O: err=%v calls=%d stateChanged=%t", err, calls.Load(), string(before) != string(after))
			}
			if !d.m.exec.diagnosticMu.TryLock() {
				t.Fatal("rejected setup retained the shared admission lock")
			}
			d.m.exec.diagnosticMu.Unlock()
		})
	}
}

func TestDiagnosticSetupStableFabricReleasesAdmissionBeforeIO(t *testing.T) {
	for _, mode := range []string{"package-approve", "package-retry", "runtime-approve", "runtime-retry", "runtime-adopt"} {
		t.Run(mode, func(t *testing.T) {
			d, invoke := setupFabricAdmissionFixture(t, mode)
			d.m.exec.fabric = &fabricService{runs: map[string]*fabricRunRecord{"applied": {Public: fabricOperation{State: "active"}}}}
			var calls, locked atomic.Int32
			spy := func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
				calls.Add(1)
				if d.m.exec.diagnosticMu.TryLock() {
					d.m.exec.diagnosticMu.Unlock()
				} else {
					locked.Add(1)
				}
				return nil, errors.New("fixture stops before participant effects")
			}
			d.packageTestRun, d.runtimeTestRun = spy, spy
			_ = invoke()
			// Approvals dispatch asynchronously; wait only for these fixture jobs.
			if mode == "package-approve" {
				d.mu.Lock()
				var id string
				for key := range d.packageRuns {
					id = key
				}
				d.mu.Unlock()
				if id == "" {
					t.Fatal("stable fabric blocked package publication")
				}
				_ = waitPackages(t, d, id)
			}
			if mode == "runtime-approve" {
				d.mu.Lock()
				var id string
				for key := range d.runtimeRuns {
					id = key
				}
				d.mu.Unlock()
				if id == "" {
					t.Fatal("stable fabric blocked runtime publication")
				}
				_ = waitRuntime(t, d, id)
			}
			if calls.Load() == 0 || locked.Load() != 0 || d.m.exec.fabric.runs["applied"].Public.State != "active" {
				t.Fatalf("stable fabric or short admission scope regressed: calls=%d locked=%d", calls.Load(), locked.Load())
			}
		})
	}
}

func TestDiagnosticSetupExistingApprovalStatusAndCancelBypassFabricAdmission(t *testing.T) {
	t.Run("packages", func(t *testing.T) {
		d, run := retainedPackageFixture(t)
		d.m.exec.fabric = &fabricService{recoveryFailed: true}
		var cancelled atomic.Bool
		run.cancel = func() { cancelled.Store(true) }
		d.m.exec.diagnosticMu.Lock()
		defer d.m.exec.diagnosticMu.Unlock()
		op, err := d.approvePackages(context.Background(), packageApproval(run.Public.ReviewID))
		if err != nil || op.OperationID != run.Public.OperationID {
			t.Fatalf("existing approval was fenced: %v", err)
		}
		if _, err := d.packageStatus(diagnosticPackageAction{OperationID: op.OperationID}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.packageOperation(context.Background(), "engine:diagnostic-package-cancel", diagnosticPackageAction{OperationID: op.OperationID}); err != nil || !cancelled.Load() {
			t.Fatalf("owner cancellation was fenced: %v", err)
		}
	})
	t.Run("runtime", func(t *testing.T) {
		d, run := retainedRuntimeFixture(t)
		d.m.exec.fabric = &fabricService{recoveryFailed: true}
		var cancelled atomic.Bool
		run.cancel = func() { cancelled.Store(true) }
		d.m.exec.diagnosticMu.Lock()
		defer d.m.exec.diagnosticMu.Unlock()
		op, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: run.Public.ReviewID})
		if err != nil || op.OperationID != run.Public.OperationID {
			t.Fatalf("existing approval was fenced: %v", err)
		}
		if _, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: op.OperationID}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: op.OperationID}); err != nil || !cancelled.Load() {
			t.Fatalf("owner cancellation was fenced: %v", err)
		}
	})
}
