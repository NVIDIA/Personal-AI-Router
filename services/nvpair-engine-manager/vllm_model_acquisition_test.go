// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const acquisitionRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const acquisitionModel = "org/model@" + acquisitionRevision

func gitBlobID(data []byte) string {
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d%c", len(data), 0)
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func acquisitionMetadata(config, tokenizer, weights []byte) string {
	weightHash := sha256.Sum256(weights)
	return fmt.Sprintf(`{"id":"org/model","sha":"%s","cardData":{"license":"apache-2.0"},"siblings":[`+
		`{"rfilename":"config.json","blobId":"%s","size":%d},`+
		`{"rfilename":"tokenizer.json","blobId":"%s","size":%d},`+
		`{"rfilename":"model.safetensors","blobId":"ignored","size":%d,"lfs":{"sha256":"%s","size":%d}}]}`,
		acquisitionRevision, gitBlobID(config), len(config), gitBlobID(tokenizer), len(tokenizer), len(weights), hex.EncodeToString(weightHash[:]), len(weights))
}

func acquisitionFixture(t *testing.T) (*vllmInstallFixture, []byte, []byte, []byte) {
	t.Helper()
	f := vllmResourceFixture(t)
	f.st.manifest.Actions = map[string]Action{
		"list_downloaded": {Builtin: "vllm", Result: &ActionResult{Array: "data", Field: "id"}},
		pullModelAction:   {Builtin: "vllm"},
		"cancel_pull":     {Builtin: "vllm"},
	}
	id := "v0-acquisition"
	writeManagedVLLMTestEnvironment(t, f.st, id, managedVLLMVersion)
	if err := writeVLLMRuntimeRecord(f.st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: id}); err != nil {
		t.Fatal(err)
	}
	f.e.vllmStorageFS = func(string) (uint64, string, error) { return 2 << 40, "fixture-device", nil }
	f.e.vllmStorageAlloc = func(path string) (uint64, uint64, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return 0, 0, err
		}
		if info.IsDir() || info.Size() < 4096 {
			return 4096, 1, nil
		}
		return uint64(info.Size()), 1, nil
	}
	config := []byte(`{"model_type":"llama"}`)
	tokenizer := []byte(`{}`)
	weights := []byte("fixture-weights")
	metadata := acquisitionMetadata(config, tokenizer, weights)
	f.e.client = &http.Client{Transport: actionBodyTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" || r.URL.Path != "/api/models/org/model/revision/"+acquisitionRevision {
			t.Fatalf("metadata request escaped public exact revision policy: %s auth=%q", r.URL, r.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(metadata)), Request: r}, nil
	})}
	return f, config, tokenizer, weights
}

