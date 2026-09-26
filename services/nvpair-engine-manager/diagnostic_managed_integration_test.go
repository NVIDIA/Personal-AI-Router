// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDiagnosticManagedLoaderRestoresCompletedBuildAfterCancelledUnpublishedAdoption(t *testing.T) {
	d, run, request := managedFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	d.runtimeTestRun = func(_ context.Context, target diagnosticInspectionTarget, _ onboardingPrivateTarget, input []byte) ([]byte, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return managedFixtureReply(input, target), nil
	}
	done := make(chan error, 1)
	go func() { _, err := d.adoptRuntime(context.Background(), request); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("adoption did not reach the native read boundary")
	}
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.runtimeAction(context.Background(), "engine:diagnostic-runtime-cancel", diagnosticRuntimeAction{OperationID: run.Public.OperationID}); err != nil {
		t.Fatal(err)
	}
	// The original adoption is still paused. Rehydrate exactly the checkpoint
	// written by the real cancellation path, before its defer can repair state.
	restarted := newDiagnosticService(d.m)
	loaded := restarted.runtimeRuns[run.Public.OperationID]
	if restarted.runtimeRecoveryFailed || loaded == nil || loaded.Public.State != "completed" || !loaded.Public.CleanupConfirmed || loaded.Public.Adopted || loaded.Public.Stage != "adoption-not-published" {
		t.Fatal("cancelled adoption stranded its completed build on restart")
	}
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled adoption published")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled adoption did not settle")
	}
}

func TestDiagnosticManagedLoaderReconcilesRegistryBeforeOperationCheckpoint(t *testing.T) {
	d, run, request := managedFixture(t)
	storage := d.managedIO()
	writes := 0
	storage.saveRun = func(value *diagnosticRuntimeRun) error {
		writes++
		if writes == 2 {
			return errors.New("fixture checkpoint lost")
		}
		return d.saveRuntime(value)
	}
	if result, err := d.adoptRuntimeWithIO(context.Background(), request, storage); err == nil || result.Record == nil {
		t.Fatal("fixture did not publish before losing checkpoint")
	}
	restarted := newDiagnosticService(d.m)
	loaded := restarted.runtimeRuns[run.Public.OperationID]
	if restarted.runtimeRecoveryFailed || loaded == nil || !loaded.Public.Adopted || loaded.Public.Stage != "runtime-adopted" {
		t.Fatal("loader lost the durable registry authority")
	}
}

func TestDiagnosticManagedRoutesRejectClientParticipantAndCommandFields(t *testing.T) {
	d, _, _ := managedFixture(t)
	for _, method := range []string{"engine:diagnostic-runtime-adopt", "engine:diagnostic-managed-runtime", "engine:diagnostic-managed-runtimes"} {
		frames := &packageShutdownFrames{frames: make(chan []byte, 4)}
		d.m.codec = NewCodec(frames)
		id := json.RawMessage(`71`)
		d.m.handleDiagnosticRuntime(context.Background(), &Message{ID: &id, Method: method, Params: json.RawMessage(`{"command":"ignored","members":[]}`)})
		var reply Message
		if json.Unmarshal(<-frames.frames, &reply) != nil || reply.Error == nil {
			t.Fatal("managed route accepted untrusted execution fields")
		}
	}
}
