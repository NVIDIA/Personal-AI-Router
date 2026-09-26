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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

const testDistributionOperation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testDistributionSource = "TEST-ONLY-source-node"

type fakeDistributionSource struct {
	plan      vllmDistributionPlan
	data      map[string][]byte
	requests  []vllmDistributionChunkRequest
	failAfter int
}

func (s *fakeDistributionSource) exportPlan(_ context.Context, req vllmDistributionPlanRequest) (vllmDistributionPlan, error) {
	if req.OperationID != s.plan.OperationID || req.Model != s.plan.Model.ID {
		return vllmDistributionPlan{}, errors.New("TEST-ONLY plan request changed")
	}
	return s.plan, nil
}

func (s *fakeDistributionSource) exportChunk(_ context.Context, req vllmDistributionChunkRequest) (vllmDistributionChunk, error) {
	if s.failAfter > 0 && len(s.requests) == s.failAfter {
		return vllmDistributionChunk{}, errors.New("TEST-ONLY interrupted source")
	}
	s.requests = append(s.requests, req)
	entry := s.plan.Model.Files[req.FileIndex]
	raw := s.data[entry.Path]
	if req.Offset < 0 || req.Length <= 0 || req.Offset+req.Length > int64(len(raw)) {
		return vllmDistributionChunk{}, errors.New("TEST-ONLY invalid range")
	}
	data := append([]byte(nil), raw[req.Offset:req.Offset+req.Length]...)
	sum := sha256.Sum256(data)
	return vllmDistributionChunk{Offset: req.Offset, Data: data, SHA256: hex.EncodeToString(sum[:])}, nil
}

func testDistributionPlan(t *testing.T, large bool) (vllmDistributionPlan, map[string][]byte) {
	t.Helper()
	modelData := []byte("TEST-ONLY-safetensors")
	if large {
		modelData = bytes.Repeat([]byte{0x5a}, int(vllmDistributionChunkSize)+17)
	}
	data := map[string][]byte{
		"config.json":           []byte(`{"model_type":"test_only"}`),
		"model.safetensors":     modelData,
		"tokenizer_config.json": []byte(`{"tokenizer_class":"TEST-ONLY"}`),
	}
	paths := make([]string, 0, len(data))
	for path := range data {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	files := make([]vllmModelFile, 0, len(paths))
	var total int64
	for _, path := range paths {
		sum := sha256.Sum256(data[path])
		files = append(files, vllmModelFile{Path: path, Size: int64(len(data[path])), SHA256: hex.EncodeToString(sum[:])})
		total += int64(len(data[path]))
	}
	fileRaw, _ := json.Marshal(files)
	manifest := sha256.Sum256(fileRaw)
	model := vllmModel{ID: "test-only/model@" + strings.Repeat("b", 40), Source: "huggingface", Revision: strings.Repeat("b", 40), License: "TEST-ONLY-license-not-clearance", MetadataSHA256: strings.Repeat("c", 64), Digest: hex.EncodeToString(manifest[:]), Bytes: total, Files: files}
	modelRaw, _ := json.Marshal(model)
	record := sha256.Sum256(modelRaw)
	plan := vllmDistributionPlan{Schema: 1, OperationID: testDistributionOperation, SourceNode: testDistributionSource, ManifestSHA: model.Digest, RecordSHA: hex.EncodeToString(record[:]), Model: model}
	if err := validateVLLMDistributionPlan(plan, vllmDistributionPlanRequest{OperationID: testDistributionOperation, Model: model.ID}); err != nil {
		t.Fatal(err)
	}
	return plan, data
}

func testDistributionExecutor(t *testing.T) (*Executor, *engineState) {
	t.Helper()
	root := t.TempDir()
	st := &engineState{manifest: &Manifest{Engine: "vllm"}, plat: &Platform{}, modelDir: root, logs: newLogBuffer()}
	e := &Executor{baseDir: filepath.Join(root, "engine-bin"), engines: map[string]*engineState{"vllm": st}, progress: newProgressHub(), reporter: NewReporter(nil)}
	e.vllmStorageFS = func(string) (uint64, string, error) { return 1 << 40, "TEST-ONLY-device", nil }
	e.vllmStorageAlloc = func(path string) (uint64, uint64, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return 0, 0, err
		}
		if info.IsDir() {
			return 0, 1, nil
		}
		return uint64(info.Size()), 1, nil
	}
	return e, st
}

