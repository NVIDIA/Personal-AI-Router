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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeFixtureReview(target diagnosticInspectionTarget, id string) []byte {
	worker := sha256.Sum256([]byte(diagnosticRuntimeWorkerPython))
	sources := map[string]any{"nccl": map[string]string{"url": "https://github.com/NVIDIA/nccl.git", "commit": "73cf112295c33aee2b895f329f592f2a9b4b0f97", "tag": "v2.30.7-1"}, "nccl-tests": map[string]string{"url": "https://github.com/NVIDIA/nccl-tests.git", "commit": "b4d5beebca8a76cf01335f724d154b9b9d394d96", "tag": "v2.20.0"}}
	plan := map[string]any{"schemaVersion": 1, "recipeId": diagnosticRuntimeRecipe, "operationId": id, "planDigest": strings.Repeat("b", 64), "workerSha256": hex.EncodeToString(worker[:]), "identity": map[string]any{"nodeId": target.NodeID, "principal": target.Principal, "uid": 1000, "home": "/home/fixture"}, "sources": sources, "mpiPackages": packageFixtureFamily(), "limits": map[string]int{"maxAttempts": 3, "maxBuildSeconds": 1800, "parallelJobs": 2}}
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "action": "review", "state": "reviewed", "effectsApplied": false, "canBuild": true, "runtimeValidated": false, "plan": plan})
	return raw
}

func runtimeFixtureReply(input []byte, target diagnosticInspectionTarget, state string, attempt int) []byte {
	var request map[string]json.RawMessage
	_ = json.Unmarshal(input, &request)
	var action, id string
	_ = json.Unmarshal(request["action"], &action)
	_ = json.Unmarshal(request["operationId"], &id)
	if action == "review" {
		return runtimeFixtureReview(target, id)
	}
	var registration any
	if state == "built" {
		var review struct {
			Plan map[string]any `json:"plan"`
		}
		_ = json.Unmarshal(runtimeFixtureReview(target, id), &review)
		prefix := "/home/fixture/.local/share/pair-nccl-build-v1/" + id + "/attempt-000" + strconv.Itoa(attempt) + "/runtime/"
		registration = map[string]any{"schemaVersion": 1, "kind": "pair-nccl-runtime-candidate-v1", "recipeId": diagnosticRuntimeRecipe, "operationId": id, "planDigest": strings.Repeat("b", 64), "identity": review.Plan["identity"], "sources": review.Plan["sources"], "binary": map[string]any{"path": prefix + "bin/all_reduce_perf", "sha256": strings.Repeat("d", 64), "size": 123, "interpreter": "/lib/ld-linux-aarch64.so.1"}, "ncclLibrary": map[string]any{"path": prefix + "lib/libnccl.so.2", "sha256": strings.Repeat("e", 64), "size": 456}, "linkValidation": "static-elf-resolution-only", "managerAdopted": false, "runtimeValidated": false, "gpuExecuted": false, "mpiExecuted": false}
	}
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "action": action, "operationId": id, "planDigest": strings.Repeat("b", 64), "state": state, "attempt": attempt, "cleanupConfirmed": state != "building" && state != "unknown", "effectsApplied": true, "runtimeValidated": false, "managerAdopted": false, "artifactsValidated": state == "built", "artifactObservedAt": time.Now().UTC().Format(time.RFC3339Nano), "registration": registration, "errorCode": nil})
	return raw
}

func waitRuntime(t *testing.T, d *diagnosticService, id string) diagnosticRuntimeOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		run := d.runtimeRuns[id]
		done := run != nil && !run.busy && run.cancel == nil
		var op diagnosticRuntimeOperation
		if run != nil {
			op = cloneRuntimeOperation(run)
		}
		d.mu.Unlock()
		if done {
			return op
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime fixture did not settle")
	return diagnosticRuntimeOperation{}
}

