// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

// The real review/approval/journal code runs, while all participant, SSH and
// native work stays behind existing in-memory seams.
func fabricRefusalFixture(t *testing.T) (*fabricService, fabricReview, *atomic.Int32) {
	t.Helper()
	f := newCableProductFixture(t)
	m := f.s.m
	s := m.exec.fabric
	dial := m.onboarding.dial
	m.onboarding.dial = func(ctx context.Context, c onboardingCandidate, a onboardingAccess) (*onboardingSSH, error) {
		client, err := dial(ctx, c, a)
		if err != nil {
			return nil, err
		}
		run := client.testRun
		client.testRun = func(ctx context.Context, cmd string, input io.Reader) ([]byte, error) {
			if strings.HasSuffix(cmd, " --fabric-address-capabilities-json") {
				return []byte(`{"protocol":"pair-fabric-address/4","persistence":"until-reboot","maxInterfaces":2}`), nil
			}
			return run(ctx, cmd, input)
		}
		return client, nil
	}
	selection := cableTestSelection()
	s.inventory = func(_ context.Context, node string) (fabricInventory, error) {
		facts := fabricServiceInventory(node)
		for i, ref := range selection.Ports {
			if ref.NodeID == node {
				facts.Principal = []string{"principal-owner", "principal-peer"}[i]
				for j := range facts.Interfaces {
					facts.Interfaces[j].PhysicalPort.SwitchID = ref.SwitchID
					facts.Interfaces[j].PhysicalPort.PortName = ref.PortName
				}
			}
		}
		return facts, nil
	}
	calls := new(atomic.Int32)
	s.control = func(_ context.Context, _ fabricTarget, r fabricControlRequest) (fabricControlResult, error) {
		calls.Add(1)
		if r.Method == "reserve" {
			return fabricControlResult{}, errors.New("synthetic reservation refusal")
		}
		return fabricControlResult{}, nil
	}
	s.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if request.Method != "inspect" || request.SelectedPortPauseApproved {
			t.Error("fixture review inspection acquired mutation permission")
		}
		return fabricWorkerResult{Protocol: fabricWorkerProtocol, Method: "inspect", OperationID: request.OperationID, NodeID: request.Target.NodeID,
			Principal: request.Target.Principal, Facts: fabricNativeFacts{Digest: strings.Repeat("a", 64), Routes: []string{}, Blockers: []string{}}}, nil
	}
	review, err := s.review(context.Background(), cableProductReviewRequest{ReviewRequest: selection}, true)
	if err != nil || !review.Executable {
		t.Fatalf("ready review unavailable: %+v %v", review, err)
	}
	s.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
		t.Error("native worker reached refusal fixture")
		return fabricWorkerResult{}, errors.New("no native effects allowed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.shutdown(ctx); err != nil {
			t.Errorf("fabric fixture shutdown: %v", err)
		}
	})
	return s, review, calls
}

// Accepted SSH host keys are one-review consent and candidate IDs do not survive
// an Engine Manager restart, so rollback re-accepts each node's current
// candidate only for the key approved for that node in this operation.
func restartFabricRollbackAccess(t *testing.T, s *fabricService, plans []cableLaunchPlan) ([]string, []string) {
	t.Helper()
	// A restarted Engine Manager has no process-local candidates. Fresh cable
	// authorization recreates them with new IDs while retaining the exact
	// endpoint, account and observed SSH identity.
	s.m.onboarding.mu.Lock()
	s.m.onboarding.targets = map[string]*onboardingPrivateTarget{}
	s.m.onboarding.mu.Unlock()
	freshIDs := make([]string, len(plans))
	freshGenerations := make([]string, len(plans))
	for i, plan := range plans {
		candidate, err := s.m.onboarding.addTarget(onboardingAddTargetRequest{Address: plan.Candidate.Address, Port: plan.Candidate.Port, Label: plan.NodeID})
		if err != nil {
			t.Fatal(err)
		}
		s.m.onboarding.mu.Lock()
		target := s.m.onboarding.targets[candidate.CandidateID]
		target.candidate = plan.Candidate
		target.candidate.CandidateID = candidate.CandidateID
		target.candidate.AccessID = "fresh-access-" + string(rune('a'+i))
		target.candidate.HostKeyTrusted = false
		freshGenerations[i] = "fresh-generation-" + string(rune('a'+i))
		target.accessGeneration = freshGenerations[i]
		target.expiresAt = time.Now().Add(time.Minute)
		target.access = onboardingAccess{user: "synthetic-user", password: "synthetic-access-input", elevationPassword: "synthetic-admin-input"}
		freshIDs[i] = candidate.CandidateID
		s.m.onboarding.mu.Unlock()
	}
	return freshIDs, freshGenerations
}

