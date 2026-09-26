// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCableCleanupDefiniteRefusalRetiresReviewAndPermitsFreshReview(t *testing.T) {
	for _, mode := range []string{"expired", "access-changed", "original-hash-changed", "save-failed", "another-operation"} {
		t.Run(mode, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			x.authorize()
			review := x.review(t)
			if !review.Available {
				t.Fatal("fixture review unavailable")
			}
			s := x.f.s
			switch mode {
			case "expired":
				binding := s.cleanupReviews[review.ReviewID]
				binding.expires = time.Now().Add(-time.Second)
				s.cleanupReviews[review.ReviewID] = binding
			case "access-changed":
				x.f.mutate("host-peer", func(p *onboardingPrivateTarget) { p.accessGeneration = "later-generation" })
			case "original-hash-changed":
				if err := os.WriteFile(s.file(x.run.RunID), append(append([]byte{}, x.original...), '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "save-failed":
				s.testCleanupSave = func(*cableCleanupRecord) error { return errors.New("synthetic-private-storage-error") }
			case "another-operation":
				s.m.onboarding.active = "synthetic-other-operation"
			}
			before, err := os.ReadFile(s.file(x.run.RunID))
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.verifyCleanupAdmission(context.Background(), x.run.RunID, review.ReviewID, true)
			if err != nil || result.Disposition != "not-started" || result.ReviewID != review.ReviewID || result.Run.RunID != x.run.RunID || result.Run.CleanupConfirmed || x.launches.Load() != 0 {
				t.Fatalf("definite refusal was not authoritative: %+v %v", result, err)
			}
			if _, ok := s.cleanupReviews[review.ReviewID]; ok {
				t.Fatal("refused review can still reach admission")
			}
			if s.cleanupRecords[x.run.RunID] != nil {
				t.Fatal("refusal fabricated an accepted attempt")
			}
			after, err := os.ReadFile(s.file(x.run.RunID))
			if err != nil || string(after) != string(before) {
				t.Fatal("refusal changed the original record")
			}
			// Restore fixture prerequisites. The retired old review must remain
			// incapable of launching, including a retry after a lost refusal reply.
			s.testCleanupSave = nil
			s.m.onboarding.active = ""
			if mode == "original-hash-changed" {
				if err := os.WriteFile(s.file(x.run.RunID), x.original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			x.authorize()
			retry, err := s.verifyCleanupAdmission(context.Background(), x.run.RunID, review.ReviewID, true)
			if err != nil || retry.Disposition != "not-started" || x.launches.Load() != 0 {
				t.Fatal("retired review launched on replay")
			}
			fresh := x.review(t)
			if !fresh.Available || fresh.ReviewID == review.ReviewID {
				t.Fatal("fresh review remained blocked after definite refusal")
			}
			accepted, err := s.verifyCleanupAdmission(context.Background(), x.run.RunID, fresh.ReviewID, true)
			if err != nil || accepted.Disposition != "accepted" || accepted.Run.CleanupRecovery == nil || accepted.Run.CleanupRecovery.ReviewID != fresh.ReviewID {
				t.Fatalf("fresh approval did not start: %+v %v", accepted, err)
			}
			run := x.done(t)
			if run.CleanupRecovery == nil || !run.CleanupRecovery.HoldReleased || run.CleanupConfirmed || x.launches.Load() != 1 {
				t.Fatal("fresh verification did not preserve separate original truth")
			}
		})
	}
}

func TestCableCleanupAcceptedHistoryWinsAfterLostReplyAndExpiry(t *testing.T) {
	x := newCleanupProductFixture(t)
	x.authorize()
	review := x.review(t)
	entered, release := make(chan struct{}), make(chan struct{})
	x.readHook = func(ctx context.Context, message cableCleanupMessage) (cableCleanupMessage, error) {
		close(entered)
		select {
		case <-release:
			return message, nil
		case <-ctx.Done():
			return message, ctx.Err()
		}
	}
	first, err := x.f.s.verifyCleanupAdmission(context.Background(), x.run.RunID, review.ReviewID, true)
	if err != nil || first.Disposition != "accepted" {
		t.Fatal("fixture admission failed")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted fixture did not launch")
	}
	// Treat first as a lost reply. Its accepted attempt takes precedence over
	// an expired/retired review and even a subsequently cancelled caller context.
	delete(x.f.s.cleanupReviews, review.ReviewID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retry, err := x.f.s.verifyCleanupAdmission(ctx, x.run.RunID, review.ReviewID, true)
	if err != nil || retry.Disposition != "accepted" || retry.Run.CleanupRecovery == nil || retry.Run.CleanupRecovery.AttemptID != first.Run.CleanupRecovery.AttemptID || x.launches.Load() != 1 {
		t.Fatal("accepted lost reply was reclassified or relaunched")
	}
	close(release)
	x.done(t)
}

func TestCableCleanupNotStartedRequiresCompleteMatchingHistory(t *testing.T) {
	for _, mode := range []string{"wrong-live-run", "wrong-accepted-run", "unknown-run", "invalid-run", "unapproved", "corrupt-history", "cancelled-without-history"} {
		t.Run(mode, func(t *testing.T) {
			x := newCleanupProductFixture(t)
			x.authorize()
			review := x.review(t)
			s := x.f.s
			runID := x.run.RunID
			approved := true
			ctx := context.Background()
			expectLaunches := int32(0)
			if mode == "wrong-accepted-run" {
				if _, err := s.verifyCleanupAdmission(ctx, runID, review.ReviewID, true); err != nil {
					t.Fatal(err)
				}
				x.done(t)
				expectLaunches = 1
			}
			switch mode {
			case "wrong-live-run", "wrong-accepted-run":
				other := *s.runs[runID]
				other.Public = cloneCableRun(other.Public)
				other.Public.RunID = strings.Repeat("b", 32)
				s.runs[other.Public.RunID] = &other
				runID = other.Public.RunID
			case "unknown-run":
				runID = strings.Repeat("c", 32)
			case "invalid-run":
				runID = "invalid"
			case "unapproved":
				approved = false
			case "corrupt-history":
				s.recoveryFailed = true
			case "cancelled-without-history":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := s.verifyCleanupAdmission(ctx, runID, review.ReviewID, approved)
			if err == nil || result.Disposition != "" || x.launches.Load() != expectLaunches {
				t.Fatal("uncertain/mismatched history became authoritative not-started")
			}
			if mode != "wrong-accepted-run" {
				if _, ok := s.cleanupReviews[review.ReviewID]; !ok {
					t.Fatal("identity/transport rejection retired another valid review")
				}
			}
		})
	}
}