func runtimeFixture(t *testing.T) (*diagnosticService, diagnosticRuntimeReview) {
	t.Helper()
	d, selection := inspectionFixture(t, "controller")
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		return runtimeFixtureReply(input, target, "built", 1), nil
	}
	review, err := d.reviewRuntime(context.Background(), selection)
	if err != nil || !review.CanBuild {
		t.Fatalf("runtime review=%+v %v", review, err)
	}
	return d, review
}

func TestDiagnosticRuntimeEmbeddedWorkerBytesAndArgumentBudget(t *testing.T) {
	program := diagnosticRuntimeProgram()
	if len("/usr/bin/python3 -I -c "+onboardingQuote(program)) >= 128<<10 {
		t.Fatal("runtime helper exceeds Linux command argument bound")
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("existing Python unavailable")
	}
	// Protocol-only module loading; the fixture name prevents every native entry
	// point. Windows uses a temporary script, not its smaller CLI argument limit.
	script := "__name__='transport_fixture'\n" + program + "\nprint(worker.hashlib.sha256(worker.SHIPPED_SOURCE).hexdigest())\n"
	path := filepath.Join(t.TempDir(), "transport.py")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, python, "-I", path).Output()
	expected := sha256.Sum256([]byte(diagnosticRuntimeWorkerPython))
	if err != nil || strings.TrimSpace(string(raw)) != hex.EncodeToString(expected[:]) {
		t.Fatalf("fixed embedded worker failed: %s %v", raw, err)
	}
}

func TestDiagnosticRuntimeBuildBindsApprovalAndKeepsStaticProofSeparate(t *testing.T) {
	d, review := runtimeFixture(t)
	var builds atomic.Int32
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if bytes.Contains(input, []byte(`"action":"build"`)) {
			builds.Add(1)
		}
		if bytes.Contains(input, []byte("password")) {
			t.Error("build protocol acquired administrator or SSH credentials")
		}
		return runtimeFixtureReply(input, target, "built", 1), nil
	}
	op, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: review.ReviewID})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: review.ReviewID})
	if err != nil || duplicate.OperationID != op.OperationID {
		t.Fatal("build approval duplicated ownership")
	}
	op = waitRuntime(t, d, op.OperationID)
	if op.State != "completed" || !op.CleanupConfirmed || op.Adopted || op.RuntimeValidated || builds.Load() != 2 {
		t.Fatalf("build=%+v calls=%d", op, builds.Load())
	}
	status, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{ReviewID: review.ReviewID})
	if err != nil || status.Operation == nil || status.Operation.OperationID != op.OperationID {
		t.Fatal("lost approval lookup failed")
	}
	if len(d.operations) != 0 || d.reservation != nil {
		t.Fatal("CPU build created a collective reservation")
	}
	journal, _ := os.ReadFile(d.runtimePath(op.OperationID))
	if bytes.Contains(journal, []byte("volatile-ssh-fixture")) {
		t.Fatal("runtime journal retained credentials")
	}
}

