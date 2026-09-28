// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func diagnosticTestProfile() diagnosticProfile {
	tool := diagnosticTool{Path: "/opt/pair/tool", SHA256: strings.Repeat("a", 64)}
	p := diagnosticProfile{GroupID: "test-group", Label: "Configured example", OwnerNodeID: "node-a", Transport: "socket", DedicatedTestWindow: true, MPI: tool, SSH: tool, KnownHosts: tool, IdentityFile: "/home/operator/.ssh/id_test"}
	for _, id := range []string{"a", "b"} {
		p.Members = append(p.Members, diagnosticMember{NodeID: "node-" + id, Principal: "principal-" + id, Host: "192.0.2.1", User: "operator", GPU: "GPU-" + id, Interface: "eth0", Manager: tool, NCCL: tool, SMI: tool})
	}
	return p
}

func TestDiagnosticRecipeStartCannotResolveAProfileOrCreateState(t *testing.T) {
	d := &diagnosticService{profiles: func() ([]diagnosticProfile, error) {
		t.Fatal("inspection-only start resolved an execution profile")
		return nil, nil
	}}
	_, err := d.dispatch(context.Background(), "engine:diagnostic-start", diagnosticRequest{GroupID: "pair-recipe/nccl-forged", Preset: diagnosticPreset})
	if err == nil || !strings.Contains(err.Error(), "inspection") || len(d.operations) != 0 || d.reservation != nil {
		t.Fatalf("inspection admitted: %v", err)
	}
}

