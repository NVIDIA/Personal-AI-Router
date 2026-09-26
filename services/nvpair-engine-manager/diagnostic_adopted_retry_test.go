// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// This uses the frozen managedFixture and runtimeTestRun seam only. No native
// helper, subprocess, socket, package, or build worker is invoked by the case.
func TestDiagnosticManagedRegistryFencesLaterBuildRetry(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprintf("registered=%t", registered), func(t *testing.T) {
			d, run, request := managedFixture(t)
			var registryBefore []byte
			if registered {
				result, err := d.adoptRuntime(context.Background(), request)
				if err != nil || result.Record == nil || !result.Operation.Adopted {
					t.Fatalf("fixture adoption failed: %v", err)
				}
				registryBefore, err = os.ReadFile(d.managedPath(request.OperationID))
				if err != nil {
					t.Fatal(err)
				}
			}
			changedNode := run.Binding.Targets[0].NodeID
			var retries atomic.Int32
			d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
				var native struct {
					Action string `json:"action"`
				}
				if err := json.Unmarshal(input, &native); err != nil {
					return nil, err
				}
				if target.NodeID != changedNode {
					return managedFixtureReply(input, target), nil
				}
				switch native.Action {
				case "status":
					// Match validate_built's stale artifact response: original attempt,
					// clean owned job, failed static evidence, no registration.
					var reply map[string]any
					_ = json.Unmarshal(runtimeFixtureReply(input, target, "failed", 1), &reply)
					reply["artifactObservedAt"] = nil
					reply["errorCode"] = "stored_runtime_changed"
					return json.Marshal(reply)
				case "retry":
					retries.Add(1)
					// Only fake the next accepted attempt; no directory or worker is
					// created. Its descriptor differs exactly by attempt/path.
					raw := managedFixtureReply(input, target)
					raw = bytes.ReplaceAll(raw, []byte(`"attempt":1`), []byte(`"attempt":2`))
					return bytes.ReplaceAll(raw, []byte("attempt-0001/"), []byte("attempt-0002/")), nil
				default:
					return nil, fmt.Errorf("unexpected mocked action %q", native.Action)
				}
			}
			status, err := d.runtimeStatus(context.Background(), diagnosticRuntimeAction{OperationID: request.OperationID})
			if err != nil || status.Operation == nil || status.Operation.State != "failed" || !status.Operation.CleanupConfirmed || status.Operation.Adopted != registered {
				t.Fatalf("stale artifact status did not reach retry boundary: %+v %v", status.Operation, err)
			}
			_, retryErr := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", diagnosticRuntimeAction{OperationID: request.OperationID, ExpectedRevision: status.Operation.Revision})
			settled := waitRuntime(t, d, request.OperationID)
			if !registered {
				if retryErr != nil || retries.Load() != 1 || settled.State != "completed" || settled.Adopted {
					t.Fatalf("unadopted build retry regressed: err=%v retryCalls=%d state=%s adopted=%t", retryErr, retries.Load(), settled.State, settled.Adopted)
				}
				return
			}
			registryAfter, err := os.ReadFile(d.managedPath(request.OperationID))
			if err != nil || !bytes.Equal(registryBefore, registryAfter) {
				t.Fatal("immutable registry bytes changed")
			}
			lookup, lookupErr := d.getManagedRuntime(request.OperationID)
			if retryErr == nil || retries.Load() != 0 {
				t.Errorf("registered operation admitted another build: retryErr=%v retryCalls=%d state=%s adopted=%t attempt=%d lookupRecord=%t lookupErr=%v", retryErr, retries.Load(), settled.State, settled.Adopted, settled.Targets[0].Attempt, lookup.Record != nil, lookupErr)
			}
		})
	}
}

func TestDiagnosticManagedRegistryPresenceFencesRetryWithoutPublishedFlag(t *testing.T) {
	d, run, request := managedFixture(t)
	path := d.managedPath(request.OperationID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unconfirmed registry publication"), 0600); err != nil {
		t.Fatal(err)
	}
	run.Public.State, run.Public.Adopted = "failed", false
	revision := run.Public.Revision
	d.runtimeTestRun = func(context.Context, diagnosticInspectionTarget, onboardingPrivateTarget, []byte) ([]byte, error) {
		t.Error("registry ownership must reject before native IO")
		return nil, errors.New("unexpected IO")
	}
	_, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-retry", diagnosticRuntimeAction{OperationID: request.OperationID, ExpectedRevision: revision})
	if err == nil || run.Public.Revision != revision || run.busy || run.cancel != nil {
		t.Fatal("uncertain registry ownership consumed or started a retry")
	}
}