func TestDiagnosticRuntimeFailureCleanupPreservesReason(t *testing.T) {
	const failure = "current device account access is unavailable or changed"
	for _, tc := range []struct {
		name, cancelError, wantReason string
		clean                         bool
	}{
		{"clean cancel", "", failure, true},
		{"cancel error", "native cancel failed", "native cancel failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, review := runtimeFixture(t)
			failedNode := review.Targets[0].NodeID
			var cancelled atomic.Bool
			d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				var request struct {
					Action string `json:"action"`
				}
				if err := json.Unmarshal(input, &request); err != nil {
					return nil, err
				}
				state := "built"
				if target.NodeID == failedNode {
					switch request.Action {
					case "build":
						state = "building"
					case "status":
						if cancelled.Load() {
							state = "cancelled"
						} else {
							state = "failed"
						}
					case "cancel":
						state = "cancelled"
						cancelled.Store(true)
					}
				}
				raw := runtimeFixtureReply(input, target, state, 1)
				if target.NodeID == failedNode && request.Action != "build" {
					var receipt map[string]any
					if err := json.Unmarshal(raw, &receipt); err != nil {
						return nil, err
					}
					receipt["cleanupConfirmed"] = cancelled.Load() && tc.clean
					if request.Action == "status" {
						if cancelled.Load() {
							receipt["errorCode"] = ""
						} else {
							receipt["errorCode"] = failure
						}
					} else {
						receipt["errorCode"] = tc.cancelError
					}
					return json.Marshal(receipt)
				}
				return raw, nil
			}
			op, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: review.ReviewID})
			if err != nil {
				t.Fatal(err)
			}
			op = waitRuntime(t, d, op.OperationID)
			row := op.Targets[0]
			var receipt diagnosticRuntimeResult
			if err := json.Unmarshal(row.Receipt, &receipt); err != nil {
				t.Fatal(err)
			}
			if op.State != "failed" || op.CleanupConfirmed != tc.clean || row.State != "cancelled" || row.CleanupConfirmed != tc.clean || row.Reason != tc.wantReason || receipt.Action != "cancel" || receipt.ErrorCode != tc.cancelError {
				t.Fatalf("failure cleanup: operation=%s clean=%t target=%s reason=%q receipt=%s error=%q", op.State, op.CleanupConfirmed, row.State, row.Reason, receipt.Action, receipt.ErrorCode)
			}
			var persisted diagnosticRuntimeRun
			journal, err := os.ReadFile(d.runtimePath(op.OperationID))
			if err != nil || json.Unmarshal(journal, &persisted) != nil || persisted.Public.Targets[0].Reason != tc.wantReason {
				t.Fatalf("failure reason was not retained in the operation journal: %v", err)
			}
			if tc.clean {
				status, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: op.OperationID})
				reason := ""
				if status.Operation != nil {
					reason = status.Operation.Targets[0].Reason
				}
				if err != nil || reason != failure {
					t.Fatalf("neutral status refresh erased the retained failure: reason=%q err=%v", reason, err)
				}
			}
		})
	}
}

func TestDiagnosticRuntimeInFlightStatusRetry(t *testing.T) {
	beforeRequest := errBeforeRemoteCommand.Error() + ": "
	for _, tc := range []struct {
		name, wantState, wantReason  string
		statusErrors                 []string
		beforeRequest                bool
		wantStatusCalls, wantCancels int32
	}{
		{"transient before request", "completed", "", []string{"Wi-Fi link briefly unavailable"}, true, 2, 0},
		{"ambiguous lost reply", "failed", "SSH command reply lost after request", []string{"SSH command reply lost after request"}, false, 1, 1},
		{"persistent before request", "failed", "first status error: " + beforeRequest + "Wi-Fi link briefly unavailable; last status error: " + beforeRequest + "cluster pin changed", []string{"Wi-Fi link briefly unavailable", "peer unreachable", "cluster pin changed"}, true, 3, 1},
		{"bounded long errors", "failed", "first status error: " + beforeRequest + strings.Repeat("f", 957-len(beforeRequest)) + "...; last status error: " + beforeRequest + strings.Repeat("l", 957-len(beforeRequest)) + "...", []string{strings.Repeat("f", 2048), "peer unreachable", strings.Repeat("l", 2048)}, true, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, review := runtimeFixture(t)
			firstNode := review.Targets[0].NodeID
			var statusCalls, cancels atomic.Int32
			d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				var request struct {
					Action string `json:"action"`
				}
				if err := json.Unmarshal(input, &request); err != nil {
					return nil, err
				}
				if target.NodeID != firstNode {
					return runtimeFixtureReply(input, target, "built", 1), nil
				}
				switch request.Action {
				case "build":
					return runtimeFixtureReply(input, target, "building", 1), nil
				case "status":
					attempt := statusCalls.Add(1)
					if attempt <= int32(len(tc.statusErrors)) {
						if tc.beforeRequest {
							return nil, fmt.Errorf("%w: %s", errBeforeRemoteCommand, tc.statusErrors[attempt-1])
						}
						return nil, errors.New(tc.statusErrors[attempt-1])
					}
					return runtimeFixtureReply(input, target, "built", 1), nil
				case "cancel":
					cancels.Add(1)
					return runtimeFixtureReply(input, target, "cancelled", 1), nil
				}
				return nil, errors.New("unexpected runtime action")
			}
			op, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: review.ReviewID})
			if err != nil {
				t.Fatal(err)
			}
			op = waitRuntime(t, d, op.OperationID)
			if op.State != tc.wantState || op.Targets[0].Reason != tc.wantReason || statusCalls.Load() != tc.wantStatusCalls || cancels.Load() != tc.wantCancels {
				t.Fatalf("status retry: state=%s reason=%q status calls=%d cancels=%d", op.State, op.Targets[0].Reason, statusCalls.Load(), cancels.Load())
			}
			if !op.CleanupConfirmed || len(op.Targets[0].Reason) > 2048 {
				t.Fatalf("status retry lost cleanup or exceeded the UI reason bound: clean=%t reason bytes=%d", op.CleanupConfirmed, len(op.Targets[0].Reason))
			}
			if tc.wantCancels == 1 {
				var receipt diagnosticRuntimeResult
				if err := json.Unmarshal(op.Targets[0].Receipt, &receipt); err != nil || receipt.Action != "cancel" || !receipt.CleanupConfirmed {
					t.Fatalf("failed status did not retain clean cancel receipt: action=%s clean=%t err=%v", receipt.Action, receipt.CleanupConfirmed, err)
				}
			}
		})
	}
}