func testDistributionRequest(plan vllmDistributionPlan) vllmDistributionRequest {
	return vllmDistributionRequest{OpID: strings.Repeat("d", 32), Engine: "vllm", OperationID: plan.OperationID, SourceNode: plan.SourceNode, Model: plan.Model.ID}
}

func TestVLLMDistributionResumesVerifiedChunksAndPromotesExactReceipt(t *testing.T) {
	plan, data := testDistributionPlan(t, true)
	e, st := testDistributionExecutor(t)
	first := &fakeDistributionSource{plan: plan, data: data, failAfter: 2}
	if _, err := e.receiveVLLMDistribution(context.Background(), first, testDistributionRequest(plan)); err == nil {
		t.Fatal("interrupted receive unexpectedly completed")
	}
	stage, _ := vllmReceiveStage(st, plan.OperationID)
	checkpoint, err := readVLLMReceiveState(stage)
	if err != nil || checkpoint.FileIndex != 1 || checkpoint.Offset != vllmDistributionChunkSize || !validSHA256(checkpoint.PrefixSHA256) {
		t.Fatalf("partial checkpoint = %+v, %v", checkpoint, err)
	}
	partial := filepath.Join(stage, filepath.FromSlash(plan.Model.Files[checkpoint.FileIndex].Path)) + ".partial"
	tail, err := os.OpenFile(partial, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tail.Write([]byte("uncommitted-tail"))
	_ = tail.Close()
	second := &fakeDistributionSource{plan: plan, data: data}
	result, err := e.receiveVLLMDistribution(context.Background(), second, testDistributionRequest(plan))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.requests) == 0 || second.requests[0].FileIndex != 1 || second.requests[0].Offset != vllmDistributionChunkSize {
		t.Fatalf("resume restarted instead of using its exact checkpoint: %+v", second.requests)
	}
	var retained vllmModel
	if json.Unmarshal(result, &retained) != nil || retained.ID != plan.Model.ID || retained.License != plan.Model.License || retained.MetadataSHA256 != plan.Model.MetadataSHA256 {
		t.Fatalf("promoted receipt lost provenance: %+v", retained)
	}
	if _, _, err := verifyVLLMModel(context.Background(), st, plan.Model.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatal("receive stage remained after atomic promotion")
	}
}

type corruptDistributionSource struct{ *fakeDistributionSource }

func (s corruptDistributionSource) exportChunk(ctx context.Context, req vllmDistributionChunkRequest) (vllmDistributionChunk, error) {
	chunk, err := s.fakeDistributionSource.exportChunk(ctx, req)
	if err == nil {
		chunk.SHA256 = strings.Repeat("0", 64)
	}
	return chunk, err
}

func TestVLLMDistributionRejectsCorruptChunkWithoutAdvancingCheckpoint(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, st := testDistributionExecutor(t)
	if _, err := e.receiveVLLMDistribution(context.Background(), corruptDistributionSource{&fakeDistributionSource{plan: plan, data: data}}, testDistributionRequest(plan)); err == nil || !strings.Contains(err.Error(), "chunk identity") {
		t.Fatalf("corrupt chunk was not refused: %v", err)
	}
	stage, _ := vllmReceiveStage(st, plan.OperationID)
	state, err := readVLLMReceiveState(stage)
	if err != nil || state.FileIndex != 0 || state.Offset != 0 {
		t.Fatalf("corruption advanced checkpoint: %+v, %v", state, err)
	}
}

type blockingDistributionSource struct {
	*fakeDistributionSource
	entered chan struct{}
}

func (s blockingDistributionSource) exportChunk(ctx context.Context, _ vllmDistributionChunkRequest) (vllmDistributionChunk, error) {
	select {
	case <-s.entered:
	default:
		close(s.entered)
	}
	<-ctx.Done()
	return vllmDistributionChunk{}, ctx.Err()
}

