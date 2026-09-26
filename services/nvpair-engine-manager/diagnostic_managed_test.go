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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func managedFixtureReply(input []byte, target diagnosticInspectionTarget) []byte {
	var request map[string]json.RawMessage
	_ = json.Unmarshal(input, &request)
	var action, id string
	_ = json.Unmarshal(request["action"], &action)
	_ = json.Unmarshal(request["operationId"], &id)
	var review map[string]any
	_ = json.Unmarshal(runtimeFixtureReview(target, id), &review)
	plan := review["plan"].(map[string]any)
	worker := sha256.Sum256([]byte(diagnosticRuntimeWorkerPython))
	plan["workerSha256"] = hex.EncodeToString(worker[:])
	identity := plan["identity"].(map[string]any)
	identity["user"] = "fixture"
	identity["publicIdentitySha256"] = strings.Repeat("a", 64)
	if action == "review" {
		raw, _ := json.Marshal(review)
		return raw
	}
	var reply map[string]any
	_ = json.Unmarshal(runtimeFixtureReply(input, target, "built", 1), &reply)
	registration := reply["registration"].(map[string]any)
	registration["identity"] = identity
	raw, _ := json.Marshal(reply)
	return raw
}

func managedFixture(t *testing.T) (*diagnosticService, *diagnosticRuntimeRun, diagnosticManagedAction) {
	t.Helper()
	d, selection := inspectionFixture(t, "controller")
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		return managedFixtureReply(input, target), nil
	}
	review, err := d.reviewRuntime(context.Background(), selection)
	if err != nil || !review.CanBuild {
		t.Fatalf("managed fixture review: %+v %v", review, err)
	}
	run := &diagnosticRuntimeRun{SchemaVersion: 1, Owner: "pair-nccl-build-controller-v1", Binding: d.runtimeReviews[review.ReviewID], Public: diagnosticRuntimeOperation{OperationID: review.OperationID, ReviewID: review.ReviewID, GroupID: review.GroupID, State: "completed", Stage: "artifacts-built", Revision: 7, StartedAt: time.Now().Add(-time.Minute).UnixMilli(), FinishedAt: time.Now().UnixMilli(), CleanupConfirmed: true, Targets: append([]diagnosticRuntimeTarget(nil), review.Targets...)}}
	for i, target := range run.Binding.Targets {
		input, _ := json.Marshal(map[string]string{"action": "status", "operationId": review.OperationID})
		run.Public.Targets[i].Receipt = managedFixtureReply(input, target)
		run.Public.Targets[i].Attempt = 1
		run.Public.Targets[i].State = "built"
	}
	d.runtimeRuns[review.OperationID] = run
	if err := d.saveRuntime(run); err != nil {
		t.Fatal(err)
	}
	return d, run, diagnosticManagedAction{OperationID: review.OperationID, ExpectedRevision: 7}
}

