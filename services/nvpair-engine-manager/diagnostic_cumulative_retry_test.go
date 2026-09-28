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
	"os"
	"sync/atomic"
	"testing"
)

func TestDiagnosticCumulativeAADWorkerRejectsFBEApprovalWithoutChangingOriginalOperation(t *testing.T) {
	// These are the actual admitted predecessor and CPU-repair worker bytes.
	// The test runs only the Go lifecycle with its existing fake worker seam.
	const oldWorker = "04905d1b06199b6274040349fae36f2db65e7a4ee3adcb0d7d39b6a03aeef288"
	const currentWorker = "538f073fee0694a6fdd2e420ca734a7094857dd50a1428934b4c1adde079a3b4"
	const originalFailure = "make_failed"
	actual := sha256.Sum256([]byte(diagnosticRuntimeWorkerPython))
	if hex.EncodeToString(actual[:]) != currentWorker {
		t.Fatal("cumulative candidate did not embed the admitted CPU-repair worker")
	}
	d, run := retainedRuntimeFixture(t)
	for i := range run.Binding.Review.Targets {
		setRuntimeReviewWorker(t, &run.Binding.Review.Targets[i], oldWorker)
		// A mutable public projection cannot replace the approved worker identity.
		setRuntimeReviewWorker(t, &run.Public.Targets[i], currentWorker)
		run.Public.Targets[i].Reason = originalFailure
	}
	if err := d.saveRuntime(run); err != nil {
		t.Fatal(err)
	}
	if op := d.runtimeSnapshot(run); op.RetrySourceStatus != "changed" || op.State != "failed" {
		t.Fatalf("old approved worker was not distinguished from the new projection: %+v", op)
	}
	before, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	diskBefore, err := os.ReadFile(d.runtimePath(run.Public.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	activeBefore := d.runtimeActive
	var calls atomic.Int32
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		calls.Add(1)
		var request struct {
			Action      string `json:"action"`
			OperationID string `json:"operationId"`
		}
		if json.Unmarshal(input, &request) != nil || request.Action != "status" || request.OperationID != run.Public.OperationID {
			return nil, errors.New("cumulative fixture rejects new work or changed operation identity")
		}
		var result map[string]any
		if err := json.Unmarshal(runtimeFixtureReply(input, target, "failed", 1), &result); err != nil {
			return nil, err
		}
		result["errorCode"] = originalFailure
		return json.Marshal(result)
	}
	op, retryErr := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", diagnosticRuntimeAction{OperationID: run.Public.OperationID, ExpectedRevision: run.Public.Revision})
	_ = waitRuntime(t, d, run.Public.OperationID)
	after, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	diskAfter, err := os.ReadFile(d.runtimePath(run.Public.OperationID))
	if retryErr == nil || op.RetrySourceStatus != "changed" || calls.Load() != 0 || !bytes.Equal(before, after) || err != nil || !bytes.Equal(diskBefore, diskAfter) || d.runtimeActive != activeBefore {
		t.Fatalf("old-source retry reached the runner or changed retained ownership: calls=%d err=%v read=%v", calls.Load(), retryErr, err)
	}
	status, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: run.Public.OperationID})
	if err != nil || status.Operation == nil || calls.Load() != int32(len(run.Binding.Targets)) {
		t.Fatalf("original-operation status was unavailable: calls=%d err=%v", calls.Load(), err)
	}
	if got := status.Operation; got.OperationID != run.Public.OperationID || got.ReviewID != run.Public.ReviewID || got.State != "failed" || !got.CleanupConfirmed || got.Adopted || got.RetrySourceStatus != "changed" {
		t.Fatalf("status lost the original failed operation: %+v", got)
	}
	for _, target := range status.Operation.Targets {
		if target.State != "failed" || target.Attempt != 1 || target.Reason != originalFailure {
			t.Fatalf("status lost the original worker failure or attempt: %+v", target)
		}
	}
	statusCalls := calls.Load()
	cancelled, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID})
	if err != nil || cancelled.OperationID != run.Public.OperationID || cancelled.State != "failed" || !cancelled.CleanupConfirmed || cancelled.RetrySourceStatus != "changed" || calls.Load() != statusCalls {
		t.Fatalf("source mismatch blocked or changed settled original cancellation: %+v %v", cancelled, err)
	}
}
