// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// Transport-only fixture. Full native plan validation is tested by the fixed
// Python adapter; this fixture never starts a native command or network call.
func mpiTransportFixture(t *testing.T) (*diagnosticService, diagnosticProfile, json.RawMessage, diagnosticParticipantRequest) {
	t.Helper()
	d := diagnosticTestService(t)
	trust := t.TempDir()
	clustertrusttest.Join(t, trust, "cluster", "node-a", "node-b")
	d.m.mesh = clustertrust.Open(trust)
	p := bootstrapProfileFixture(t)
	body := map[string]any{"schemaVersion": 1, "recipeId": "pair-two-spark-nccl-socket-smoke-v1", "operationId": p.Bootstrap.OperationID, "groupId": p.GroupID, "ownerNodeId": p.OwnerNodeID, "profileDigest": profileDigest(p), "createdAt": time.Now().Add(-time.Second).UnixMilli(), "expiresAt": time.Now().Add(time.Minute).UnixMilli()}
	canonical, _ := json.Marshal(body)
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	body["planDigest"] = digest
	raw, _ := json.Marshal(body)
	r := diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: p.Bootstrap.OperationID, ProfileDigest: profileDigest(p), BootstrapPlanDigest: digest, ExpiresAt: body["expiresAt"].(int64)}
	r.ExecutionDeadlineAt = r.ExpiresAt
	return d, p, raw, r
}

func mpiTransportReply(p diagnosticProfile, r diagnosticParticipantRequest, action string) diagnosticMPIResult {
	zero := 0
	return diagnosticMPIResult{SchemaVersion: 1, Action: action, NodeID: p.OwnerNodeID, OperationID: r.OperationID, PlanDigest: r.BootstrapPlanDigest, ProfileDigest: r.ProfileDigest, State: "completed", Output: diagnosticTestOutput(), ExitCode: &zero,
		CleanupConfirmed: true, UnitOwned: true, UnitProcessesGone: true, UnitMetadataRemoved: true, RankCleanupConfirmed: true, AuthorizationRemoved: true, AgentGone: true, PublicArtifactsRemoved: true}
}

func TestDiagnosticMPIPrivateIdentityUsesRawStdinOnly(t *testing.T) {
	d, p, raw, r := mpiTransportFixture(t)
	key := []byte("test-only volatile input, not a private key")
	defer clear(key)
	d.bootstrapTestRun = func(_ context.Context, input io.Reader) ([]byte, error) {
		wire, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		header, tail, ok := bytes.Cut(wire, []byte{'\n'})
		if !ok || !bytes.Equal(tail, key) || bytes.Contains(header, key) {
			t.Fatal("volatile input entered JSON or failed raw stdin transfer")
		}
		var request map[string]json.RawMessage
		if json.Unmarshal(header, &request) != nil || len(request) != 2 || request["action"] == nil || request["plan"] == nil {
			t.Fatal("native request gained a command or credential field")
		}
		return json.Marshal(mpiTransportReply(p, r, "start-coordinator"))
	}
	if _, err := d.mpiNative(context.Background(), "start-coordinator", p, raw, r, key); err != nil {
		t.Fatal(err)
	}
	d.bootstrapTestRun = func(context.Context, io.Reader) ([]byte, error) { return nil, errors.New(string(key)) }
	if _, err := d.mpiNative(context.Background(), "start-coordinator", p, raw, r, key); err == nil || strings.Contains(err.Error(), string(key)) {
		t.Fatal("native failure exposed volatile input")
	}
}

func TestDiagnosticMPIReplyIdentityAndCompleteCleanupAreRequired(t *testing.T) {
	d, p, raw, r := mpiTransportFixture(t)
	for _, mutate := range []func(*diagnosticMPIResult){
		func(v *diagnosticMPIResult) { v.NodeID = "node-b" },
		func(v *diagnosticMPIResult) { v.PlanDigest = strings.Repeat("f", 64) },
		func(v *diagnosticMPIResult) { v.ProfileDigest = strings.Repeat("e", 64) },
		func(v *diagnosticMPIResult) { v.UnitOwned = false },
		func(v *diagnosticMPIResult) { v.UnitProcessesGone = false },
		func(v *diagnosticMPIResult) { v.UnitMetadataRemoved = false },
		func(v *diagnosticMPIResult) { v.RankCleanupConfirmed = false },
		func(v *diagnosticMPIResult) { v.AuthorizationRemoved = false },
		func(v *diagnosticMPIResult) { v.AgentGone = false },
		func(v *diagnosticMPIResult) { v.PublicArtifactsRemoved = false },
	} {
		d.bootstrapTestRun = func(context.Context, io.Reader) ([]byte, error) {
			reply := mpiTransportReply(p, r, "cleanup")
			mutate(&reply)
			return json.Marshal(reply)
		}
		if _, err := d.mpiNative(context.Background(), "cleanup", p, raw, r, nil); err == nil {
			t.Fatal("unbound or incomplete cleanup reply accepted")
		}
	}
}