func TestVLLMDistributionCancelOwnsExactActiveStageAndCleanup(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, st := testDistributionExecutor(t)
	source := blockingDistributionSource{fakeDistributionSource: &fakeDistributionSource{plan: plan, data: data}, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := e.receiveVLLMDistribution(context.Background(), source, testDistributionRequest(plan))
		done <- err
	}()
	select {
	case <-source.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("receive did not enter chunk transfer")
	}
	wrong := testDistributionRequest(plan)
	wrong.Cancel, wrong.SourceNode = true, "TEST-ONLY-wrong-source"
	if _, err := e.cancelVLLMDistribution(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatalf("wrong source cancelled receive: %v", err)
	}
	cancel := testDistributionRequest(plan)
	cancel.Cancel = true
	if _, err := e.cancelVLLMDistribution(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("receive did not settle by cancellation: %v", err)
	}
	stage, _ := vllmReceiveStage(st, plan.OperationID)
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatal("cancelled receive stage remained")
	}
}

func TestVLLMDistributionExplicitCancelFencesQueuedReceiver(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, st := testDistributionExecutor(t)
	request := testDistributionRequest(plan)
	source := &fakeDistributionSource{plan: plan, data: data}
	st.opMu.Lock()
	locked := true
	defer func() {
		if locked {
			st.opMu.Unlock()
		}
	}()
	receiveDone := make(chan error, 1)
	go func() {
		_, err := e.receiveVLLMDistribution(context.Background(), source, request)
		receiveDone <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st.mu.Lock()
		queued := st.distributionOperationID == request.OperationID && st.distributionSourceNode == request.SourceNode &&
			st.distributionModel == request.Model && st.distributionCancel != nil && st.distributionDone != nil
		st.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receiver did not publish its exact queued ownership")
		}
		time.Sleep(time.Millisecond)
	}
	type cancellationResult struct {
		result json.RawMessage
		err    error
	}
	cancelDone := make(chan cancellationResult, 1)
	cancel := request
	cancel.Cancel = true
	go func() {
		result, err := e.cancelVLLMDistribution(context.Background(), cancel)
		cancelDone <- cancellationResult{result: result, err: err}
	}()
	select {
	case err := <-receiveDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued receiver cancellation = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exact cancellation did not stop the queued receiver")
	}
	if len(source.requests) != 0 {
		t.Fatal("queued receiver reached payload I/O before cancellation")
	}
	select {
	case result := <-cancelDone:
		t.Fatalf("cancellation acknowledged before its queued receiver fence: %s, %v", result.result, result.err)
	default:
	}
	locked = false
	st.opMu.Unlock()
	select {
	case result := <-cancelDone:
		if result.err != nil || !bytes.Contains(result.result, []byte(`"absent":true`)) {
			t.Fatalf("exact queued cancellation = %s, %v", result.result, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exact cancellation did not settle after the lifecycle lock opened")
	}
}

func TestVLLMDistributionQueuedOperationLockHonorsCancellation(t *testing.T) {
	plan, _ := testDistributionPlan(t, false)
	for _, tc := range []struct {
		name string
		run  func(context.Context, *Executor) error
	}{
		{"receive", func(ctx context.Context, e *Executor) error {
			_, err := e.receiveVLLMDistribution(ctx, nil, testDistributionRequest(plan))
			return err
		}},
		{"cancel", func(ctx context.Context, e *Executor) error {
			request := testDistributionRequest(plan)
			request.Cancel = true
			_, err := e.cancelVLLMDistribution(ctx, request)
			return err
		}},
		{"export", func(ctx context.Context, e *Executor) error {
			return e.withVLLMExportLease(ctx, func(*engineState) error {
				return errors.New("cancelled export reached the protected operation")
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st := testDistributionExecutor(t)
			st.opMu.Lock()
			defer st.opMu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				done <- tc.run(ctx, e)
			}()
			<-started
			select {
			case err := <-done:
				t.Fatalf("queued operation returned before cancellation: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("queued operation cancellation = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("queued operation stayed behind the lifecycle lock after cancellation")
			}
		})
	}
}

func TestVLLMDistributionShutdownCancelsTransferAndPreservesResumeCheckpoint(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, st := testDistributionExecutor(t)
	source := blockingDistributionSource{fakeDistributionSource: &fakeDistributionSource{plan: plan, data: data}, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := e.receiveVLLMDistribution(context.Background(), source, testDistributionRequest(plan))
		done <- err
	}()
	<-source.entered
	if err := e.stopAll(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown did not cancel transfer: %v", err)
	}
	stage, _ := vllmReceiveStage(st, plan.OperationID)
	if _, err := readVLLMReceiveState(stage); err != nil {
		t.Fatalf("shutdown discarded resumable checkpoint: %v", err)
	}
}

func TestVLLMDistributionPlanAndChunkAreExactPublicReceipts(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, _ := testDistributionExecutor(t)
	if _, err := e.receiveVLLMDistribution(context.Background(), &fakeDistributionSource{plan: plan, data: data}, testDistributionRequest(plan)); err != nil {
		t.Fatal(err)
	}
	e.vllmNodeID = testDistributionSource
	exported, err := e.exportVLLMDistributionPlan(context.Background(), vllmDistributionPlanRequest{OperationID: plan.OperationID, Model: plan.Model.ID})
	if err != nil || exported.RecordSHA != plan.RecordSHA || exported.Model.MetadataSHA256 != plan.Model.MetadataSHA256 {
		t.Fatalf("exported plan = %+v, %v", exported, err)
	}
	chunk, err := e.exportVLLMDistributionChunk(context.Background(), vllmDistributionChunkRequest{OperationID: plan.OperationID, Model: plan.Model.ID, ManifestSHA: plan.ManifestSHA, FileIndex: 0, Offset: 0, Length: plan.Model.Files[0].Size})
	if err != nil || !validSHA256(chunk.SHA256) || int64(len(chunk.Data)) != plan.Model.Files[0].Size {
		t.Fatalf("exported chunk = %+v, %v", chunk, err)
	}
}

// exportLeaseProbe runs onVerifying each time export verification checks its
// context while the export lease is free after having been held.
type exportLeaseProbe struct {
	context.Context
	st          *engineState
	onVerifying func()
	held        []bool
}

func (p *exportLeaseProbe) Err() error {
	held := !p.st.opMu.TryLock()
	if !held {
		p.st.opMu.Unlock()
	}
	p.held = append(p.held, held)
	if !held && slices.Contains(p.held, true) {
		p.onVerifying()
	}
	return p.Context.Err()
}

func TestVLLMExportPlanServesChunksWhileHashing(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, st := testDistributionExecutor(t)
	if _, err := e.receiveVLLMDistribution(context.Background(), &fakeDistributionSource{plan: plan, data: data}, testDistributionRequest(plan)); err != nil {
		t.Fatal(err)
	}
	e.vllmNodeID = testDistributionSource
	var served int
	probe := &exportLeaseProbe{Context: context.Background(), st: st}
	probe.onVerifying = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		chunk, err := e.exportVLLMDistributionChunk(ctx, vllmDistributionChunkRequest{OperationID: strings.Repeat("e", 32), Model: plan.Model.ID, ManifestSHA: plan.ManifestSHA, FileIndex: 0, Offset: 0, Length: plan.Model.Files[0].Size})
		if err != nil || chunk.SHA256 != plan.Model.Files[0].SHA256 {
			t.Fatalf("concurrent copy chunk during plan verification = %+v, %v", chunk, err)
		}
		served++
	}
	exported, err := e.exportVLLMDistributionPlan(probe, vllmDistributionPlanRequest{OperationID: plan.OperationID, Model: plan.Model.ID})
	if err != nil || exported.RecordSHA != plan.RecordSHA {
		t.Fatalf("exported plan = %+v, %v", exported, err)
	}
	if served == 0 || !probe.held[len(probe.held)-1] {
		t.Fatalf("plan verification served %d chunks; lease pattern %v", served, probe.held)
	}
}

func TestVLLMExportPlanRefusesFilesChangedWhileHashing(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, st := testDistributionExecutor(t)
	if _, err := e.receiveVLLMDistribution(context.Background(), &fakeDistributionSource{plan: plan, data: data}, testDistributionRequest(plan)); err != nil {
		t.Fatal(err)
	}
	e.vllmNodeID = testDistributionSource
	dir, err := vllmModelPath(st, plan.Model.ID)
	if err != nil {
		t.Fatal(err)
	}
	touched := filepath.Join(dir, filepath.FromSlash(plan.Model.Files[0].Path))
	base := time.Now().Add(-time.Hour)
	var touches int
	probe := &exportLeaseProbe{Context: context.Background(), st: st}
	probe.onVerifying = func() {
		touches++
		stamp := base.Add(time.Duration(touches) * time.Second)
		if err := os.Chtimes(touched, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.exportVLLMDistributionPlan(probe, vllmDistributionPlanRequest{OperationID: plan.OperationID, Model: plan.Model.ID})
	if err == nil || !strings.Contains(err.Error(), "changed during export verification") {
		t.Fatalf("plan exported a file modified while it was hashed: %v", err)
	}
}

func TestVLLMExportPlanAllowsVerificationBeforeHeadersAndPreservesCancellation(t *testing.T) {
	plan, _ := testDistributionPlan(t, false)
	base := http.DefaultTransport.(*http.Transport)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != controlVLLMExportPlanPath {
			http.NotFound(w, r)
			return
		}
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plan)
	}))
	defer srv.Close()
	client := &remoteClient{
		http:      newRemoteHTTPClient(base, 20*time.Millisecond),
		readyHTTP: newRemoteHTTPClient(base, time.Second),
		base:      srv.URL,
	}
	defer client.http.CloseIdleConnections()
	defer client.readyHTTP.CloseIdleConnections()
	req := vllmDistributionPlanRequest{OperationID: plan.OperationID, Model: plan.Model.ID}
	got, err := client.vllmExportPlan(context.Background(), req)
	if err != nil || got.RecordSHA != plan.RecordSHA {
		t.Fatalf("verified export plan was cut off by ordinary header budget: %+v, %v", got, err)
	}

	cancelled := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var request vllmDistributionPlanRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer blocked.Close()
	client.base = blocked.URL
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = client.vllmExportPlan(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled export plan error = %v, want context deadline", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancelled export plan remained active on source")
	}
}

