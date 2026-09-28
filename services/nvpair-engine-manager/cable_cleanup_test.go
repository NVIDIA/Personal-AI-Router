// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
)

type cleanupProductFixture struct {
	f          *cableProductFixture
	run        cableprobe.Run
	original   []byte
	launches   atomic.Int32
	finishes   atomic.Int32
	readHook   func(context.Context, cableCleanupMessage) (cableCleanupMessage, error)
	finishHook func(context.Context, cableCleanupMessage) (cableCleanupMessage, error)
}

func newCleanupProductFixture(t *testing.T) *cleanupProductFixture {
	t.Helper()
	f := newCableProductFixture(t)
	f.failLaunch = "host-peer"
	run := f.done(f.start(f.ready()))
	if run.CleanupConfirmed || run.State != "failed" {
		t.Fatal("fixture must retain unknown worker cleanup")
	}
	f.s.mu.Lock()
	owned := f.s.runs[run.RunID]
	for i := range owned.Plans {
		owned.Plans[i].WorkerOrigin = "controller-running-image:" + owned.Plans[i].WorkerSHA256
	}
	if err := f.s.save(owned); err != nil {
		t.Fatal(err)
	}
	f.s.mu.Unlock()
	original, err := os.ReadFile(f.s.file(run.RunID))
	if err != nil {
		t.Fatal(err)
	}
	x := &cleanupProductFixture{f: f, run: run, original: original}
	binding := f.s.workerBinding
	f.s.workerBinding = func(ctx context.Context, target cableprobe.Target) (cableWorkerBinding, error) {
		value, err := binding(ctx, target)
		value.CleanupProtocol = cableCleanupProtocol
		value.CleanupScope = &cableCleanupScope{PID: 77, StartTicks: "123", BootID: "00000000-0000-4000-8000-000000000001", PIDNS: "pid:[1]", MountNS: "mnt:[2]", NetNS: "net:[3]", UserNS: "user:[4]"}
		return value, err
	}
	f.s.cleanupLaunch = func(ctx context.Context, _ *onboardingSSH, plan cableLaunchPlan, request cableCleanupRequest, _ string) (*cableCleanupWorker, error) {
		x.launches.Add(1)
		ready := cableCleanupMessage{Protocol: cableCleanupProtocol, RunID: request.RunID, ReviewID: request.ReviewID, AttemptID: request.AttemptID, Challenge: request.Challenge, NodeID: request.NodeID, Principal: request.Principal, State: "ready", Code: "clear", Scope: request.Scope, LockDevice: 1, LockInode: 2, Passes: 2, Processes: 20, Candidates: 0, RemainingMs: 5000}
		return &cableCleanupWorker{close: func() {}, read: func(ctx context.Context) (cableCleanupMessage, error) {
			if x.readHook != nil {
				return x.readHook(ctx, ready)
			}
			return ready, ctx.Err()
		}, finish: func(ctx context.Context, command cableCleanupCommand) (cableCleanupMessage, error) {
			x.finishes.Add(1)
			data, err := os.ReadFile(f.s.cleanupFile(run.RunID))
			var pending cableCleanupRecord
			if err != nil || onboardingDecode(data, &pending) != nil || pending.Attempts[len(pending.Attempts)-1].Public.State != "release-pending" {
				return ready, errors.New("close request preceded retained pending proof")
			}
			if command.AttemptID != request.AttemptID || command.Challenge != request.Challenge || command.Command != "release" {
				return ready, errors.New("wrong fixture close command")
			}
			closed := ready
			closed.State = "closed"
			closed.Code = "released"
			closed.CleanupConfirmed = true
			if x.finishHook != nil {
				return x.finishHook(ctx, closed)
			}
			return closed, ctx.Err()
		}}, nil
	}
	return x
}
func (x *cleanupProductFixture) authorize() {
	x.f.mutate("host-peer", func(p *onboardingPrivateTarget) {
		p.candidate.AccessAvailable = true
		p.candidate.HostKeyTrusted = true
		p.expiresAt = time.Now().Add(time.Minute)
		p.accessGeneration = "cleanup-fixture-generation"
		p.access = onboardingAccess{user: "synthetic-user", password: "synthetic-access-input", elevationPassword: "synthetic-admin-input"}
	})
}
func (x *cleanupProductFixture) review(t *testing.T) cableprobe.CleanupReview {
	t.Helper()
	review, err := x.f.s.reviewCleanup(context.Background(), cableCleanupReviewRequest{RunID: x.run.RunID})
	if err != nil {
		t.Fatal(err)
	}
	return review
}
func (x *cleanupProductFixture) done(t *testing.T) cableprobe.Run {
	t.Helper()
	x.f.s.mu.Lock()
	done := x.f.s.cleanupDone
	x.f.s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("bounded recovery fixture did not settle")
		}
	}
	run, err := x.f.s.status(cableprobe.StatusRequest{RunID: x.run.RunID})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(x.f.s.file(x.run.RunID))
	if err != nil || !bytes.Equal(data, x.original) {
		t.Fatal("cleanup verification rewrote the original operation envelope")
	}
	plain := cloneCableRun(run)
	plain.CleanupRecovery = nil
	if !reflect.DeepEqual(plain, x.run) {
		t.Fatal("cleanup recovery rewrote the original public result")
	}
	return run
}

