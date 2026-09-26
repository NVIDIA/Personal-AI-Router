// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type packageShutdownFrames struct {
	frames   chan []byte
	readGate <-chan struct{}
}

func (f *packageShutdownFrames) Read([]byte) (int, error) {
	if f.readGate != nil {
		<-f.readGate
	}
	return 0, io.EOF
}
func (f *packageShutdownFrames) Write(data []byte) (int, error) {
	f.frames <- append([]byte(nil), data...)
	return len(data), nil
}

func preparePackageShutdown(t *testing.T, d *diagnosticService) {
	t.Helper()
	if len(d.m.exec.engines) != 0 {
		t.Fatal("shutdown fixture must not own any engine process")
	}
	frames := &packageShutdownFrames{frames: make(chan []byte, 4)}
	d.m.codec = NewCodec(frames)
	id := json.RawMessage(`91`)
	d.m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: prepareShutdownMethod})
	select {
	case raw := <-frames.frames:
		var reply Message
		if json.Unmarshal(raw, &reply) != nil || reply.ID == nil || string(*reply.ID) != "91" || reply.Error != nil {
			t.Fatalf("invalid prepare-shutdown acknowledgement: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prepare-shutdown did not acknowledge")
	}
}

func TestDiagnosticPackageShutdownFencesDelayedApprovalPublication(t *testing.T) {
	for _, mode := range []string{"prepare-ack", "manager-shutdown-snapshot"} {
		t.Run(mode, func(t *testing.T) {
			d, selection := inspectionFixture(t, "controller")
			var effects atomic.Int32
			d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				if bytes.Contains(input, []byte(`"action":"review"`)) {
					return packageFixtureReview(target, false), nil
				}
				effects.Add(1)
				var request map[string]json.RawMessage
				_ = json.Unmarshal(input, &request)
				return packageFixtureResult(request, true), nil
			}
			review, err := d.reviewPackages(context.Background(), selection)
			if err != nil || !review.CanApprove {
				t.Fatalf("fixture review: %+v %v", review, err)
			}
			// The existing access-owner mutex holds the submitted approval before its
			// final publication, without adding a production test seam.
			d.m.onboarding.mu.Lock()
			submitted := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(submitted)
				_, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID))
				done <- err
			}()
			<-submitted
			if mode == "prepare-ack" {
				preparePackageShutdown(t, d)
			} else {
				d.shutdown()
			}
			d.m.onboarding.mu.Unlock()
			select {
			case err := <-done:
				if err == nil {
					t.Error("delayed approval published after shutdown closed/snapshotted admission")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("delayed approval did not settle")
			}
			d.mu.Lock()
			published := len(d.packageRuns)
			ids := []string{}
			for id := range d.packageRuns {
				ids = append(ids, id)
			}
			d.mu.Unlock()
			for _, id := range ids {
				_ = waitPackages(t, d, id)
			}
			if published != 0 || effects.Load() != 0 {
				t.Fatalf("shutdown admitted %d package jobs and %d mocked effects", published, effects.Load())
			}
		})
	}
}

func TestDiagnosticPackageShutdownFencesRetryAfterReconciliationBarrier(t *testing.T) {
	d, run := retainedPackageFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var effects atomic.Int32
	d.packageTestRun = func(_ context.Context, _ diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		if bytes.Contains(input, []byte(`"action":"reconcile"`)) {
			close(entered)
			<-release
			return bytes.Replace(packageFixtureResult(request, false), []byte(`"state":"cancelled"`), []byte(`"state":"failed"`), 1), nil
		}
		effects.Add(1)
		return packageFixtureResult(request, true), nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: run.Public.OperationID, Elevation: packageApproval(run.Public.ReviewID).Elevation})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not reach existing reconciliation boundary")
	}
	preparePackageShutdown(t, d)
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Error("delayed retry published after prepare-shutdown acknowledgement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not settle")
	}
	settled := waitPackages(t, d, run.Public.OperationID)
	if effects.Load() != 0 || settled.State == "running" || settled.State == "completed" {
		t.Fatalf("shutdown resumed work: effects=%d state=%s", effects.Load(), settled.State)
	}
	// Terminal admission is not permission to forget previously retained work.
	status, err := d.packageStatus(diagnosticPackageAction{OperationID: run.Public.OperationID})
	if err != nil || status.Operation == nil || status.Operation.OperationID != run.Public.OperationID {
		t.Fatal("shutdown discarded retained recovery ownership")
	}
}

