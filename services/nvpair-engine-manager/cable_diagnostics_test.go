// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"nvpair-shared/cableprobe"
)

func TestCableDiagnosticsRetainFixedTerminalFailureAndCounts(t *testing.T) {
	for _, test := range []struct{ message, phase, code, finalCode string }{
		{"Reviewed identity or native ports changed during the cable observation.", "observation", "current-revalidation-failed", ""},
		{"Final reviewed identity or ports changed; observations discarded.", "final-validation", "final-revalidation-failed", "final-revalidation-failed"},
		{"Final native port observations remained stale; observations discarded.", "final-validation", "facts-stale", "facts-stale"},
		{"Cable receive budget exhausted.", "observation", "receive-budget-exhausted", ""},
		{"Fixed-profile cable transmission failed.", "observation", "transmit-failed", ""},
		{"synthetic-untrusted-private-marker", "observation", "worker-failed", ""},
		{"Cable receive failed.", "observation", "receive-failed", "facts-stale"},
	} {
		t.Run(test.code, func(t *testing.T) {
			f := newCableProductFixture(t)
			launch := f.s.launch
			f.s.launch = func(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, admin string) (*cableWorker, error) {
				worker, err := launch(ctx, client, plan, request, admin)
				if err != nil {
					return worker, err
				}
				read := worker.read
				worker.read = func(ctx context.Context) (cableWorkerMessage, error) {
					message, err := read(ctx)
					if err == nil && message.State == "completed" {
						message.State, message.Message = "failed", test.message
						message.FinalValidationCode = test.finalCode
						if test.finalCode != "" {
							message.Observations = nil
						}
						message.Sent, message.Received = 7, 11
					}
					return message, err
				}
				return worker, nil
			}
			run := f.done(f.start(f.ready()))
			if run.State != "failed" || !run.CleanupConfirmed || run.Diagnostics == nil || run.Diagnostics.Failure == nil || run.Diagnostics.Failure.Phase != test.phase || run.Diagnostics.Failure.Code != test.code {
				t.Fatalf("terminal failure/cleanup was conflated: %+v", run.Diagnostics)
			}
			for _, row := range run.Diagnostics.Participants {
				if row.Phase != test.phase || row.Code != test.code || row.FinalValidationCode != test.finalCode || row.WorkerState != "failed" || row.Sent == nil || *row.Sent != 7 || row.Received == nil || *row.Received != 11 || !row.CleanupConfirmed {
					t.Fatalf("participant outcome lost before retention: %+v", row)
				}
			}
			data, err := os.ReadFile(f.s.file(run.RunID))
			if err != nil || strings.Contains(string(data), "synthetic-untrusted-private-marker") {
				t.Fatal("retained receipt missing or contains untrusted worker text")
			}
			var retained cableProductRun
			if onboardingDecode(data, &retained) != nil || !validCableProductRecord(retained) || !reflect.DeepEqual(retained.Public.Diagnostics, run.Diagnostics) {
				t.Fatal("public diagnostics did not survive the actual operation journal")
			}
		})
	}
}

func TestCableDiagnosticsKeepPrimaryFailureWhenCleanupReplacesMessage(t *testing.T) {
	f := newCableProductFixture(t)
	f.eofResult = true
	run := f.done(f.start(f.ready()))
	if run.State != "failed" || !run.CleanupConfirmed || run.Diagnostics == nil {
		t.Fatal("expected failed observation with confirmed cleanup")
	}
	for _, row := range run.Diagnostics.Participants {
		if row.Phase != "observation" || row.Code != "worker-output-invalid" || !row.CleanupConfirmed || row.WorkerState != "" || row.Sent != nil || row.Received != nil {
			t.Fatalf("cleanup acknowledgement replaced the first failure or fabricated counters: %+v", row)
		}
	}
}