func TestCableCleanupExplicitReleasePreservesOriginalAndReplay(t *testing.T) {
	x := newCleanupProductFixture(t)
	if review := x.review(t); review.Available || len(review.Targets) != 1 || review.Targets[0].NodeID != "host-peer" {
		t.Fatal("only unresolved target must request fresh access")
	}
	x.authorize()
	review := x.review(t)
	if !review.Available {
		t.Fatalf("valid cleanup fixture unavailable: %+v", review)
	}
	if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, false); err == nil || x.launches.Load() != 0 {
		t.Fatal("missing explicit approval reached inspector")
	}
	if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true); err != nil {
		t.Fatal(err)
	}
	run := x.done(t)
	if run.CleanupConfirmed || run.CleanupRecovery == nil || !run.CleanupRecovery.HoldReleased || run.CleanupRecovery.State != "released" || x.f.s.held() {
		t.Fatalf("separate release was conflated or lost: %+v", run.CleanupRecovery)
	}
	if x.launches.Load() != 1 || x.finishes.Load() != 1 {
		t.Fatal("unexpected inspection scope")
	}
	if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true); err != nil || x.launches.Load() != 1 {
		t.Fatal("same-review replay launched a second inspector")
	}
	reloaded := newCableProductService(x.f.s.m)
	if reloaded.recoveryFailed || reloaded.held() {
		t.Fatal("durable reviewed release did not survive reload")
	}
	other := &cableProductRun{Public: cableprobe.Run{RunID: "unrelated", State: "failed", CleanupConfirmed: false}}
	reloaded.runs[other.Public.RunID] = other
	if !reloaded.held() {
		t.Fatal("one recovery released an unrelated operation hold")
	}
}

func TestCableCleanupFailureAndCancellationKeepHold(t *testing.T) {
	for _, mode := range []string{"blocked", "replayed-ready", "close-failure", "replayed-close", "scope-change", "pending-write", "commit-write", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			x.authorize()
			review := x.review(t)
			entered := make(chan struct{})
			switch mode {
			case "blocked":
				x.readHook = func(_ context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
					m.State = "blocked"
					m.Code = "candidate-active"
					return m, nil
				}
			case "replayed-ready":
				x.readHook = func(_ context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
					m.Challenge = "foreign"
					return m, nil
				}
			case "close-failure":
				x.finishHook = func(_ context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
					return m, errors.New("synthetic close failure")
				}
			case "replayed-close":
				x.finishHook = func(_ context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
					m.AttemptID = "foreign"
					return m, nil
				}
			case "scope-change":
				x.finishHook = func(_ context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
					old := x.f.s.workerBinding
					x.f.s.workerBinding = func(ctx context.Context, t cableprobe.Target) (cableWorkerBinding, error) {
						b, e := old(ctx, t)
						b.CleanupScope.PID++
						return b, e
					}
					return m, nil
				}
			case "pending-write", "commit-write":
				x.f.s.testCleanupSave = func(r *cableCleanupRecord) error {
					state := r.Attempts[len(r.Attempts)-1].Public.State
					if (mode == "pending-write" && state == "release-pending") || (mode == "commit-write" && state == "released") {
						return errors.New("synthetic persistence failure")
					}
					return nil
				}
			case "cancel":
				x.readHook = func(ctx context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
					close(entered)
					<-ctx.Done()
					return m, ctx.Err()
				}
			}
			if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true); err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("fixture not entered")
				}
				if _, err := x.f.s.cancelCleanup(x.run.RunID); err != nil {
					t.Fatal(err)
				}
			}
			run := x.done(t)
			if run.CleanupRecovery == nil || run.CleanupRecovery.HoldReleased || !x.f.s.held() {
				t.Fatalf("incomplete verification released hold: %+v", run.CleanupRecovery)
			}
			if mode == "cancel" && run.CleanupRecovery.State != "cancelled" {
				t.Fatal("cancelled recovery lost disposition")
			}
			reloaded := newCableProductService(x.f.s.m)
			if !reloaded.held() {
				t.Fatal("failure escaped hold after restart")
			}
		})
	}
}

