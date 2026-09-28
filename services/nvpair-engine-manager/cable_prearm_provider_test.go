// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
)

func prearmTestLine(t *testing.T, request cableProbeOnceRequest) string {
	t.Helper()
	line, err := json.Marshal(cablePrearmMessage{State: "prearm-failed", RunID: request.RunID, ReviewID: request.Review.ReviewID,
		Code: "prepare-configure-failed", ResourceOutcome: "rollback-unconfirmed"})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func TestCablePrearmExactWireShape(t *testing.T) {
	plan, _, _ := launchTestPlan(t)
	request := launchTestRequest(plan)
	line := prearmTestLine(t, request)
	var base map[string]any
	if json.Unmarshal([]byte(line), &base) != nil {
		t.Fatal("fixture")
	}
	cases := map[string]string{"valid": line, "duplicate": strings.Replace(line, "{", `{"sent":0,`, 1), "trailing": line + ` {}`}
	cases["field-case"] = strings.Replace(line, `"code":`, `"Code":`, 1)
	for key := range base {
		missing := map[string]any{}
		for name, value := range base {
			if name != key {
				missing[name] = value
			}
		}
		encoded, _ := json.Marshal(missing)
		cases["missing-"+key] = string(encoded)
		missing[key] = nil
		encoded, _ = json.Marshal(missing)
		cases["null-"+key] = string(encoded)
	}
	for name, change := range map[string]map[string]any{
		"run": {"runId": "foreign"}, "review": {"reviewId": "foreign"}, "code": {"code": "synthetic-private-error"},
		"outcome": {"resourceOutcome": "all-clean"}, "sent": {"sent": 1}, "received": {"received": 1},
		"cleanup": {"cleanupConfirmed": true}, "text": {"message": "synthetic-private-error"},
		"lease": {"remainingMs": 1000}, "observations": {"observations": []any{}}, "type": {"sent": "0"},
	} {
		candidate := map[string]any{}
		for key, value := range base {
			candidate[key] = value
		}
		for key, value := range change {
			candidate[key] = value
		}
		encoded, _ := json.Marshal(candidate)
		cases[name] = string(encoded)
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			message, err := decodeCablePrearmLine([]byte(input), request.RunID, request.Review.ReviewID)
			if (err == nil) != (name == "valid") {
				t.Fatalf("unexpected admission: %v", err)
			}
			if err == nil && (message.CleanupConfirmed || message.Prearm == nil) {
				t.Fatal("diagnostic became cleanup")
			}
		})
	}
}