func TestFabricRollbackReacceptsOnlyTheOperationHostKeys(t *testing.T) {
	s, review, _ := fabricRefusalFixture(t)
	s.mu.Lock()
	plans := append([]cableLaunchPlan(nil), s.reviews[review.ReviewID].access.plans...)
	s.mu.Unlock()
	freshIDs, freshGenerations := restartFabricRollbackAccess(t, s, plans)
	s.m.cables.mu.Lock()
	reviewsBefore := len(s.m.cables.reviews)
	s.m.cables.mu.Unlock()
	r := &fabricRunRecord{Public: fabricOperation{OperationID: review.ReviewID, Targets: review.Targets}, Plans: plans}
	got, err := s.refreshRollbackPlans(context.Background(), r)
	if err != nil {
		t.Fatalf("approved operation host keys were not re-accepted for rollback: %v", err)
	}
	for i, plan := range got {
		if plan.Candidate.CandidateID != freshIDs[i] || plan.Candidate.CandidateID == plans[i].Candidate.CandidateID || plan.AccessGeneration != freshGenerations[i] {
			t.Fatalf("rollback did not bind only the fresh candidate: old=%s fresh=%s got=%s generation=%s", plans[i].Candidate.CandidateID, freshIDs[i], plan.Candidate.CandidateID, plan.AccessGeneration)
		}
	}
	s.m.cables.mu.Lock()
	reviewsAfter := len(s.m.cables.reviews)
	s.m.cables.mu.Unlock()
	if reviewsAfter != reviewsBefore+1 {
		t.Fatalf("restart rollback used %d cable reviews; want one exact current review", reviewsAfter-reviewsBefore)
	}
	r.Plans[len(r.Plans)-1].Candidate.HostKeySHA256 = "SHA256:" + strings.Repeat("b", 43)
	if _, err := s.refreshRollbackPlans(context.Background(), r); err == nil {
		t.Fatal("a changed host key authorized rollback")
	}
}

func TestFabricRollbackRejectsAmbientTrustWithAnotherHostKey(t *testing.T) {
	s, review, calls := fabricRefusalFixture(t)
	s.mu.Lock()
	plans := append([]cableLaunchPlan(nil), s.reviews[review.ReviewID].access.plans...)
	s.mu.Unlock()
	s.m.onboarding.mu.Lock()
	for _, target := range s.m.onboarding.targets {
		target.candidate.HostKeyTrusted = true
		target.candidate.HostKeySHA256 = "SHA256:" + strings.Repeat("b", 43)
		break
	}
	s.m.onboarding.mu.Unlock()
	r := &fabricRunRecord{Public: fabricOperation{OperationID: review.ReviewID, Targets: review.Targets}, Plans: plans}
	beforePublic := cloneFabricOperation(r.Public)
	before := append([]cableLaunchPlan(nil), r.Plans...)
	if _, err := s.refreshRollbackPlans(context.Background(), r); err == nil {
		t.Fatal("ambient trust replaced the operation-approved SSH fingerprint")
	}
	if !reflect.DeepEqual(r.Public, beforePublic) || !reflect.DeepEqual(r.Plans, before) || calls.Load() != 0 {
		t.Fatal("changed-key refusal changed the retained plan or reached a fabric mutation")
	}
}