func TestCableCleanupReviewAndAccessAreNarrow(t *testing.T) {
	x := newCleanupProductFixture(t)
	review := x.review(t)
	owner := x.f.s.m.onboarding
	owner.observeKey = func(_ context.Context, c onboardingCandidate, _ string) (string, bool, bool, error) {
		return c.HostKeySHA256, true, false, nil
	}
	request := onboardingBatchAccess{Purpose: "cable", CandidateIDs: []string{x.f.ids["host-peer"]}, Username: "synthetic-user", Auth: "password", Password: "synthetic-access-input", ElevationPassword: "synthetic-admin-input"}
	if _, err := owner.bindAccess(context.Background(), request); err != nil {
		t.Fatal("scoped cleanup reauthorization was blocked:", err)
	}
	request.CandidateIDs = []string{x.f.ids["host-owner"]}
	if _, err := owner.bindAccess(context.Background(), request); err == nil {
		t.Fatal("cleanup exception authorized a resolved/unrelated candidate")
	}
	x.f.s.mu.Lock()
	binding := x.f.s.cleanupReviews[review.ReviewID]
	binding.expires = time.Now().Add(-time.Second)
	x.f.s.cleanupReviews[review.ReviewID] = binding
	x.f.s.mu.Unlock()
	if x.f.s.cleanupAccessAllowed([]string{x.f.ids["host-peer"]}) {
		t.Fatal("expired review retained an access exception")
	}
	x.authorize()
	fresh := x.review(t)
	x.f.s.mu.Lock()
	binding = x.f.s.cleanupReviews[fresh.ReviewID]
	binding.expires = time.Now().Add(-time.Second)
	x.f.s.cleanupReviews[fresh.ReviewID] = binding
	x.f.s.mu.Unlock()
	if _, err := x.f.s.verifyCleanup(context.Background(), fresh.ReviewID, true); err == nil || x.launches.Load() != 0 {
		t.Fatal("expired review launched inspection")
	}
}

func TestCableCleanupExpiredProofCannotCommitAfterFinalScopeRead(t *testing.T) {
	x := newCleanupProductFixture(t)
	x.authorize()
	review := x.review(t)
	x.readHook = func(_ context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
		m.RemainingMs = 100
		return m, nil
	}
	x.finishHook = func(_ context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
		previous := x.f.s.workerBinding
		x.f.s.workerBinding = func(ctx context.Context, target cableprobe.Target) (cableWorkerBinding, error) {
			time.Sleep(150 * time.Millisecond)
			return previous(ctx, target)
		}
		return m, nil
	}
	if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true); err != nil {
		t.Fatal(err)
	}
	run := x.done(t)
	if run.CleanupRecovery == nil || run.CleanupRecovery.HoldReleased || !x.f.s.held() {
		t.Fatal("expired ready proof crossed final scope read and committed release")
	}
}