func TestDiagnosticParticipantProgramClassifiesPreExecutionOnly(t *testing.T) {
	d, review := runtimeFixture(t)
	target := d.runtimeReviews[review.ReviewID].Targets[0]
	d.m.onboarding.dial = func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
		return nil, errors.New("Wi-Fi link briefly unavailable")
	}
	_, err := d.participantProgram(context.Background(), target, onboardingPrivateTarget{}, "print(1)", nil)
	if !errors.Is(err, errBeforeRemoteCommand) {
		t.Fatalf("failed dial was not classified before execution: %v", err)
	}
	d.m.onboarding.dial = func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
		return &onboardingSSH{testRun: func(context.Context, string, io.Reader) ([]byte, error) {
			return nil, errors.New("SSH command reply lost after request")
		}}, nil
	}
	_, err = d.participantProgram(context.Background(), target, onboardingPrivateTarget{}, "print(1)", nil)
	if err == nil || errors.Is(err, errBeforeRemoteCommand) {
		t.Fatalf("ambiguous command failure was marked safe to retry: %v", err)
	}
}

func TestDiagnosticRuntimeExplicitCancelClearsStaleReason(t *testing.T) {
	d, run := retainedRuntimeFixture(t)
	for i := range run.Public.Targets {
		run.Public.Targets[i].CleanupConfirmed = true
	}
	run.Public.Targets[0].CleanupConfirmed = false
	run.Public.Targets[0].Reason = "earlier build failure"
	run.Public.CleanupConfirmed = false
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		return runtimeFixtureReply(input, target, "cancelled", 1), nil
	}
	op, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID})
	if err != nil || op.State != "cancelled" || !op.CleanupConfirmed || op.Targets[0].Reason != "" {
		t.Fatalf("explicit cancellation: operation=%s clean=%t reason=%q err=%v", op.State, op.CleanupConfirmed, op.Targets[0].Reason, err)
	}
	status, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: op.OperationID})
	if err != nil || status.Operation == nil || status.Operation.Targets[0].Reason != "" {
		t.Fatalf("explicit cancellation status kept stale reason: %v", err)
	}
}

