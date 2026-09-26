// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only transport and cluster I/O are substituted. The actual review, package
// validation, byte transfer, service sequence, invitation, convergence, journal
// and terminal cleanup code executes. No listener or host is created.
type onboardingControllerFixture struct {
	t                                              *testing.T
	mu                                             sync.Mutex
	s                                              *onboardingService
	pkg                                            onboardingPackage
	info                                           onboardingPlatformInfo
	ids                                            []string
	devices                                        map[string]*onboardingFixtureDevice
	clusterID                                      string
	invites, responses, cancellations, rosterReads int
	delayRoster                                    int
	requests                                       map[string]onboardingInvite
	expected                                       map[string]string
	loseReply                                      bool
	statusFailures                                 int
	cancelUncertain                                bool
	failStarts                                     int
	blockAddress                                   string
	entered                                        chan struct{}
	release                                        chan struct{}
}
type onboardingFixtureDevice struct {
	nodeID                                                 string
	installed, serviceInstalled, started, paired, finished bool
	transfers                                              int
	receipt                                                onboardingInstallReceipt
}

const onboardingFixtureController = "11111111-1111-4111-8111-111111111111"

func newOnboardingControllerFixture(t *testing.T, count int) *onboardingControllerFixture {
	t.Helper()
	info, pkg, _ := onboardingInstallFixture(t)
	info.UserRuntime, info.FreeBytes = true, 4<<30
	info.Hostname = "fixture-spark"
	f := &onboardingControllerFixture{t: t, pkg: pkg, info: info, devices: map[string]*onboardingFixtureDevice{}, requests: map[string]onboardingInvite{}, expected: map[string]string{}}
	f.s = NewManager(nil, newTestExecutor(t, testEngineManifest(fakeEngineBin)), nil).onboarding
	f.s.imports[pkg.source.ArtifactID] = onboardingArtifactSource{onboardingArtifact: pkg.source.onboardingArtifact, File: pkg.file}
	for i := 0; i < count; i++ {
		c, err := f.s.addTarget(onboardingAddTargetRequest{Address: fmt.Sprintf("192.0.2.%d", i+10)})
		if err != nil {
			t.Fatal(err)
		}
		p := f.s.targets[c.CandidateID]
		p.candidate.AccessID, p.candidate.AccessLabel = "fixture-access", "approved-user (password)"
		p.candidate.AccessAvailable = true
		p.candidate.HostKeySHA256 = fmt.Sprintf("SHA256:fixture-%d", i)
		p.access = onboardingAccess{user: "approved-user", password: "transient-fixture-password"}
		p.accessGeneration, p.lifetime, p.expiresAt = "first-entry", "session", time.Now().Add(time.Minute)
		f.ids = append(f.ids, c.CandidateID)
		f.devices[c.Address] = &onboardingFixtureDevice{nodeID: fmt.Sprintf("22222222-2222-4222-8222-%012d", i+1)}
	}
	f.s.dial = func(ctx context.Context, c onboardingCandidate, a onboardingAccess) (*onboardingSSH, error) {
		if !c.HostKeyTrusted || a.user != "approved-user" || a.password == "" {
			return nil, errors.New("fake transport rejected unapproved access")
		}
		if f.devices[c.Address] == nil {
			return nil, errors.New("unselected device")
		}
		return &onboardingSSH{testRun: func(ctx context.Context, command string, input io.Reader) ([]byte, error) {
			return f.run(ctx, c.Address, command, input)
		}}, nil
	}
	f.s.testCluster = f.cluster
	return f
}
func (f *onboardingControllerFixture) inspect() onboardingReview {
	f.t.Helper()
	r := onboardingInspectRequest{CandidateIDs: f.ids, ArtifactID: f.pkg.source.ArtifactID}
	for _, id := range f.ids {
		r.AcceptedHostKeys = append(r.AcceptedHostKeys, struct {
			CandidateID string `json:"candidateId"`
			SHA256      string `json:"sha256"`
		}{id, f.s.targets[id].candidate.HostKeySHA256})
	}
	review, err := f.s.inspect(context.Background(), r)
	if err != nil || !review.CanApprove {
		f.t.Fatalf("inspection not ready: %+v %v", review, err)
	}
	for _, id := range f.ids {
		if f.s.targets[id].candidate.HostKeyTrusted {
			f.t.Fatal("session consent became existing OS trust")
		}
	}
	return review
}
func (f *onboardingControllerFixture) run(ctx context.Context, address, command string, input io.Reader) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if address == f.blockAddress && command == onboardingPython(onboardingReceiveScript) {
		close(f.entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.release:
		}
	}
	var raw []byte
	if input != nil {
		raw, _ = io.ReadAll(input)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devices[address]
	if command == onboardingPython(onboardingInspectScript) {
		info := f.info
		info.ExistingPAIR = d.installed
		return onboardingMarshal(info), nil
	}
	if command == onboardingPython(onboardingReceiveScript) {
		parts := bytes.SplitN(raw, []byte("\n"), 2)
		var h struct {
			OperationID string `json:"operationId"`
			SHA256      string `json:"sha256"`
			Bytes       int64  `json:"bytes"`
		}
		if len(parts) != 2 || json.Unmarshal(parts[0], &h) != nil {
			return nil, errors.New("invalid fixed transfer header")
		}
		hash := sha256.Sum256(parts[1])
		if h.SHA256 != f.pkg.source.SHA256 || hex.EncodeToString(hash[:]) != h.SHA256 || int64(len(parts[1])) != h.Bytes {
			return nil, errors.New("transfer bytes differ from review")
		}
		d.receipt, _ = onboardingInstallPaths(f.info, f.pkg, h.OperationID)
		d.receipt.ManifestSHA256, _ = onboardingPackageManifestHash(f.pkg)
		d.receipt.StartupLifetime = "session"
		d.receipt.Installed = true
		d.receipt.Recoverable = true
		d.installed = true
		d.transfers++
		return onboardingMarshal(d.receipt), nil
	}
	var request struct {
		Receipt  onboardingInstallReceipt `json:"receipt"`
		Action   string                   `json:"action"`
		Lifetime string                   `json:"lifetime"`
		Method   string                   `json:"method"`
		Params   map[string]any           `json:"params"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Receipt.BundlePath != d.receipt.BundlePath || !d.installed {
		return nil, errors.New("command not bound to activated receipt")
	}
	switch command {
	case onboardingPython(onboardingOwnedScript + onboardingServiceScript):
		state := ""
		switch request.Action {
		case "install":
			d.serviceInstalled = true
			state = "installed"
		case "start":
			if !d.serviceInstalled {
				return nil, errors.New("start before install")
			}
			if f.failStarts > 0 {
				f.failStarts--
				return nil, errors.New("fixture start failure")
			}
			d.started = true
			state = "started"
		default:
			return nil, errors.New("unapproved service action")
		}
		if request.Lifetime != "session" {
			return nil, errors.New("lifetime changed")
		}
		return onboardingMarshal(map[string]any{"unit": "nvidia-pair-headless.service", "operation": request.Action, "state": state, "persistence": "user-session", "requestedLifetime": "session", "effectiveLifetime": "session"}), nil
	case onboardingPython(onboardingOwnedScript + onboardingRecoveryScript):
		if !d.started || d.paired {
			return nil, errors.New("not an owned unpaired parent")
		}
		return onboardingMarshal(map[string]any{"nodeId": d.nodeID, "state": "installed-not-paired", "recoverable": true}), nil
	case onboardingPython(onboardingOwnedScript + onboardingControlScript):
		if !d.started {
			return nil, errors.New("parent not ready")
		}
		var result any
		switch request.Method {
		case "cluster:get-node-id":
			group := ""
			if d.paired {
				group = f.clusterID
			}
			result = map[string]any{"nodeUuid": d.nodeID, "clusterId": group}
		case "cluster:respond-to-invite":
			if request.Params["pin"] != "654321" || request.Params["inviteId"] != "invite-"+d.nodeID || request.Params["accept"] != true {
				return nil, errors.New("incorrect invitation completion")
			}
			f.responses++
			d.paired = true
			for key, invite := range f.requests {
				if invite.ToNodeID == d.nodeID {
					invite.State = "paired"
					invite.Pin = ""
					f.requests[key] = invite
				}
			}
			result = map[string]any{"accepted": true}
		case "nodes:get-initial":
			result = map[string]any{"nodes": []map[string]string{{"nodeUuid": onboardingFixtureController, "state": "member"}}}
		default:
			return nil, errors.New("unapproved control method")
		}
		return onboardingMarshal(map[string]any{"result": result}), nil
	case onboardingPython(onboardingOwnedScript + onboardingFinishScript):
		if !d.paired {
			return nil, errors.New("finish before pairing")
		}
		d.finished = true
		return []byte(`{"stagingCleaned":true}`), nil
	case onboardingPython(onboardingOwnedScript + onboardingCleanupScript):
		if d.paired {
			return nil, errors.New("committed membership is not rollback")
		}
		d.started = false
		return []byte(`{"cleanupConfirmed":true,"bundleRetained":true}`), nil
	case onboardingPython(onboardingOwnedScript + onboardingResumeScript):
		return []byte(`{"resumable":true}`), nil
	}
	return nil, errors.New("unexpected fixed command")
}
func (f *onboardingControllerFixture) cluster(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "cluster:get-node-id":
		return onboardingMarshal(map[string]any{"nodeUuid": onboardingFixtureController, "clusterId": f.clusterID}), nil
	case "cluster:invite-node":
		raw, _ := json.Marshal(params)
		var p map[string]string
		_ = json.Unmarshal(raw, &p)
		d := f.devices[p["address"]]
		if d == nil || p["nodeId"] != d.nodeID || !d.started {
			return nil, errors.New("invite not bound to ready selected member")
		}
		key := p["requestKey"]
		if !onboardingID.MatchString(key) {
			return nil, errors.New("missing durable request key")
		}
		if invite, ok := f.requests[key]; ok {
			if invite.ToNodeID != d.nodeID || f.expected[key] != p["expectedClusterId"] {
				return nil, errors.New("key tuple changed")
			}
			return onboardingMarshal(invite), nil
		}
		if expected, ok := p["expectedClusterId"]; !ok || expected != f.clusterID {
			return nil, &onboardingClusterError{Code: -32004, Message: "reviewed cluster changed"}
		}
		f.invites++
		f.clusterID = "33333333-3333-4333-8333-333333333333"
		invite := onboardingInvite{InviteID: "invite-" + d.nodeID, Pin: "654321", State: "pending", ClusterID: f.clusterID, FromNodeUUID: onboardingFixtureController, ToNodeID: d.nodeID}
		f.requests[key] = invite
		f.expected[key] = p["expectedClusterId"]
		if f.loseReply {
			f.loseReply = false
			return nil, errors.New("fixture lost invitation reply")
		}
		return onboardingMarshal(invite), nil
	case "cluster:invite-status":
		if f.statusFailures > 0 {
			f.statusFailures--
			return nil, errors.New("fixture status temporarily unavailable")
		}
		raw, _ := json.Marshal(params)
		var p map[string]string
		_ = json.Unmarshal(raw, &p)
		for key, invite := range f.requests {
			if key == p["requestKey"] || invite.InviteID == p["inviteId"] {
				return onboardingMarshal(invite), nil
			}
		}
		return nil, &onboardingClusterError{Code: -32001, Message: "unknown requestKey"}
	case "cluster:cancel-invite":
		f.cancellations++
		if f.cancelUncertain {
			return nil, errors.New("fixture cancel response uncertain")
		}
		raw, _ := json.Marshal(params)
		var p map[string]string
		_ = json.Unmarshal(raw, &p)
		for key, invite := range f.requests {
			if invite.InviteID == p["inviteId"] {
				invite.State = "canceled"
				invite.Pin = ""
				f.requests[key] = invite
				return onboardingMarshal(invite), nil
			}
		}
		return nil, &onboardingClusterError{Code: -32001, Message: "unknown invite"}
	case "nodes:get-initial":
		f.rosterReads++
		nodes := []map[string]string{}
		if f.rosterReads > f.delayRoster {
			for _, d := range f.devices {
				if d.paired {
					nodes = append(nodes, map[string]string{"nodeUuid": d.nodeID, "state": "member"})
				}
			}
		}
		return onboardingMarshal(map[string]any{"nodes": nodes}), nil
	}
	return nil, errors.New("unapproved local cluster method")
}
func waitOnboardingTerminal(t *testing.T, s *onboardingService, id string) onboardingOperation {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		op := cloneOnboardingOperation(s.operations[id].Public)
		s.mu.Unlock()
		if op.State != "running" {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("controller did not settle")
	return onboardingOperation{}
}
func TestOnboardingControllerTwoTargetHappyPathAndDelayedMembership(t *testing.T) {
	f := newOnboardingControllerFixture(t, 2)
	f.delayRoster = 2
	review := f.inspect()
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	terminal := waitOnboardingTerminal(t, f.s, op.OperationID)
	if terminal.State != "completed" || terminal.FinishedAt == 0 || terminal.Revision <= op.Revision {
		t.Fatalf("not completed: %+v", terminal)
	}
	for _, target := range terminal.Targets {
		if target.Stage != "paired" || target.NodeID == "" || !target.CleanupConfirmed {
			t.Fatalf("unverified terminal target: %+v", target)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invites != 2 || f.responses != 2 || f.rosterReads < 4 {
		t.Fatalf("duplicate/missing handshake or convergence: %d/%d/%d", f.invites, f.responses, f.rosterReads)
	}
	for _, d := range f.devices {
		if d.transfers != 1 || !d.started || !d.finished {
			t.Fatal("full owned path did not execute once")
		}
	}
	again, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil || again.OperationID != op.OperationID {
		t.Fatal("lost approve response duplicated operation")
	}
	data, err := os.ReadFile(f.s.operationPath(op.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("654321")) || bytes.Contains(data, []byte("transient-fixture-password")) {
		t.Fatal("secret persisted in journal")
	}
	for _, p := range f.s.targets {
		if p.access.password != "" || p.candidate.AccessAvailable {
			t.Fatal("terminal access retained")
		}
	}
}
func TestOnboardingControllerInviteCheckpointFailureNeverSendsCompletion(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	review := f.inspect()
	f.s.testSave = func(run *onboardingRun) error {
		for _, p := range run.Plans {
			if p.InviteID != "" {
				return errors.New("fixture journal unavailable")
			}
		}
		return nil
	}
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	terminal := waitOnboardingTerminal(t, f.s, op.OperationID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if terminal.State == "completed" || f.responses != 0 || f.cancellations != 1 || f.invites != 1 {
		t.Fatalf("unsafe checkpoint path: %+v invites=%d completion=%d cancel=%d", terminal, f.invites, f.responses, f.cancellations)
	}
}
func TestOnboardingCredentialExpiryAndNewerEntryPreservation(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	p := f.s.targets[f.ids[0]]
	p.expiresAt = time.Now().Add(-time.Second)
	f.s.expireAccess(time.Now())
	if p.access.password != "" || p.candidate.AccessAvailable {
		t.Fatal("abandoned access did not expire")
	}
	p.access = onboardingAccess{user: "approved-user", password: "new-entry"}
	p.accessGeneration = "newer"
	p.candidate.AccessAvailable = true
	run := &onboardingRun{Public: onboardingOperation{OperationID: newOpID(), State: "running", Targets: []onboardingTargetState{{CandidateID: f.ids[0], Stage: "paired"}}}, Plans: map[string]onboardingPlan{f.ids[0]: {AccessGeneration: "older"}}}
	f.s.executeOperation(context.Background(), run, nil)
	if p.access.password != "new-entry" || !p.candidate.AccessAvailable {
		t.Fatal("terminal release erased a newer access entry")
	}
}
func TestOnboardingRecoveryDoesNotDropLateUnfinishedOrCorruptHistory(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			f := newOnboardingControllerFixture(t, 1)
			id := f.ids[0]
			target := f.s.targets[id]
			for i := 0; i < 40; i++ {
				rid := fmt.Sprintf("%032x", i+1)
				state := "completed"
				stage := "paired"
				if i == 39 {
					state = "running"
					stage = "starting"
				}
				run := &onboardingRun{Public: onboardingOperation{OperationID: rid, Revision: 1, State: state, Targets: []onboardingTargetState{{CandidateID: id, Stage: stage}}}, Plans: map[string]onboardingPlan{id: {Candidate: target.candidate, Info: f.info, Username: "approved-user"}}}
				if err := f.s.saveRun(run); err != nil {
					t.Fatal(err)
				}
			}
			if corrupt {
				if err := os.WriteFile(f.s.operationPath(strings.Repeat("f", 32)), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reloaded := newOnboardingService(f.s.m)
			if len(reloaded.operations) != 40 || reloaded.operations[fmt.Sprintf("%032x", 40)].Public.State != "interrupted" {
				t.Fatal("late unfinished operation vanished")
			}
			if reloaded.recoveryRequired != corrupt {
				t.Fatal("corrupt history did not fence new setup")
			}
		})
	}
}
func TestOnboardingRetryCheckpointFailureDoesNotLaunch(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	id := f.ids[0]
	p := f.s.targets[id]
	run := &onboardingRun{Public: onboardingOperation{OperationID: newOpID(), Revision: 1, State: "failed", Targets: []onboardingTargetState{{CandidateID: id, Stage: "verification-failed"}}}, Deadline: time.Now().Add(time.Minute).UnixMilli(), Plans: map[string]onboardingPlan{id: {Candidate: p.candidate, Review: onboardingReviewTarget{HostKeySHA256: p.candidate.HostKeySHA256}, Username: p.access.user}}, cancelTarget: map[string]bool{}}
	f.s.operations[run.Public.OperationID] = run
	f.s.testSave = func(*onboardingRun) error { return errors.New("unavailable") }
	_, err := f.s.operationRequest(context.Background(), "engine:onboarding-retry", onboardingOperationRequest{OperationID: run.Public.OperationID})
	if err == nil || f.s.active != "" || !f.s.recoveryRequired {
		t.Fatal("retry ran without durable checkpoint")
	}
}
func TestOnboardingBoundedFileRejectsOversizeAndNonRegular(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "oversize")
	if err := os.WriteFile(file, make([]byte, 1025), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnboardingFile(file, 1024); err == nil {
		t.Fatal("oversized private input accepted")
	}
	if _, err := readOnboardingFile(dir, 1024); err == nil {
		t.Fatal("nonregular private input accepted")
	}
}

func TestOnboardingOriginalReceiptRetryReusesIdentityAndPaths(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	f.failStarts = 1
	review := f.inspect()
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	first := waitOnboardingTerminal(t, f.s, op.OperationID)
	if first.State != "failed" || first.Targets[0].CleanupConfirmed {
		t.Fatalf("uncertain start was falsely complete: %+v", first)
	}
	f.s.mu.Lock()
	old := f.s.operations[op.OperationID].Plans[f.ids[0]].Receipt
	p := f.s.targets[f.ids[0]]
	p.access = onboardingAccess{user: "approved-user", password: "fresh-fixture-access"}
	p.accessGeneration = "second-entry"
	p.candidate.AccessAvailable = true
	f.s.mu.Unlock()
	if !old.Installed || !old.ServiceInstalled || old.ServiceStarted {
		t.Fatalf("prior effects not retained: %+v", old)
	}
	if _, err = f.s.operationRequest(context.Background(), "engine:onboarding-retry", onboardingOperationRequest{OperationID: op.OperationID}); err != nil {
		t.Fatal(err)
	}
	last := waitOnboardingTerminal(t, f.s, op.OperationID)
	if last.State != "completed" || last.Targets[0].NodeID == "" {
		t.Fatalf("owned retry did not complete: %+v", last)
	}
	plan := f.s.operations[op.OperationID].Plans[f.ids[0]]
	if plan.Receipt.StagePath != old.StagePath || plan.Receipt.BundlePath != old.BundlePath || plan.Receipt.ManifestSHA256 != old.ManifestSHA256 {
		t.Fatal("retry changed original operation binding")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invites != 1 || f.responses != 1 {
		t.Fatal("retry duplicated pairing")
	}
	for _, d := range f.devices {
		if d.transfers != 2 {
			t.Fatal("resume did not use verified original stream")
		}
	}
}
func TestOnboardingCancelQueuedTargetDoesNotCancelOtherTarget(t *testing.T) {
	f := newOnboardingControllerFixture(t, 2)
	f.blockAddress = "192.0.2.10"
	f.entered = make(chan struct{})
	f.release = make(chan struct{})
	review := f.inspect()
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("first target never reached transfer")
	}
	_, err = f.s.operationRequest(context.Background(), "engine:onboarding-cancel", onboardingOperationRequest{OperationID: op.OperationID, CandidateID: f.ids[1]})
	if err != nil {
		t.Fatal(err)
	}
	close(f.release)
	last := waitOnboardingTerminal(t, f.s, op.OperationID)
	if last.Targets[0].Stage != "paired" || last.Targets[1].Stage != "cancelled" || !last.Targets[1].CleanupConfirmed {
		t.Fatalf("per-target cancellation crossed ownership: %+v", last)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.devices["192.0.2.11"].transfers != 0 || f.invites != 1 {
		t.Fatal("cancelled queued target acquired effects")
	}
}
func TestOnboardingTerminalCancelCleansOnlyOriginalOwnedInstall(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	f.failStarts = 1
	review := f.inspect()
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	_ = waitOnboardingTerminal(t, f.s, op.OperationID)
	f.s.mu.Lock()
	p := f.s.targets[f.ids[0]]
	p.access = onboardingAccess{user: "approved-user", password: "cleanup-fixture-access"}
	p.candidate.AccessAvailable = true
	p.accessGeneration = "cleanup-entry"
	f.s.mu.Unlock()
	_, err = f.s.operationRequest(context.Background(), "engine:onboarding-cancel", onboardingOperationRequest{OperationID: op.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	last := waitOnboardingTerminal(t, f.s, op.OperationID)
	if last.State != "cancelled" || !last.Targets[0].CleanupConfirmed {
		t.Fatalf("cleanup did not settle: %+v", last)
	}
	if p.access.password != "" || p.candidate.AccessAvailable {
		t.Fatal("cleanup access was retained after terminal cancellation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devices["192.0.2.10"]
	if !d.installed || d.started || d.paired || f.invites != 0 {
		t.Fatal("cancel erased installation or invented membership")
	}
}

func reauthorizeOnboardingFixture(f *onboardingControllerFixture) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	for _, id := range f.ids {
		p := f.s.targets[id]
		p.access = onboardingAccess{user: "approved-user", password: "fresh-fixture-access"}
		p.candidate.AccessAvailable = true
		p.accessGeneration = newOpID()
	}
}
func TestOnboardingClusterChangeDuringTransferCannotAdmitInvitation(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	f.blockAddress = "192.0.2.10"
	f.entered = make(chan struct{})
	f.release = make(chan struct{})
	review := f.inspect()
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("transfer did not begin")
	}
	f.mu.Lock()
	f.clusterID = "44444444-4444-4444-8444-444444444444"
	f.mu.Unlock()
	close(f.release)
	last := waitOnboardingTerminal(t, f.s, op.OperationID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if last.State == "completed" || f.invites != 0 || f.responses != 0 || last.TargetClusterID != "" {
		t.Fatalf("changed seed cluster crossed review: %+v invites=%d responses=%d", last, f.invites, f.responses)
	}
}
func TestOnboardingLostInviteReplyRetriesOriginalKeyAfterOwnFounding(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	f.loseReply = true
	f.statusFailures = 1
	review := f.inspect()
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	first := waitOnboardingTerminal(t, f.s, op.OperationID)
	if first.State != "failed" {
		t.Fatalf("lost reply did not retain uncertainty: %+v", first)
	}
	plan := f.s.invitationPlan(f.s.operations[op.OperationID], f.ids[0])
	if plan.InviteRequestKey == "" || plan.InviteExpectedClusterID != "" || plan.InviteID != "" {
		t.Fatalf("intent was not durably ahead of send: %+v", plan)
	}
	reauthorizeOnboardingFixture(f)
	_, err = f.s.operationRequest(context.Background(), "engine:onboarding-retry", onboardingOperationRequest{OperationID: op.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	last := waitOnboardingTerminal(t, f.s, op.OperationID)
	if last.State != "completed" {
		t.Fatalf("same-key recovery failed: %+v", last)
	}
	finalPlan := f.s.invitationPlan(f.s.operations[op.OperationID], f.ids[0])
	if finalPlan.InviteRequestKey != plan.InviteRequestKey || finalPlan.InviteExpectedClusterID != "" {
		t.Fatal("retry mutated original idempotency tuple")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invites != 1 || f.responses != 1 {
		t.Fatal("lost response produced a second invitation")
	}
}
func TestOnboardingUnknownCancelNeverClaimsCleanOrStopsParent(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	f.loseReply = true
	f.statusFailures = 1
	review := f.inspect()
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	_ = waitOnboardingTerminal(t, f.s, op.OperationID)
	f.mu.Lock()
	f.cancelUncertain = true
	f.mu.Unlock()
	reauthorizeOnboardingFixture(f)
	_, err = f.s.operationRequest(context.Background(), "engine:onboarding-cancel", onboardingOperationRequest{OperationID: op.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	uncertain := waitOnboardingTerminal(t, f.s, op.OperationID)
	if uncertain.State == "cancelled" || uncertain.Targets[0].CleanupConfirmed {
		t.Fatalf("uncertain inviter became clean: %+v", uncertain)
	}
	f.mu.Lock()
	if !f.devices["192.0.2.10"].started {
		t.Fatal("unknown invite cancellation stopped potential pairing parent")
	}
	f.cancelUncertain = false
	f.mu.Unlock()
	reauthorizeOnboardingFixture(f)
	_, err = f.s.operationRequest(context.Background(), "engine:onboarding-cancel", onboardingOperationRequest{OperationID: op.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	last := waitOnboardingTerminal(t, f.s, op.OperationID)
	if last.State != "cancelled" || !last.Targets[0].CleanupConfirmed {
		t.Fatalf("confirmed cancellation did not settle: %+v", last)
	}
}
func TestOnboardingInvitationIntentCheckpointPrecedesAnyInviteSend(t *testing.T) {
	f := newOnboardingControllerFixture(t, 1)
	review := f.inspect()
	f.s.testSave = func(run *onboardingRun) error {
		for _, p := range run.Plans {
			if p.InviteRequestKey != "" {
				return errors.New("fixture intent checkpoint unavailable")
			}
		}
		return nil
	}
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	last := waitOnboardingTerminal(t, f.s, op.OperationID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if last.State == "completed" || f.invites != 0 || f.responses != 0 {
		t.Fatal("invitation was sent ahead of durable intent")
	}
}
