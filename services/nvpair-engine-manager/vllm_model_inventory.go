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
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	vllmRetainedModelLimit        = 128
	vllmModelReceiptMaxBytes      = 1 << 20
	vllmModelReceiptMaxFiles      = 20000
	vllmModelReceiptMaxPathBytes  = 4096
	vllmRetainedReceiveStageLimit = 8
)

// retainedVLLMModels returns exact model IDs from PAIR-owned model receipts.
// It deliberately verifies only bounded receipt and filesystem metadata here;
// SelectVLLMModel hashes every retained file before a start can have effects.
func retainedVLLMModels(ctx context.Context, st *engineState) ([]string, error) {
	info, err := os.Lstat(st.modelDir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("managed vLLM model root is unavailable or redirected")
	}
	if err := validateVLLMOwnedPath(st.modelDir, st.modelDir); err != nil {
		return nil, err
	}

	root, err := os.Open(st.modelDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The selected link, three exact Hugging Face runtime/cache directories and
	// at most one exact resumable acquisition stage are the only non-model
	// entries admitted at this level.
	entries, readErr := root.ReadDir(vllmRetainedModelLimit + vllmRetainedReceiveStageLimit + 6)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if len(entries) > vllmRetainedModelLimit+vllmRetainedReceiveStageLimit+5 {
		return nil, errors.New("managed vLLM model inventory exceeds its entry bound")
	}

	modelDirs := make([]string, 0, len(entries))
	selected, downloadStage, receiveStages := false, false, 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		path := filepath.Join(st.modelDir, name)
		entryInfo, err := os.Lstat(path)
		if err != nil {
			return nil, errors.New("managed vLLM model inventory changed during inspection")
		}
		switch name {
		case "selected":
			if selected || entryInfo.Mode()&os.ModeSymlink == 0 {
				return nil, errors.New("managed vLLM selection entry is invalid")
			}
			selected = true
			continue
		case ".hf-home", ".hf-xet", "huggingface":
			if !entryInfo.IsDir() || entryInfo.Mode()&os.ModeSymlink != 0 || validateVLLMOwnedPath(st.modelDir, path) != nil {
				return nil, errors.New("managed vLLM auxiliary directory is invalid or redirected")
			}
			continue
		}
		if strings.HasPrefix(name, ".download-") {
			if downloadStage || !entryInfo.IsDir() || entryInfo.Mode()&os.ModeSymlink != 0 || validateVLLMOwnedPath(st.modelDir, path) != nil {
				return nil, errors.New("managed vLLM resumable stage is invalid or duplicated")
			}
			stage, err := readVLLMDownloadRecord(path)
			if err != nil {
				return nil, errors.New("managed vLLM resumable stage receipt is invalid")
			}
			_, revision, modelErr := parseExactVLLMHFModel(stage.Model)
			if stage.Schema != 1 || stage.Model == "" || name != vllmDownloadStageName(stage.Model) ||
				modelErr != nil || revision != stage.Revision || strings.TrimSpace(stage.License) == "" || !validSHA256(stage.MetadataSHA256) ||
				stage.SnapshotBytes == 0 || stage.SnapshotBytes > uint64(maxVLLMModelBytes) || stage.SnapshotFiles <= 0 || stage.SnapshotFiles > vllmModelReceiptMaxFiles {
				return nil, errors.New("managed vLLM resumable stage receipt is invalid")
			}
			downloadStage = true
			continue
		}
		if strings.HasPrefix(name, ".receive-") {
			operationID := strings.TrimPrefix(name, ".receive-")
			if receiveStages >= vllmRetainedReceiveStageLimit || !vllmPullOperationToken.MatchString(operationID) ||
				!entryInfo.IsDir() || entryInfo.Mode()&os.ModeSymlink != 0 || validateVLLMOwnedPath(st.modelDir, path) != nil {
				return nil, errors.New("managed vLLM receive stage is invalid or exceeds its bound")
			}
			state, err := readVLLMReceiveState(path)
			if err != nil || state.OperationID != operationID || !cableIdentifier(state.SourceNode, 128) {
				return nil, errors.New("managed vLLM receive checkpoint is invalid")
			}
			if _, _, err := parseExactVLLMHFModel(state.Model); err != nil {
				return nil, errors.New("managed vLLM receive model identity is invalid")
			}
			receiveStages++
			continue
		}
		if !vllmGroupDigest.MatchString(name) || !entryInfo.IsDir() || entryInfo.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("managed vLLM model inventory contains an unknown entry")
		}
		modelDirs = append(modelDirs, path)
	}
	if len(modelDirs) > vllmRetainedModelLimit {
		return nil, errors.New("managed vLLM model inventory exceeds its model bound")
	}

	models := make([]string, 0, len(modelDirs))
	accepted := make(map[string]bool, len(modelDirs))
	for _, path := range modelDirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A digest-shaped cache/staging directory has no authority by name alone.
		// Only a regular PAIR receipt turns it into a retained catalog entry.
		if _, err := os.Lstat(filepath.Join(path, "pair-model.json")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, errors.New("managed vLLM model inventory changed during receipt inspection")
		}
		model, err := readRetainedVLLMModelReceipt(path)
		if err != nil {
			return nil, err
		}
		if err := validateRetainedVLLMModelReceipt(ctx, st, path, model); err != nil {
			return nil, err
		}
		if accepted[model.ID] {
			return nil, errors.New("managed vLLM model inventory contains a duplicate receipt")
		}
		accepted[model.ID] = true
		models = append(models, model.ID)
	}
	if selected {
		id, err := selectedVLLMModel(st)
		if err != nil || !accepted[id] {
			return nil, errors.New("managed vLLM selection does not name an accepted retained model")
		}
	}
	sort.Strings(models)
	return models, nil
}

