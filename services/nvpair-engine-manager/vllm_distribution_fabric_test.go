// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A model copy uses the fabric only over a freshly re-proven link between the
// exact source and target; a pair the fabric does not link uses management.
func TestFabricLaneBetweenUsesOnlyAQualifiedLinkBetweenThePair(t *testing.T) {
	empty := newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	if _, err := empty.laneBetween(context.Background(), "node-a", "node-b"); !errors.Is(err, errNoFabricLane) {
		t.Fatalf("no fabric was not reported as absent: %v", err)
	}
	s, r := qualifiedDirectFabricFixture(t)
	lane, err := s.laneBetween(context.Background(), "node-a", "node-b")
	if err != nil || lane.OperationID != r.Public.OperationID || lane.Qualification != r.Public.QualificationDigest ||
		lane.SourceAddress != "172.31.240.1" || lane.TargetAddress != "172.31.240.2" {
		t.Fatalf("the lowest-index reciprocal lane was not chosen: %+v %v", lane, err)
	}
	if back, err := s.laneBetween(context.Background(), "node-b", "node-a"); err != nil || back.SourceAddress != "172.31.240.2" || back.TargetAddress != "172.31.240.1" {
		t.Fatalf("the reverse copy did not use the source's end of the lane: %+v %v", back, err)
	}
	if _, err := s.laneBetween(context.Background(), "node-a", "node-c"); !errors.Is(err, errNoFabricLane) {
		t.Fatalf("a pair outside the fabric did not fall back to management: %v", err)
	}
	for name, spoil := range map[string]func(*fabricService, *fabricRunRecord){
		"requalification fails": func(s *fabricService, _ *fabricRunRecord) {
			s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
				return fabricControlResult{}, errors.New("route changed")
			}
		},
		"recovery required":  func(_ *fabricService, r *fabricRunRecord) { r.Public.State = "recovery-required" },
		"incomplete history": func(s *fabricService, _ *fabricRunRecord) { s.recoveryFailed = true },
	} {
		t.Run(name, func(t *testing.T) {
			s, r := qualifiedDirectFabricFixture(t)
			spoil(s, r)
			if _, err := s.laneBetween(context.Background(), "node-a", "node-b"); err == nil || errors.Is(err, errNoFabricLane) {
				t.Fatalf("a fabric that cannot be proven did not fail closed: %v", err)
			}
		})
	}
	s, r = qualifiedDirectFabricFixture(t)
	r.Public.CleanupConfirmed = true
	if _, err := s.laneBetween(context.Background(), "node-a", "node-b"); !errors.Is(err, errNoFabricLane) {
		t.Fatalf("a cleaned-up fabric still routed copies: %v", err)
	}
}

func TestFabricTransferHoldBlocksRollbackUntilTheCopyEnds(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	id, digest := r.Public.OperationID, r.Public.QualificationDigest
	hold := strings.Repeat("e", 32)
	owner := fabricTransferHold{OperationID: strings.Repeat("d", 32), Source: "node-a", Target: "node-b", Model: "owner/model@" + strings.Repeat("c", 40)}
	if err := s.acquireTransferHold(context.Background(), id, strings.Repeat("0", 64), hold, owner); err == nil {
		t.Fatal("a copy held a fabric under a different qualification")
	}
	if err := s.acquireTransferHold(context.Background(), id, digest, hold, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.acquireTransferHold(context.Background(), id, digest, hold, owner); err == nil {
		t.Fatal("one copy took the same hold twice")
	}
	if _, err := s.cancel(id, true); !errors.Is(err, errFabricTransferActive) {
		t.Fatalf("rollback under a running copy was admitted: %v", err)
	}
	s.mu.Lock()
	r.Public.State = "recovery-required"
	s.mu.Unlock()
	if _, err := s.recover(context.Background(), id, true); !errors.Is(err, errFabricTransferActive) {
		t.Fatalf("recovery under a running copy was admitted: %v", err)
	}
	s.mu.Lock()
	r.Public.State = "active"
	s.mu.Unlock()
	if err := s.releaseTransferHold(id, hold, owner); err != nil {
		t.Fatal(err)
	}
	s.refreshAccess = func(context.Context, *fabricRunRecord) ([]cableLaunchPlan, error) {
		return nil, errors.New("rollback access is outside this test")
	}
	if _, err := s.cancel(id, true); err != nil {
		t.Fatalf("rollback stayed blocked after the copy ended: %v", err)
	}
	s.mu.Lock()
	done := r.done
	s.mu.Unlock()
	<-done
}

// Rollback and a starting copy race for the same fabric. Exactly one wins.
func TestFabricTransferHoldCancelRace(t *testing.T) {
	for range 64 {
		s, r := qualifiedDirectFabricFixture(t)
		s.refreshAccess = func(context.Context, *fabricRunRecord) ([]cableLaunchPlan, error) {
			return nil, errors.New("rollback access is outside this race")
		}
		id, digest := r.Public.OperationID, r.Public.QualificationDigest
		owner := fabricTransferHold{OperationID: strings.Repeat("d", 32), Source: "node-a", Target: "node-b", Model: "owner/model@" + strings.Repeat("c", 40)}
		var ready, done sync.WaitGroup
		var holdErr, cancelErr error
		ready.Add(2)
		done.Add(2)
		start := make(chan struct{})
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // an admitted rollback must refuse the copy rather than wait for it
			holdErr = s.acquireTransferHold(ctx, id, digest, strings.Repeat("e", 32), owner)
		}()
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, cancelErr = s.cancel(id, true)
		}()
		ready.Wait()
		close(start)
		done.Wait()
		if holdErr == nil && cancelErr == nil {
			t.Fatal("a rollback began under a running model copy")
		}
		if holdErr != nil && cancelErr != nil {
			t.Fatalf("neither side won: hold=%v cancel=%v", holdErr, cancelErr)
		}
		s.mu.Lock()
		rollback := r.done
		s.mu.Unlock()
		<-rollback
	}
}

