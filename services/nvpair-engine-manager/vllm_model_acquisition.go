// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The immutable Hugging Face snapshot pattern is adapted from independently
// sealed prior Engine Manager source evidence. This implementation keeps the
// current schema-v2 runtime, selection, group, diagnostic and proxy owners.

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	maxVLLMModelBytes       int64 = 512 << 30
	vllmPullTimeout               = 5*time.Hour + 55*time.Minute
	vllmDownloadStageRecord       = ".pair-download.json"
)

var vllmDownloadProgressInterval = 2 * time.Second

var (
	vllmHFRepoPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	vllmHFRevisionPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	vllmPullOperationToken = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type vllmContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r vllmContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(buffer) > 64<<10 {
		buffer = buffer[:64<<10]
	}
	n, err := r.reader.Read(buffer)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

type vllmHFSibling struct {
	Path string `json:"rfilename"`
	Blob string `json:"blobId"`
	Size int64  `json:"size"`
	LFS  *struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"lfs"`
}

type vllmHFMetadata struct {
	ID       string `json:"id"`
	SHA      string `json:"sha"`
	CardData struct {
		License     json.RawMessage `json:"license"`
		LicenseName json.RawMessage `json:"license_name"`
	} `json:"cardData"`
	Siblings []vllmHFSibling `json:"siblings"`
}

type vllmDownloadRecord struct {
	Schema         int    `json:"schema"`
	Model          string `json:"model"`
	Revision       string `json:"revision"`
	License        string `json:"license"`
	MetadataSHA256 string `json:"metadataSha256"`
	SnapshotBytes  uint64 `json:"snapshotBytes"`
	SnapshotFiles  int    `json:"snapshotFiles"`
}

func readVLLMDownloadRecord(stage string) (vllmDownloadRecord, error) {
	var record vllmDownloadRecord
	var raw json.RawMessage
	if err := readVLLMJSON(stage, filepath.Join(stage, vllmDownloadStageRecord), 32<<10, &raw); err != nil {
		return record, err
	}
	if err := decodeActionParams(raw, &record); err != nil {
		return record, err
	}
	return record, nil
}

type vllmPullParams struct {
	Name        string `json:"name,omitempty"`
	Model       string `json:"model"`
	OperationID string `json:"operationId,omitempty"`
}

type vllmUserPullCancellation struct{ operationID string }

func (*vllmUserPullCancellation) Error() string { return "vLLM pull canceled by user" }

func parseExactVLLMHFModel(value string) (string, string, error) {
	if value == "" || len(value) > 240 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", "", errors.New("vLLM download requires exact owner/repository@40-character-commit")
	}
	at := strings.LastIndexByte(value, '@')
	if at <= 0 || strings.Count(value, "@") != 1 {
		return "", "", errors.New("vLLM download requires exact owner/repository@40-character-commit")
	}
	repo, revision := value[:at], value[at+1:]
	if !vllmHFRepoPattern.MatchString(repo) || !vllmHFRevisionPattern.MatchString(revision) {
		return "", "", errors.New("vLLM download requires exact owner/repository@40-character-commit")
	}
	return repo, revision, nil
}

func vllmMetadataLicense(meta vllmHFMetadata) (string, error) {
	var license string
	if err := json.Unmarshal(meta.CardData.License, &license); err != nil {
		return "", errors.New("vLLM model metadata must declare a license")
	}
	if license == "other" {
		if err := json.Unmarshal(meta.CardData.LicenseName, &license); err != nil {
			return "", errors.New("vLLM model metadata must name its declared license")
		}
	}
	license = strings.TrimSpace(license)
	if license == "" || len(license) > 128 || strings.ContainsAny(license, "\x00\r\n") {
		return "", errors.New("vLLM model metadata must declare a bounded license identifier")
	}
	return license, nil
}

func selectedVLLMMetadataDigest(meta vllmHFMetadata, license string) (string, error) {
	type fileIdentity struct {
		Path, Blob, SHA256 string
		Size               uint64
	}
	selected := make([]fileIdentity, 0, len(meta.Siblings))
	for _, sibling := range meta.Siblings {
		if !allowedVLLMAsset(sibling.Path) {
			continue
		}
		size, err := vllmHFSiblingBytes(sibling)
		if err != nil {
			return "", err
		}
		identity := fileIdentity{Path: sibling.Path, Blob: strings.ToLower(sibling.Blob), Size: size}
		if sibling.LFS != nil {
			identity.SHA256 = strings.ToLower(sibling.LFS.SHA256)
			if !validSHA256(identity.SHA256) {
				return "", fmt.Errorf("vLLM model source has invalid LFS identity for %s", sibling.Path)
			}
		} else if len(identity.Blob) != 40 || strings.Trim(identity.Blob, "0123456789abcdef") != "" {
			return "", fmt.Errorf("vLLM model source has invalid Git identity for %s", sibling.Path)
		}
		selected = append(selected, identity)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Path < selected[j].Path })
	canonical, err := json.Marshal(struct {
		ID, Revision, License string
		Files                 []fileIdentity
	}{meta.ID, meta.SHA, license, selected})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (e *Executor) fetchVLLMHFMetadata(ctx context.Context, repo, revision string) (vllmHFMetadata, string, string, error) {
	var meta vllmHFMetadata
	url := "https://huggingface.co/api/models/" + repo + "/revision/" + revision + "?blobs=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return meta, "", "", err
	}
	client := *e.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return meta, "", "", errors.New("vLLM public model metadata is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return meta, "", "", errors.New("vLLM managed acquisition accepts public ungated Hugging Face models only; PAIR does not request or use Hub credentials")
	}
	if response.StatusCode != http.StatusOK {
		return meta, "", "", fmt.Errorf("vLLM public model metadata returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(body) > 8<<20 {
		return meta, "", "", errors.New("vLLM model metadata exceeds its bounded response limit")
	}
	if json.Unmarshal(body, &meta) != nil || meta.ID != repo || meta.SHA != revision || len(meta.Siblings) == 0 || len(meta.Siblings) > vllmModelReceiptMaxFiles+1 {
		return meta, "", "", errors.New("vLLM model source identity does not match the requested immutable revision")
	}
	license, err := vllmMetadataLicense(meta)
	if err != nil {
		return meta, "", "", err
	}
	metadataDigest, err := selectedVLLMMetadataDigest(meta, license)
	return meta, license, metadataDigest, err
}

func allowedVLLMAsset(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".json", ".safetensors", ".model", ".txt", ".tiktoken", ".jinja":
		return true
	}
	return strings.HasPrefix(base, "license") || strings.HasPrefix(base, "readme")
}

func scanVLLMSnapshot(ctx context.Context, dir, model string) ([]vllmModelFile, string, int64, error) {
	files := []vllmModelFile{}
	var total int64
	weights, tokenizer := false, false
	err := filepath.WalkDir(dir, func(value string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if value == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, value)
		if err != nil {
			return err
		}
		if rel == ".cache" || strings.HasPrefix(rel, ".cache"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("vLLM snapshots cannot contain redirected files")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("vLLM snapshot contains a nonregular file")
		}
		if rel == vllmDownloadStageRecord || rel == vllmReceiveStateFile || rel == "pair-model.json" {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !allowedVLLMAsset(rel) {
			return errors.New("vLLM snapshot contains unsupported assets; use a safetensors generation snapshot")
		}
		if info.Size() < 0 || total > maxVLLMModelBytes-info.Size() || len(files) >= vllmModelReceiptMaxFiles {
			return errors.New("vLLM snapshot exceeds the bounded model library limit")
		}
		total += info.Size()
		file, err := os.Open(value)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, vllmContextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		files = append(files, vllmModelFile{Path: rel, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
		weights = weights || strings.HasSuffix(rel, ".safetensors")
		base := filepath.Base(rel)
		tokenizer = tokenizer || base == "tokenizer.json" || base == "tokenizer.model" || base == "tokenizer_config.json"
		return nil
	})
	if err != nil {
		return nil, "", 0, err
	}
	var config struct {
		ModelType string `json:"model_type"`
	}
	if err := readVLLMJSON(dir, filepath.Join(dir, "config.json"), 4<<20, &config); err != nil || config.ModelType == "" || !weights || !tokenizer {
		return nil, "", 0, errors.New("vLLM requires config, tokenizer and safetensors model files")
	}
	known := make(map[string]bool, len(files))
	for _, file := range files {
		known[file.Path] = true
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Path, ".safetensors.index.json") {
			continue
		}
		var index struct {
			WeightMap map[string]string `json:"weight_map"`
		}
		indexLimit := int64(8 << 20)
		if isQwen38ProfileModel(model) && file.Path == "model.safetensors.index.json" {
			indexLimit = vllmQwen38ShardIndexBytes
		}
		if err := readVLLMJSON(dir, filepath.Join(dir, filepath.FromSlash(file.Path)), indexLimit, &index); err != nil || len(index.WeightMap) == 0 {
			return nil, "", 0, errors.New("invalid vLLM shard index")
		}
		for _, shard := range index.WeightMap {
			if !known[shard] {
				return nil, "", 0, errors.New("vLLM model snapshot is missing an indexed shard")
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	manifest, _ := json.Marshal(files)
	digest := sha256.Sum256(manifest)
	return files, hex.EncodeToString(digest[:]), total, nil
}

func vllmDownloadStageName(model string) string {
	digest := sha256.Sum256([]byte(model))
	return ".download-" + hex.EncodeToString(digest[:])
}

func syncVLLMDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if runtime.GOOS == "windows" {
		syncErr = nil
	}
	return errors.Join(syncErr, closeErr)
}

func prepareVLLMDownloadStage(root string, record vllmDownloadRecord) (string, bool, error) {
	stage := filepath.Join(root, vllmDownloadStageName(record.Model))
	if err := validateVLLMOwnedPath(root, stage); err != nil {
		return "", false, err
	}
	info, err := os.Lstat(stage)
	if os.IsNotExist(err) {
		if err := os.Mkdir(stage, 0o700); err != nil {
			return "", false, err
		}
		if err := writeVLLMJSON(stage, filepath.Join(stage, vllmDownloadStageRecord), record); err != nil {
			_ = os.Remove(stage)
			return "", false, err
		}
		if err := errors.Join(syncVLLMDirectory(stage), syncVLLMDirectory(root)); err != nil {
			return "", false, err
		}
		return stage, false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("vLLM resumable stage is unavailable or redirected")
	}
	retained, err := readVLLMDownloadRecord(stage)
	if err != nil || retained != record {
		return "", false, errors.New("vLLM resumable stage does not match this exact model metadata")
	}
	return stage, true, nil
}

func vllmDownloadEnv(st *engineState, env []string, stage, model string) ([]string, error) {
	if err := validateVLLMOwnedPath(st.modelDir, stage); err != nil {
		return nil, err
	}
	cache := filepath.Join(stage, ".cache", "pair")
	paths := map[string]string{
		"XDG_CACHE_HOME": filepath.Join(cache, "xdg"), "XDG_CONFIG_HOME": filepath.Join(cache, "config"),
		"HF_HOME": filepath.Join(cache, "hf-home"), "HF_HUB_CACHE": filepath.Join(cache, "hf-hub"),
		"HF_XET_CACHE": filepath.Join(cache, "hf-xet"), "TMPDIR": filepath.Join(cache, "tmp"),
		"TEMP": filepath.Join(cache, "tmp"), "TMP": filepath.Join(cache, "tmp"),
	}
	values := make(map[string]string, len(env)+16)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, errors.New("invalid managed vLLM environment entry")
		}
		values[key] = value
	}
	for key, value := range paths {
		if err := validateVLLMOwnedPath(stage, value); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(value, 0o700); err != nil {
			return nil, err
		}
		values[key] = value
	}
	for key, value := range map[string]string{
		"HF_HUB_OFFLINE": "0", "TRANSFORMERS_OFFLINE": "0", "HF_HUB_DISABLE_XET": "1",
		"HF_HUB_DOWNLOAD_TIMEOUT": "120", "HF_XET_CHUNK_CACHE_SIZE_BYTES": "0",
		"HF_XET_SHARD_CACHE_SIZE_LIMIT": "0", "HF_HUB_DISABLE_UPDATE_CHECK": "1",
		"HF_HUB_DISABLE_TELEMETRY": "1", "HF_HUB_DISABLE_IMPLICIT_TOKEN": "1",
		"HF_HUB_DISABLE_PROGRESS_BARS": "1",
	} {
		values[key] = value
	}
	// Only the pinned Qwen runtime seals hf-xet; generic downloads keep the regular path.
	if isQwen38ProfileModel(model) {
		values["HF_HUB_DISABLE_XET"] = "0"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

func (e *Executor) vllmActivePython(st *engineState) (string, error) {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || record.Active == "" || record.Removing {
		return "", errors.New("vLLM has no active PAIR-owned runtime; install vLLM before acquiring a model")
	}
	python, _, _, err := validateVLLMRuntimeBinaries(st, record.Active)
	return python, err
}

func completedVLLMDownloadBytes(stage string, siblings []vllmHFSibling) (uint64, error) {
	var completed uint64
	for _, sibling := range siblings {
		if !allowedVLLMAsset(sibling.Path) {
			continue
		}
		expected, err := vllmHFSiblingBytes(sibling)
		if err != nil {
			return 0, err
		}
		path := filepath.Join(stage, filepath.FromSlash(sibling.Path))
		if err := validateVLLMOwnedPath(stage, path); err != nil {
			return 0, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || uint64(info.Size()) > expected {
			return 0, fmt.Errorf("vLLM download progress observed an invalid selected file")
		}
		completed += uint64(info.Size())
	}
	return completed, nil
}

func vllmDownloadOperationPercent(completed, total uint64) int {
	if total == 0 {
		return 5
	}
	percent := 5 + int(completed*89/total)
	if percent > 94 {
		return 94
	}
	return percent
}

// runVLLMDownload reports only confirmed bytes at the metadata-selected final
// paths. It stats that fixed, bounded list (at most 20,000; 24 for Qwen3.8)
// rather than walking or hashing the cache tree on every tick. Repeated percent
// values are coalesced; final provenance hashing remains a separate gate.
func (e *Executor) runVLLMDownload(ctx context.Context, python, repo, revision, stage string, env []string, siblings []vllmHFSibling, total uint64, operationID, progressStage string) error {
	const script = `from huggingface_hub import snapshot_download
import sys
snapshot_download(repo_id=sys.argv[1], revision=sys.argv[2], local_dir=sys.argv[3], allow_patterns=sys.argv[4:], token=False)`
	args := []string{"-I", "-B", "-c", script, repo, revision, stage,
		"*.json", "*.safetensors", "*.model", "*.txt", "*.tiktoken", "*.jinja", "LICENSE*", "README*"}
	initialBytes, _ := completedVLLMDownloadBytes(stage, siblings)
	initialPercent := vllmDownloadOperationPercent(initialBytes, total)
	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "pull", Stage: progressStage, Percent: initialPercent, Message: repo + "@" + revision, OperationID: operationID})
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(vllmDownloadProgressInterval)
		defer ticker.Stop()
		lastPercent := initialPercent
		emit := func() {
			completed, err := completedVLLMDownloadBytes(stage, siblings)
			if err != nil || total == 0 {
				return
			}
			// Resolving/storage occupy 1..2, download occupies 5..94, byte
			// verification is 95, and only a promoted receipt reaches 100.
			percent := vllmDownloadOperationPercent(completed, total)
			if percent == lastPercent {
				return
			}
			lastPercent = percent
			e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "pull", Stage: progressStage, Percent: percent, Message: repo + "@" + revision, OperationID: operationID})
		}
		for {
			select {
			case <-monitorCtx.Done():
				emit()
				return
			case <-ticker.C:
				emit()
			}
		}
	}()
	var err error
	if e.vllmExec != nil {
		_, err = e.vllmExec(ctx, python, args, env)
	} else {
		_, err = runManagedVLLMCommand(ctx, filepath.Dir(python), python, args, env)
	}
	stopMonitor()
	<-monitorDone
	if err != nil {
		return fmt.Errorf("public ungated Hugging Face download did not complete; PAIR does not use Hub credentials: %w", err)
	}
	return nil
}

func verifyVLLMDownloadedFiles(ctx context.Context, stage string, meta vllmHFMetadata, files []vllmModelFile) error {
	expected := make(map[string]vllmHFSibling, len(meta.Siblings))
	for _, sibling := range meta.Siblings {
		if allowedVLLMAsset(sibling.Path) {
			expected[sibling.Path] = sibling
		}
	}
	for _, file := range files {
		source, ok := expected[file.Path]
		if !ok {
			return errors.New("downloaded vLLM file is absent from the pinned repository revision")
		}
		sourceBytes, err := vllmHFSiblingBytes(source)
		if err != nil || sourceBytes != uint64(file.Size) {
			return errors.New("vLLM model source file size mismatch")
		}
		if source.LFS != nil {
			if !strings.EqualFold(source.LFS.SHA256, file.SHA256) {
				return errors.New("vLLM model content digest mismatch")
			}
		} else {
			value := filepath.Join(stage, filepath.FromSlash(file.Path))
			opened, err := os.Open(value)
			if err != nil {
				return err
			}
			hash := sha1.New()
			_, _ = fmt.Fprintf(hash, "blob %d%c", file.Size, 0)
			_, copyErr := io.Copy(hash, vllmContextReader{ctx: ctx, reader: opened})
			closeErr := opened.Close()
			if copyErr != nil || closeErr != nil || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), source.Blob) {
				return errors.New("vLLM repository file identity mismatch")
			}
		}
		delete(expected, file.Path)
	}
	if len(expected) != 0 {
		return errors.New("downloaded vLLM snapshot omitted pinned repository files")
	}
	return nil
}

func (e *Executor) promoteVLLMDownloadedModel(ctx context.Context, st *engineState, stage string, model vllmModel) (json.RawMessage, error) {
	destination, err := vllmModelPath(st, model.ID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(destination); err == nil {
		return nil, errors.New("vLLM model id already exists; existing data was retained")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := writeVLLMJSON(stage, filepath.Join(stage, "pair-model.json"), model); err != nil {
		return nil, err
	}
	if err := syncVLLMDirectory(stage); err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if st.stopPending > 0 || e.shuttingDown.Load() {
		return nil, context.Canceled
	}
	if err := os.Rename(stage, destination); err != nil {
		return nil, err
	}
	if err := syncVLLMDirectory(st.modelDir); err != nil {
		return nil, err
	}
	return json.Marshal(model)
}

func (e *Executor) pullVLLMModel(ctx context.Context, st *engineState, exactModel, operationID string) (result json.RawMessage, resultErr error) {
	repo, revision, err := parseExactVLLMHFModel(exactModel)
	if err != nil {
		return nil, err
	}
	python, err := e.vllmActivePython(st)
	if err != nil {
		return nil, err
	}
	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "pull", Stage: "resolving", Percent: 1, Message: exactModel, OperationID: operationID})
	meta, license, metadataDigest, err := e.fetchVLLMHFMetadata(ctx, repo, revision)
	if err != nil {
		return nil, err
	}
	plan, err := planVLLMSnapshot(meta.Siblings)
	if err != nil {
		return nil, err
	}
	if err := validateQwen38SnapshotPlan(exactModel, plan); err != nil {
		return nil, err
	}
	destination, err := vllmModelPath(st, exactModel)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(destination); err == nil {
		model, dir, verifyErr := verifyVLLMModel(ctx, st, exactModel)
		if verifyErr != nil {
			return nil, verifyErr
		}
		if model.MetadataSHA256 == "" {
			if err := verifyVLLMDownloadedFiles(ctx, dir, meta, model.Files); err != nil {
				return nil, errors.New("retained vLLM model lacks immutable metadata and no longer matches its exact source revision")
			}
			model.MetadataSHA256, model.License, model.Revision = metadataDigest, license, revision
			st.mu.Lock()
			if err = ctx.Err(); err == nil && st.stopPending == 0 && !e.shuttingDown.Load() {
				err = writeVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), model)
				if err == nil {
					err = syncVLLMDirectory(dir)
				}
			} else if err == nil {
				err = context.Canceled
			}
			st.mu.Unlock()
			if err != nil {
				return nil, err
			}
		}
		return json.Marshal(model)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	record := vllmDownloadRecord{Schema: 1, Model: exactModel, Revision: revision, License: license, MetadataSHA256: metadataDigest, SnapshotBytes: plan.Bytes, SnapshotFiles: plan.Files}
	stageName := vllmDownloadStageName(exactModel)
	filesystem, allocation := e.vllmStorageFilesystem(), e.vllmStorageAllocation()
	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "pull", Stage: "reviewing-storage", Percent: 2, Message: exactModel, OperationID: operationID})
	review, err := reviewVLLMDownloadStorage(st.modelDir, stageName, plan.Bytes, filesystem, allocation)
	if err != nil {
		return nil, err
	}
	if err := requireVLLMStorageReady(review); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(st.modelDir, 0o700); err != nil {
		return nil, err
	}
	stage, resumed, err := prepareVLLMDownloadStage(st.modelDir, record)
	if err != nil {
		return nil, err
	}
	stageIdentity, err := os.Lstat(stage)
	if err != nil {
		return nil, err
	}
	completedDownload := false
	defer func() {
		// A canceled or interrupted transfer is the resumable checkpoint. Once
		// download completed, any provenance failure makes the bytes untrusted and
		// the exact owned stage is removed instead of becoming a permanent hold.
		if resultErr != nil && completedDownload && !errors.Is(resultErr, context.Canceled) && !errors.Is(resultErr, context.DeadlineExceeded) {
			if current, err := os.Lstat(stage); err == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 && os.SameFile(stageIdentity, current) {
				_ = os.RemoveAll(stage)
				_ = syncVLLMDirectory(st.modelDir)
			}
		}
	}()
	review, err = reviewVLLMDownloadStorage(st.modelDir, stageName, plan.Bytes, filesystem, allocation)
	if err != nil || !review.Ready {
		return nil, errors.Join(err, requireVLLMStorageReady(review))
	}
	runtimeDir := filepath.Dir(filepath.Dir(python))
	if filepath.Base(runtimeDir) == "venv" {
		runtimeDir = filepath.Dir(runtimeDir)
	}
	env, cleanup, err := e.vllmChildEnv(st, runtimeDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env, err = vllmDownloadEnv(st, env, stage, exactModel)
	if err != nil {
		return nil, err
	}
	stageVerb := "downloading"
	if resumed {
		stageVerb = "resuming"
	}
	if err := e.runVLLMDownload(ctx, python, repo, revision, stage, env, meta.Siblings, plan.Bytes, operationID, stageVerb); err != nil {
		return nil, err
	}
	completedDownload = true
	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "pull", Stage: "verifying", Percent: 95, Message: exactModel, OperationID: operationID})
	files, digest, size, err := scanVLLMSnapshot(ctx, stage, exactModel)
	if err != nil {
		return nil, err
	}
	if uint64(size) != plan.Bytes || len(files) != plan.Files {
		return nil, errors.New("downloaded vLLM snapshot size or file count differs from pinned metadata")
	}
	if err := verifyVLLMDownloadedFiles(ctx, stage, meta, files); err != nil {
		return nil, err
	}
	model := vllmModel{ID: exactModel, Source: "huggingface", Revision: revision, License: license, MetadataSHA256: metadataDigest, Digest: digest, Bytes: size, Files: files}
	if isQwen38ProfileModel(exactModel) {
		if err := validateQwen38LocalProfile(model, stage); err != nil {
			return nil, err
		}
	}
	allocated, err := vllmStageAllocatedBytes(stage, review.Device, filesystem, allocation)
	ceiling, addErr := addVLLMStorageBytes(plan.Bytes, vllmDownloadCacheCeilingBytes)
	if err != nil || addErr != nil || allocated > ceiling {
		return nil, errors.Join(err, addErr, errors.New("vLLM stage exceeds the reviewed payload-plus-cache ceiling"))
	}
	available, device, err := filesystem(stage)
	if err != nil || device != review.Device || available < vllmPostOperationReserveBytes {
		return nil, errors.New("vLLM storage no longer retains the reviewed post-operation reserve")
	}
	for _, transient := range []string{filepath.Join(stage, ".cache"), filepath.Join(stage, vllmDownloadStageRecord)} {
		if err := validateVLLMOwnedPath(stage, transient); err != nil {
			return nil, err
		}
		if err := os.RemoveAll(transient); err != nil {
			return nil, err
		}
	}
	result, err = e.promoteVLLMDownloadedModel(ctx, st, stage, model)
	if err == nil {
		completedDownload = false
	}
	return result, err
}

func listDownloadedVLLMModels(ctx context.Context, st *engineState) (json.RawMessage, error) {
	ids, err := retainedVLLMModels(ctx, st)
	if err != nil {
		return nil, err
	}
	models := make([]vllmModel, 0, len(ids))
	for _, id := range ids {
		model, _, err := readVLLMModel(st, id)
		if err != nil {
			return nil, err
		}
		model.Files = nil
		models = append(models, model)
	}
	return json.Marshal(map[string]any{"data": models})
}

func (e *Executor) vllmAcquisitionAction(ctx context.Context, st *engineState, action string, params json.RawMessage) (json.RawMessage, error) {
	switch action {
	case "list_downloaded":
		return listDownloadedVLLMModels(ctx, st)
	case "cancel_pull":
		var request vllmPullParams
		if err := decodeActionParams(params, &request); err != nil {
			return nil, err
		}
		st.mu.Lock()
		accepted := st.pullCancel != nil && request.Model == st.pullModel && request.OperationID == st.pullOperationID && vllmPullOperationToken.MatchString(request.OperationID)
		if accepted {
			st.pullCancel()
		}
		st.mu.Unlock()
		return json.Marshal(map[string]bool{"accepted": accepted})
	case pullModelAction:
		if err := e.rejectVLLMGroupMutation("vllm", "pull models into"); err != nil {
			return nil, err
		}
		if err := adoptedVLLMMutationError(st, "pull models into"); err != nil {
			return nil, err
		}
		ctx, timeoutCancel := context.WithTimeout(ctx, vllmPullTimeout)
		defer timeoutCancel()
		st.opMu.Lock()
		defer st.opMu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := e.rejectVLLMGroupMutation("vllm", "pull models into"); err != nil {
			return nil, err
		}
		mutationContext, finishMutation, err := e.beginVLLMMutation(ctx, st)
		if err != nil {
			return nil, err
		}
		defer finishMutation()
		// Admission precedes request-dependent effects and validation. A live
		// diagnostic or serving-group owner therefore wins deterministically even
		// when an older caller submits malformed pull parameters.
		var request vllmPullParams
		if err := decodeActionParams(params, &request); err != nil {
			return nil, err
		}
		if request.Model == "" {
			request.Model = request.Name
		}
		if request.Name != "" && request.Name != request.Model {
			return nil, errors.New("vLLM pull name and model must identify the same exact snapshot")
		}
		if _, _, err := parseExactVLLMHFModel(request.Model); err != nil {
			return nil, err
		}
		if request.OperationID == "" {
			request.OperationID = newOpID()
		}
		if !vllmPullOperationToken.MatchString(request.OperationID) {
			return nil, errors.New("vLLM pull operationId must be 32 lowercase hexadecimal characters")
		}
		pullContext, cancel := context.WithCancelCause(mutationContext)
		userCancel := &vllmUserPullCancellation{operationID: request.OperationID}
		st.mu.Lock()
		st.pullCancel = func() { cancel(userCancel) }
		st.pullModel, st.pullOperationID = request.Model, request.OperationID
		st.mu.Unlock()
		defer func() {
			cancel(nil)
			st.mu.Lock()
			if st.pullOperationID == request.OperationID {
				st.pullCancel, st.pullModel, st.pullOperationID = nil, "", ""
			}
			st.mu.Unlock()
		}()
		result, err := e.pullVLLMModel(pullContext, st, request.Model, request.OperationID)
		if err != nil && context.Cause(pullContext) == userCancel {
			e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "pull", Stage: "canceled", Message: "partial download retained for resume", OperationID: request.OperationID})
			return json.Marshal(map[string]any{"cancelled": true, "operationId": request.OperationID, "model": request.Model, "resumable": true})
		}
		if err != nil {
			return nil, err
		}
		message := request.Model
		var receipt vllmModel
		if json.Unmarshal(result, &receipt) == nil && strings.TrimSpace(receipt.License) != "" {
			message += " (license: " + receipt.License + "; notices retained with model)"
		}
		e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "pull", Stage: "success", Percent: 100, Message: message, OperationID: request.OperationID})
		e.pokeLoaded()
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported managed vLLM acquisition action %q", action)
	}
}

func isVLLMAcquisitionAction(action string) bool {
	return action == pullModelAction || action == "cancel_pull" || action == "list_downloaded"
}
