// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func mpiLookupFixture(t *testing.T, state string, clean bool) (*diagnosticService, *diagnosticMPIReviewBinding, diagnosticMPIRecoveryControl) {
	t.Helper()
	d, p, _, r := mpiRecoveryFixture(t)
	control := diagnosticMPIRecoveryControl{BuildOperationID: strings.Repeat("c", 32)}
	for i := range p.Members {
		pin, ok := d.m.mesh.PinSHA256(p.Members[i].Principal)
		if !ok {
			t.Fatal("missing pin")
		}
		p.Members[i].ClusterPinSHA256 = pin
		control.Members = append(control.Members, diagnosticMPIRecoveryMember{NodeID: p.Members[i].NodeID, Principal: p.Members[i].Principal, ClusterPinSHA256: pin})
	}
	r.ProfileDigest = profileDigest(p)
	body := map[string]any{"schemaVersion": 1, "recipeId": "pair-two-spark-nccl-socket-smoke-v1", "operationId": r.OperationID, "groupId": r.GroupID, "ownerNodeId": p.OwnerNodeID, "profileDigest": r.ProfileDigest, "createdAt": r.ExpiresAt - 60000, "expiresAt": r.ExpiresAt}
	canonical, _ := json.Marshal(body)
	sum := sha256.Sum256(canonical)
	r.BootstrapPlanDigest = hex.EncodeToString(sum[:])
	body["planDigest"] = r.BootstrapPlanDigest
	plan, _ := json.Marshal(body)
	controllerPin, _ := d.m.mesh.PinSHA256("node-b")
	bound := &diagnosticMPIReviewBinding{Controller: "node-b", ControllerPin: controllerPin, Profile: p, Plan: plan, PrivateKey: []byte("nonsecret fixture key"), Public: diagnosticMPIReview{ReviewID: strings.Repeat("d", 32), BuildOperationID: control.BuildOperationID, OperationID: r.OperationID, GroupID: r.GroupID, OwnerNodeID: p.OwnerNodeID, ExpiresAt: r.ExpiresAt}}
	d.mu.Lock()
	err := d.publishMPIReviewLocked(bound, mpiReviewStorage())
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if state != "" {
		marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
		if err != nil {
			t.Fatal(err)
		}
		marker.State = "consumed"
		d.mu.Lock()
		err = d.persistMPIReviewMarker(marker, false, mpiReviewStorage())
		d.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		mpiRecoverySnapshot(t, d, p, plan, r)
		op := mpiReviewTestOperation(bound)
		op.State = state
		op.CleanupConfirmed = clean
		if state == "passed" {
			op.Samples, _ = parseDiagnosticOutput(diagnosticTestOutput())
		}
		if err := d.saveOperation(op); err != nil {
			t.Fatal(err)
		}
	}
	return d, bound, control
}

func TestDiagnosticMPILookupRecoversOriginalUncleanOperationAfterRestart(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "failed", false)
	d.mpiReviews = nil
	d.operations = map[string]diagnosticOperation{}
	d.recoverOperations()
	result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
	if err != nil || result.Reference == nil || result.Operation == nil || result.RecoveryRequired || result.Operation.CleanupConfirmed || result.Operation.OperationID != bound.Public.OperationID {
		t.Fatalf("original unclean operation lost: %+v %v", result, err)
	}
	// A fresh renderer uses these returned selectors. Cancellation is directed
	// to that original operation; this fixture replaces only the live cancel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.cancels[result.Operation.OperationID] = cancel
	_, err = d.dispatch(context.Background(), "engine:diagnostic-cancel", diagnosticRequest{GroupID: result.Reference.GroupID, OperationID: result.Reference.OperationID})
	if err != nil || ctx.Err() == nil || len(d.operations) != 1 {
		t.Fatalf("recovered selectors did not cancel original operation: %v", err)
	}
}

func TestDiagnosticMPILookupLostReplyAndCompletedResultDoNotMintAnotherRun(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "passed", true)
	d.mpiReviews = nil
	d.operations = map[string]diagnosticOperation{}
	for range 2 {
		result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
		if err != nil || result.Reference == nil || result.Reference.OperationID != bound.Public.OperationID || result.Operation == nil || result.Operation.State != "passed" || len(result.Operation.Samples) != 21 {
			t.Fatalf("completed reference lost: %+v %v", result, err)
		}
	}
	if len(d.cancels) != 0 || len(d.mpiReviews) != 0 {
		t.Fatal("read-only lookup admitted another run")
	}
}