func TestRouteVLLMDistributionHoldsTheLaneOrUsesManagement(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	s.m.exec.fabric = s
	hold := strings.Repeat("e", 32)
	model := "owner/model@" + strings.Repeat("c", 40)
	address, release, err := s.m.routeVLLMDistribution(context.Background(), "node-a", "node-b", model, hold)
	if err != nil || address != "172.31.240.1" {
		t.Fatalf("a linked pair did not route over its lane: %q %v", address, err)
	}
	if _, err := s.cancel(r.Public.OperationID, true); !errors.Is(err, errFabricTransferActive) {
		t.Fatalf("the routed copy did not hold its fabric: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	held := len(r.TransferHolds)
	s.mu.Unlock()
	if held != 0 {
		t.Fatal("the copy's hold outlived its release")
	}
	if address, release, err := s.m.routeVLLMDistribution(context.Background(), "node-b", "node-c", model, hold); err != nil || address != "" || release == nil {
		t.Fatalf("an unlinked pair did not use management: %q %v", address, err)
	}
	if address, _, err := (&Manager{exec: &Executor{}}).routeVLLMDistribution(context.Background(), "node-a", "node-b", model, hold); err != nil || address != "" {
		t.Fatalf("a manager without fabric ownership did not use management: %q %v", address, err)
	}
	s.recoveryFailed = true
	if _, _, err := s.m.routeVLLMDistribution(context.Background(), "node-a", "node-b", model, hold); err == nil {
		t.Fatal("an unprovable fabric routed a copy")
	}
}

func TestRemoteDistributionHoldNeedsExactTerminalOrCancellation(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	s.m.exec.fabric = s
	p := remoteParam{Node: "node-b", Engine: "vllm", SourceNode: "node-a",
		Model: "owner/model@" + strings.Repeat("c", 40), OperationID: strings.Repeat("e", 32)}
	acquire := func() func() error {
		t.Helper()
		_, release, err := s.m.routeVLLMDistribution(context.Background(), p.SourceNode, p.Node, p.Model, p.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		return release
	}
	held := func(want bool) {
		t.Helper()
		s.mu.Lock()
		got := len(r.TransferHolds) > 0
		s.mu.Unlock()
		if got != want {
			t.Fatalf("fabric hold present = %v, want %v", got, want)
		}
	}
	frame := streamFrame{OpID: p.OperationID, Engine: "vllm", Op: "distribute"}
	release := acquire()
	if err := s.m.settleRemoteVLLMDistribution(p, streamFrame{}, errors.New("lost stream"), release); err == nil {
		t.Fatal("transport failure was treated as terminal")
	}
	held(true)
	if _, _, err := s.m.routeVLLMDistribution(context.Background(), p.SourceNode, p.Node, p.Model, p.OperationID); err == nil {
		t.Fatal("an unresolved copy started again")
	}
	if _, err := s.cancel(r.Public.OperationID, true); !errors.Is(err, errFabricTransferActive) {
		t.Fatalf("rollback under uncertain copy = %v", err)
	}
	cancel := p
	cancel.Cancel = true
	frame.Type = "result"
	frame.Result = json.RawMessage(`{"cancelled":true,"operationId":"` + p.OperationID + `"}`)
	wrong := cancel
	wrong.Node = "node-c"
	wrongRelease, err := s.transferHoldRelease(fabricTransferHold{OperationID: wrong.OperationID, Source: wrong.SourceNode, Target: wrong.Node, Model: wrong.Model})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.settleRemoteVLLMDistribution(wrong, frame, nil, wrongRelease); err != nil {
		t.Fatal(err)
	}
	held(true)
	owner := fabricTransferHold{OperationID: cancel.OperationID, Source: cancel.SourceNode, Target: cancel.Node, Model: cancel.Model}
	cancelRelease, err := s.transferHoldRelease(owner)
	if err != nil {
		t.Fatal(err)
	}
	delayedCancelRelease, err := s.transferHoldRelease(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.settleRemoteVLLMDistribution(cancel, frame, nil, cancelRelease); err != nil {
		t.Fatal(err)
	}
	held(false)
	retryRelease := acquire()
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := s.m.settleRemoteVLLMDistribution(cancel, frame, nil, delayedCancelRelease); err != nil {
		t.Fatal(err)
	}
	held(true)
	if err := retryRelease(); err != nil {
		t.Fatal(err)
	}
	held(false)

	release = acquire()
	frame.Type, frame.Result = "error", nil
	if err := s.m.settleRemoteVLLMDistribution(p, frame, errors.New("peer worker failed"), release); err == nil {
		t.Fatal("peer error was not returned")
	}
	held(false)

	release = acquire()
	modelRaw, _ := json.Marshal(vllmModel{ID: p.Model, Source: "huggingface", Revision: strings.Repeat("c", 40),
		MetadataSHA256: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64), Bytes: 123})
	frame.Type, frame.Result = "result", modelRaw
	wrongFrame := frame
	wrongFrame.OpID = strings.Repeat("f", 32)
	if err := s.m.settleRemoteVLLMDistribution(p, wrongFrame, nil, release); err == nil {
		t.Fatal("a foreign terminal operation cleared the hold")
	}
	held(true)
	if err := s.m.settleRemoteVLLMDistribution(p, frame, nil, release); err != nil {
		t.Fatal(err)
	}
	held(false)
}

func TestFabricTransferHoldSurvivesRestartUntilExactCancellation(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	owner := fabricTransferHold{OperationID: strings.Repeat("d", 32), Source: "node-a", Target: "node-b", Model: "owner/model@" + strings.Repeat("c", 40)}
	if err := s.acquireTransferHold(context.Background(), r.Public.OperationID, r.Public.QualificationDigest, strings.Repeat("e", 32), owner); err != nil {
		t.Fatal(err)
	}
	s.closeAdmission()
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	restored := newFabricService(s.m)
	s.m.exec.fabric = restored
	retained := restored.runs[r.Public.OperationID]
	if restored.recoveryFailed || retained == nil || len(retained.TransferHolds) != 1 {
		t.Fatalf("restart lost the unresolved transfer hold: recovery=%v holds=%v", restored.recoveryFailed, retained)
	}
	if _, err := restored.cancel(r.Public.OperationID, true); !errors.Is(err, errFabricTransferActive) {
		t.Fatalf("restart admitted rollback before exact copy closure: %v", err)
	}
	restored.mu.Lock()
	retained.Public.State = "recovery-required"
	restored.mu.Unlock()
	if _, err := restored.recover(context.Background(), r.Public.OperationID, true); !errors.Is(err, errFabricTransferActive) {
		t.Fatalf("restart admitted recovery before exact copy closure: %v", err)
	}
	restored.mu.Lock()
	retained.Public.State = "active"
	restored.mu.Unlock()
	frame := streamFrame{Type: "result", OpID: owner.OperationID, Engine: "vllm", Op: "distribute",
		Result: json.RawMessage(`{"cancelled":true,"operationId":"` + owner.OperationID + `"}`)}
	cancel := remoteParam{Node: owner.Target, Engine: "vllm", SourceNode: owner.Source, Model: owner.Model, OperationID: owner.OperationID, Cancel: true}
	wrong := cancel
	wrong.Node = "node-c"
	wrongRelease, err := restored.transferHoldRelease(fabricTransferHold{OperationID: wrong.OperationID, Source: wrong.SourceNode, Target: wrong.Node, Model: wrong.Model})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.settleRemoteVLLMDistribution(wrong, frame, nil, wrongRelease); err != nil {
		t.Fatal(err)
	}
	if again := newFabricService(s.m); again.recoveryFailed || len(again.runs[r.Public.OperationID].TransferHolds) != 1 {
		t.Fatal("wrong copy identity cleared the retained hold")
	}
	cancelRelease, err := restored.transferHoldRelease(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.settleRemoteVLLMDistribution(cancel, frame, nil, cancelRelease); err != nil {
		t.Fatal(err)
	}
	if again := newFabricService(s.m); again.recoveryFailed || len(again.runs[r.Public.OperationID].TransferHolds) != 0 {
		t.Fatal("exact cancellation closure did not durably clear the hold")
	}
}

func TestLocalDistributionCancellationClearsRestartedHold(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	owner := fabricTransferHold{OperationID: strings.Repeat("d", 32), Source: "node-a", Target: "node-b", Model: "owner/model@" + strings.Repeat("c", 40)}
	if err := s.acquireTransferHold(context.Background(), r.Public.OperationID, r.Public.QualificationDigest, strings.Repeat("e", 32), owner); err != nil {
		t.Fatal(err)
	}
	restored := newFabricService(s.m)
	restored.m.exec.fabric = restored
	restored.m.exec.vllmNodeID = owner.Target
	restored.m.exec.engines = map[string]*engineState{"vllm": {
		manifest: &Manifest{Engine: "vllm"}, plat: &Platform{}, modelDir: t.TempDir(), logs: newLogBuffer(),
	}}
	request := vllmDistributionRequest{OpID: strings.Repeat("f", 32), Engine: "vllm", OperationID: owner.OperationID,
		SourceNode: owner.Source, Model: owner.Model, Cancel: true}
	result, err := restored.m.distributeVLLMModelHere(context.Background(), request)
	if err != nil || !bytes.Contains(result, []byte(`"absent":true`)) {
		t.Fatalf("local exact cancellation = %s, %v", result, err)
	}
	if again := newFabricService(s.m); again.recoveryFailed || len(again.runs[r.Public.OperationID].TransferHolds) != 0 {
		t.Fatal("local exact cancellation did not durably clear the restarted hold")
	}
}

func TestFabricTransferHoldBoundFitsRetainedRecord(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	model := "owner/model@" + strings.Repeat("c", 40)
	for index := range maxFabricTransferHolds {
		owner := fabricTransferHold{OperationID: fmt.Sprintf("%032x", index+1), Source: "node-a", Target: "node-b", Model: model}
		if err := s.acquireTransferHold(context.Background(), r.Public.OperationID, r.Public.QualificationDigest, fmt.Sprintf("%032x", index+100), owner); err != nil {
			t.Fatalf("hold %d/%d: %v", index+1, maxFabricTransferHolds, err)
		}
	}
	raw, err := os.ReadFile(s.file(r.Public.OperationID))
	if err != nil || len(raw) > maxFabricRecordBytes {
		t.Fatalf("maximum transfer journal bytes = %d, error = %v", len(raw), err)
	}
	restored := newFabricService(s.m)
	if restored.recoveryFailed || len(restored.runs[r.Public.OperationID].TransferHolds) != maxFabricTransferHolds {
		t.Fatal("maximum bounded transfer journal did not reload")
	}
	extra := fabricTransferHold{OperationID: strings.Repeat("f", 32), Source: "node-a", Target: "node-b", Model: model}
	if err := s.acquireTransferHold(context.Background(), r.Public.OperationID, r.Public.QualificationDigest, strings.Repeat("a", 32), extra); err == nil {
		t.Fatal("transfer journal admitted more than its retained bound")
	}
}

func TestFabricTransferHoldPersistenceFailurePreservesPriorOwner(t *testing.T) {
	s, r := qualifiedDirectFabricFixture(t)
	holdID := strings.Repeat("e", 32)
	owner := fabricTransferHold{OperationID: strings.Repeat("d", 32), Source: "node-a", Target: "node-b", Model: "owner/model@" + strings.Repeat("c", 40)}
	var failSaves atomic.Bool
	s.testSave = func(*fabricRunRecord) error {
		if failSaves.Load() {
			return errors.New("synthetic fabric journal write failure")
		}
		return nil
	}
	failSaves.Store(true)
	if err := s.acquireTransferHold(context.Background(), r.Public.OperationID, r.Public.QualificationDigest, holdID, owner); err == nil || len(r.TransferHolds) != 0 {
		t.Fatalf("failed hold persistence admitted a copy: %v", err)
	}
	failSaves.Store(false)
	if err := s.acquireTransferHold(context.Background(), r.Public.OperationID, r.Public.QualificationDigest, holdID, owner); err != nil {
		t.Fatal(err)
	}
	failSaves.Store(true)
	if err := s.releaseTransferHold(r.Public.OperationID, holdID, owner); err == nil || len(r.TransferHolds) != 1 {
		t.Fatalf("failed release persistence dropped the prior hold: %v", err)
	}
	if _, err := s.cancel(r.Public.OperationID, true); !errors.Is(err, errFabricTransferActive) {
		t.Fatalf("failed release persistence reopened rollback: %v", err)
	}
	failSaves.Store(false)
	restored := newFabricService(s.m)
	if restored.recoveryFailed || len(restored.runs[r.Public.OperationID].TransferHolds) != 1 {
		t.Fatal("restart lost the last authoritative persisted hold")
	}
}

func distributionFabricTestRequest() vllmDistributionRequest {
	return vllmDistributionRequest{OpID: strings.Repeat("a", 32), Engine: "vllm", OperationID: strings.Repeat("b", 32),
		SourceNode: "node-b", Model: "owner/model@" + strings.Repeat("c", 40)}
}

func TestDistributionRequestAcceptsOnlyAPrivateFabricLaneAddress(t *testing.T) {
	req := distributionFabricTestRequest()
	if err := validateVLLMDistributionRequest(req); err != nil || vllmDistributionNetwork(req) != "management" {
		t.Fatalf("a management copy was refused: %v", err)
	}
	req.SourceAddress = "172.31.240.1"
	if err := validateVLLMDistributionRequest(req); err != nil || vllmDistributionNetwork(req) != "fabric" {
		t.Fatalf("a fabric lane address was refused: %v", err)
	}
	for _, address := range []string{"8.8.8.8", "127.0.0.1", "fd00::1", "172.031.240.1", "172.31.240.1:14323", "fabric"} {
		spoiled := req
		spoiled.SourceAddress = address
		if validateVLLMDistributionRequest(spoiled) == nil {
			t.Fatalf("source address %q was accepted", address)
		}
	}
	req.Cancel = true
	if validateVLLMDistributionRequest(req) == nil {
		t.Fatal("a cancellation carried a copy route")
	}
}

func TestLocalDistributionRefusesACallerChosenRoute(t *testing.T) {
	m := &Manager{exec: &Executor{vllmNodeID: "node-a"}}
	req := distributionFabricTestRequest()
	req.SourceAddress = "172.31.240.2"
	if _, err := m.distributeVLLMModelHere(context.Background(), req); err == nil || !strings.Contains(err.Error(), "chooses the copy route") {
		t.Fatalf("a caller chose the copy route: %v", err)
	}
}

func TestDistributionProgressCarriesItsNetworkAcrossEveryHop(t *testing.T) {
	var local map[string]any
	e := &Executor{progress: newProgressHub(), emit: func(method string, params any) {
		if method == "engine:pull-progress" {
			local, _ = params.(map[string]any)
		}
	}}
	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "distribute", Stage: "receiving", Percent: 12, Message: "owner/model", OperationID: strings.Repeat("b", 32), Network: "fabric"})
	if local["network"] != "fabric" {
		t.Fatalf("the local progress notification lost its network: %v", local)
	}
	frame, err := json.Marshal(streamFrame{Type: "progress", Engine: "vllm", Op: "distribute", Stage: "receiving", Network: "fabric"})
	if err != nil || !bytes.Contains(frame, []byte(`"network":"fabric"`)) {
		t.Fatalf("the stream frame lost its network: %s", frame)
	}
	var output bytes.Buffer
	m := &Manager{codec: NewCodec(&output)}
	m.remoteProgressFn(strings.Repeat("d", 32), "node-b")(streamFrame{Type: "progress", OpID: strings.Repeat("b", 32), Engine: "vllm", Op: "distribute", Stage: "receiving", Percent: 40, Network: "management"})
	if !bytes.Contains(output.Bytes(), []byte(`"method":"engine:remote-progress"`)) || !bytes.Contains(output.Bytes(), []byte(`"network":"management"`)) {
		t.Fatalf("the relayed progress lost its network: %s", output.String())
	}
}