func writeAcquisitionSnapshot(t *testing.T, args []string, config, tokenizer, weights []byte) {
	t.Helper()
	if len(args) < 7 || args[0] != "-I" || args[1] != "-B" || args[4] != "org/model" || args[5] != acquisitionRevision {
		t.Fatalf("download argv is not exact and isolated: %#v", args)
	}
	stage := args[6]
	for name, data := range map[string][]byte{"config.json": config, "tokenizer.json": tokenizer, "model.safetensors": weights} {
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVLLMDownloadEnvEnablesXetOnlyForPinnedQwen(t *testing.T) {
	for _, tc := range []struct {
		name, model, inherited, want string
	}{
		{"generic", acquisitionModel, "0", "1"},
		{"pinned Qwen", vllmQwen38ModelID, "1", "0"},
		{"different Qwen revision", vllmQwen38Repository + "@" + strings.Repeat("0", 40), "0", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stage := filepath.Join(root, "stage")
			env, err := vllmDownloadEnv(&engineState{modelDir: root}, []string{"HF_HUB_DISABLE_XET=" + tc.inherited}, stage, tc.model)
			if err != nil {
				t.Fatal(err)
			}
			joined := "\n" + strings.Join(env, "\n") + "\n"
			if !strings.Contains(joined, "\nHF_HUB_DISABLE_XET="+tc.want+"\n") {
				t.Fatalf("model %q: unexpected Xet policy in %q", tc.model, joined)
			}
			if !strings.Contains(joined, "\nHF_XET_CACHE="+filepath.Join(stage, ".cache", "pair", "hf-xet")+"\n") {
				t.Fatal("download environment lacks its owned Xet cache")
			}
		})
	}
}

func TestVLLMManagedAcquisitionPromotesImmutableReceiptAndInventory(t *testing.T) {
	f, config, tokenizer, weights := acquisitionFixture(t)
	var mu sync.Mutex
	var stages []string
	f.e.emit = func(method string, payload any) {
		if method != "engine:pull-progress" {
			return
		}
		raw, _ := json.Marshal(payload)
		var progress struct{ Stage, OperationID string }
		_ = json.Unmarshal(raw, &progress)
		if progress.OperationID != strings.Repeat("b", 32) {
			t.Errorf("progress operation id = %q", progress.OperationID)
		}
		mu.Lock()
		stages = append(stages, progress.Stage)
		mu.Unlock()
	}
	f.e.vllmExec = func(_ context.Context, _ string, args, env []string) ([]byte, error) {
		joined := strings.Join(env, "\n")
		for _, required := range []string{"HF_HUB_DISABLE_IMPLICIT_TOKEN=1", "HF_HUB_OFFLINE=0", "HF_HUB_DISABLE_XET=1"} {
			if !strings.Contains(joined, required) {
				t.Errorf("download environment lacks %s", required)
			}
		}
		if strings.Contains(joined, "HF_TOKEN=") {
			t.Fatal("download inherited a Hugging Face credential")
		}
		writeAcquisitionSnapshot(t, args, config, tokenizer, weights)
		return nil, nil
	}
	params, _ := json.Marshal(vllmPullParams{Model: acquisitionModel, OperationID: strings.Repeat("b", 32)})
	raw, err := f.e.Action(context.Background(), "vllm", pullModelAction, params)
	if err != nil {
		t.Fatal(err)
	}
	var model vllmModel
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatal(err)
	}
	if model.ID != acquisitionModel || model.Revision != acquisitionRevision || model.License != "apache-2.0" || !validSHA256(model.MetadataSHA256) || len(model.Files) != 3 {
		t.Fatalf("promoted receipt = %+v", model)
	}
	if _, _, err := verifyVLLMModel(context.Background(), f.st, acquisitionModel); err != nil {
		t.Fatalf("promoted content did not reverify: %v", err)
	}
	listed, err := f.e.Action(context.Background(), "vllm", "list_downloaded", nil)
	if err != nil || !strings.Contains(string(listed), acquisitionModel) {
		t.Fatalf("downloaded inventory = %s, %v", listed, err)
	}
	if _, err := os.Lstat(filepath.Join(f.st.modelDir, vllmDownloadStageName(acquisitionModel))); !os.IsNotExist(err) {
		t.Fatalf("successful stage was not atomically promoted: %v", err)
	}
	mu.Lock()
	gotStages := strings.Join(stages, ",")
	mu.Unlock()
	for _, want := range []string{"resolving", "reviewing-storage", "downloading", "verifying", "success"} {
		if !strings.Contains(gotStages, want) {
			t.Fatalf("progress %q lacks %q", gotStages, want)
		}
	}
}

func TestVLLMManagedAcquisitionUpgradesVerifiedLegacyReceiptMetadata(t *testing.T) {
	f, config, tokenizer, weights := acquisitionFixture(t)
	f.e.vllmExec = func(_ context.Context, _ string, args, _ []string) ([]byte, error) {
		writeAcquisitionSnapshot(t, args, config, tokenizer, weights)
		return nil, nil
	}
	params, _ := json.Marshal(vllmPullParams{Model: acquisitionModel, OperationID: strings.Repeat("8", 32)})
	if _, err := f.e.Action(context.Background(), "vllm", pullModelAction, params); err != nil {
		t.Fatal(err)
	}
	model, dir, err := readVLLMModel(f.st, acquisitionModel)
	if err != nil {
		t.Fatal(err)
	}
	model.MetadataSHA256 = ""
	if err := writeVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), model); err != nil {
		t.Fatal(err)
	}
	f.e.vllmExec = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("download should not run for verified retained bytes")
	}
	params, _ = json.Marshal(vllmPullParams{Model: acquisitionModel, OperationID: strings.Repeat("9", 32)})
	if _, err := f.e.Action(context.Background(), "vllm", pullModelAction, params); err != nil {
		t.Fatal(err)
	}
	upgraded, _, err := readVLLMModel(f.st, acquisitionModel)
	if err != nil || !validSHA256(upgraded.MetadataSHA256) {
		t.Fatalf("legacy receipt metadata was not upgraded: %+v, %v", upgraded, err)
	}
}