func TestVLLMDistributionReviewsTargetCapacityBeforeFirstChunk(t *testing.T) {
	plan, data := testDistributionPlan(t, false)
	e, st := testDistributionExecutor(t)
	e.vllmStorageFS = func(string) (uint64, string, error) { return 1, "TEST-ONLY-device", nil }
	source := &fakeDistributionSource{plan: plan, data: data}
	if _, err := e.receiveVLLMDistribution(context.Background(), source, testDistributionRequest(plan)); err == nil || !strings.Contains(err.Error(), "storage review held") {
		t.Fatalf("insufficient target capacity was not held: %v", err)
	}
	if len(source.requests) != 0 {
		t.Fatal("target requested payload before its capacity review passed")
	}
	stage, _ := vllmReceiveStage(st, plan.OperationID)
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatal("capacity refusal created a receive stage")
	}
}

func TestVLLMDistributionControlIsPinGatedAndBrokerPayloadFree(t *testing.T) {
	control, _ := os.ReadFile("controlserver.go")
	remote, _ := os.ReadFile("remote.go")
	manager, _ := os.ReadFile("manager.go")
	for _, route := range []string{
		"controlVLLMExportPlanPath, s.requirePin",
		"controlVLLMExportChunkPath, s.requirePin",
		"controlVLLMReceivePath, s.requirePin",
	} {
		if !strings.Contains(string(control), route) {
			t.Fatalf("distribution route is not paired-mTLS gated: %s", route)
		}
	}
	if !strings.Contains(string(remote), `case "engine:remote-distribute-model"`) ||
		!strings.Contains(string(manager), `"engine:remote-distribute-model"`) {
		t.Fatal("typed remote distribution method is not dispatched")
	}
	if strings.Contains(string(remote), "exportVLLMDistributionChunk") || strings.Contains(string(remote), "chunk.Data") {
		t.Fatal("controller/broker remote dispatcher carries model payload bytes")
	}
	for _, name := range []string{"vllm_distribution.go", "vllm_distribution_control.go"} {
		raw, _ := os.ReadFile(name)
		lower := strings.ToLower(string(raw))
		for _, forbidden := range []string{"sftp", "ssh", "tar -", "xz ", "rdma"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s contains forbidden transport/claim %q", name, forbidden)
			}
		}
	}
}

func TestVLLMRemotePullPreservesCallerCancellationIdentity(t *testing.T) {
	want := strings.Repeat("e", 32)
	got, err := remotePullOperationID(remoteParam{Engine: "vllm", OperationID: want})
	if err != nil || got != want {
		t.Fatalf("remote pull operation = %q, %v", got, err)
	}
	if _, err := remotePullOperationID(remoteParam{Engine: "vllm", OperationID: "main"}); err == nil {
		t.Fatal("mutable/invalid remote pull operation id was accepted")
	}
	generated, err := remotePullOperationID(remoteParam{Engine: "ollama"})
	if err != nil || !vllmPullOperationToken.MatchString(generated) {
		t.Fatalf("generic remote pull lost generated correlation id: %q, %v", generated, err)
	}
}