func TestDiagnosticMPIReviewCertificateGenerationCannotBeReplacedBeforeStart(t *testing.T) {
	d, p, raw, _ := mpiTransportFixture(t)
	// The fixture's original certificate hashes differ from the currently
	// admitted same-principal test identities, simulating repinning after review.
	key := []byte("test-only volatile input")
	_, err := d.startBootstrap(p, raw, key)
	if err == nil || !strings.Contains(err.Error(), "pin changed") {
		t.Fatalf("changed certificate generation reached launch: %v", err)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("rejected start retained the volatile identity")
	}
	if _, err := os.Stat(d.coordinatorBootstrapPath(p.Bootstrap.OperationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("repinned review published coordinator ownership")
	}
	if len(d.cancels) != 0 || len(d.operations) != 0 {
		t.Fatal("repinned review reserved execution")
	}
}

func TestDiagnosticMPICoordinatorSnapshotRetainsOriginalExpiredLease(t *testing.T) {
	d, p, raw, r := mpiTransportFixture(t)
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		t.Fatal("fixture")
	}
	body["createdAt"] = time.Now().Add(-2 * time.Minute).UnixMilli()
	body["expiresAt"] = time.Now().Add(-time.Minute).UnixMilli()
	delete(body, "planDigest")
	canonical, _ := json.Marshal(body)
	sum := sha256.Sum256(canonical)
	body["planDigest"] = hex.EncodeToString(sum[:])
	raw, _ = json.Marshal(body)
	r.ExpiresAt = body["expiresAt"].(int64)
	r.ExecutionDeadlineAt = 0 // Legacy retained snapshot predates execution deadlines.
	r.BootstrapPlanDigest = body["planDigest"].(string)
	filename := d.coordinatorBootstrapPath(r.OperationID)
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(diagnosticCoordinatorBootstrap{Owner: "pair-mpi-coordinator-v1", Profile: p, Plan: raw, Request: r})
	if err := os.WriteFile(filename, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	got, plan, request, err := d.loadCoordinatorBootstrap(r.OperationID)
	if err != nil || profileDigest(got) != profileDigest(p) || !bytes.Equal(plan, raw) || request != r {
		t.Fatalf("cleanup lost original expired lease: %v", err)
	}
	if _, err := validateDiagnosticMPIBinding(got, plan, request, true); err == nil {
		t.Fatal("expired recovery snapshot granted launch authority")
	}
	if err := os.WriteFile(filename, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.loadCoordinatorBootstrap(r.OperationID); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("malformed snapshot became an absent legacy fallback")
	}
}

func TestDiagnosticMPIExecutionDeadlineMatchesSnapshotAndPrepareContext(t *testing.T) {
	for _, test := range []struct {
		name   string
		lease  time.Duration
		parent time.Duration
	}{
		{"fresh review", diagnosticLease, 0},
		{"aged review", time.Minute, 0},
		{"earlier service deadline", diagnosticLease, 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, p, raw, _ := fabricMPIAdmissionFixture(t)
			now := time.Now()
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			expires := now.Add(test.lease).UnixMilli()
			body["createdAt"], body["expiresAt"] = expires-diagnosticLease.Milliseconds(), expires
			delete(body, "planDigest")
			canonical, _ := json.Marshal(body)
			sum := sha256.Sum256(canonical)
			body["planDigest"] = hex.EncodeToString(sum[:])
			raw, _ = json.Marshal(body)
			limit := expires
			if test.parent != 0 {
				var cancel context.CancelFunc
				d.ctx, cancel = context.WithDeadline(d.ctx, now.Add(test.parent))
				defer cancel()
				limit = min(limit, now.Add(test.parent).UnixMilli())
			}
			type observedDeadline struct {
				context int64
				request diagnosticParticipantRequest
				plan    json.RawMessage
				err     error
			}
			observed := make(chan observedDeadline, 1)
			// Stop in the first local prepare before lease writes, transport, or
			// native work. Only temporary storage and the real deadline path run.
			d.preflight = func(ctx context.Context, _ diagnosticProfile, member diagnosticMember, _ string) error {
				deadline, _ := ctx.Deadline()
				_, stored, request, err := d.loadCoordinatorBootstrap(p.Bootstrap.OperationID)
				observed <- observedDeadline{deadline.UnixMilli(), request, stored, err}
				return errors.New("deadline fixture stops before participant effects")
			}
			before := time.Now().Add(diagnosticDeadline).UnixMilli()
			op, err := d.startBootstrap(p, raw, []byte("test-only volatile input"))
			after := time.Now().Add(diagnosticDeadline).UnixMilli()
			if err != nil {
				t.Fatal(err)
			}
			waitDiagnostic(t, d, op.OperationID)
			select {
			case got := <-observed:
				bound := got.request.ExecutionDeadlineAt
				if got.err != nil || bound < min(before, limit) || bound > min(after, limit) || got.context != bound || got.request.ExpiresAt != expires || !bytes.Equal(got.plan, raw) {
					t.Fatalf("deadline lost, widened, or changed the reviewed plan: context=%d request=%+v err=%v", got.context, got.request, got.err)
				}
			default:
				t.Fatal("operation never reached the local deadline observation")
			}
		})
	}
}

func TestDiagnosticMPIExecutionDeadlineRejectsLiveLegacyAndLeaseExtension(t *testing.T) {
	_, p, raw, original := mpiTransportFixture(t)
	for _, deadline := range []int64{0, time.Now().UnixMilli() - 1, original.ExpiresAt + 1} {
		r := original
		r.ExecutionDeadlineAt = deadline
		if _, err := validateDiagnosticMPIBinding(p, raw, r, true); err == nil {
			t.Fatalf("invalid live execution deadline accepted: %d", deadline)
		}
		_, err := validateDiagnosticMPIBinding(p, raw, r, false)
		if (deadline <= original.ExpiresAt) != (err == nil) {
			t.Fatalf("legacy cleanup readability or retained deadline bound changed: %d: %v", deadline, err)
		}
	}
}