func TestDiagnosticPackageShutdownPreservesRetainedCleanup(t *testing.T) {
	d, run := retainedPackageFixture(t)
	var cleanups atomic.Int32
	d.packageTestRun = func(_ context.Context, _ diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if !bytes.Contains(input, []byte(`"action":"cancel"`)) {
			t.Error("shutdown recovery attempted new package work")
		}
		cleanups.Add(1)
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		return packageFixtureResult(request, false), nil
	}
	preparePackageShutdown(t, d)
	op, err := d.packageOperation(context.Background(), "engine:diagnostic-package-cancel", diagnosticPackageAction{OperationID: run.Public.OperationID, Elevation: packageApproval(run.Public.ReviewID).Elevation})
	if err != nil || op.OperationID != run.Public.OperationID || !op.CleanupConfirmed || op.State != "cancelled" || cleanups.Load() != 1 {
		t.Fatalf("retained cleanup was lost: %+v %v calls=%d", op, err, cleanups.Load())
	}
	if _, err := d.retryPackages(context.Background(), diagnosticPackageAction{OperationID: op.OperationID, Elevation: packageApproval(run.Public.ReviewID).Elevation}); err == nil {
		t.Fatal("shutdown reopened retry admission")
	}
}

func TestDiagnosticPackageShutdownUsesManagerRunContextBeforeReadLoopExits(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, _ []byte) ([]byte, error) {
		return packageFixtureReview(target, false), nil
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	readGate := make(chan struct{})
	defer func() {
		select {
		case <-readGate:
		default:
			close(readGate)
		}
	}()
	frames := &packageShutdownFrames{frames: make(chan []byte, 8), readGate: readGate}
	d.m.codec = NewCodec(frames)
	done := make(chan error, 1)
	go func() { done <- d.m.Run(context.Background()) }()
	select {
	case raw := <-frames.frames:
		if !bytes.Contains(raw, []byte(`"method":"engine:ready"`)) {
			t.Fatal("manager readiness was not observed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not become ready")
	}
	d.m.cancel()
	d.mu.Lock()
	ownedContextCancelled := d.ctx.Err() != nil
	d.mu.Unlock()
	if !ownedContextCancelled {
		t.Error("package work is not bound to Manager.Run cancellation while input remains blocked")
	}
	if _, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID)); err == nil {
		t.Error("cancelled Manager.Run admitted a package job before read-loop exit")
	}
	d.mu.Lock()
	published := len(d.packageRuns)
	d.mu.Unlock()
	if published != 0 {
		t.Error("cancelled manager published package ownership")
	}
	close(readGate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not finish after releasing input")
	}
}

func TestDiagnosticPackageSignalFailureSurvivesProductStatus(t *testing.T) {
	d, selection := inspectionFixture(t, "controller")
	d.packageTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		if bytes.Contains(input, []byte(`"action":"review"`)) {
			return packageFixtureReview(target, false), nil
		}
		var request map[string]json.RawMessage
		_ = json.Unmarshal(input, &request)
		var reply map[string]any
		_ = json.Unmarshal(packageFixtureResult(request, false), &reply)
		reply["state"] = "failed"
		reply["archiveReceipt"] = map[string]any{"schemaVersion": 1, "operationId": reply["operationId"], "planDigest": reply["planDigest"], "state": "failed", "phase": "install", "downloadExitCode": 0, "installExitCode": -15, "errorCode": "archive_install_failed"}
		return json.Marshal(reply)
	}
	review, err := d.reviewPackages(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.approvePackages(context.Background(), packageApproval(review.ReviewID))
	if err != nil {
		t.Fatal(err)
	}
	op = waitPackages(t, d, op.OperationID)
	status, err := d.packageStatus(diagnosticPackageAction{OperationID: op.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(status)
	if op.State != "failed" || op.RuntimeValidated || !op.CleanupConfirmed || !bytes.Contains(raw, []byte(`"installExitCode":-15`)) {
		t.Fatal("product status lost or promoted the signal-terminated installation evidence")
	}
}