func TestCableCleanupAccessRechecksFinalAdmission(t *testing.T) {
	x := newCleanupProductFixture(t)
	x.authorize()
	review := x.review(t)
	owner := x.f.s.m.onboarding
	observing, releaseAccess, inspecting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	owner.observeKey = func(_ context.Context, c onboardingCandidate, _ string) (string, bool, bool, error) {
		close(observing)
		<-releaseAccess
		return c.HostKeySHA256, true, false, nil
	}
	x.readHook = func(ctx context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
		close(inspecting)
		<-ctx.Done()
		return m, ctx.Err()
	}
	bound := make(chan error, 1)
	go func() {
		_, err := owner.bindAccess(context.Background(), onboardingBatchAccess{Purpose: "cable", CandidateIDs: []string{x.f.ids["host-peer"]}, Username: "synthetic-user", Auth: "password", Password: "synthetic-access-input", ElevationPassword: "synthetic-admin-input"})
		bound <- err
	}()
	select {
	case <-observing:
	case <-time.After(time.Second):
		t.Fatal("access observation did not start")
	}
	if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inspecting:
	case <-time.After(time.Second):
		t.Fatal("inspection did not start")
	}
	close(releaseAccess)
	if err := <-bound; err == nil {
		t.Error("pending access replaced the active inspector generation")
	}
	if x.f.s.cleanupAccessAllowed([]string{x.f.ids["host-peer"]}) {
		t.Error("active recovery still admits access replacement")
	}
	_, _ = x.f.s.cancelCleanup(x.run.RunID)
	x.done(t)
}

func TestCableCleanupPendingRestartAndTamperedProofStayHeld(t *testing.T) {
	x := newCleanupProductFixture(t)
	x.authorize()
	review := x.review(t)
	if _, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true); err != nil {
		t.Fatal(err)
	}
	x.done(t)
	data, err := os.ReadFile(x.f.s.cleanupFile(x.run.RunID))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"pending", "missing-join", "wrong-challenge", "scope-shape", "original-hash"} {
		t.Run(mode, func(t *testing.T) {
			var record cableCleanupRecord
			if onboardingDecode(data, &record) != nil {
				t.Fatal("invalid fixture record")
			}
			a := record.Attempts[0]
			switch mode {
			case "pending":
				a.Public.State = "release-pending"
				a.Public.Code = "release-pending"
				a.Public.HoldReleased = false
			case "missing-join":
				a.Proofs[0].Joined = false
			case "wrong-challenge":
				a.Proofs[0].Ready.Challenge = "foreign"
			case "scope-shape":
				a.Plans[0].Runtime.CleanupScope.PID = 0
			case "original-hash":
				record.OriginalSHA256 = strings.Repeat("f", 64)
			}
			if err := writeJSONAtomic(x.f.s.cleanupFile(x.run.RunID), record); err != nil {
				t.Fatal(err)
			}
			reloaded := newCableProductService(x.f.s.m)
			if !reloaded.held() {
				t.Fatal("uncommitted or invalid proof released after reload")
			}
			original, err := os.ReadFile(x.f.s.file(x.run.RunID))
			if err != nil || !bytes.Equal(original, x.original) {
				t.Fatal("recovery reload changed original record")
			}
		})
	}
}

func TestCableCleanupShutdownAndInitialJournalFailureDoNotRelease(t *testing.T) {
	for _, mode := range []string{"initial-write", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			x.authorize()
			review := x.review(t)
			entered := make(chan struct{})
			if mode == "initial-write" {
				x.f.s.testCleanupSave = func(*cableCleanupRecord) error { return errors.New("synthetic initial write failure") }
			} else {
				x.readHook = func(ctx context.Context, m cableCleanupMessage) (cableCleanupMessage, error) {
					close(entered)
					<-ctx.Done()
					return m, ctx.Err()
				}
			}
			_, err := x.f.s.verifyCleanup(context.Background(), review.ReviewID, true)
			if mode == "initial-write" {
				if err == nil || x.launches.Load() != 0 {
					t.Fatal("inspection escaped unretained approval")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("inspection did not enter")
			}
			stopped := make(chan struct{})
			go func() { x.f.s.shutdown(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not join owned verification")
			}
			run := x.done(t)
			if run.CleanupRecovery.HoldReleased || !x.f.s.held() {
				t.Fatal("shutdown released old hold")
			}
		})
	}
}