func TestCableDiagnosticsDoNotFabricatePrelaunchCounts(t *testing.T) {
	f := newCableProductFixture(t)
	f.failLaunch = "host-peer"
	run := f.done(f.start(f.ready()))
	if run.CleanupConfirmed || run.Diagnostics == nil || run.Diagnostics.Failure == nil || run.Diagnostics.Failure.Code != "launch-failed" {
		t.Fatal("lost launch must retain failure and cleanup uncertainty")
	}
	for _, row := range run.Diagnostics.Participants {
		if row.Sent != nil || row.Received != nil || row.WorkerState != "" {
			t.Fatal("an unreported terminal result became zero packet counters")
		}
		if row.NodeID == "host-peer" && (row.Phase != "launch" || row.Code != "launch-failed" || row.CleanupConfirmed) {
			t.Fatalf("cleanup replaced the launch cause: %+v", row)
		}
	}
}

func TestCableDiagnosticsRecordValidation(t *testing.T) {
	f := newCableProductFixture(t)
	run := f.done(f.start(f.ready()))
	if !validCableDiagnostics(run) || run.Diagnostics.Failure != nil {
		t.Fatal("completed diagnostics are invalid")
	}
	for _, test := range []struct {
		name   string
		change func(*cableprobe.Run)
		valid  bool
	}{
		{"legacy", func(r *cableprobe.Run) { r.Diagnostics = nil }, true},
		{"reported-state-without-counters", func(r *cableprobe.Run) {
			r.Diagnostics.Participants[0].Sent = nil
			r.Diagnostics.Participants[0].Received = nil
		}, true},
		{"unknown-phase", func(r *cableprobe.Run) { r.Diagnostics.Participants[0].Phase = "remote-text" }, false},
		{"unknown-code", func(r *cableprobe.Run) { r.Diagnostics.Participants[0].Code = "remote-text" }, false},
		{"foreign-node", func(r *cableprobe.Run) { r.Diagnostics.Participants[0].NodeID = "foreign" }, false},
		{"missing-row", func(r *cableprobe.Run) { r.Diagnostics.Participants = r.Diagnostics.Participants[:1] }, false},
		{"reordered-rows", func(r *cableprobe.Run) {
			r.Diagnostics.Participants[0], r.Diagnostics.Participants[1] = r.Diagnostics.Participants[1], r.Diagnostics.Participants[0]
		}, false},
		{"partial-counters", func(r *cableprobe.Run) { r.Diagnostics.Participants[0].Sent = nil }, false},
		{"unreported-counters", func(r *cableprobe.Run) { r.Diagnostics.Participants[0].WorkerState = "" }, false},
		{"send-bound", func(r *cableprobe.Run) { *r.Diagnostics.Participants[0].Sent = 41 }, false},
		{"receive-bound", func(r *cableprobe.Run) { *r.Diagnostics.Participants[0].Received = 257 }, false},
		{"unknown-global-code", func(r *cableprobe.Run) {
			r.Diagnostics.Failure = &cableprobe.Failure{Phase: "observation", Code: "remote-text"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCableRun(run)
			test.change(&candidate)
			if validCableDiagnostics(candidate) != test.valid {
				t.Fatal("diagnostic bounds or legacy compatibility changed")
			}
		})
	}
	data, _ := json.Marshal(run)
	if strings.Contains(string(data), "synthetic-admin-input") {
		t.Fatal("diagnostics retained account input")
	}
}

func TestCableDiagnosticFailureCodesNeverExposeErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{context.Canceled, "cancelled"}, {context.DeadlineExceeded, "deadline-exceeded"}, {errCableFactsStale, "facts-stale"}, {errors.New("synthetic-private-error"), "worker-failed"},
	} {
		failure := cableFailure("observation", "worker-failed", test.err)
		if failure.Code != test.code || failure.Phase != "observation" {
			t.Fatal("fixed failure classification changed")
		}
		data, _ := json.Marshal(failure)
		if strings.Contains(string(data), "synthetic-private-error") {
			t.Fatal("arbitrary error text escaped")
		}
	}
}
