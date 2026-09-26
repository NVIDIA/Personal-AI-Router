// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"nvpair-shared/cableprobe"
)

func TestCableRetainedResumePreservesActualPrearmHistoryAndHold(t *testing.T) {
	f := newCableProductFixture(t)
	launch := f.s.launch
	var launches, terminalReads, terminalWrites, terminalJoins atomic.Int32
	f.s.launch = func(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, admin string) (*cableWorker, error) {
		launches.Add(1)
		if plan.NodeID != "host-peer" {
			return launch(ctx, client, plan, request, admin)
		}
		message, err := decodeCablePrearmLine([]byte(prearmTestLine(t, request)), request.RunID, request.Review.ReviewID)
		if err != nil {
			return nil, err
		}
		return &cableWorker{
			read: func(context.Context) (cableWorkerMessage, error) {
				if terminalReads.Add(1) != 1 {
					return cableWorkerMessage{}, io.EOF
				}
				return message, nil
			},
			send: func(cableProbeCommand) error {
				terminalWrites.Add(1)
				return errors.New("prearm terminal cannot accept commands")
			},
			closeInput: func() error { return nil },
			wait: func(context.Context) error {
				terminalJoins.Add(1)
				return nil
			},
			close: func() {},
		}, nil
	}
	original := f.done(f.start(f.ready()))
	if original.State != "failed" || original.CleanupConfirmed || original.Diagnostics == nil || original.Diagnostics.Failure == nil || *original.Diagnostics.Failure != (cableprobe.Failure{Phase: "arm", Code: "prepare-configure-failed"}) {
		t.Fatal("fixture did not retain the real prearm primary and cleanup hold")
	}
	found := false
	for _, row := range original.Diagnostics.Participants {
		if row.NodeID == "host-peer" {
			found = row.PreparationResourceOutcome == "rollback-unconfirmed" && !row.CleanupConfirmed
		}
	}
	if !found || terminalReads.Load() != 1 || terminalWrites.Load() != 0 || terminalJoins.Load() != 1 || f.count("start-attempt:") != 0 {
		t.Fatal("prearm evidence or its single-terminal boundary was lost")
	}
	before, err := os.ReadFile(f.s.file(original.RunID))
	if err != nil {
		t.Fatal(err)
	}
	launchesBefore := launches.Load()
	// A fresh GUI may have no current account targets. Retained discovery and
	// explicit opening must still use the original journal, never a new review.
	f.s.m.onboarding.mu.Lock()
	f.s.m.onboarding.targets = map[string]*onboardingPrivateTarget{}
	f.s.m.onboarding.mu.Unlock()
	loaded := newCableProductService(f.s.m)
	defer loaded.shutdown()
	var newWork atomic.Int32
	loaded.launch = func(context.Context, *onboardingSSH, cableLaunchPlan, cableProbeOnceRequest, string) (*cableWorker, error) {
		newWork.Add(1)
		return nil, errors.New("retained discovery must not launch a probe")
	}
	loaded.cleanupLaunch = func(context.Context, *onboardingSSH, cableLaunchPlan, cableCleanupRequest, string) (*cableCleanupWorker, error) {
		newWork.Add(1)
		return nil, errors.New("retained discovery must not launch cleanup")
	}
	catalog, err := loaded.retainedRuns(context.Background())
	if err != nil || !catalog.Held || catalog.Limited || len(catalog.Runs) != 1 {
		t.Fatalf("prearm history was not discoverable after reload: %+v %v", catalog, err)
	}
	row := catalog.Runs[0]
	if row.RunID != original.RunID || row.ReviewID != original.ReviewID || row.OwnerNodeID != original.OwnerNodeID || row.Revision != original.Revision || row.StartedAt != original.StartedAt || row.CleanupConfirmed {
		t.Fatal("catalog changed the original prearm envelope")
	}
	opened, err := loaded.status(cableprobe.StatusRequest{RunID: row.RunID})
	if err != nil || !reflect.DeepEqual(opened, original) || !loaded.held() {
		t.Fatalf("explicit opening changed prearm history or released cleanup: %+v %v", opened, err)
	}
	after, err := os.ReadFile(loaded.file(original.RunID))
	if err != nil || !bytes.Equal(before, after) || len(loaded.reviews) != 0 || len(loaded.cleanupReviews) != 0 || len(loaded.cleanupRecords) != 0 || newWork.Load() != 0 || launches.Load() != launchesBefore {
		t.Fatal("retained catalog/open mutated the original journal or requested new work")
	}
}