func TestDiagnosticManagedAdoptionFreshStaticOnlyAndPublicProjection(t *testing.T) {
	d, run, request := managedFixture(t)
	var calls atomic.Int32
	d.runtimeTestRun = func(ctx context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		calls.Add(1)
		var native map[string]json.RawMessage
		_ = json.Unmarshal(input, &native)
		if len(native) != 5 || string(native["action"]) != `"status"` || bytes.Contains(input, []byte("password")) {
			t.Fatalf("adoption sent an effectful or credential-bearing request: %s", input)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 75*time.Second {
			t.Fatal("unbounded adoption read")
		}
		return managedFixtureReply(input, target), nil
	}
	result, err := d.adoptRuntime(context.Background(), request)
	if err != nil || result.Record == nil || !result.Operation.Adopted || !result.Record.Adopted || result.Record.RunAvailable || result.Record.RuntimeValidated || result.Operation.RuntimeValidated || calls.Load() != 2 {
		t.Fatalf("adoption=%+v error=%v calls=%d", result, err, calls.Load())
	}
	if result.Record.ApprovedRevision != 7 || len(result.Record.Targets) != 2 || result.Operation.Stage != "runtime-adopted" || run.busy || d.runtimeActive != "" {
		t.Fatal("adoption lost its revision, pair, or settled ownership")
	}
	for _, target := range result.Record.Targets {
		if bytes.Contains(target.Registration, []byte(`"managerAdopted":true`)) {
			t.Fatal("historical native evidence was relabelled")
		}
	}
	lookup, err := d.getManagedRuntime(request.OperationID)
	if err != nil || lookup.Record == nil {
		t.Fatalf("lookup: %+v %v", lookup, err)
	}
	inventory := d.managedRuntimeInventory()
	if inventory.RecoveryRequired || len(inventory.Records) != 1 {
		t.Fatalf("inventory=%+v", inventory)
	}
	public, _ := json.Marshal(inventory)
	for _, forbidden := range []string{"volatile-ssh-fixture", "accessGeneration", "accessId", "binding", "password"} {
		if bytes.Contains(public, []byte(forbidden)) {
			t.Fatalf("public registry leaked %s", forbidden)
		}
	}
	raw, err := readOnboardingFile(d.managedPath(request.OperationID), 512<<10)
	if err != nil || bytes.Contains(raw, []byte("volatile-ssh-fixture")) || bytes.Contains(raw, []byte("accessGeneration")) {
		t.Fatal("private registry lost bounded secret-free persistence")
	}
	if len(d.operations) != 0 || d.reservation != nil || len(d.cancels) != 0 {
		t.Fatal("adoption created execution or GPU reservation")
	}
}

func TestDiagnosticManagedLostReplyRetainsOriginalRevisionAndOneRegistry(t *testing.T) {
	d, run, request := managedFixture(t)
	first, err := d.adoptRuntime(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	d.runtimeTestRun = func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
		t.Error("idempotent record recovery contacted a participant")
		return nil, errors.New("unexpected")
	}
	d.mu.Lock()
	run.Public.Revision += 4
	d.mu.Unlock()
	second, err := d.adoptRuntime(context.Background(), request)
	if err != nil || second.Record == nil || second.Record.ApprovedRevision != request.ExpectedRevision || second.Record.AdoptedAt != first.Record.AdoptedAt || second.Operation.Revision != first.Operation.Revision+4 {
		t.Fatalf("idempotent recovery=%+v %v", second, err)
	}
	if _, err := d.adoptRuntime(context.Background(), diagnosticManagedAction{OperationID: request.OperationID, ExpectedRevision: run.Public.Revision}); err == nil {
		t.Fatal("replaced the original adoption revision")
	}
	entries, err := os.ReadDir(filepath.Dir(d.managedPath(request.OperationID)))
	if err != nil || len(entries) != 1 {
		t.Fatal("duplicate registry authority")
	}
}

func TestDiagnosticManagedRegistrySurvivesMissingOperationCheckpoint(t *testing.T) {
	d, run, request := managedFixture(t)
	storage := d.managedIO()
	writes := 0
	storage.saveRun = func(run *diagnosticRuntimeRun) error {
		writes++
		if writes == 2 {
			return errors.New("fixture operation checkpoint failed")
		}
		return d.saveRuntime(run)
	}
	result, err := d.adoptRuntimeWithIO(context.Background(), request, storage)
	if err == nil || result.Record == nil || !result.Operation.Adopted {
		t.Fatal("registry publication boundary was not represented")
	}
	raw, err := readOnboardingFile(d.runtimePath(request.OperationID), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var restored diagnosticRuntimeRun
	if json.Unmarshal(raw, &restored) != nil || restored.Public.Adopted {
		t.Fatal("fixture did not retain pre-adoption operation checkpoint")
	}
	adopted, err := d.managedRuntimeAdopted(&restored)
	if err != nil || !adopted {
		t.Fatalf("registry could not reconcile restart: %v", err)
	}
	d.runtimeRuns[request.OperationID] = &restored
	d.runtimeRecoveryFailed = false
	result, err = d.adoptRuntime(context.Background(), request)
	if err != nil || !result.Operation.Adopted || result.Record == nil || result.Record.ApprovedRevision != 7 || run.Public.ReviewID != result.Record.ReviewID {
		t.Fatalf("restart adoption=%+v %v", result, err)
	}
}

func TestDiagnosticManagedFailedPublicationNeverMintsAdoption(t *testing.T) {
	d, run, request := managedFixture(t)
	storage := d.managedIO()
	storage.write = func(string, any) error { return errors.New("fixture registry denied") }
	result, err := d.adoptRuntimeWithIO(context.Background(), request, storage)
	if err == nil || result.Record != nil || result.Operation.Adopted || result.Operation.Stage != "adoption-not-published" || result.Operation.Revision <= request.ExpectedRevision || run.busy || d.runtimeActive != "" {
		t.Fatalf("failed publication=%+v %v", result, err)
	}
	if _, err := os.Lstat(d.managedPath(request.OperationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed registry write left adoption authority")
	}
	if _, err := d.adoptRuntime(context.Background(), request); err == nil {
		t.Fatal("old explicit revision replayed after failed publication")
	}
}

func TestDiagnosticManagedWriteErrorAfterRenameReconcilesExactPublication(t *testing.T) {
	d, _, request := managedFixture(t)
	storage := d.managedIO()
	storage.write = func(path string, value any) error {
		if err := writeManagedFile(path, value); err != nil {
			return err
		}
		return errors.New("fixture lost completion after registry rename")
	}
	result, err := d.adoptRuntimeWithIO(context.Background(), request, storage)
	if err != nil || result.Record == nil || !result.Operation.Adopted || result.Operation.Stage != "runtime-adopted" {
		t.Fatalf("published registry was misclassified as absent: %+v %v", result, err)
	}
}

func TestDiagnosticManagedUnsyncedRenameIsNotRecoveryAuthority(t *testing.T) {
	d, run, request := managedFixture(t)
	storage := d.managedIO()
	storage.sync = func(string) error { return errors.New("fixture durable sync unavailable") }
	storage.write = func(path string, value any) error {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		// Exercise the real atomic rename, then fail its durability confirmation.
		if err := writeJSONAtomic(path, value); err != nil {
			return err
		}
		return storage.sync(path)
	}
	result, err := d.adoptRuntimeWithIO(context.Background(), request, storage)
	if err == nil || result.Record != nil || result.Operation.Adopted || result.Operation.Stage != "adoption-publication-unknown" {
		t.Fatal("unconfirmed rename became successful adoption")
	}
	if _, err := os.Stat(d.managedPath(request.OperationID)); err != nil {
		t.Fatal("fixture did not rename a real record")
	}
	if record, err := d.readManaged(run, storage); err == nil || record != nil {
		t.Fatal("unsynced record was accepted as recovery authority")
	}
	if lookup, err := d.getManagedRuntimeWithIO(request.OperationID, storage); err == nil || lookup.Record != nil {
		t.Fatal("lookup accepted an unsynced rename")
	}
	if adopted, err := d.managedRuntimeAdoptedWithIO(run, storage); err == nil || adopted {
		t.Fatal("loader accessor accepted an unsynced rename")
	}
	if inventory := d.managedRuntimeInventoryWithIO(storage); !inventory.RecoveryRequired || len(inventory.Records) != 0 {
		t.Fatal("inventory hid failed durability or exposed its record")
	}
	d.runtimeTestRun = func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
		t.Error("failed durability recovery contacted a participant")
		return nil, errors.New("unexpected native call")
	}
	if result, err := d.adoptRuntimeWithIO(context.Background(), request, storage); err == nil || result.Record != nil || result.Operation.Adopted {
		t.Fatal("retry accepted unsynced adoption")
	}

	// Reload the actual checkpoint with fresh in-memory flags while keeping the
	// filesystem sync failure. Exercise the same accessor used by the loader.
	raw, err := readOnboardingFile(d.runtimePath(request.OperationID), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var restored diagnosticRuntimeRun
	if json.Unmarshal(raw, &restored) != nil {
		t.Fatal("invalid restart checkpoint")
	}
	restarted := &diagnosticService{m: d.m, ctx: context.Background(), runtimeRuns: map[string]*diagnosticRuntimeRun{request.OperationID: &restored}}
	if result, err := restarted.adoptRuntimeWithIO(context.Background(), request, storage); err == nil || result.Record != nil || restored.Public.Adopted {
		t.Fatal("restart accepted existing record without successful sync")
	}
	if adopted, err := restarted.managedRuntimeAdoptedWithIO(&restored, storage); err == nil || adopted {
		t.Fatal("restart accessor bypassed durability proof")
	}
	if inventory := restarted.managedRuntimeInventoryWithIO(storage); !inventory.RecoveryRequired || len(inventory.Records) != 0 {
		t.Fatal("restart inventory released failed durability")
	}

	// Once synchronization succeeds, the actual product loader may reconcile
	// the existing record. It retains the one original approved revision.
	loaded := newDiagnosticService(d.m)
	if loaded.runtimeRecoveryFailed || loaded.runtimeRuns[request.OperationID] == nil || !loaded.runtimeRuns[request.OperationID].Public.Adopted {
		t.Fatal("successful restart sync could not recover the published record")
	}
	lookup, err := loaded.getManagedRuntime(request.OperationID)
	if err != nil || lookup.Record == nil || lookup.Record.ApprovedRevision != request.ExpectedRevision {
		t.Fatal("recovered commit lost original revision")
	}
	loaded.runtimeRecoveryFailed = true
	if inventory := loaded.managedRuntimeInventory(); !inventory.RecoveryRequired || len(inventory.Records) != 1 {
		t.Fatal("valid record erased the global recovery hold")
	}
}

func TestDiagnosticManagedRejectsChangedArtifactsIdentityPinsAndAccess(t *testing.T) {
	for _, kind := range []string{"hash", "source", "home", "user", "public-identity", "worker", "account", "generation", "pin", "not-built", "stale-revision"} {
		t.Run(kind, func(t *testing.T) {
			d, run, request := managedFixture(t)
			if kind == "pin" {
				run.Binding.Pins[run.Binding.Targets[0].Principal] = strings.Repeat("0", 64)
			}
			if kind == "account" {
				d.m.onboarding.targets[run.Binding.Targets[0].Candidate.CandidateID].access.user = "other"
			}
			if kind == "stale-revision" {
				request.ExpectedRevision--
			}
			if kind == "worker" {
				run.Binding.Review.Targets[0].Review = bytes.ReplaceAll(run.Binding.Review.Targets[0].Review, []byte(`"workerSha256":"`), []byte(`"unusedWorker":"`))
			}
			d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				raw := managedFixtureReply(input, target)
				switch kind {
				case "hash":
					raw = bytes.ReplaceAll(raw, []byte(strings.Repeat("d", 64)), []byte(strings.Repeat("f", 64)))
				case "source":
					raw = bytes.ReplaceAll(raw, []byte("73cf112295c33aee2b895f329f592f2a9b4b0f97"), []byte(strings.Repeat("f", 40)))
				case "home":
					raw = bytes.ReplaceAll(raw, []byte("/home/fixture"), []byte("/home/other"))
				case "user":
					raw = bytes.ReplaceAll(raw, []byte(`"user":"fixture"`), []byte(`"user":"other"`))
				case "public-identity":
					raw = bytes.ReplaceAll(raw, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("f", 64)))
				case "not-built":
					raw = bytes.ReplaceAll(raw, []byte(`"state":"built"`), []byte(`"state":"failed"`))
				case "generation":
					d.m.onboarding.targets[target.Candidate.CandidateID].accessGeneration = newOpID()
				}
				return raw, nil
			}
			result, err := d.adoptRuntime(context.Background(), request)
			if err == nil || result.Record != nil || run.Public.Adopted {
				t.Fatalf("changed %s accepted: %+v %v", kind, result, err)
			}
			if _, err := os.Lstat(d.managedPath(request.OperationID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected adoption wrote registry authority")
			}
		})
	}
}

func TestDiagnosticManagedShutdownAndSetupInterlocks(t *testing.T) {
	for _, kind := range []string{"package", "runtime", "recovery", "shutdown-before", "shutdown-during"} {
		t.Run(kind, func(t *testing.T) {
			d, run, request := managedFixture(t)
			switch kind {
			case "package":
				d.packageActive = "other"
			case "runtime":
				d.runtimeActive = "other"
			case "recovery":
				d.runtimeRecoveryFailed = true
			case "shutdown-before":
				d.closePackageAdmission()
			}
			calls := 0
			d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				calls++
				if !run.busy || d.runtimeActive != request.OperationID || run.Public.Stage != "adopting-runtime" {
					t.Fatal("adoption did not reserve its lane before reads")
				}
				if kind == "shutdown-during" {
					d.closePackageAdmission()
				}
				return managedFixtureReply(input, target), nil
			}
			result, err := d.adoptRuntime(context.Background(), request)
			if err == nil || result.Record != nil || run.Public.Adopted {
				t.Fatal("interlock allowed adoption")
			}
			if kind != "shutdown-during" && calls != 0 {
				t.Fatal("held admission contacted participants")
			}
		})
	}
}

func TestDiagnosticManagedInventoryRejectsForeignCorruptOrAlteredRecords(t *testing.T) {
	d, run, request := managedFixture(t)
	if _, err := d.adoptRuntime(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	path := d.managedPath(request.OperationID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"owner", "artifact", "binding", "flags"} {
		var disk diagnosticManagedDisk
		_ = json.Unmarshal(raw, &disk)
		switch kind {
		case "owner":
			disk.Owner = "foreign"
		case "artifact":
			disk.Record.Targets[0].Registration = bytes.ReplaceAll(disk.Record.Targets[0].Registration, []byte(strings.Repeat("d", 64)), []byte(strings.Repeat("f", 64)))
		case "binding":
			disk.Binding.Controller = "other"
		case "flags":
			disk.Record.RunAvailable = true
		}
		if err := writeJSONAtomic(path, disk); err != nil {
			t.Fatal(err)
		}
		if adopted, err := d.managedRuntimeAdopted(run); err == nil || adopted {
			t.Fatalf("registry %s drift accepted", kind)
		}
		inventory := d.managedRuntimeInventory()
		if !inventory.RecoveryRequired || len(inventory.Records) != 0 {
			t.Fatal("corrupt registry exposed as available")
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.getManagedRuntime("../other"); err == nil {
		t.Fatal("caller-selected path accepted")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "foreign.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if !d.managedRuntimeInventory().RecoveryRequired {
		t.Fatal("foreign registry entry was ignored")
	}
}

func TestDiagnosticManagedCancellationEndsOnlyAdoptionAndKeepsCompletedBuild(t *testing.T) {
	d, run, request := managedFixture(t)
	t.Cleanup(d.closePackageAdmission)
	started := make(chan struct{})
	d.runtimeTestRun = func(ctx context.Context, _ diagnosticInspectionTarget, _ onboardingPrivateTarget, _ []byte) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan diagnosticManagedAdoption, 1)
	go func() { result, _ := d.adoptRuntime(context.Background(), request); done <- result }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("adoption fixture did not start")
	}
	status, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: request.OperationID})
	if err != nil || status.Operation.Stage != "adopting-runtime" {
		t.Fatal("overlapping status erased live adoption")
	}
	if _, err := d.adoptRuntime(context.Background(), request); err == nil {
		t.Fatal("overlapping duplicate adoption was admitted")
	}
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: request.OperationID}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Record != nil || result.Operation.Adopted || result.Operation.State != "completed" || result.Operation.Stage != "adoption-not-published" || !result.Operation.CleanupConfirmed || run.busy || d.runtimeActive != "" {
			t.Fatalf("cancelled adoption stranded build: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled adoption did not settle")
	}
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		return managedFixtureReply(input, target), nil
	}
	request.ExpectedRevision = run.Public.Revision
	result, err := d.adoptRuntime(context.Background(), request)
	if err != nil || result.Record == nil {
		t.Fatalf("fresh explicit adoption was stranded after cancellation: %v", err)
	}
}