func TestDiagnosticMPILookupFailedCleanDoesNotReenterNativeAfterUpgrade(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "failed", true)
	var old diagnosticOperation
	if err := readDiagnosticJSON(d.operationPath(bound.Public.OperationID), &old); err != nil {
		t.Fatal(err)
	}
	old.Message = "native MPI coordinator did not complete the fixed collective"
	if err := d.saveOperation(old); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, path := range []string{d.operationPath(old.OperationID), d.operationPath(old.OperationID) + ".profile", d.coordinatorBootstrapPath(old.OperationID), d.mpiReviewMarkerPath(bound.Public.ReviewID)} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = raw
	}
	d.bootstrapTestRun = func(context.Context, io.Reader) ([]byte, error) {
		t.Fatal("closed historical result reentered native helper")
		return nil, nil
	}
	d.mpiReviews = nil
	d.operations = map[string]diagnosticOperation{}
	d.recoverOperations()
	for range 2 {
		result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
		if err != nil || result.RecoveryRequired || result.Reference == nil || result.Operation == nil || result.Reference.OperationID != old.OperationID || result.Operation.State != "failed" || !result.Operation.CleanupConfirmed || result.Operation.Message != old.Message {
			t.Fatalf("failed-clean history became held, replaced or hidden: %+v %v", result, err)
		}
	}
	// A consumed old approval may recover its existing result; it must never
	// start again or renew the saved plan after helper source changes.
	op, err := d.approveMPIWithIO("node-b", bound.Public.ReviewID, mpiReviewStorage(), func(diagnosticProfile, json.RawMessage, []byte) (diagnosticOperation, error) {
		t.Fatal("old approval replayed the failed collective")
		return diagnosticOperation{}, nil
	})
	if err != nil || op.OperationID != old.OperationID || op.State != "failed" || !op.CleanupConfirmed || len(d.cancels) != 0 || len(d.mpiReviews) != 0 {
		t.Fatalf("old approval changed execution ownership: %+v %v", op, err)
	}
	for path, raw := range before {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("historical operation, plan or review was rewritten")
		}
	}
}

func TestDiagnosticMPILookupUnstartedReviewUsesOriginalPositiveClosure(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "", false)
	d.mpiReviews = nil
	result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
	if err != nil || result.Reference == nil || result.Operation != nil || result.Reference.ReviewID != bound.Public.ReviewID {
		t.Fatalf("unstarted selector lost: %v", err)
	}
	closed, err := d.closeUnstartedMPIReview("node-b", result.Reference.ReviewID)
	if err != nil || !closed.ReviewClosed {
		t.Fatalf("original review did not close: %v", err)
	}
	result, err = d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
	if err != nil || result.Reference != nil || result.Operation != nil || result.RecoveryRequired {
		t.Fatalf("confirmed closed review remained unresolved: %v", err)
	}
}

func TestDiagnosticMPILookupEmptyInventoryPreservesStartupUncertainty(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "", false)
	closed, err := d.closeUnstartedMPIReview("node-b", bound.Public.ReviewID)
	if err != nil || !closed.ReviewClosed {
		t.Fatalf("fixture review did not close: %v", err)
	}
	// Startup raises this fence when native ownership survives a missing or
	// malformed lease. An empty Go inventory cannot prove that ownership gone.
	d.recoveryFailed = true
	result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
	if err != nil || result.Reference != nil || result.Operation != nil || !result.RecoveryRequired {
		t.Fatalf("startup uncertainty became confirmed absence: %+v %v", result, err)
	}
	d.recoveryFailed = false
	result, err = d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
	if err != nil || result.Reference != nil || result.Operation != nil || result.RecoveryRequired {
		t.Fatalf("resolved empty inventory remained uncertain: %+v %v", result, err)
	}
}

func TestDiagnosticMPILookupKnownUncleanOperationRemainsRecoverableWithFence(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "failed", false)
	d.recoveryFailed = true
	result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
	if err != nil || result.Reference == nil || result.Operation == nil || result.RecoveryRequired || result.Operation.CleanupConfirmed || result.Reference.OperationID != bound.Public.OperationID {
		t.Fatalf("known original cancellation identity lost behind fence: %+v %v", result, err)
	}
}

