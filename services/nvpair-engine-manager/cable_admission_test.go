// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
)

func TestCableApprovalCancellationAfterInitialAdmissionHasNoEffects(t *testing.T) {
	f := newCableProductFixture(t)
	review := f.ready()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.s.m.onboarding.mu.Lock()
	released := false
	defer func() {
		if !released {
			f.s.m.onboarding.mu.Unlock()
		}
	}()
	finished := make(chan error, 1)
	go func() {
		_, err := f.s.start(ctx, cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true})
		finished <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for f.s.m.exec.diagnosticMu.TryLock() {
		f.s.m.exec.diagnosticMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("approval did not enter diagnostic admission")
		}
		time.Sleep(time.Millisecond)
	}
	// Admission has begun, but account validation cannot finish. Cancellation
	// must still stop publication now that no inventory HTTP read observes it.
	cancel()
	f.s.m.onboarding.mu.Unlock()
	released = true
	select {
	case err := <-finished:
		var refusal *cableStartNotStartedError
		if !errors.Is(err, context.Canceled) || errors.As(err, &refusal) {
			t.Fatalf("late cancellation became an accepted or definitive result: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled approval did not finish")
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if len(f.s.runs) != 0 || f.count("launch:") != 0 {
		t.Fatal("cancelled approval created an operation or launched a worker")
	}
}

func TestCablePrepareShutdownClosesPendingAdmission(t *testing.T) {
	f := newCableProductFixture(t)
	review := f.ready()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Hold the existing access lock so approval has entered diagnostic admission
	// but cannot publish a run. Shutdown closes admission independently of it.
	f.s.m.onboarding.mu.Lock()
	released := false
	defer func() {
		if !released {
			f.s.m.onboarding.mu.Unlock()
		}
	}()
	type approvalResult struct {
		run cableprobe.Run
		err error
	}
	finished := make(chan approvalResult, 1)
	request := cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true}
	go func() { run, err := f.s.start(ctx, request); finished <- approvalResult{run, err} }()
	deadline := time.Now().Add(3 * time.Second)
	for f.s.m.exec.diagnosticMu.TryLock() {
		f.s.m.exec.diagnosticMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("approval did not enter diagnostic admission")
		}
		time.Sleep(time.Millisecond)
	}

	// net.Pipe is entirely in memory. Dispatch the actual preparation handler
	// with an empty engine owner and wait for its JSON-RPC acknowledgement.
	managerConn, clientConn := net.Pipe()
	defer managerConn.Close()
	defer clientConn.Close()
	f.s.m.codec = NewCodec(managerConn)
	if err := clientConn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	id := json.RawMessage("901")
	f.s.m.handleMessage(ctx, &Message{JSONRPC: "2.0", ID: &id, Method: prepareShutdownMethod})
	var response Message
	if err := json.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatalf("prepare-shutdown was not acknowledged: %v", err)
	}
	if response.ID == nil || string(*response.ID) != string(id) || response.Error != nil {
		t.Fatalf("unexpected shutdown acknowledgement: %+v", response)
	}
	if f.s.ctx.Err() != nil {
		t.Fatal("fixture cancelled the service context and masked the admission race")
	}
	f.s.m.onboarding.mu.Unlock()
	released = true
	select {
	case result := <-finished:
		if result.err == nil {
			f.done(result.run)
		}
		if launches := f.count("launch:"); launches != 0 {
			t.Fatalf("%d worker launches followed the prepare-shutdown acknowledgement", launches)
		}
		if result.err == nil || !strings.Contains(result.err.Error(), "shutting down") {
			t.Fatalf("pending approval was not rejected by shutdown: %v", result.err)
		}
		var refusal *cableStartNotStartedError
		if errors.As(result.err, &refusal) {
			t.Fatal("shutdown fabricated a definitive not-started result")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending approval did not settle")
	}
	f.s.mu.Lock()
	runs := len(f.s.runs)
	f.s.mu.Unlock()
	if runs != 0 {
		t.Fatal("shutdown allowed a new retained operation")
	}
	f.s.shutdown()
	if _, err := f.s.start(ctx, request); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("repeated shutdown reopened admission: %v", err)
	}
}