func TestFabricRollbackUsesCurrentAuthorizedEndpointForSameDevice(t *testing.T) {
	s, review, _ := fabricRefusalFixture(t)
	s.mu.Lock()
	plans := append([]cableLaunchPlan(nil), s.reviews[review.ReviewID].access.plans...)
	s.mu.Unlock()
	remote := slices.IndexFunc(plans, func(plan cableLaunchPlan) bool { return plan.NodeID == "host-peer" })
	if remote < 0 {
		t.Fatal("fixture remote plan unavailable")
	}
	oldID := plans[remote].Candidate.CandidateID
	s.m.onboarding.mu.Lock()
	current := *s.m.onboarding.targets[oldID]
	delete(s.m.onboarding.targets, oldID)
	current.candidate.CandidateID = newOpID()
	current.candidate.Address = "192.0.2.8"
	current.candidate.AccessID = "fresh-access-address"
	current.accessGeneration = "fresh-generation-address"
	s.m.onboarding.targets[current.candidate.CandidateID] = &current
	s.m.onboarding.mu.Unlock()
	s.m.peers.mu.Lock()
	s.m.peers.peers["host-peer"] = ecPeer{nodeID: "host-peer", clusterUUID: "principal-peer", addresses: []string{"192.0.2.8"}, port: 14323}
	s.m.peers.mu.Unlock()
	dial := s.m.onboarding.dial
	s.m.onboarding.dial = func(ctx context.Context, candidate onboardingCandidate, access onboardingAccess) (*onboardingSSH, error) {
		fixtureCandidate := candidate
		if fixtureCandidate.Address == "192.0.2.8" {
			fixtureCandidate.Address = "192.0.2.7"
		}
		return dial(ctx, fixtureCandidate, access)
	}
	r := &fabricRunRecord{Public: fabricOperation{OperationID: review.ReviewID, Targets: review.Targets}, Plans: plans}
	got, err := s.refreshRollbackPlans(context.Background(), r)
	if err != nil {
		t.Fatalf("same device at its current authorized endpoint was rejected: %v", err)
	}
	if got[remote].Candidate.CandidateID != current.candidate.CandidateID || got[remote].Candidate.Address != current.candidate.Address || got[remote].Candidate.HostKeySHA256 != plans[remote].Candidate.HostKeySHA256 || got[remote].AccessGeneration != current.accessGeneration {
		t.Fatal("rollback did not bind the current endpoint to the retained device identity")
	}
}

func TestFabricRollbackRejectsCandidateRotationDuringReview(t *testing.T) {
	s, review, calls := fabricRefusalFixture(t)
	s.mu.Lock()
	plans := append([]cableLaunchPlan(nil), s.reviews[review.ReviewID].access.plans...)
	s.mu.Unlock()
	r := &fabricRunRecord{Public: fabricOperation{OperationID: review.ReviewID, Targets: review.Targets}, Plans: plans}
	before := append([]cableLaunchPlan(nil), r.Plans...)
	dial := s.m.onboarding.dial
	var rotated atomic.Bool
	s.m.onboarding.dial = func(ctx context.Context, candidate onboardingCandidate, access onboardingAccess) (*onboardingSSH, error) {
		if rotated.CompareAndSwap(false, true) {
			s.m.onboarding.mu.Lock()
			current := s.m.onboarding.targets[candidate.CandidateID]
			if current != nil {
				copy := *current
				delete(s.m.onboarding.targets, candidate.CandidateID)
				copy.candidate.CandidateID = newOpID()
				s.m.onboarding.targets[copy.candidate.CandidateID] = &copy
			}
			s.m.onboarding.mu.Unlock()
		}
		return dial(ctx, candidate, access)
	}
	if _, err := s.refreshRollbackPlans(context.Background(), r); err == nil {
		t.Fatal("candidate rotation during rollback review was accepted")
	}
	if !rotated.Load() || !reflect.DeepEqual(r.Plans, before) || calls.Load() != 0 {
		t.Fatal("rollback race refusal changed the retained plan or reached a fabric mutation")
	}
}