func TestDiagnosticRecipeNamespacePreservesLegacyStatusAndCancellation(t *testing.T) {
	d := diagnosticTestService(t)
	p := diagnosticTestProfile()
	p.GroupID = "pair-recipe-old" // Valid before this slice; slash IDs were not.
	d.profiles = func() ([]diagnosticProfile, error) { return []diagnosticProfile{p}, nil }
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", "principal-a", "principal-b")
	d.m.mesh = clustertrust.Open(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := strings.Repeat("c", 32)
	op := diagnosticOperation{OperationID: id, GroupID: p.GroupID, OwnerNodeID: p.OwnerNodeID, State: "running", MemberNodeIDs: p.descriptor().MemberNodeIDs}
	d.operations[id], d.cancels[id] = op, cancel
	r := diagnosticRequest{GroupID: p.GroupID, OperationID: id}
	if value, err := d.dispatch(context.Background(), "engine:diagnostic-status", r); err != nil || value.(diagnosticOperation).OperationID != id {
		t.Fatalf("status lost: %v", err)
	}
	if value, err := d.dispatch(context.Background(), "engine:diagnostic-cancel", r); err != nil || value.(diagnosticOperation).State != "cancelling" || ctx.Err() == nil {
		t.Fatalf("cancel lost: %v", err)
	}
	if diagnosticToken.MatchString("pair-recipe/nccl-forged") {
		t.Fatal("inspection namespace overlaps profile grammar")
	}
}

func diagnosticTestService(t *testing.T) *diagnosticService {
	t.Helper()
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	m := NewManager(nil, ex, clustertrust.Open(t.TempDir()))
	d := m.exec.diagnostics
	p := diagnosticTestProfile()
	d.profiles = func() ([]diagnosticProfile, error) { return []diagnosticProfile{p}, nil }
	d.participant = func(_ context.Context, _ diagnosticProfile, member diagnosticMember, action string, _ diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
		return diagnosticParticipantResult{NodeID: member.NodeID, Prepared: action == "prepare", CleanupConfirmed: action == "cancel"}, nil
	}
	d.run = func(context.Context, diagnosticProfile, string) ([]diagnosticSample, error) {
		return parseDiagnosticOutput(diagnosticTestOutput())
	}
	t.Cleanup(d.shutdown)
	return d
}

func diagnosticTestOutput() string {
	var out strings.Builder
	out.WriteString("# size count type redop root time algbw busbw #wrong time algbw busbw #wrong\n")
	for size := 8; size <= 8388608; size *= 2 {
		fmt.Fprintf(&out, "%d %d float sum -1 12.3 1.2 1.3 0 12.4 1.1 1.2 0\n", size, size/4)
	}
	return out.String()
}

func waitDiagnostic(t *testing.T, d *diagnosticService, id string) diagnosticOperation {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		d.mu.Lock()
		op := d.operations[id]
		_, active := d.cancels[id]
		d.mu.Unlock()
		if !active {
			return op
		}
		select {
		case <-deadline:
			t.Fatal("diagnostic did not settle")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestDiagnosticPositiveRunnerRequiresSamplesAndCleanup(t *testing.T) {
	d := diagnosticTestService(t)
	op, err := d.start(diagnosticTestProfile())
	if err != nil || op.State != "preparing" || op.OperationID == "" {
		t.Fatalf("start=%+v err=%v", op, err)
	}
	op = waitDiagnostic(t, d, op.OperationID)
	if op.State != "passed" || !op.CleanupConfirmed || op.FinishedAt == 0 || len(op.Samples) != 21 {
		t.Fatalf("terminal=%+v", op)
	}
}

func TestDiagnosticBusyAndUncertainPrepareBothCleanAttemptedParticipants(t *testing.T) {
	d := diagnosticTestService(t)
	var mu sync.Mutex
	var prepared, cancelled []string
	d.participant = func(_ context.Context, _ diagnosticProfile, m diagnosticMember, action string, _ diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if action == "prepare" {
			prepared = append(prepared, m.NodeID)
			if m.NodeID == "node-b" {
				return diagnosticParticipantResult{}, errors.New("busy or acknowledgement lost")
			}
		}
		if action == "cancel" {
			cancelled = append(cancelled, m.NodeID)
		}
		return diagnosticParticipantResult{NodeID: m.NodeID, Prepared: true, CleanupConfirmed: true}, nil
	}
	d.run = func(context.Context, diagnosticProfile, string) ([]diagnosticSample, error) {
		t.Error("runner reached after busy prepare")
		return nil, nil
	}
	op, _ := d.start(diagnosticTestProfile())
	op = waitDiagnostic(t, d, op.OperationID)
	mu.Lock()
	defer mu.Unlock()
	if op.State != "failed" || !op.CleanupConfirmed || len(prepared) != 2 || len(cancelled) != 2 {
		t.Fatalf("terminal=%+v prepare=%v cancel=%v", op, prepared, cancelled)
	}
}

func TestDiagnosticCleanupUncertaintyCannotPass(t *testing.T) {
	d := diagnosticTestService(t)
	d.participant = func(_ context.Context, _ diagnosticProfile, m diagnosticMember, action string, _ diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
		return diagnosticParticipantResult{NodeID: m.NodeID, Prepared: true, CleanupConfirmed: action != "cancel"}, nil
	}
	op, _ := d.start(diagnosticTestProfile())
	op = waitDiagnostic(t, d, op.OperationID)
	if op.State != "failed" || op.CleanupConfirmed {
		t.Fatalf("uncertain cleanup=%+v", op)
	}
}

func TestDiagnosticCancellationPreservesOwnedCleanup(t *testing.T) {
	d := diagnosticTestService(t)
	d.participant = func(_ context.Context, _ diagnosticProfile, m diagnosticMember, action string, _ diagnosticParticipantRequest) (diagnosticParticipantResult, error) {
		result := diagnosticParticipantResult{NodeID: m.NodeID, Prepared: true, CleanupConfirmed: true}
		if action == "cancel" {
			result.ExecutionError = "context canceled / killed"
		}
		return result, nil
	}
	entered := make(chan struct{})
	d.run = func(ctx context.Context, _ diagnosticProfile, _ string) ([]diagnosticSample, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	op, _ := d.start(diagnosticTestProfile())
	<-entered
	d.mu.Lock()
	current := d.operations[op.OperationID]
	current.State = "cancelling"
	d.operations[op.OperationID] = current
	d.cancels[op.OperationID]()
	d.mu.Unlock()
	op = waitDiagnostic(t, d, op.OperationID)
	if op.State != "cancelled" || !op.CleanupConfirmed {
		t.Fatalf("cancelled=%+v", op)
	}
}

func TestDiagnosticProfileIsPlatformNeutralAndRejectsUnsafeConfiguration(t *testing.T) {
	p := diagnosticTestProfile()
	if err := p.validate(); err != nil {
		t.Fatalf("Linux profile cannot be selected from this controller: %v", err)
	}
	for _, change := range []func(*diagnosticProfile){
		func(p *diagnosticProfile) { p.DedicatedTestWindow = false },
		func(p *diagnosticProfile) { p.Transport = "rdma" },
		func(p *diagnosticProfile) { p.Members[1].Principal = p.Members[0].Principal },
		func(p *diagnosticProfile) { p.MPI.Path = "/bin/sh -c bad" },
		func(p *diagnosticProfile) { p.IdentityFile = "/tmp/key;other" },
		func(p *diagnosticProfile) { p.Members[0].Host = "arbitrary.example" },
	} {
		candidate := diagnosticTestProfile()
		change(&candidate)
		if candidate.validate() == nil {
			t.Fatalf("unsafe profile accepted: %+v", candidate)
		}
	}
}

func TestDiagnosticNotConfiguredAndStrictRequests(t *testing.T) {
	base := t.TempDir()
	if p, err := loadDiagnosticProfiles(base); err != nil || len(p) != 0 {
		t.Fatalf("missing profiles=%v %v", p, err)
	}
	if err := os.WriteFile(filepath.Join(base, "diagnostic-groups.json"), []byte(`{"groups":[],"shell":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDiagnosticProfiles(base); err == nil {
		t.Fatal("unknown operator config key accepted")
	}
	for _, body := range []string{`{"groupId":"x","shell":"bad"}`, `{"groupId":"x","host":"example"}`, `{"groupId":"x","preset":"nccl-smoke"} {}`} {
		if _, err := decodeDiagnosticRequest(json.RawMessage(body)); err == nil {
			t.Fatalf("unsafe request accepted: %s", body)
		}
	}
	d := diagnosticTestService(t)
	if _, err := d.profile("not-configured"); err == nil {
		t.Fatal("missing group manufactured")
	}
}

func TestDiagnosticParserFailsIncompleteNonfiniteOrWrongResults(t *testing.T) {
	good := diagnosticTestOutput()
	for _, bad := range []string{"", strings.Replace(good, "8388608", "8388607", 1), strings.Replace(good, "8 2 float", "8 nonsense float", 1), strings.Replace(good, "8 2 float", "8 3 float", 1), strings.Replace(good, "sum -1", "sum other", 1), strings.Replace(good, "sum -1", "sum 0", 1), strings.Replace(good, "12.3", "NaN", 1), strings.Replace(good, "1.3 0", "1.3 1", 1), strings.Replace(good, "1.2 0\n", "1.2 1\n", 1), good + "extra output\n"} {
		if _, err := parseDiagnosticOutput(bad); err == nil {
			t.Fatal("invalid NCCL result accepted")
		}
	}
}

func TestDiagnosticReservationFencesManagedStartAndEnable(t *testing.T) {
	d := diagnosticTestService(t)
	d.reservation = &diagnosticLeaseRecord{}
	if err := d.m.exec.Start(context.Background(), "fake"); err == nil || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("start bypassed: %v", err)
	}
	if err := d.m.exec.setDesiredEnabled("fake", true); err == nil {
		t.Fatal("enable bypassed")
	}
	if err := d.m.exec.setDesiredEnabled("fake", false); err != nil {
		t.Fatal(err)
	}
	d.reservation = nil
}

func TestDiagnosticInstallLockOrderDoesNotBlockPreparation(t *testing.T) {
	d := diagnosticTestService(t)
	st, err := d.m.exec.state("fake")
	if err != nil {
		t.Fatal(err)
	}
	st.opMu.Lock()
	finished := make(chan error, 1)
	go func() { finished <- d.m.exec.Install(context.Background(), "fake") }()
	// Install must wait for the per-engine lock without retaining a diagnostic
	// read lock. A preparation writer therefore remains able to enter.
	entered := make(chan struct{})
	go func() {
		d.m.exec.diagnosticMu.Lock()
		d.mu.Lock()
		d.reservation = &diagnosticLeaseRecord{}
		d.mu.Unlock()
		d.m.exec.diagnosticMu.Unlock()
		close(entered)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		st.opMu.Unlock()
		t.Fatal("install/preparation lock-order cycle")
	}
	st.opMu.Unlock()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("install escaped reservation")
		}
	case <-time.After(time.Second):
		t.Fatal("install did not settle")
	}
	d.reservation = nil
}

func TestDiagnosticUnpinnedControlCannotReadOrRun(t *testing.T) {
	d := diagnosticTestService(t)
	server := &controlServer{exec: d.m.exec, mesh: d.m.mesh}
	request := httptest.NewRequest("POST", diagnosticControlPath, strings.NewReader(`{"method":"engine:diagnostic-start","request":{"groupId":"test-group","preset":"nccl-smoke"}}`))
	reply := httptest.NewRecorder()
	server.mux().ServeHTTP(reply, request)
	if reply.Code != 403 {
		t.Fatalf("unpinned request=%d", reply.Code)
	}
}

func TestDiagnosticRecoveryFailsClosedForUnknownRankButNotEmptyTombstone(t *testing.T) {
	for _, hasRank := range []bool{false, true} {
		t.Run(fmt.Sprint(hasRank), func(t *testing.T) {
			d := diagnosticTestService(t)
			dir := d.runDir(strings.Repeat("c", 32))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cancelled"), []byte("cancelled"), 0600); err != nil {
				t.Fatal(err)
			}
			if hasRank {
				if err := os.WriteFile(filepath.Join(dir, "lease.json"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "rank.json"), []byte(`{"pid":123,"startTicks":"1"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			d.recover(context.Background())
			if d.reserved() != hasRank {
				t.Fatalf("recovery fence=%v want %v", d.reserved(), hasRank)
			}
		})
	}
}

func TestDiagnosticRecoveryPrunesExpiredTerminalHistoryWithoutFencingStarts(t *testing.T) {
	d := diagnosticTestService(t)
	for i := 0; i < 129; i++ {
		id := fmt.Sprintf("%032x", i)
		dir := d.runDir(id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		lease := diagnosticLeaseRecord{Request: diagnosticParticipantRequest{OperationID: id, ExpiresAt: time.Now().Add(-2 * time.Minute).UnixMilli()}}
		if err := writeJSONAtomic(filepath.Join(dir, "lease.json"), lease); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONAtomic(filepath.Join(dir, "rank.json"), diagnosticRankRecord{Done: true, Clean: true}); err != nil {
			t.Fatal(err)
		}
	}
	d.recover(context.Background())
	if d.reserved() {
		t.Fatal("ordinary terminal history fenced managed starts")
	}
	entries, err := os.ReadDir(filepath.Join(d.m.exec.baseDir, "diagnostic-runs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("retained expired history=%d %v", len(entries), err)
	}
}

func TestDiagnosticSerializedFailureIsBoundedAndControlFree(t *testing.T) {
	d := diagnosticTestService(t)
	d.run = func(context.Context, diagnosticProfile, string) ([]diagnosticSample, error) {
		return nil, errors.New(strings.Repeat("MPI failed\nrank stopped\t\x00\x1b", 200))
	}
	op, _ := d.start(diagnosticTestProfile())
	op = waitDiagnostic(t, d, op.OperationID)
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var decoded diagnosticOperation
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State != "failed" || !decoded.CleanupConfirmed || len(decoded.Message) > 2048 || strings.ContainsAny(decoded.Message, "\n\t\r\x00\x1b") {
		t.Fatalf("invalid public failure=%+v", decoded)
	}
}

func TestDiagnosticOwnerRestartRecoversFailedTerminalWithoutInventingCleanup(t *testing.T) {
	d := diagnosticTestService(t)
	id := strings.Repeat("d", 32)
	op := diagnosticOperation{OperationID: id, GroupID: "test-group", OwnerNodeID: "node-a", Preset: diagnosticPreset, State: "preparing", StartedAt: time.Now().Add(-time.Second).UnixMilli(), MemberNodeIDs: []string{"node-a", "node-b"}}
	if err := d.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	d.recover(context.Background())
	op = d.operations[id]
	if op.State != "failed" || op.CleanupConfirmed || op.FinishedAt < op.StartedAt {
		t.Fatalf("restarted operation=%+v", op)
	}
	var persisted diagnosticOperation
	if err := readDiagnosticJSON(d.operationPath(id), &persisted); err != nil || persisted.State != "failed" {
		t.Fatalf("terminal receipt=%+v %v", persisted, err)
	}
}

func TestDiagnosticRetryCleanupSettlesInterruptedOperationWithoutInventingPass(t *testing.T) {
	d := diagnosticTestService(t)
	id := strings.Repeat("e", 32)
	op := diagnosticOperation{OperationID: id, GroupID: "test-group", OwnerNodeID: "node-a", Preset: diagnosticPreset, State: "cancelling", StartedAt: time.Now().Add(-time.Second).UnixMilli(), MemberNodeIDs: []string{"node-a", "node-b"}}
	d.operations[id] = op
	d.retryCleanup(context.Background(), diagnosticTestProfile(), op)
	op = d.operations[id]
	if op.State != "failed" || !op.CleanupConfirmed {
		t.Fatalf("cleanup retry=%+v", op)
	}
	var persisted diagnosticOperation
	if err := readDiagnosticJSON(d.operationPath(id), &persisted); err != nil || !persisted.CleanupConfirmed {
		t.Fatalf("retry receipt=%+v %v", persisted, err)
	}
}