func TestDiagnosticManagedFreshUnknownCleanupKeepsRuntimeOwnershipHeld(t *testing.T) {
	d, run, request := managedFixture(t)
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		var reply map[string]any
		_ = json.Unmarshal(managedFixtureReply(input, target), &reply)
		reply["state"] = "cleanup-unknown"
		reply["cleanupConfirmed"] = false
		reply["artifactsValidated"] = false
		reply["artifactObservedAt"] = nil
		reply["registration"] = nil
		raw, _ := json.Marshal(reply)
		return raw, nil
	}
	result, err := d.adoptRuntime(context.Background(), request)
	if err == nil || result.Record != nil || result.Operation.CleanupConfirmed || result.Operation.State != "failed" || result.Operation.Targets[0].CleanupConfirmed || d.runtimeActive != request.OperationID || run.busy {
		t.Fatalf("new cleanup uncertainty was released: %+v %v", result, err)
	}
	request.ExpectedRevision = run.Public.Revision
	if _, err := d.adoptRuntime(context.Background(), request); err == nil {
		t.Fatal("cleanup-held adoption was admitted")
	}
}

func TestDiagnosticManagedLargeRecordStrictRoundTripAndCapacity(t *testing.T) {
	d, run, request := managedFixture(t)
	var review map[string]any
	_ = json.Unmarshal(run.Binding.Review.Targets[0].Review, &review)
	// A native plan may retain substantial bounded prerequisite/tool metadata.
	review["plan"].(map[string]any)["tools"] = map[string]any{"fixtureMetadata": strings.Repeat("p", 70<<10)}
	run.Binding.Review.Targets[0].Review, _ = json.Marshal(review)
	result, err := d.adoptRuntime(context.Background(), request)
	if err != nil || result.Record == nil {
		t.Fatalf("large bounded record adoption: %v", err)
	}
	path := d.managedPath(request.OperationID)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) <= 64<<10 || len(raw) > 512<<10 {
		t.Fatalf("fixture record size=%d: %v", len(raw), err)
	}
	if lookup, err := d.getManagedRuntime(request.OperationID); err != nil || lookup.Record == nil {
		t.Fatalf("writer/reader size bounds differ: %v", err)
	}
	for _, suffix := range []string{"{}", ` ,"unknown":true`} {
		bad := append(append([]byte(nil), raw...), []byte(suffix)...)
		if err := os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := d.getManagedRuntime(request.OperationID); err == nil {
			t.Fatal("registry accepted trailing input")
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	for i := 1; i < diagnosticManagedLimit; i++ {
		if err := os.WriteFile(filepath.Join(dir, strings.Repeat("a", i)+".fixture"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeManagedFile(filepath.Join(dir, strings.Repeat("0", 32)+".json"), map[string]any{"fixture": true}); err == nil {
		t.Fatal("65th registry entry passed publication limit")
	}
}