func TestFabricRollbackRejectsAmbiguousCurrentCandidates(t *testing.T) {
	s, review, calls := fabricRefusalFixture(t)
	s.mu.Lock()
	plans := append([]cableLaunchPlan(nil), s.reviews[review.ReviewID].access.plans...)
	s.mu.Unlock()
	s.m.onboarding.mu.Lock()
	duplicate := *s.m.onboarding.targets[plans[0].Candidate.CandidateID]
	duplicate.candidate.CandidateID = newOpID()
	duplicate.candidate.AccessID = "duplicate-access"
	duplicate.accessGeneration = "duplicate-generation"
	s.m.onboarding.targets[duplicate.candidate.CandidateID] = &duplicate
	s.m.onboarding.mu.Unlock()
	r := &fabricRunRecord{Public: fabricOperation{OperationID: review.ReviewID, Targets: review.Targets}, Plans: plans}
	before := append([]cableLaunchPlan(nil), r.Plans...)
	if _, err := s.refreshRollbackPlans(context.Background(), r); err == nil {
		t.Fatal("ambiguous current candidates authorized rollback")
	}
	if !reflect.DeepEqual(r.Plans, before) || calls.Load() != 0 {
		t.Fatal("ambiguous-candidate refusal changed the retained plan or reached a fabric mutation")
	}
}

func TestFabricRecoveryUsesFreshCandidatesAfterRestart(t *testing.T) {
	s, review, _ := fabricRefusalFixture(t)
	s.mu.Lock()
	record := s.reviews[review.ReviewID]
	plans := append([]cableLaunchPlan(nil), record.access.plans...)
	s.mu.Unlock()
	freshIDs, freshGenerations := restartFabricRollbackAccess(t, s, plans)
	id := newOpID()
	r := &fabricRunRecord{
		AdministratorApproved:     true,
		SelectedPortPauseApproved: true,
		OwnerPrincipal:            record.access.ownerPrincipal,
		Public: fabricOperation{SchemaVersion: 1, OperationID: id, ReviewID: id, OwnerNodeID: review.OwnerNodeID, RecipeID: review.RecipeID,
			State: "recovery-required", Targets: review.Targets, EffectsApplied: true, CreatedAt: time.Now().Add(-time.Minute).UnixMilli()},
		Plans: plans, Attempted: make([]bool, len(plans)), Reserved: make([]bool, len(plans)),
	}
	for i := range r.Attempted {
		r.Attempted[i] = true
	}
	s.runs[id] = r
	var controlCalls, rollbackCalls atomic.Int32
	var unexpectedWorker atomic.Bool
	s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
		controlCalls.Add(1)
		return fabricControlResult{}, nil
	}
	s.worker = func(_ context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if request.Method != "rollback" {
			unexpectedWorker.Store(true)
			return fabricWorkerResult{}, errors.New("unexpected fabric worker method")
		}
		rollbackCalls.Add(1)
		return fabricWorkerResult{CleanupConfirmed: true}, nil
	}
	if _, err := s.recover(context.Background(), id, true); err != nil {
		t.Fatalf("restart recovery was not admitted: %v", err)
	}
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("restart recovery did not settle")
	}
	op, err := s.status(id)
	if err != nil || op.State != "cancelled" || !op.CleanupConfirmed {
		t.Fatalf("restart recovery did not confirm cleanup: %+v %v", op, err)
	}
	if unexpectedWorker.Load() || rollbackCalls.Load() != int32(len(plans)) || controlCalls.Load() != int32(2*len(plans)) {
		t.Fatalf("unexpected recovery calls: worker=%d control=%d unexpected=%v", rollbackCalls.Load(), controlCalls.Load(), unexpectedWorker.Load())
	}
	for i, plan := range r.Plans {
		if plan.Candidate.CandidateID != freshIDs[i] || plan.Candidate.CandidateID == plans[i].Candidate.CandidateID || plan.AccessGeneration != freshGenerations[i] {
			t.Fatalf("recovery retained stale candidate custody: old=%s fresh=%s got=%s generation=%s", plans[i].Candidate.CandidateID, freshIDs[i], plan.Candidate.CandidateID, plan.AccessGeneration)
		}
	}
}