func TestCablePrearmProviderCompleteGrammarAndSecondaryFailures(t *testing.T) {
	for _, mode := range []string{"complete", "extra-terminal", "repeated-prearm", "extra-malformed", "process-failure", "cancel-after-parse", "stderr-after-parse", "stderr-pending", "armed-then-prearm", "terminal-before-armed", "armed-eof"} {
		t.Run(mode, func(t *testing.T) {
			plan, _, _ := launchTestPlan(t)
			request := launchTestRequest(plan)
			prearm := prearmTestLine(t, request) + "\n"
			ordinary := strings.SplitAfter(launchTestMessages(t, request, true), "\n")
			output := prearm
			switch mode {
			case "extra-terminal":
				output += ordinary[1]
			case "repeated-prearm":
				output += prearm
			case "extra-malformed":
				output += "{}\n"
			case "armed-then-prearm":
				output = ordinary[0] + prearm
			case "terminal-before-armed":
				output = ordinary[1]
			case "armed-eof":
				output = ordinary[0]
			}
			session := &launchTestSession{output: strings.NewReader(output), done: make(chan struct{})}
			if mode == "process-failure" {
				session.waitErr = errors.New("synthetic-private-process-error")
			}
			var stderrWriter *io.PipeWriter
			if mode == "stderr-after-parse" || mode == "stderr-pending" {
				var reader *io.PipeReader
				reader, stderrWriter = io.Pipe()
				session.stderr = reader
				defer reader.Close()
				defer stderrWriter.Close()
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
			worker, err := openCableWorker(parent, client, plan, request, "synthetic-admin-input")
			if err != nil {
				t.Fatal(err)
			}
			defer worker.close()
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if mode == "extra-malformed" {
				// Force the later transport failure before the first consumer read.
				select {
				case <-session.done:
				case <-ctx.Done():
					t.Fatal("invalid suffix did not close worker")
				}
			}
			message, readErr := worker.read(ctx)
			wantPrearm := mode != "armed-then-prearm" && mode != "terminal-before-armed" && mode != "armed-eof"
			if wantPrearm && (readErr != nil || !validCablePrearmWorkerMessage(message, request.RunID, request.Review.ReviewID)) {
				t.Fatalf("parsed primary was lost: %+v %v", message, readErr)
			}
			if wantPrearm {
				for _, command := range []string{"start", "cancel"} {
					if worker.send(cableProbeCommand{RunID: request.RunID, Command: command}) == nil {
						t.Fatal("terminal accepted command")
					}
				}
			}
			if mode == "cancel-after-parse" {
				cancel()
			}
			if mode == "stderr-after-parse" {
				if _, err := io.WriteString(stderrWriter, strings.Repeat("x", (16<<10)+1)); err != nil {
					t.Fatal(err)
				}
			}
			_ = worker.closeInput()
			session.finish()
			joinCtx := ctx
			if mode == "stderr-pending" {
				var cancelJoin context.CancelFunc
				joinCtx, cancelJoin = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancelJoin()
			}
			joinErr := worker.wait(joinCtx)
			if (joinErr == nil) != (mode == "complete") {
				t.Fatalf("grammar/transport accepted incorrectly: %v", joinErr)
			}
			if wantPrearm && (message.Prearm.Code != "prepare-configure-failed" || message.CleanupConfirmed) {
				t.Fatal("secondary failure changed primary")
			}
		})
	}
}

func TestCablePrearmCoordinatorKeepsTerminalAndHold(t *testing.T) {
	for _, mode := range []string{"complete", "join-failure", "input-close-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newCableProductFixture(t)
			launch := f.s.launch
			reads, sends, joins := 0, 0, 0
			f.s.launch = func(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, admin string) (*cableWorker, error) {
				if request.Protocol != cableWorkerProtocol {
					t.Error("request omitted explicit protocol")
				}
				if plan.NodeID != "host-peer" {
					return launch(ctx, client, plan, request, admin)
				}
				message, err := decodeCablePrearmLine([]byte(prearmTestLine(t, request)), request.RunID, request.Review.ReviewID)
				if err != nil {
					return nil, err
				}
				return &cableWorker{
					read: func(context.Context) (cableWorkerMessage, error) {
						reads++
						if reads != 1 {
							return cableWorkerMessage{}, io.EOF
						}
						return message, nil
					},
					send: func(cableProbeCommand) error { sends++; return errors.New("terminal cannot consume commands") },
					closeInput: func() error {
						if mode == "input-close-failure" {
							return errors.New("synthetic-private-close-error")
						}
						return nil
					},
					wait: func(context.Context) error {
						joins++
						if mode == "join-failure" {
							return errors.New("synthetic-private-join-error")
						}
						return nil
					},
					close: func() {},
				}, nil
			}
			run := f.done(f.start(f.ready()))
			if run.State != "failed" || run.CleanupConfirmed || !f.s.held() || reads != 1 || sends != 0 || joins != 1 || f.count("start-attempt:") != 0 || f.count("cancel:") != 1 {
				t.Fatalf("pre-arm terminal/barrier/hold changed: %+v reads=%d sends=%d joins=%d", run, reads, sends, joins)
			}
			if run.Diagnostics == nil || run.Diagnostics.Failure == nil || *run.Diagnostics.Failure != (cableprobe.Failure{Phase: "arm", Code: "prepare-configure-failed"}) {
				t.Fatal("primary preparation failure lost")
			}
			for _, row := range run.Diagnostics.Participants {
				if row.NodeID != "host-peer" {
					if !row.CleanupConfirmed {
						t.Fatal("other armed peer was not cleaned")
					}
					continue
				}
				if row.Phase != "arm" || row.Code != "prepare-configure-failed" || row.WorkerState != "failed" || row.Sent == nil || row.Received == nil || *row.Sent != 0 || *row.Received != 0 || row.CleanupConfirmed || row.PreparationResourceOutcome != "rollback-unconfirmed" {
					t.Fatalf("projection changed: %+v", row)
				}
			}
			data, err := os.ReadFile(f.s.file(run.RunID))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "synthetic-private-") || strings.Contains(string(data), "synthetic-admin-input") {
				t.Fatal("private input retained")
			}
			var record cableProductRun
			if onboardingDecode(data, &record) != nil || !validCableProductRecord(record) || !reflect.DeepEqual(record.Public.Diagnostics, run.Diagnostics) {
				t.Fatal("diagnostics did not survive journal")
			}
			if mode == "complete" {
				for _, change := range []struct {
					name  string
					edit  func(*cableprobe.Run, *cableprobe.ParticipantDiagnostic)
					valid bool
				}{
					{"optional-outcome", func(_ *cableprobe.Run, p *cableprobe.ParticipantDiagnostic) { p.PreparationResourceOutcome = "" }, true},
					{"unknown-outcome", func(_ *cableprobe.Run, p *cableprobe.ParticipantDiagnostic) {
						p.PreparationResourceOutcome = "remote-text"
					}, false},
					{"counter", func(_ *cableprobe.Run, p *cableprobe.ParticipantDiagnostic) { *p.Sent = 1 }, false},
					{"cleanup", func(_ *cableprobe.Run, p *cableprobe.ParticipantDiagnostic) { p.CleanupConfirmed = true }, false},
					{"global-cleanup", func(r *cableprobe.Run, _ *cableprobe.ParticipantDiagnostic) { r.CleanupConfirmed = true }, false},
					{"phase", func(_ *cableprobe.Run, p *cableprobe.ParticipantDiagnostic) { p.Phase = "observation" }, false},
					{"global-phase", func(r *cableprobe.Run, _ *cableprobe.ParticipantDiagnostic) {
						r.Diagnostics.Failure.Phase = "observation"
					}, false},
					{"final", func(_ *cableprobe.Run, p *cableprobe.ParticipantDiagnostic) { p.FinalValidationCode = "facts-stale" }, false},
				} {
					t.Run(change.name, func(t *testing.T) {
						candidate := cloneCableRun(run)
						for i := range candidate.Diagnostics.Participants {
							if candidate.Diagnostics.Participants[i].NodeID == "host-peer" {
								change.edit(&candidate, &candidate.Diagnostics.Participants[i])
							}
						}
						if validCableDiagnostics(candidate) != change.valid {
							t.Fatal("invalid diagnostic or cleanup promotion admitted")
						}
					})
				}
			}
			loaded := newCableProductService(f.s.m)
			if loaded.recoveryFailed || !loaded.held() || loaded.runs[run.RunID] == nil {
				t.Fatal("held pre-arm record did not reload")
			}
			after, err := os.ReadFile(f.s.file(run.RunID))
			if err != nil || string(after) != string(data) {
				t.Fatal("reload rewrote failed operation")
			}
		})
	}
}