func readRetainedVLLMModelReceipt(dir string) (vllmModel, error) {
	var model vllmModel
	var raw json.RawMessage
	if err := readVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), vllmModelReceiptMaxBytes, &raw); err != nil {
		return model, errors.New("managed vLLM model receipt is unavailable or malformed")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&model); err != nil {
		return model, errors.New("managed vLLM model receipt is unavailable or malformed")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return model, errors.New("managed vLLM model receipt must contain exactly one value")
	}
	return model, nil
}

func validateRetainedVLLMModelReceipt(ctx context.Context, st *engineState, dir string, model vllmModel) error {
	if len(model.ID) > 512 || !remoteModelID(model.ID) {
		return errors.New("managed vLLM model receipt has an invalid exact id")
	}
	want, err := vllmModelPath(st, model.ID)
	if err != nil || filepath.Clean(want) != filepath.Clean(dir) {
		return errors.New("managed vLLM model receipt does not match its digest directory")
	}
	if len(model.Files) == 0 || len(model.Files) > vllmModelReceiptMaxFiles || model.Bytes < 0 || !validSHA256(model.Digest) {
		return errors.New("managed vLLM model receipt exceeds its metadata bounds")
	}
	if model.Source == "huggingface" {
		repo, revision, err := parseExactVLLMHFModel(model.ID)
		if err != nil || repo+"@"+revision != model.ID || model.Revision != revision || strings.TrimSpace(model.License) == "" ||
			(model.MetadataSHA256 != "" && !validSHA256(model.MetadataSHA256)) {
			return errors.New("managed vLLM Hugging Face provenance is invalid")
		}
	}
	manifest, err := json.Marshal(model.Files)
	if err != nil {
		return errors.New("managed vLLM model receipt file manifest is malformed")
	}
	digest := sha256.Sum256(manifest)
	if model.Digest != hex.EncodeToString(digest[:]) {
		return errors.New("managed vLLM model receipt file manifest digest changed")
	}

	seen := make(map[string]bool, len(model.Files))
	var total int64
	for _, file := range model.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(file.Path))
		if file.Path == "" || len(file.Path) > vllmModelReceiptMaxPathBytes || filepath.IsAbs(clean) || clean == "." || filepath.ToSlash(clean) != file.Path || seen[file.Path] || file.Size < 0 || !validSHA256(file.SHA256) || total > math.MaxInt64-file.Size {
			return errors.New("managed vLLM model receipt contains an invalid file entry")
		}
		seen[file.Path] = true
		total += file.Size
		path := filepath.Join(dir, clean)
		if err := validateVLLMOwnedPath(dir, path); err != nil {
			return errors.New("managed vLLM model receipt contains a redirected file")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			return errors.New("managed vLLM retained model file metadata changed")
		}
	}
	if total != model.Bytes {
		return errors.New("managed vLLM model receipt byte total does not match")
	}
	return nil
}