func TestDiagnosticMPILookupDoesNotHideOrphanedOrChangedOwnership(t *testing.T) {
	for _, change := range []string{"marker-missing", "marker-malformed", "pair-changed", "profile-changed"} {
		t.Run(change, func(t *testing.T) {
			d, bound, control := mpiLookupFixture(t, "failed", false)
			switch change {
			case "marker-missing":
				if err := os.Remove(d.mpiReviewMarkerPath(bound.Public.ReviewID)); err != nil {
					t.Fatal(err)
				}
			case "marker-malformed":
				if err := os.WriteFile(d.mpiReviewMarkerPath(bound.Public.ReviewID), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "pair-changed":
				control.Members[1].ClusterPinSHA256 = strings.Repeat("0", 64)
			case "profile-changed":
				if err := os.WriteFile(d.coordinatorBootstrapPath(bound.Public.OperationID), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control); err == nil {
				t.Fatal("lost or changed ownership became an empty successful lookup")
			}
		})
	}
}

func TestDiagnosticMPILookupResponseRequiresExplicitCertaintyAndReference(t *testing.T) {
	_, _, control := mpiLookupFixture(t, "", false)
	for _, raw := range []string{`{}`, `{"reference":null,"operation":null}`, `{"reference":null,"operation":null,"recoveryRequired":null}`, `{"reference":null,"operation":{},"recoveryRequired":false}`} {
		if _, err := parseMPIRecovery([]byte(raw), control); err == nil {
			t.Fatal("incomplete reply became confirmed no-operation")
		}
	}
	if _, err := parseMPIRecovery([]byte(`{"reference":null,"operation":null,"recoveryRequired":false}`), control); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticMPILookupPartialLossNeverBecomesEmptySuccess(t *testing.T) {
	for _, remaining := range []string{"operation-and-profile", "profile-only", "memory-only"} {
		t.Run(remaining, func(t *testing.T) {
			d, bound, control := mpiLookupFixture(t, "failed", false)
			for _, filename := range []string{d.mpiReviewMarkerPath(bound.Public.ReviewID), d.coordinatorBootstrapPath(bound.Public.OperationID)} {
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
			}
			if remaining == "memory-only" {
				d.recoverOperations()
			}
			if remaining != "operation-and-profile" {
				if err := os.Remove(d.operationPath(bound.Public.OperationID)); err != nil {
					t.Fatal(err)
				}
			}
			if remaining == "memory-only" {
				if err := os.Remove(d.operationPath(bound.Public.OperationID) + ".profile"); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control); err == nil {
				t.Fatalf("surviving %s was hidden as %+v", remaining, result)
			}
		})
	}
}

func TestDiagnosticMPILookupPrefersOriginalUncleanOperationOverNewUnstartedReview(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "failed", false)
	marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
	if err != nil {
		t.Fatal(err)
	}
	marker.ReviewID = strings.Repeat("e", 32)
	marker.OperationID = strings.Repeat("f", 32)
	marker.GroupID = "pair-smoke-" + marker.OperationID
	marker.State = "unconsumed"
	marker.ExpiresAt = time.Now().Add(2 * time.Minute).UnixMilli()
	d.mu.Lock()
	err = d.persistMPIReviewMarker(marker, true, mpiReviewStorage())
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
	if err != nil || result.Reference == nil || result.Reference.OperationID != bound.Public.OperationID {
		t.Fatalf("new unstarted review hid original unclean operation: %v", err)
	}
}

func TestDiagnosticMPILookupRejectsContradictoryMarkerBeforeFiltering(t *testing.T) {
	for _, state := range []string{"running", "passed", "bootstrap-only"} {
		t.Run(state, func(t *testing.T) {
			operationState := state
			if state == "bootstrap-only" {
				operationState = "running"
			}
			d, bound, control := mpiLookupFixture(t, operationState, state == "passed")
			if state == "bootstrap-only" {
				for _, path := range []string{d.operationPath(bound.Public.OperationID), d.operationPath(bound.Public.OperationID) + ".profile"} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			}
			marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
			if err != nil {
				t.Fatal(err)
			}
			marker.BuildOperationID = strings.Repeat("e", 32)
			d.mu.Lock()
			err = d.persistMPIReviewMarker(marker, false, mpiReviewStorage())
			d.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control); err == nil {
				t.Fatalf("contradictory marker hid retained %s ownership: %+v", state, result)
			}
		})
	}
}

func TestDiagnosticMPILookupOlderBuildRemainsSeparate(t *testing.T) {
	for _, state := range []string{"", "passed"} {
		t.Run(state, func(t *testing.T) {
			d, _, control := mpiLookupFixture(t, state, state == "passed")
			control.BuildOperationID = strings.Repeat("e", 32)
			result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control)
			if err != nil || result.Reference != nil || result.Operation != nil || result.RecoveryRequired {
				t.Fatalf("valid older build contaminated current inventory: %+v %v", result, err)
			}
		})
	}
}

func TestDiagnosticMPILookupRequestedBuildControllerChangeHolds(t *testing.T) {
	d, bound, control := mpiLookupFixture(t, "running", false)
	marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
	if err != nil {
		t.Fatal(err)
	}
	marker.Controller = "node-a"
	d.mu.Lock()
	err = d.persistMPIReviewMarker(marker, false, mpiReviewStorage())
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control); err == nil {
		t.Fatalf("changed original controller became confirmed absence: %+v", result)
	}
}

func TestDiagnosticMPILookupSnapshotRequiresConsumedMarkerBeforeFiltering(t *testing.T) {
	for _, state := range []string{"unconsumed", "closed"} {
		t.Run(state, func(t *testing.T) {
			d, bound, control := mpiLookupFixture(t, "running", false)
			for _, path := range []string{d.operationPath(bound.Public.OperationID), d.operationPath(bound.Public.OperationID) + ".profile"} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			marker, err := d.readMPIReviewMarker(bound.Public.ReviewID, mpiReviewStorage())
			if err != nil {
				t.Fatal(err)
			}
			marker.State = state
			d.mu.Lock()
			err = d.persistMPIReviewMarker(marker, false, mpiReviewStorage())
			d.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			control.BuildOperationID = strings.Repeat("e", 32)
			if result, err := d.recoverMPIAsCoordinator(context.Background(), "node-b", control); err == nil {
				t.Fatalf("%s marker hid a published snapshot: %+v", state, result)
			}
		})
	}
}