func TestDiagnosticRuntimeCloseReviewAndShutdownFencePublication(t *testing.T) {
	for _, mode := range []string{"review-close", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			d, review := runtimeFixture(t)
			if mode == "review-close" {
				status, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{ReviewID: review.ReviewID, CloseUnstartedReview: true})
				if err != nil || !status.ReviewClosed {
					t.Fatal("unstarted review closure failed")
				}
			} else {
				d.closePackageAdmission()
			}
			if _, err := d.approveRuntime(diagnosticRuntimeAction{ReviewID: review.ReviewID}); err == nil {
				t.Fatal("closed admission launched a build")
			}
			if len(d.runtimeRuns) != 0 {
				t.Fatal("closed review published runtime ownership")
			}
		})
	}
}

func TestDiagnosticRuntimeRejectsMismatchedAndStaleArtifactClaims(t *testing.T) {
	d, review := runtimeFixture(t)
	target := d.runtimeReviews[review.ReviewID].Targets[0]
	request, _ := json.Marshal(map[string]string{"action": "status", "operationId": review.OperationID})
	raw := runtimeFixtureReply(request, target, "built", 1)
	result, err := runtimeResult(raw, "status", review.OperationID, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	var reviewed diagnosticRuntimeResult
	_ = json.Unmarshal(review.Targets[0].Review, &reviewed)
	plan, err := parseRuntimePlan(reviewed.Plan, target, review.OperationID)
	if err != nil || !validRuntimeRegistration(result, plan) {
		t.Fatal("valid fixture rejected")
	}
	for _, kind := range []string{"stale", "identity", "source", "path", "runtime"} {
		t.Run(kind, func(t *testing.T) {
			bad := result
			switch kind {
			case "stale":
				bad.ArtifactsValidated = false
			case "identity":
				bad.Registration = bytes.ReplaceAll(bad.Registration, []byte(target.NodeID), []byte("foreign-node"))
			case "source":
				bad.Registration = bytes.ReplaceAll(bad.Registration, []byte("73cf112295c33aee2b895f329f592f2a9b4b0f97"), []byte(strings.Repeat("f", 40)))
			case "path":
				bad.Registration = bytes.ReplaceAll(bad.Registration, []byte("/home/fixture/"), []byte("/home/foreign/"))
			case "runtime":
				bad.Registration = bytes.ReplaceAll(bad.Registration, []byte(`"runtimeValidated":false`), []byte(`"runtimeValidated":true`))
			}
			if validRuntimeRegistration(bad, plan) {
				t.Fatal("invalid build evidence accepted")
			}
		})
	}
}

func retainedRuntimeFixture(t *testing.T) (*diagnosticService, *diagnosticRuntimeRun) {
	t.Helper()
	d, review := runtimeFixture(t)
	run := &diagnosticRuntimeRun{SchemaVersion: 1, Owner: "pair-nccl-build-controller-v1", Binding: d.runtimeReviews[review.ReviewID], Public: diagnosticRuntimeOperation{OperationID: review.OperationID, ReviewID: review.ReviewID, GroupID: review.GroupID, State: "failed", Stage: "build-failed", Revision: 10, StartedAt: time.Now().UnixMilli(), CleanupConfirmed: true, Targets: append([]diagnosticRuntimeTarget(nil), review.Targets...)}}
	for i := range run.Public.Targets {
		run.Public.Targets[i].State = "failed"
		run.Public.Targets[i].Attempt = 1
	}
	d.runtimeRuns[run.Public.OperationID] = run
	if err := d.saveRuntime(run); err != nil {
		t.Fatal(err)
	}
	return d, run
}

func TestDiagnosticRuntimeExplicitRetryRevisionCannotBeReplayed(t *testing.T) {
	d, run := retainedRuntimeFixture(t)
	var retries atomic.Int32
	var second atomic.Bool
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		attempt := 1
		if second.Load() {
			attempt = 2
		}
		if bytes.Contains(input, []byte(`"action":"retry"`)) {
			retries.Add(1)
			if !bytes.Contains(input, []byte(`"expectedAttempt":1`)) {
				t.Error("native retry lost its exact attempt binding")
			}
			second.Store(true)
			attempt = 2
		}
		return runtimeFixtureReply(input, target, "failed", attempt), nil
	}
	request := diagnosticRuntimeAction{OperationID: run.Public.OperationID, ExpectedRevision: run.Public.Revision}
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", request); err != nil {
		t.Fatal(err)
	}
	_ = waitRuntime(t, d, run.Public.OperationID)
	before := retries.Load()
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", request); err == nil {
		t.Fatal("old retry revision advanced a later attempt")
	}
	if retries.Load() != before {
		t.Fatal("replayed retry made a native call")
	}
}

