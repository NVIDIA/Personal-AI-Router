// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// Only the Go binding envelope is exercised here. No native adapter is invoked.
func mpiRecoveryFixture(t *testing.T) (*diagnosticService, diagnosticProfile, json.RawMessage, diagnosticParticipantRequest) {
	t.Helper()
	d := diagnosticTestService(t)
	clusterDir := t.TempDir()
	clustertrusttest.Join(t, clusterDir, "cluster", "node-a", "node-b")
	d.m.mesh = clustertrust.Open(clusterDir)
	p := bootstrapProfileFixture(t)
	r := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: p.Bootstrap.OperationID, ProfileDigest: profileDigest(p), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	body := map[string]any{"schemaVersion": 1, "recipeId": "pair-two-spark-nccl-socket-smoke-v1", "operationId": r.OperationID, "groupId": r.GroupID, "ownerNodeId": p.OwnerNodeID, "profileDigest": r.ProfileDigest, "createdAt": r.ExpiresAt - 60000, "expiresAt": r.ExpiresAt}
	canonical, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	r.BootstrapPlanDigest = hex.EncodeToString(sum[:])
	body["planDigest"] = r.BootstrapPlanDigest
	plan, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateDiagnosticMPIBinding(p, plan, r, false); err != nil {
		t.Fatal(err)
	}
	return d, p, plan, r
}