func TestVLLMManagedAcquisitionCancelPreservesAndResumesExactStage(t *testing.T) {
	f, config, tokenizer, weights := acquisitionFixture(t)
	started := make(chan struct{})
	f.e.vllmExec = func(ctx context.Context, _ string, args, _ []string) ([]byte, error) {
		stage := args[6]
		if err := os.WriteFile(filepath.Join(stage, "partial.safetensors"), []byte("partial"), 0o600); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	opID := strings.Repeat("c", 32)
	params, _ := json.Marshal(vllmPullParams{Model: acquisitionModel, OperationID: opID})
	type outcome struct {
		raw json.RawMessage
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		raw, err := f.e.Action(context.Background(), "vllm", pullModelAction, params)
		done <- outcome{raw, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("pull did not start")
	}
	cancelParams, _ := json.Marshal(vllmPullParams{Model: acquisitionModel, OperationID: opID})
	accepted, err := f.e.Action(context.Background(), "vllm", "cancel_pull", cancelParams)
	if err != nil || !strings.Contains(string(accepted), `"accepted":true`) {
		t.Fatalf("cancel = %s, %v", accepted, err)
	}
	result := <-done
	if result.err != nil || !strings.Contains(string(result.raw), `"resumable":true`) {
		t.Fatalf("cancel outcome = %s, %v", result.raw, result.err)
	}
	stage := filepath.Join(f.st.modelDir, vllmDownloadStageName(acquisitionModel))
	if _, err := os.Lstat(filepath.Join(stage, vllmDownloadStageRecord)); err != nil {
		t.Fatalf("cancel did not retain exact resume receipt: %v", err)
	}
	listed, err := f.e.Action(context.Background(), "vllm", "list_downloaded", nil)
	if err != nil || string(listed) != `{"data":[]}` {
		t.Fatalf("partial stage corrupted downloaded inventory: %s, %v", listed, err)
	}
	retained := f.e.ModelsResult(context.Background()).RetainedByEngine
	if models, ok := retained["vllm"]; !ok || len(models) != 0 {
		t.Fatalf("partial stage erased authoritative empty retained inventory: %+v", retained)
	}
	var sawResume bool
	f.e.emit = func(method string, payload any) {
		raw, _ := json.Marshal(payload)
		sawResume = sawResume || method == "engine:pull-progress" && strings.Contains(string(raw), `"stage":"resuming"`)
	}
	f.e.vllmExec = func(_ context.Context, _ string, args, _ []string) ([]byte, error) {
		_ = os.Remove(filepath.Join(args[6], "partial.safetensors"))
		writeAcquisitionSnapshot(t, args, config, tokenizer, weights)
		return nil, nil
	}
	resumeParams, _ := json.Marshal(vllmPullParams{Model: acquisitionModel, OperationID: strings.Repeat("d", 32)})
	if _, err := f.e.Action(context.Background(), "vllm", pullModelAction, resumeParams); err != nil {
		t.Fatal(err)
	}
	if !sawResume {
		t.Fatal("second exact pull did not report resumable continuation")
	}
}

func TestVLLMManagedAcquisitionRefusesMutableOrCredentialedSources(t *testing.T) {
	for _, value := range []string{"org/model", "https://huggingface.co/org/model", "org/model@main", "org/model@AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, _, err := parseExactVLLMHFModel(value); err == nil {
			t.Errorf("mutable/URL source %q accepted", value)
		}
	}
	f, _, _, _ := acquisitionFixture(t)
	f.e.client = &http.Client{Transport: actionBodyTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("gated")), Request: r}, nil
	})}
	params, _ := json.Marshal(vllmPullParams{Model: acquisitionModel})
	if _, err := f.e.Action(context.Background(), "vllm", pullModelAction, params); err == nil || !strings.Contains(err.Error(), "public ungated") || !strings.Contains(err.Error(), "does not request or use Hub credentials") {
		t.Fatalf("gated refusal = %v", err)
	}
}