func TestDiagnosticRuntimeCancelRevokesRetryDuringStatusBarrier(t *testing.T) {
	d, run := retainedRuntimeFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		if bytes.Contains(input, []byte(`"action":"retry"`)) || bytes.Contains(input, []byte(`"action":"build"`)) {
			t.Error("cancelled retry reached a build effect")
		}
		return runtimeFixtureReply(input, target, "failed", 1), nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", diagnosticRuntimeAction{OperationID: run.Public.OperationID, ExpectedRevision: run.Public.Revision})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retry did not reach its status boundary")
	}
	op, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID})
	if err != nil || op.State != "cancelling" {
		t.Fatalf("cancel=%+v %v", op, err)
	}
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled retry was published")
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not return")
	}
	op = waitRuntime(t, d, run.Public.OperationID)
	if op.State != "cancelled" {
		t.Fatalf("cancelled retry state=%s", op.State)
	}
}

func TestDiagnosticRuntimeOldCleanCancelCannotReleaseAnotherLane(t *testing.T) {
	d, run := retainedRuntimeFixture(t)
	run.Public.State = "completed"
	other := newOpID()
	d.runtimeActive = other
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID}); err != nil {
		t.Fatal(err)
	}
	if d.runtimeActive != other {
		t.Fatal("old cancellation replaced another active build owner")
	}
}

func TestDiagnosticRuntimeRestartPreservesRunningOwnershipAndRejectsMalformedJournal(t *testing.T) {
	d, run := retainedRuntimeFixture(t)
	run.Public.State = "running"
	run.Public.CleanupConfirmed = false
	for i := range run.Public.Targets {
		run.Public.Targets[i].State = "building"
		run.Public.Targets[i].CleanupConfirmed = false
	}
	if err := d.saveRuntime(run); err != nil {
		t.Fatal(err)
	}
	d.m.onboarding = newOnboardingService(d.m)
	restarted := newDiagnosticService(d.m)
	for _, target := range d.m.onboarding.targets {
		if target.candidate.AccessAvailable || target.access.password != "" {
			t.Fatal("restart restored credential material")
		}
	}
	for _, target := range d.m.onboarding.targets {
		target.candidate.AccessAvailable = true
		target.expiresAt = time.Now().Add(time.Minute)
		target.accessGeneration = newOpID()
		target.access = onboardingAccess{user: "fixture", password: "fresh-fixture"}
	}
	restarted.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		return runtimeFixtureReply(input, target, "building", 1), nil
	}
	status, err := restarted.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: run.Public.OperationID})
	if err != nil || status.Operation == nil || status.Operation.State != "running" || status.Operation.CleanupConfirmed || restarted.runtimeActive != run.Public.OperationID {
		t.Fatalf("running recovery=%+v %v", status, err)
	}
	bad := restarted.runtimePath(newOpID())
	if err := os.WriteFile(bad, []byte(`{"operation":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := newDiagnosticService(d.m)
	if !corrupt.runtimeRecoveryFailed {
		t.Fatal("malformed runtime ownership was silently ignored")
	}
}
