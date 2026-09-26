// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func setRuntimeReviewWorker(t *testing.T, target *diagnosticRuntimeTarget, hash string) {
	t.Helper()
	var review map[string]any
	if err := json.Unmarshal(target.Review, &review); err != nil {
		t.Fatal(err)
	}
	review["plan"].(map[string]any)["workerSha256"] = hash
	raw, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	target.Review = raw
}

func runtimeSourceSignal(t *testing.T, op diagnosticRuntimeOperation) string {
	t.Helper()
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	status, _ := fields["retrySourceStatus"].(string)
	return status
}

func TestDiagnosticRuntimeRetrySourceUsesImmutablePlans(t *testing.T) {
	for _, scenario := range []string{"current", "changed", "missing", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			d, run := retainedRuntimeFixture(t)
			worker := sha256.Sum256([]byte(diagnosticRuntimeWorkerPython))
			for i := range run.Binding.Review.Targets {
				setRuntimeReviewWorker(t, &run.Binding.Review.Targets[i], hex.EncodeToString(worker[:]))
			}
			expected := scenario
			switch scenario {
			case "current":
				setRuntimeReviewWorker(t, &run.Public.Targets[0], strings.Repeat("f", 64))
			case "changed":
				setRuntimeReviewWorker(t, &run.Binding.Review.Targets[1], strings.Repeat("f", 64))
			case "missing":
				run.Binding.Review.Targets = run.Binding.Review.Targets[:1]
				expected = "unknown"
			case "malformed":
				run.Binding.Review.Targets[1].Review = []byte("{}")
				expected = "unknown"
			}
			before, _ := json.Marshal(run)
			op := d.runtimeSnapshot(run)
			after, _ := json.Marshal(run)
			if status := runtimeSourceSignal(t, op); status != expected || !bytes.Equal(before, after) {
				t.Fatalf("source signal=%q want=%q or snapshot mutated retained state", status, expected)
			}
		})
	}
}

func TestDiagnosticRuntimeChangedOrUnknownSourceRetryHasNoEffects(t *testing.T) {
	for _, scenario := range []string{"changed", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			d, run := retainedRuntimeFixture(t)
			if scenario == "changed" {
				setRuntimeReviewWorker(t, &run.Binding.Review.Targets[1], strings.Repeat("f", 64))
			} else {
				run.Binding.Review.Targets[1].Review = []byte("{}")
			}
			calls := 0
			d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				calls++
				return runtimeFixtureReply(input, target, "failed", 1), nil
			}
			before, _ := json.Marshal(run)
			diskBefore, err := os.ReadFile(d.runtimePath(run.Public.OperationID))
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", diagnosticRuntimeAction{OperationID: run.Public.OperationID, ExpectedRevision: run.Public.Revision})
			_ = waitRuntime(t, d, run.Public.OperationID)
			after, _ := json.Marshal(run)
			diskAfter, readErr := os.ReadFile(d.runtimePath(run.Public.OperationID))
			if err == nil || calls != 0 || !bytes.Equal(before, after) || readErr != nil || !bytes.Equal(diskBefore, diskAfter) {
				t.Fatalf("%s source retry changed ownership or reached runner: calls=%d err=%v", scenario, calls, err)
			}
			// Cleanup of an already-settled original operation remains available.
			if op, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID}); err != nil || !op.CleanupConfirmed || runtimeSourceSignal(t, op) != scenario {
				t.Fatalf("source mismatch blocked original cancellation: %+v %v", op, err)
			}
		})
	}
}

func TestDiagnosticRuntimeSourceSignalRefreshKeepsOriginalFailure(t *testing.T) {
	d, run := retainedRuntimeFixture(t)
	setRuntimeReviewWorker(t, &run.Binding.Review.Targets[1], strings.Repeat("f", 64))
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if !bytes.Contains(input, []byte(`"action":"status"`)) {
			t.Fatal("source refresh requested a new build")
		}
		return runtimeFixtureReply(input, target, "failed", 1), nil
	}
	result, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: run.Public.OperationID})
	if err != nil || result.Operation == nil || runtimeSourceSignal(t, *result.Operation) != "changed" || result.Operation.State != "failed" || !result.Operation.CleanupConfirmed || result.Operation.Adopted || result.Operation.OperationID != run.Public.OperationID {
		t.Fatalf("status lost original failure or source hold: %+v %v", result, err)
	}
}
