// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
)

// Real cleanup launcher, existing in-memory SSH-session fixture. All stdout,
// stderr, process completion and scheduling are synthetic; no native command,
// SSH connection, sudo, inspector binary, proc scan or lock is executed.
func TestCableCleanupLauncherWaitsForStderrBeforeRelease(t *testing.T) {
	for _, mode := range []string{"stderr-overflow", "caller-cancel", "stderr-eof"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				plan, _, _ := launchTestRuntimePlan(t)
				scope := cableCleanupScope{PID: 200, StartTicks: "123", BootID: "11111111-2222-3333-4444-555555555555", PIDNS: "pid:[1]", MountNS: "mnt:[2]", NetNS: "net:[3]", UserNS: "user:[4]"}
				plan.Runtime.CleanupProtocol, plan.Runtime.CleanupScope = cableCleanupProtocol, &scope
				request := cableCleanupRequest{Protocol: cableCleanupProtocol, RunID: "synthetic-run", ReviewID: "synthetic-review", AttemptID: "synthetic-attempt", Challenge: strings.Repeat("a", 32), NodeID: plan.NodeID, Principal: plan.Principal, UID: plan.Info.UID, Scope: scope, WorkerSHA256: plan.WorkerSHA256, OriginalWorkerSHA256: strings.Repeat("b", 64)}
				ready := cableCleanupMessage{Protocol: request.Protocol, RunID: request.RunID, ReviewID: request.ReviewID, AttemptID: request.AttemptID, Challenge: request.Challenge, NodeID: request.NodeID, Principal: request.Principal, State: "ready", Code: "clear", Scope: scope, LockDevice: 1, LockInode: 2, Passes: 2, Processes: 4, Candidates: 0, RemainingMs: 5000}
				closed := ready
				closed.State, closed.Code, closed.RemainingMs, closed.CleanupConfirmed = "closed", "released", 0, true
				if !validCleanupReady(ready, request) || !validCleanupClosed(closed, ready) {
					t.Fatal("fixture must supply a fully bound valid ready/closed protocol")
				}
				readyJSON, err := json.Marshal(ready)
				if err != nil {
					t.Fatal(err)
				}
				closedJSON, err := json.Marshal(closed)
				if err != nil {
					t.Fatal(err)
				}
				stderr, writer := io.Pipe()
				defer stderr.Close()
				defer writer.Close()
				session := &launchTestSession{output: strings.NewReader(string(readyJSON) + "\n" + string(closedJSON) + "\n"), stderr: stderr, done: make(chan struct{})}
				client := &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}
				worker, err := openCableCleanupWorker(context.Background(), client, plan, request, "synthetic-administrator-input")
				if err != nil {
					t.Fatal(err)
				}
				defer worker.close()
				message, err := worker.read(context.Background())
				if err != nil || !validCleanupReady(message, request) {
					t.Fatalf("fixture ready message was not admitted: %v", err)
				}
				// Process exit and complete stdout deliberately precede stderr EOF.
				// x/crypto Session.Wait does not join an externally read StderrPipe.
				session.finish()
				finishCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type outcome struct {
					message cableCleanupMessage
					err     error
				}
				finished := make(chan outcome, 1)
				go func() {
					message, err := worker.finish(finishCtx, cableCleanupCommand{AttemptID: request.AttemptID, Challenge: request.Challenge, Command: "release"})
					finished <- outcome{message, err}
				}()
				synctest.Wait()
				var result outcome
				early := false
				select {
				case result = <-finished:
					early = true
					if result.err == nil {
						t.Error("cleanup finish accepted release while the custom stderr reader was still blocked")
					}
				default:
				}
				if mode == "stderr-overflow" {
					if _, err := io.WriteString(writer, strings.Repeat("x", (16<<10)+1)); err != nil {
						t.Fatal(err)
					}
				} else if mode == "caller-cancel" {
					cancel()
				}
				_ = writer.Close()
				synctest.Wait()
				if !early {
					result = <-finished
				}
				if mode == "stderr-eof" {
					if result.err != nil || !validCleanupClosed(result.message, ready) {
						t.Fatalf("clean stderr EOF did not allow the fully joined bound closure: %v", result.err)
					}
					return
				}
				if result.err == nil {
					t.Fatalf("%s was accepted as successful cleanup release (released reply=%v)", mode, validCleanupClosed(result.message, ready))
				}
				if mode == "caller-cancel" && !errors.Is(result.err, context.Canceled) {
					t.Fatalf("caller cancellation was replaced by another disposition: %v", result.err)
				}
			})
		})
	}
}