func mpiRecoveryWrite(t *testing.T, filename string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func mpiRecoveryLease(t *testing.T, d *diagnosticService, p diagnosticProfile, plan json.RawMessage, r diagnosticParticipantRequest) {
	t.Helper()
	dir := d.runDir(r.OperationID)
	mpiRecoveryWrite(t, filepath.Join(dir, "profile.json"), p)
	mpiRecoveryWrite(t, filepath.Join(dir, "bootstrap-plan.json"), plan)
	mpiRecoveryWrite(t, filepath.Join(dir, "lease.json"), diagnosticLeaseRecord{Request: r, Member: p.Members[0]})
}

func mpiRecoverySnapshot(t *testing.T, d *diagnosticService, p diagnosticProfile, plan json.RawMessage, r diagnosticParticipantRequest) {
	t.Helper()
	mpiRecoveryWrite(t, d.coordinatorBootstrapPath(r.OperationID), diagnosticCoordinatorBootstrap{Owner: "pair-mpi-coordinator-v1", Request: r, Profile: p, Plan: plan})
}

func TestDiagnosticMPIRecoveryCancellationRequiresCompleteRetainedLease(t *testing.T) {
	d, p, plan, original := mpiRecoveryFixture(t)
	mpiRecoveryLease(t, d, p, plan, original)
	if managed, err := d.participantCancellationBinding(original, "node-a"); err != nil || !managed {
		t.Fatalf("original binding lost: %v", err)
	}
	for _, change := range []struct {
		name  string
		apply func(*diagnosticParticipantRequest)
	}{
		{"omitted managed digest", func(r *diagnosticParticipantRequest) { r.BootstrapPlanDigest = "" }},
		{"changed plan", func(r *diagnosticParticipantRequest) { r.BootstrapPlanDigest = strings.Repeat("e", 64) }},
		{"changed expiry", func(r *diagnosticParticipantRequest) { r.ExpiresAt++ }},
		{"changed profile", func(r *diagnosticParticipantRequest) { r.ProfileDigest = strings.Repeat("f", 64) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			r := original
			change.apply(&r)
			if clean, err := d.cancelParticipant(context.Background(), r); err == nil || clean || strings.Contains(err.Error(), "only on Linux") {
				t.Fatalf("binding reached native cancellation: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(d.runDir(r.OperationID), "cancelled")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected binding wrote a cancellation fence")
			}
		})
	}
	if _, err := d.participantCancellationBinding(original, "node-b"); err == nil || !strings.Contains(err.Error(), "retained diagnostic coordinator") {
		t.Fatalf("retained owner was not enforced: %v", err)
	}
}

func TestDiagnosticMPIRecoveryPreLeaseApprovalStillOwnsCancellation(t *testing.T) {
	d, p, plan, r := mpiRecoveryFixture(t)
	mpiRecoverySnapshot(t, d, p, plan, r)
	if managed, err := d.participantCancellationBinding(r, "node-a"); err != nil || managed {
		t.Fatalf("original pre-lease fence lost: %v", err)
	}
	if _, err := d.participantCancellationBinding(r, "node-b"); err == nil {
		t.Fatal("pre-lease coordinator approval lost its owner")
	}
	changed := r
	changed.BootstrapPlanDigest = ""
	if _, err := d.participantCancellationBinding(changed, "node-a"); err == nil {
		t.Fatal("pre-lease managed approval was downgraded")
	}
}

func TestDiagnosticMPIRecoveryLeaseLossRetainsBootstrapHold(t *testing.T) {
	for _, artifact := range []string{"profile.json", "bootstrap-plan.json", "corrupt-lease"} {
		t.Run(artifact, func(t *testing.T) {
			d, _, _, r := mpiRecoveryFixture(t)
			dir := d.runDir(r.OperationID)
			filename := artifact
			if artifact == "corrupt-lease" {
				filename = "lease.json"
			}
			mpiRecoveryWrite(t, filepath.Join(dir, filename), "invalid retained state")
			mpiRecoveryWrite(t, filepath.Join(dir, "cancelled"), "cancelled")
			if _, err := d.participantCancellationBinding(r, ""); err == nil {
				t.Fatal("missing/corrupt lease accepted bootstrap ownership as absent")
			}
			d.recover(context.Background())
			if !d.recoveryFailed {
				t.Fatal("restart forgot incomplete bootstrap ownership")
			}
		})
	}
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native-operation")
	if err := os.Mkdir(nativeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if present, err := diagnosticBootstrapArtifactsPresent(filepath.Join(root, "rank"), nativeRoot); err != nil || !present {
		t.Fatalf("native root alone lost ownership: %v", err)
	}
}

func TestDiagnosticMPIRecoveryCannotRelabelManagedArtifactsAsLegacy(t *testing.T) {
	d, p, plan, r := mpiRecoveryFixture(t)
	mpiRecoveryLease(t, d, p, plan, r)
	r.BootstrapPlanDigest = ""
	mpiRecoveryWrite(t, filepath.Join(d.runDir(r.OperationID), "lease.json"), diagnosticLeaseRecord{Request: r, Member: p.Members[0]})
	mpiRecoveryWrite(t, filepath.Join(d.runDir(r.OperationID), "rank.json"), diagnosticRankRecord{Done: true, Clean: true})
	if _, err := d.participantCancellationBinding(r, ""); err == nil {
		t.Fatal("managed artifacts admitted legacy cleanup")
	}
	d.recover(context.Background())
	if !d.recoveryFailed {
		t.Fatal("clean rank hid managed bootstrap ownership")
	}
}

func TestDiagnosticMPIRecoveryFindsNativeRootsWithoutGoRunDirectory(t *testing.T) {
	d, p, plan, r := mpiRecoveryFixture(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, r.OperationID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.checkBootstrapRoots(context.Background(), root); err == nil {
		t.Fatal("native-only resource root was forgotten")
	}
	mpiRecoveryLease(t, d, p, plan, r)
	if err := d.checkBootstrapRoots(context.Background(), root); err != nil {
		t.Fatalf("retained native binding rejected: %v", err)
	}
}

func TestDiagnosticMPIRecoveryStatusAndCancelUseDurableProfile(t *testing.T) {
	d, p, plan, r := mpiRecoveryFixture(t)
	mpiRecoverySnapshot(t, d, p, plan, r)
	d.profiles = func() ([]diagnosticProfile, error) {
		t.Fatal("managed status/cancel read moving legacy profiles")
		return nil, nil
	}
	op := diagnosticOperation{OperationID: r.OperationID, GroupID: r.GroupID, OwnerNodeID: p.OwnerNodeID, State: "running"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.operations[r.OperationID], d.cancels[r.OperationID] = op, cancel
	request := diagnosticRequest{GroupID: r.GroupID, OperationID: r.OperationID}
	if _, err := d.dispatch(context.Background(), "engine:diagnostic-status", request); err != nil {
		t.Fatal(err)
	}
	if _, err := d.dispatch(context.Background(), "engine:diagnostic-cancel", request); err != nil || ctx.Err() == nil {
		t.Fatalf("durable cancellation unavailable: %v", err)
	}
	if err := os.WriteFile(d.coordinatorBootstrapPath(r.OperationID), []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.operationProfile("engine:diagnostic-status", request); err == nil {
		t.Fatal("malformed managed snapshot fell back to another profile")
	}
}

func TestDiagnosticMPIRecoveryManagedRetryNeverUsesLegacyCleanup(t *testing.T) {
	d, p, _, r := mpiRecoveryFixture(t)
	// Missing coordinator state must hold, even if a legacy participant seam
	// would report clean. The managed retry must never call that seam.
	d.participant = func(context.Context, diagnosticProfile, diagnosticMember, string, diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
		t.Fatal("managed cleanup downgraded to legacy")
		return diagnosticParticipantResult{}, nil
	}
	op := diagnosticOperation{OperationID: r.OperationID, GroupID: r.GroupID, OwnerNodeID: p.OwnerNodeID, State: "failed"}
	d.retryCleanup(context.Background(), p, op)
	if d.operations[r.OperationID].CleanupConfirmed || !d.recoveryFailed {
		t.Fatal("missing managed snapshot was reported clean")
	}
}