func assertFabricNotStarted(t *testing.T, op fabricOperation, review fabricReview, err error) {
	t.Helper()
	if err != nil || op.State != "not-started" || !op.CleanupConfirmed || op.EffectsApplied || op.EffectsUnconfirmed || op.OperationID != review.ReviewID || op.ReviewID != review.ReviewID || op.OwnerNodeID != review.OwnerNodeID {
		t.Fatalf("definitive refusal not admitted: %+v %v", op, err)
	}
	want, _ := json.Marshal(review.Targets)
	got, _ := json.Marshal(op.Targets)
	if string(want) != string(got) {
		t.Fatal("refusal lost exact review scope")
	}
}

func TestFabricRefusalDefiniteAdmissionIsDurable(t *testing.T) {
	for _, reason := range []string{"expired", "inventory", "access", "busy"} {
		t.Run(reason, func(t *testing.T) {
			s, review, calls := fabricRefusalFixture(t)
			switch reason {
			case "expired":
				r := s.reviews[review.ReviewID]
				r.expires = time.Now().Add(-time.Second)
				s.reviews[review.ReviewID] = r
			case "inventory":
				s.inventory = func(context.Context, string) (fabricInventory, error) {
					return fabricInventory{}, errors.New("synthetic stale routes")
				}
			case "access":
				for _, target := range s.m.onboarding.targets {
					target.expiresAt = time.Now().Add(-time.Second)
				}
			case "busy":
				s.m.exec.diagnostics.mu.Lock()
				s.m.exec.diagnostics.cancels["other-operation"] = func() {}
				s.m.exec.diagnostics.mu.Unlock()
			}
			op, err := s.approve(context.Background(), review.ReviewID, true)
			assertFabricNotStarted(t, op, review, err)
			op, err = s.status(review.ReviewID)
			assertFabricNotStarted(t, op, review, err)
			restored := newFabricService(s.m)
			op, err = restored.status(review.ReviewID)
			assertFabricNotStarted(t, op, review, err)
			op, err = restored.approve(context.Background(), review.ReviewID, true)
			assertFabricNotStarted(t, op, review, err)
			if calls.Load() != 0 || s.held() || restored.held() {
				t.Fatal("no-effects refusal invoked participants or retained a fabric hold")
			}
		})
	}
}

func TestFabricRefusalReloadClosesKnownUnusedReviewButNotUnknown(t *testing.T) {
	s, review, calls := fabricRefusalFixture(t)
	restored := newFabricService(s.m)
	op, err := restored.status(review.ReviewID)
	assertFabricNotStarted(t, op, review, err)
	op, err = restored.cancel(review.ReviewID)
	assertFabricNotStarted(t, op, review, err)
	if _, err = restored.status(strings.Repeat("f", 32)); err == nil {
		t.Fatal("unknown status invented no-effects proof")
	}
	if _, err = restored.approve(context.Background(), strings.Repeat("f", 32), true); err == nil {
		t.Fatal("unknown approval invented no-effects proof")
	}
	if calls.Load() != 0 {
		t.Fatal("unused review invoked participant effects")
	}
}

