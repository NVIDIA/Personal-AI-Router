// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFabricShutdownDeadlineRetainsPendingRollbackHold(t *testing.T) {
	s, r := fabricServiceFixture(t)
	r.Public.State = "rolling-back"
	before := cloneFabricOperation(r.Public)
	cancelled := false
	r.cancel = func() { cancelled = true }
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := s.shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "owned cleanup is still pending") {
		t.Fatalf("pending rollback became successful shutdown: %v", err)
	}
	if cancelled || !s.held() || !reflect.DeepEqual(r.Public, before) {
		t.Fatal("shutdown timeout cancelled explicit rollback or changed its retained ownership")
	}
	select {
	case <-r.done:
		t.Fatal("shutdown fabricated worker completion")
	default:
	}
	close(r.done)
	if err := s.shutdown(ctx); err != nil {
		t.Fatalf("already joined cleanup was reported pending: %v", err)
	}
	if !s.held() {
		t.Fatal("a repeated shutdown reopened admission")
	}
}

func TestFabricShutdownCancelsApplyingButStillRequiresJoin(t *testing.T) {
	s, r := fabricServiceFixture(t)
	applyCtx, cancelApply := context.WithCancel(context.Background())
	defer cancelApply()
	r.cancel = cancelApply
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unjoined apply was not reported pending: %v", err)
	}
	if applyCtx.Err() == nil || !s.held() {
		t.Fatal("shutdown did not cancel apply and close admission")
	}
	select {
	case <-r.done:
		t.Fatal("apply cancellation was mistaken for cleanup completion")
	default:
	}
	close(r.done)
}

func TestFabricClosedAdmissionRejectsDelayedRecoveryPublication(t *testing.T) {
	s, r := fabricServiceFixture(t)
	r.Public.State = "recovery-required"
	r.done = nil
	before := cloneFabricOperation(r.Public)
	plans := append([]cableLaunchPlan(nil), r.Plans...)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRefresh()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.refreshAccess = func(ctx context.Context, _ *fabricRunRecord) ([]cableLaunchPlan, error) {
		close(entered)
		select {
		case <-release:
			return plans, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	finished := make(chan error, 1)
	go func() {
		_, err := s.recover(ctx, r.Public.OperationID, true)
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("recovery did not reach its read-only access refresh")
	}
	s.closeAdmission()
	if err := s.shutdown(ctx); err != nil {
		t.Fatalf("unpublished recovery was mistaken for an owned worker: %v", err)
	}
	releaseRefresh()
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "shutdown") {
			t.Fatalf("delayed recovery passed shutdown publication fence: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("delayed recovery did not settle")
	}
	if r.done != nil || !reflect.DeepEqual(r.Public, before) || !reflect.DeepEqual(r.Plans, plans) || !s.held() {
		t.Fatal("shutdown allowed new rollback ownership after its join snapshot")
	}
	s.refreshAccess = func(context.Context, *fabricRunRecord) ([]cableLaunchPlan, error) {
		t.Error("closed recovery reached access refresh")
		return nil, errors.New("unexpected refresh")
	}
	if _, err := s.recover(context.Background(), r.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("closed recovery was not rejected before refresh: %v", err)
	}
}

func TestFabricClosedAdmissionPreservesStableAddresses(t *testing.T) {
	s, r := fabricServiceFixture(t)
	r.Public.State = "active"
	r.Public.EffectsApplied = true
	r.Attempted = []bool{true, true}
	r.done = nil
	before := cloneFabricOperation(r.Public)
	s.closeAdmission()
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cancel(r.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("closed stable configuration started a new rollback: %v", err)
	}
	if !reflect.DeepEqual(r.Public, before) || r.done != nil || !s.held() {
		t.Fatal("shutdown changed stable addresses or reopened admission")
	}
	var absent *fabricService
	absent.closeAdmission()
	if err := absent.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFabricClosedApprovalRetainsDefiniteRefusalWithoutEffects(t *testing.T) {
	s, review, calls := fabricRefusalFixture(t)
	s.closeAdmission()
	op, err := s.approve(context.Background(), review.ReviewID, true)
	assertFabricNotStarted(t, op, review, err)
	if calls.Load() != 0 || !s.held() {
		t.Fatal("closed approval reached participants or reopened admission")
	}
	restored := newFabricService(s.m)
	op, err = restored.status(review.ReviewID)
	assertFabricNotStarted(t, op, review, err)
}

func TestFabricClosedAdmissionStillAllowsExactReservationRelease(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	s := m.exec.fabric
	request := fabricSetupControlRequest(m)
	request.Method = "release"
	owner := m.mesh.NodeUUID()
	s.reservation = &fabricReservation{OperationID: request.OperationID, OwnerPrincipal: owner, Target: request.Target}
	s.closeAdmission()
	if _, err := s.localControl(context.Background(), owner, request); err != nil || s.reservation != nil {
		t.Fatalf("closed admission blocked exact reservation cleanup: %v", err)
	}
	if !s.held() {
		t.Fatal("reservation cleanup reopened a closed service")
	}
}