func TestVLLMStorageReviewCreditsExactResumableStage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "models")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	stageName := vllmDownloadStageName(acquisitionModel)
	stage := filepath.Join(root, stageName)
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(stage, "model.partial")
	if err := os.WriteFile(partial, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	const available = uint64(100 << 30)
	filesystem := func(string) (uint64, string, error) { return available, "fixture-device", nil }
	allocation := func(path string) (uint64, uint64, error) {
		if path == partial {
			return 80 << 30, 1, nil
		}
		return 0, 1, nil
	}
	review, err := reviewVLLMDownloadStorage(root, stageName, vllmQwen38SnapshotBytes, filesystem, allocation)
	if err != nil || !review.Ready || review.StagedBytes != 80<<30 || review.RequiredAvailableBytes >= vllmQwen38SnapshotBytes+vllmDownloadCacheCeilingBytes+vllmPostOperationReserveBytes {
		t.Fatalf("resume review = %+v, %v", review, err)
	}
	other := filepath.Join(root, ".download-other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	review, err = reviewVLLMDownloadStorage(root, stageName, vllmQwen38SnapshotBytes, filesystem, allocation)
	if err != nil || review.Ready || review.UnresolvedStages != 1 {
		t.Fatalf("different unresolved stage was not held: %+v, %v", review, err)
	}
}

func TestVLLMLicenseNameRetainsOtherClassification(t *testing.T) {
	var metadata vllmHFMetadata
	metadata.CardData.License = json.RawMessage(`"other"`)
	metadata.CardData.LicenseName = json.RawMessage(`"nvidia-open-model-license"`)
	license, err := vllmMetadataLicense(metadata)
	if err != nil || license != "nvidia-open-model-license" {
		t.Fatalf("license = %q, %v", license, err)
	}
}

func TestVLLMDownloadProgressUsesCompletedPinnedBytesAndCoalesces(t *testing.T) {
	stage := t.TempDir()
	siblings := []vllmHFSibling{{Path: "config.json", Size: 25}, {Path: "model.safetensors", Size: 75}}
	executor := &Executor{progress: newProgressHub()}
	originalInterval := vllmDownloadProgressInterval
	vllmDownloadProgressInterval = 5 * time.Millisecond
	t.Cleanup(func() { vllmDownloadProgressInterval = originalInterval })
	var mu sync.Mutex
	var percents []int
	executor.emit = func(method string, payload any) {
		if method != "engine:pull-progress" {
			return
		}
		raw, _ := json.Marshal(payload)
		var event struct {
			Percent     int    `json:"percent"`
			OperationID string `json:"operationId"`
		}
		_ = json.Unmarshal(raw, &event)
		if event.OperationID != strings.Repeat("e", 32) {
			t.Errorf("progress lost operation binding: %q", event.OperationID)
		}
		mu.Lock()
		percents = append(percents, event.Percent)
		mu.Unlock()
	}
	executor.vllmExec = func(_ context.Context, _ string, _ []string, _ []string) ([]byte, error) {
		if err := os.WriteFile(filepath.Join(stage, "config.json"), make([]byte, 25), 0o600); err != nil {
			return nil, err
		}
		time.Sleep(30 * time.Millisecond)
		if err := os.WriteFile(filepath.Join(stage, "model.safetensors"), make([]byte, 75), 0o600); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := executor.runVLLMDownload(context.Background(), "python", "org/model", acquisitionRevision, stage, nil, siblings, 100, strings.Repeat("e", 32), "downloading"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]int(nil), percents...)
	mu.Unlock()
	if len(got) < 3 || got[0] != 5 || got[len(got)-1] != 94 {
		t.Fatalf("byte progress = %v, want monotonic download phase 5..94", got)
	}
	found25 := false
	for i, percent := range got {
		found25 = found25 || percent == 27
		if i > 0 && percent == got[i-1] {
			t.Fatalf("duplicate percent was not coalesced: %v", got)
		}
		if i > 0 && percent < got[i-1] {
			t.Fatalf("progress regressed: %v", got)
		}
	}
	if !found25 {
		t.Fatalf("byte progress never reflected the completed 25-byte file: %v", got)
	}
}

func TestVLLMRemoteCancelEndpointRequiresExactBinding(t *testing.T) {
	f := vllmResourceFixture(t)
	f.st.manifest.Actions = map[string]Action{"cancel_pull": {Builtin: "vllm"}}
	opID := strings.Repeat("f", 32)
	canceled := false
	f.st.pullModel, f.st.pullOperationID = acquisitionModel, opID
	f.st.pullCancel = func() { canceled = true }
	server := &controlServer{exec: f.e}
	body, _ := json.Marshal(cancelPullRequest{Engine: "vllm", Model: acquisitionModel, OperationID: opID})
	recorder := httptest.NewRecorder()
	server.handleCancelPull(recorder, httptest.NewRequest(http.MethodPost, controlCancelPullPath, strings.NewReader(string(body))))
	if recorder.Code != http.StatusOK || !canceled || !strings.Contains(recorder.Body.String(), `"accepted":true`) {
		t.Fatalf("typed cancel = HTTP %d %s canceled=%v", recorder.Code, recorder.Body.String(), canceled)
	}
	recorder = httptest.NewRecorder()
	server.handleCancelPull(recorder, httptest.NewRequest(http.MethodPost, controlCancelPullPath, strings.NewReader(`{"engine":"vllm","model":"org/model@main","operationId":"`+opID+`"}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mutable remote cancel model returned HTTP %d", recorder.Code)
	}
}

func TestQwen38IsAnExactSupportedAcquisitionCandidate(t *testing.T) {
	repo, revision, err := parseExactVLLMHFModel(vllmQwen38ModelID)
	if err != nil || repo != vllmQwen38Repository || revision != vllmQwen38Revision {
		t.Fatalf("Qwen candidate = %q %q %v", repo, revision, err)
	}
	if err := validateQwen38SnapshotPlan(vllmQwen38ModelID, vllmSnapshotPlan{Files: vllmQwen38SnapshotFiles, Bytes: vllmQwen38SnapshotBytes}); err != nil {
		t.Fatal(err)
	}
}

func TestQwen38ShardIndexUsesPinnedBoundWithoutRelaxingGenericModels(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"config.json":                      `{"model_type":"qwen4_exp"}`,
		"tokenizer.json":                   `{}`,
		"model-00001-of-00010.safetensors": "first shard",
		"model-fp8-mtp-ple.safetensors":    "MTP shard",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	indexPath := filepath.Join(dir, "model.safetensors.index.json")
	index := []byte(`{"metadata":{"total_size":1},"weight_map":{"first":"model-00001-of-00010.safetensors","mtp":"model-fp8-mtp-ple.safetensors"}}`)
	index = append(index, bytes.Repeat([]byte(" "), int(vllmQwen38ShardIndexBytes)-len(index))...)
	if err := os.WriteFile(indexPath, index, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := scanVLLMSnapshot(context.Background(), dir, vllmQwen38ModelID); err != nil {
		t.Fatalf("pinned Qwen index rejected: %v", err)
	}
	if _, _, _, err := scanVLLMSnapshot(context.Background(), dir, acquisitionModel); err == nil || err.Error() != "invalid vLLM shard index" {
		t.Fatalf("generic model accepted the oversized index: %v", err)
	}
	if err := os.WriteFile(indexPath, append(index, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := scanVLLMSnapshot(context.Background(), dir, vllmQwen38ModelID); err == nil || err.Error() != "invalid vLLM shard index" {
		t.Fatalf("Qwen accepted an index beyond the pinned bound: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte(`{"weight_map":{"missing":"absent.safetensors"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := scanVLLMSnapshot(context.Background(), dir, vllmQwen38ModelID); err == nil || err.Error() != "vLLM model snapshot is missing an indexed shard" {
		t.Fatalf("Qwen accepted a missing indexed shard: %v", err)
	}
}