func TestFabricRefusalCannotRaceAcceptedApprovalOrLostReply(t *testing.T) {
	s, review, calls := fabricRefusalFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	read := s.inventory
	var first atomic.Bool
	s.inventory = func(ctx context.Context, node string) (fabricInventory, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return read(ctx, node)
	}
	type result struct {
		op  fabricOperation
		err error
	}
	accepted := make(chan result, 1)
	go func() { op, err := s.approve(context.Background(), review.ReviewID, true); accepted <- result{op, err} }()
	<-entered
	status := make(chan result, 1)
	duplicate := make(chan result, 1)
	go func() { op, err := s.status(review.ReviewID); status <- result{op, err} }()
	go func() {
		op, err := s.approve(context.Background(), review.ReviewID, true)
		duplicate <- result{op, err}
	}()
	select {
	case early := <-status:
		close(release)
		<-accepted
		t.Fatalf("status escaped in-flight admission: %+v", early)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for _, ch := range []chan result{accepted, status, duplicate} {
		select {
		case r := <-ch:
			if r.err != nil || r.op.State == "not-started" || r.op.OperationID != review.ReviewID {
				t.Fatalf("acceptance replaced or lost: %+v", r)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("serialized approval did not settle")
		}
	}
	shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopShutdown()
	if err := s.shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	restored := newFabricService(s.m)
	op, err := restored.status(review.ReviewID)
	if err != nil || op.State == "not-started" {
		t.Fatalf("lost accepted reply was relabeled: %+v %v", op, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("duplicate approval repeated participant reservation: %d", calls.Load())
	}
}

func TestFabricRefusalChangedOwnerAndUncertainJournalStayUnknown(t *testing.T) {
	t.Run("changed owner", func(t *testing.T) {
		s, review, calls := fabricRefusalFixture(t)
		op, err := s.status(review.ReviewID)
		assertFabricNotStarted(t, op, review, err)
		dir := t.TempDir()
		clustertrusttest.Join(t, dir, "different-cluster", "different-owner", "different-peer")
		s.m.mesh = clustertrust.Open(dir)
		if _, err = s.status(review.ReviewID); err == nil {
			t.Fatal("different principal admitted prior refusal")
		}
		if _, err = s.approve(context.Background(), review.ReviewID, true); err == nil {
			t.Fatal("different principal replayed prior refusal")
		}
		if _, err = s.cancel(review.ReviewID); err == nil {
			t.Fatal("different principal cancelled prior refusal")
		}
		if calls.Load() != 0 {
			t.Fatal("owner mismatch invoked participants")
		}
	})
	t.Run("uncertain accepted file", func(t *testing.T) {
		s, review, calls := fabricRefusalFixture(t)
		if err := os.MkdirAll(filepath.Dir(s.file(review.ReviewID)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONAtomic(s.file(review.ReviewID), map[string]string{"incomplete": "accepted outcome is not readable"}); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(s.file(review.ReviewID))
		if _, err := s.status(review.ReviewID); err == nil {
			t.Fatal("existing uncertain file became no-effects proof")
		}
		if _, err := s.approve(context.Background(), review.ReviewID, true); err == nil {
			t.Fatal("approval replaced existing outcome")
		}
		after, _ := os.ReadFile(s.file(review.ReviewID))
		if string(before) != string(after) || calls.Load() != 0 {
			t.Fatal("uncertain outcome was overwritten or executed")
		}
	})
}

func TestFabricRefusalRecordCannotCarryEffects(t *testing.T) {
	s, review, _ := fabricRefusalFixture(t)
	op, err := s.status(review.ReviewID)
	assertFabricNotStarted(t, op, review, err)
	baseline := *s.runs[review.ReviewID]
	for _, change := range []func(*fabricRunRecord){
		func(r *fabricRunRecord) { r.Public.EffectsApplied = true },
		func(r *fabricRunRecord) { r.Public.EffectsUnconfirmed = true },
		func(r *fabricRunRecord) { r.Public.CleanupConfirmed = false },
		func(r *fabricRunRecord) { r.Attempted = []bool{true} },
		func(r *fabricRunRecord) { r.Reserved = []bool{true} },
		func(r *fabricRunRecord) { r.Plans = []cableLaunchPlan{{NodeID: "unexpected"}} },
	} {
		record := baseline
		change(&record)
		if validFabricRecord(record) {
			t.Fatal("no-effects outcome accepted execution evidence")
		}
	}
}
