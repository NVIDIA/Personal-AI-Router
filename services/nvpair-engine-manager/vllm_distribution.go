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
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	vllmDistributionSchema    = 1
	vllmDistributionChunkSize = int64(32 << 20)
	vllmReceiveStateFile      = ".nvpair-receive.json"
	vllmDistributionTimeout   = 5*time.Hour + 55*time.Minute
	vllmDistributionLockPoll  = 10 * time.Millisecond
)

type vllmDistributionRequest struct {
	OpID        string `json:"opId"`
	Engine      string `json:"engine"`
	OperationID string `json:"operationId"`
	SourceNode  string `json:"sourceNode"`
	Model       string `json:"model"`
	Cancel      bool   `json:"cancel,omitempty"`
	// SourceAddress is the source's fabric lane chosen by the fabric-owning
	// controller; empty means the copy uses the management network.
	SourceAddress string `json:"sourceAddress,omitempty"`
}

type vllmDistributionPlanRequest struct {
	OperationID string `json:"operationId"`
	Model       string `json:"model"`
}

type vllmDistributionPlan struct {
	Schema      int       `json:"schema"`
	OperationID string    `json:"operationId"`
	SourceNode  string    `json:"sourceNode"`
	ManifestSHA string    `json:"manifestSha256"`
	RecordSHA   string    `json:"recordSha256"`
	Model       vllmModel `json:"model"`
}

type vllmDistributionChunkRequest struct {
	OperationID string `json:"operationId"`
	Model       string `json:"model"`
	ManifestSHA string `json:"manifestSha256"`
	FileIndex   int    `json:"fileIndex"`
	Offset      int64  `json:"offset"`
	Length      int64  `json:"length"`
}

type vllmDistributionChunk struct {
	Offset int64
	Data   []byte
	SHA256 string
}

type vllmDistributionSource interface {
	exportPlan(context.Context, vllmDistributionPlanRequest) (vllmDistributionPlan, error)
	exportChunk(context.Context, vllmDistributionChunkRequest) (vllmDistributionChunk, error)
}

type vllmReceiveState struct {
	Schema       int    `json:"schema"`
	OperationID  string `json:"operationId"`
	SourceNode   string `json:"sourceNode"`
	Model        string `json:"model"`
	Revision     string `json:"revision"`
	ManifestSHA  string `json:"manifestSha256"`
	PlanSHA      string `json:"planSha256"`
	FileIndex    int    `json:"fileIndex"`
	Offset       int64  `json:"offset"`
	PrefixSHA256 string `json:"prefixSha256,omitempty"`
}

func validateVLLMDistributionRequest(req vllmDistributionRequest) error {
	_, _, modelErr := parseExactVLLMHFModel(req.Model)
	if req.Engine != "vllm" || !vllmPullOperationToken.MatchString(req.OperationID) ||
		!vllmPullOperationToken.MatchString(req.OpID) || !cableIdentifier(req.SourceNode, 128) || modelErr != nil {
		return errors.New("vLLM distribution requires exact engine, operation, source and immutable model identities")
	}
	if req.SourceAddress != "" {
		address := net.ParseIP(req.SourceAddress)
		if req.Cancel || address == nil || address.To4() == nil || !address.IsPrivate() || address.IsLoopback() || address.String() != req.SourceAddress {
			return errors.New("vLLM distribution source address must be one private IPv4 fabric lane")
		}
	}
	return nil
}

func vllmDistributionNetwork(req vllmDistributionRequest) string {
	if req.SourceAddress != "" {
		return "fabric"
	}
	return "management"
}

func validateVLLMDistributionPlan(plan vllmDistributionPlan, req vllmDistributionPlanRequest) error {
	_, revision, modelErr := parseExactVLLMHFModel(plan.Model.ID)
	if plan.Schema != vllmDistributionSchema || plan.OperationID != req.OperationID ||
		!vllmPullOperationToken.MatchString(plan.OperationID) || !cableIdentifier(plan.SourceNode, 128) ||
		plan.Model.ID != req.Model || modelErr != nil || plan.Model.Source != "huggingface" ||
		plan.Model.Revision != revision || strings.TrimSpace(plan.Model.License) == "" ||
		!validSHA256(plan.Model.MetadataSHA256) || !validSHA256(plan.ManifestSHA) ||
		!validSHA256(plan.RecordSHA) || plan.ManifestSHA != plan.Model.Digest ||
		len(plan.Model.Files) == 0 || len(plan.Model.Files) > vllmModelReceiptMaxFiles ||
		plan.Model.Bytes <= 0 || plan.Model.Bytes > maxVLLMModelBytes {
		return errors.New("vLLM distribution plan identity is invalid")
	}
	var total int64
	previous := ""
	for _, file := range plan.Model.Files {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))
		if file.Path == "" || clean != file.Path || filepath.IsAbs(file.Path) ||
			strings.HasPrefix(file.Path, "../") || strings.ContainsAny(file.Path, "\\:\x00\r\n") ||
			file.Size <= 0 || !validSHA256(file.SHA256) || file.Path <= previous ||
			total > maxVLLMModelBytes-file.Size {
			return errors.New("vLLM distribution file plan is invalid")
		}
		total += file.Size
		previous = file.Path
	}
	if total != plan.Model.Bytes {
		return errors.New("vLLM distribution byte plan is invalid")
	}
	filesRaw, _ := json.Marshal(plan.Model.Files)
	manifest := sha256.Sum256(filesRaw)
	if hex.EncodeToString(manifest[:]) != plan.ManifestSHA {
		return errors.New("vLLM distribution manifest digest is invalid")
	}
	modelRaw, _ := json.Marshal(plan.Model)
	record := sha256.Sum256(modelRaw)
	if hex.EncodeToString(record[:]) != plan.RecordSHA {
		return errors.New("vLLM distribution model record digest is invalid")
	}
	return nil
}

func (e *Executor) withVLLMExportLease(ctx context.Context, run func(*engineState) error) error {
	if err := e.rejectVLLMGroupMutation("vllm", "export models from"); err != nil {
		return err
	}
	st, err := e.state("vllm")
	if err != nil {
		return err
	}
	if err := lockVLLMDistributionOperation(ctx, st); err != nil {
		return err
	}
	defer st.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	st.mu.Lock()
	blocked := e.shuttingDown.Load() || st.stopPending > 0
	st.mu.Unlock()
	if blocked {
		return context.Canceled
	}
	return run(st)
}

func (e *Executor) exportVLLMDistributionPlan(ctx context.Context, req vllmDistributionPlanRequest) (plan vllmDistributionPlan, resultErr error) {
	if !vllmPullOperationToken.MatchString(req.OperationID) || !cableIdentifier(e.vllmNodeID, 128) {
		return plan, errors.New("invalid vLLM export-plan request")
	}
	if _, _, err := parseExactVLLMHFModel(req.Model); err != nil {
		return plan, errors.New("invalid vLLM export-plan model")
	}
	var model vllmModel
	var dir string
	err := e.withVLLMExportLease(ctx, func(st *engineState) error {
		var err error
		if model, dir, err = readVLLMModel(st, req.Model); err != nil {
			return err
		}
		return validateRetainedVLLMModelReceipt(ctx, st, dir, model)
	})
	if err != nil {
		return plan, err
	}
	// Hashing a retained model takes minutes. Holding the export lease that long
	// starves chunk requests from copies already in flight, so the lease is
	// retaken only to prove the hashed files are still the retained ones.
	hashed, err := hashVLLMModelFiles(ctx, dir, model)
	if err != nil {
		return plan, err
	}
	modelRaw, _ := json.Marshal(model)
	record := sha256.Sum256(modelRaw)
	err = e.withVLLMExportLease(ctx, func(st *engineState) error {
		current, currentDir, err := readVLLMModel(st, req.Model)
		if err != nil {
			return err
		}
		currentRaw, _ := json.Marshal(current)
		if currentDir != dir || !bytes.Equal(currentRaw, modelRaw) || !sameVLLMModelFiles(dir, model, hashed) {
			return errors.New("vLLM retained model changed during export verification")
		}
		if isQwen38ProfileModel(model.ID) {
			if err := validateQwen38LocalProfile(model, dir); err != nil {
				return err
			}
		}
		plan = vllmDistributionPlan{Schema: vllmDistributionSchema, OperationID: req.OperationID, SourceNode: e.vllmNodeID, ManifestSHA: model.Digest, RecordSHA: hex.EncodeToString(record[:]), Model: model}
		return validateVLLMDistributionPlan(plan, req)
	})
	return plan, err
}

func (e *Executor) exportVLLMDistributionChunk(ctx context.Context, req vllmDistributionChunkRequest) (chunk vllmDistributionChunk, resultErr error) {
	if !vllmPullOperationToken.MatchString(req.OperationID) || !validSHA256(req.ManifestSHA) ||
		req.FileIndex < 0 || req.Offset < 0 || req.Length <= 0 || req.Length > vllmDistributionChunkSize {
		return chunk, errors.New("invalid vLLM export-chunk request")
	}
	if _, _, err := parseExactVLLMHFModel(req.Model); err != nil {
		return chunk, errors.New("invalid vLLM export-chunk model")
	}
	err := e.withVLLMExportLease(ctx, func(st *engineState) error {
		model, dir, err := readVLLMModel(st, req.Model)
		if err != nil {
			return err
		}
		if model.Digest != req.ManifestSHA || req.FileIndex >= len(model.Files) {
			return errors.New("vLLM export manifest changed")
		}
		entry := model.Files[req.FileIndex]
		if req.Offset > entry.Size || req.Length > entry.Size-req.Offset {
			return errors.New("vLLM export range exceeds its bound file")
		}
		path := filepath.Join(dir, filepath.FromSlash(entry.Path))
		if validateVLLMOwnedPath(dir, path) != nil {
			return errors.New("vLLM export path escaped retained model")
		}
		opened, err := os.Open(path)
		if err != nil {
			return err
		}
		defer opened.Close()
		info, err := opened.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
			return errors.New("vLLM export file changed")
		}
		data := make([]byte, req.Length)
		if _, err = io.ReadFull(io.NewSectionReader(opened, req.Offset, req.Length), data); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		chunk = vllmDistributionChunk{Offset: req.Offset, Data: data, SHA256: hex.EncodeToString(sum[:])}
		return nil
	})
	return chunk, err
}

func vllmReceiveStage(st *engineState, operationID string) (string, error) {
	if !vllmPullOperationToken.MatchString(operationID) {
		return "", errors.New("invalid vLLM receive operation")
	}
	stage := filepath.Join(st.modelDir, ".receive-"+operationID)
	if validateVLLMOwnedPath(st.modelDir, stage) != nil {
		return "", errors.New("invalid vLLM receive stage")
	}
	return stage, nil
}

var vllmReceiveSyncDirectory = syncVLLMDirectory

func writeVLLMReceiveState(stage string, state vllmReceiveState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := filepath.Join(stage, vllmReceiveStateFile)
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(raw, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return vllmReceiveSyncDirectory(stage)
}

func readVLLMReceiveState(stage string) (vllmReceiveState, error) {
	return readVLLMReceiveStatePath(filepath.Join(stage, vllmReceiveStateFile))
}

func readVLLMReceiveStatePath(path string) (vllmReceiveState, error) {
	var state vllmReceiveState
	file, err := os.Open(path)
	if err != nil {
		return state, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, (4<<20)+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil || decoder.Decode(new(any)) != io.EOF {
		return state, errors.New("invalid vLLM receive checkpoint")
	}
	return state, nil
}

func reconcileVLLMReceiveCheckpoint(stage string, req vllmDistributionRequest, plan vllmDistributionPlan, initial vllmReceiveState) (vllmReceiveState, error) {
	path := filepath.Join(stage, vllmReceiveStateFile)
	tmp := path + ".tmp"
	current, currentErr := readVLLMReceiveStatePath(path)
	temporary, temporaryErr := readVLLMReceiveStatePath(tmp)
	currentExists, temporaryExists := currentErr == nil, temporaryErr == nil
	if currentErr != nil && !os.IsNotExist(currentErr) {
		return initial, errors.New("existing vLLM receive checkpoint is invalid")
	}
	if temporaryErr != nil && !os.IsNotExist(temporaryErr) {
		return initial, errors.New("temporary vLLM receive checkpoint is foreign or invalid")
	}
	if currentExists && validateVLLMReceiveState(current, req, plan) != nil {
		return initial, errors.New("existing vLLM receive checkpoint belongs to another operation")
	}
	if temporaryExists {
		if validateVLLMReceiveState(temporary, req, plan) != nil {
			return initial, errors.New("temporary vLLM receive checkpoint belongs to another operation")
		}
		if currentExists {
			currentBytes := completedVLLMReceiveBytes(plan, current)
			temporaryBytes := completedVLLMReceiveBytes(plan, temporary)
			if temporaryBytes < currentBytes || temporaryBytes-currentBytes > vllmDistributionChunkSize {
				return initial, errors.New("temporary vLLM receive checkpoint is not the next durable state")
			}
		}
		if err := os.Rename(tmp, path); err != nil {
			return initial, err
		}
		if err := vllmReceiveSyncDirectory(stage); err != nil {
			return initial, err
		}
		return temporary, nil
	}
	if currentExists {
		return current, nil
	}
	entries, err := os.ReadDir(stage)
	if err != nil || len(entries) != 0 {
		return initial, errors.New("vLLM receive stage lacks an owned checkpoint")
	}
	if err := writeVLLMReceiveState(stage, initial); err != nil {
		return initial, err
	}
	return initial, nil
}

func vllmDistributionPlanSHA(plan vllmDistributionPlan) string {
	raw, _ := json.Marshal(plan)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateVLLMReceiveState(state vllmReceiveState, req vllmDistributionRequest, plan vllmDistributionPlan) error {
	if state.Schema != vllmDistributionSchema || state.OperationID != req.OperationID ||
		state.SourceNode != req.SourceNode || state.Model != req.Model || state.Revision != plan.Model.Revision ||
		state.ManifestSHA != plan.ManifestSHA || state.PlanSHA != vllmDistributionPlanSHA(plan) ||
		state.FileIndex < 0 || state.FileIndex > len(plan.Model.Files) || state.Offset < 0 ||
		(state.FileIndex == len(plan.Model.Files) && state.Offset != 0) ||
		(state.FileIndex < len(plan.Model.Files) && state.Offset > plan.Model.Files[state.FileIndex].Size) ||
		(state.Offset > 0 && !validSHA256(state.PrefixSHA256)) || (state.Offset == 0 && state.PrefixSHA256 != "") {
		return errors.New("vLLM receive checkpoint does not match this operation")
	}
	return nil
}

func ensureVLLMReceiveParent(stage, target string) error {
	parent := filepath.Dir(target)
	rel, err := filepath.Rel(stage, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("vLLM receive parent escaped its stage")
	}
	current := stage
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("vLLM receive parent is redirected")
		}
	}
	return nil
}

func hashVLLMReceivePrefix(ctx context.Context, path string, expected int64) (hash.Hash, error) {
	h := sha256.New()
	before, lstatErr := os.Lstat(path)
	if expected == 0 && os.IsNotExist(lstatErr) {
		return h, nil
	}
	if lstatErr != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != expected {
		return nil, errors.New("vLLM receive partial ownership changed")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected || !os.SameFile(before, info) {
		return nil, errors.New("vLLM receive partial size changed")
	}
	if _, err = io.Copy(h, vllmContextReader{ctx: ctx, reader: io.LimitReader(file, expected)}); err != nil {
		return nil, err
	}
	return h, nil
}

func reconcileVLLMReceivePartial(path string, committed, maximum int64) error {
	before, err := os.Lstat(path)
	if os.IsNotExist(err) && committed == 0 {
		return nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 ||
		before.Size() < committed || before.Size() > maximum || before.Size()-committed > vllmDistributionChunkSize {
		return errors.New("vLLM receive partial cannot be reconciled to its checkpoint")
	}
	if before.Size() == committed {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, info) {
		file.Close()
		return errors.New("vLLM receive partial changed during reconciliation")
	}
	return errors.Join(file.Truncate(committed), file.Sync(), file.Close())
}

func verifyVLLMReceiveFile(ctx context.Context, path string, entry vllmModelFile) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
		return errors.New("vLLM received file identity changed")
	}
	h := sha256.New()
	if _, err = io.Copy(h, vllmContextReader{ctx: ctx, reader: file}); err != nil || hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
		return errors.New("vLLM received file digest changed")
	}
	return nil
}

func verifyVLLMReceiveCompleted(ctx context.Context, stage string, plan vllmDistributionPlan, state vllmReceiveState) error {
	for index := 0; index < state.FileIndex; index++ {
		entry := plan.Model.Files[index]
		if err := verifyVLLMReceiveFile(ctx, filepath.Join(stage, filepath.FromSlash(entry.Path)), entry); err != nil {
			return errors.New("completed vLLM receive file changed")
		}
	}
	return nil
}

func (e *Executor) receiveVLLMDistribution(ctx context.Context, source vllmDistributionSource, req vllmDistributionRequest) (result json.RawMessage, resultErr error) {
	if err := validateVLLMDistributionRequest(req); err != nil || req.Cancel {
		if err == nil {
			err = errors.New("cancel is not a receive operation")
		}
		return nil, err
	}
	if err := e.rejectVLLMGroupMutation("vllm", "distribute models into"); err != nil {
		return nil, err
	}
	ctx, timeoutCancel := context.WithTimeout(ctx, vllmDistributionTimeout)
	defer timeoutCancel()
	st, err := e.state("vllm")
	if err != nil {
		return nil, err
	}
	if err := adoptedVLLMMutationError(st, "distribute models into"); err != nil {
		return nil, err
	}
	queuedCtx, distributionCancel := context.WithCancel(ctx)
	distributionDone := make(chan struct{})
	st.mu.Lock()
	if st.distributionCancel != nil {
		st.mu.Unlock()
		distributionCancel()
		return nil, errors.New("another vLLM distribution operation is active")
	}
	st.distributionOperationID, st.distributionSourceNode = req.OperationID, req.SourceNode
	st.distributionModel, st.distributionCancel, st.distributionDone = req.Model, distributionCancel, distributionDone
	st.mu.Unlock()
	defer func() {
		distributionCancel()
		st.mu.Lock()
		if st.distributionOperationID == req.OperationID && st.distributionDone == distributionDone {
			st.distributionOperationID, st.distributionSourceNode, st.distributionModel = "", "", ""
			st.distributionCancel, st.distributionDone = nil, nil
			close(distributionDone)
		}
		st.mu.Unlock()
	}()
	if err := lockVLLMDistributionOperation(queuedCtx, st); err != nil {
		return nil, err
	}
	defer st.opMu.Unlock()
	if err := e.rejectVLLMGroupMutation("vllm", "distribute models into"); err != nil {
		return nil, err
	}
	receiveCtx, release, err := e.beginVLLMMutation(queuedCtx, st)
	if err != nil {
		return nil, err
	}
	defer release()

	destination, err := vllmModelPath(st, req.Model)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Lstat(destination); statErr == nil {
		existing, _, verifyErr := verifyVLLMModel(receiveCtx, st, req.Model)
		if verifyErr != nil {
			return nil, verifyErr
		}
		checkpoint := filepath.Join(destination, vllmReceiveStateFile)
		if _, checkpointErr := os.Lstat(checkpoint); checkpointErr == nil {
			state, readErr := readVLLMReceiveState(destination)
			if readErr != nil || state.OperationID != req.OperationID || state.SourceNode != req.SourceNode ||
				state.Model != req.Model || state.FileIndex != len(existing.Files) || state.Offset != 0 {
				return nil, errors.New("promoted vLLM receive checkpoint ownership changed")
			}
			if err := vllmReceiveSyncDirectory(st.modelDir); err != nil {
				return nil, errors.New("promoted vLLM model parent sync retry failed")
			}
			if err := os.Remove(checkpoint); err != nil {
				return nil, err
			}
			if err := vllmReceiveSyncDirectory(destination); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(checkpointErr) {
			return nil, checkpointErr
		}
		return json.Marshal(existing)
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}

	plan, err := source.exportPlan(receiveCtx, vllmDistributionPlanRequest{OperationID: req.OperationID, Model: req.Model})
	if err != nil {
		return nil, err
	}
	if plan.SourceNode != req.SourceNode {
		return nil, errors.New("vLLM distribution source identity changed")
	}
	if err = validateVLLMDistributionPlan(plan, vllmDistributionPlanRequest{OperationID: req.OperationID, Model: req.Model}); err != nil {
		return nil, err
	}
	if err = validateQwen38SnapshotPlan(plan.Model.ID, vllmSnapshotPlan{Files: len(plan.Model.Files), Bytes: uint64(plan.Model.Bytes)}); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(st.modelDir, 0o700); err != nil {
		return nil, err
	}
	stage, err := vllmReceiveStage(st, req.OperationID)
	if err != nil {
		return nil, err
	}
	stageName := filepath.Base(stage)
	state := vllmReceiveState{Schema: vllmDistributionSchema, OperationID: req.OperationID, SourceNode: req.SourceNode, Model: req.Model, Revision: plan.Model.Revision, ManifestSHA: plan.ManifestSHA, PlanSHA: vllmDistributionPlanSHA(plan)}
	if info, statErr := os.Lstat(stage); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("vLLM receive stage ownership changed")
		}
		state, err = reconcileVLLMReceiveCheckpoint(stage, req, plan, state)
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	} else {
		review, err := reviewVLLMDownloadStorage(st.modelDir, stageName, uint64(plan.Model.Bytes), e.vllmStorageFilesystem(), e.vllmStorageAllocation())
		if err != nil || requireVLLMStorageReady(review) != nil {
			if err != nil {
				return nil, err
			}
			return nil, requireVLLMStorageReady(review)
		}
		if err = os.Mkdir(stage, 0o700); err != nil {
			return nil, err
		}
		if err = vllmReceiveSyncDirectory(st.modelDir); err != nil {
			return nil, err
		}
		if err = writeVLLMReceiveState(stage, state); err != nil {
			return nil, err
		}
	}
	if err = validateVLLMReceiveState(state, req, plan); err != nil {
		return nil, err
	}
	if err = verifyVLLMReceiveCompleted(receiveCtx, stage, plan, state); err != nil {
		return nil, err
	}
	filesystem, allocation := e.vllmStorageFilesystem(), e.vllmStorageAllocation()
	storage, err := reviewVLLMDownloadStorage(st.modelDir, stageName, uint64(plan.Model.Bytes), filesystem, allocation)
	if err != nil || requireVLLMStorageReady(storage) != nil {
		if err != nil {
			return nil, err
		}
		return nil, requireVLLMStorageReady(storage)
	}
	startPercent := int(completedVLLMReceiveBytes(plan, state) * 94 / plan.Model.Bytes)
	network := vllmDistributionNetwork(req)
	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "distribute", Stage: "receiving", Percent: startPercent, Message: req.Model, OperationID: req.OperationID, Network: network})
	lastPercent := startPercent
	for state.FileIndex < len(plan.Model.Files) {
		entry := plan.Model.Files[state.FileIndex]
		finalPath := filepath.Join(stage, filepath.FromSlash(entry.Path))
		if validateVLLMOwnedPath(stage, finalPath) != nil || ensureVLLMReceiveParent(stage, finalPath) != nil {
			return nil, errors.New("vLLM receive path is invalid")
		}
		partial := finalPath + ".partial"
		if state.Offset == entry.Size {
			if _, partialErr := os.Lstat(partial); os.IsNotExist(partialErr) {
				if err := verifyVLLMReceiveFile(receiveCtx, finalPath, entry); err != nil {
					return nil, err
				}
				state.FileIndex++
				state.Offset, state.PrefixSHA256 = 0, ""
				if err = writeVLLMReceiveState(stage, state); err != nil {
					return nil, err
				}
				continue
			}
		}
		if err := reconcileVLLMReceivePartial(partial, state.Offset, entry.Size); err != nil {
			return nil, err
		}
		h, err := hashVLLMReceivePrefix(receiveCtx, partial, state.Offset)
		if err != nil && !(state.Offset == 0 && os.IsNotExist(err)) {
			return nil, err
		}
		if state.Offset > 0 && hex.EncodeToString(h.Sum(nil)) != state.PrefixSHA256 {
			return nil, errors.New("vLLM receive partial prefix changed")
		}
		for state.Offset < entry.Size {
			length := min(vllmDistributionChunkSize, entry.Size-state.Offset)
			chunk, err := source.exportChunk(receiveCtx, vllmDistributionChunkRequest{OperationID: req.OperationID, Model: req.Model, ManifestSHA: plan.ManifestSHA, FileIndex: state.FileIndex, Offset: state.Offset, Length: length})
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(chunk.Data)
			if chunk.Offset != state.Offset || int64(len(chunk.Data)) != length || chunk.SHA256 != hex.EncodeToString(sum[:]) {
				return nil, errors.New("vLLM distribution chunk identity changed")
			}
			before, beforeErr := os.Lstat(partial)
			flags := os.O_WRONLY
			if os.IsNotExist(beforeErr) {
				if state.Offset != 0 {
					return nil, errors.New("vLLM receive partial disappeared")
				}
				flags |= os.O_CREATE | os.O_EXCL
			} else if beforeErr != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != state.Offset {
				return nil, errors.New("vLLM receive partial ownership changed")
			}
			file, err := os.OpenFile(partial, flags, 0o600)
			if err != nil {
				return nil, err
			}
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() != state.Offset || (beforeErr == nil && !os.SameFile(before, info)) {
				file.Close()
				return nil, errors.New("vLLM receive partial ownership changed")
			}
			_, seekErr := file.Seek(state.Offset, io.SeekStart)
			written, writeErr := file.Write(chunk.Data)
			if writeErr == nil && written != len(chunk.Data) {
				writeErr = io.ErrShortWrite
			}
			syncErr, closeErr := file.Sync(), file.Close()
			if err = errors.Join(seekErr, writeErr, syncErr, closeErr); err != nil {
				return nil, err
			}
			_, _ = h.Write(chunk.Data)
			state.Offset += length
			state.PrefixSHA256 = hex.EncodeToString(h.Sum(nil))
			if err = writeVLLMReceiveState(stage, state); err != nil {
				return nil, err
			}
			percent := int(completedVLLMReceiveBytes(plan, state) * 94 / plan.Model.Bytes)
			if percent != lastPercent {
				lastPercent = percent
				e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "distribute", Stage: "receiving", Percent: percent, Message: req.Model, OperationID: req.OperationID, Network: network})
			}
		}
		if state.PrefixSHA256 != entry.SHA256 {
			return nil, errors.New("vLLM received file digest mismatch")
		}
		if err = os.Rename(partial, finalPath); err != nil {
			return nil, err
		}
		if err = vllmReceiveSyncDirectory(filepath.Dir(finalPath)); err != nil {
			return nil, err
		}
		state.FileIndex++
		state.Offset, state.PrefixSHA256 = 0, ""
		if err = writeVLLMReceiveState(stage, state); err != nil {
			return nil, err
		}
	}

	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "distribute", Stage: "verifying", Percent: 95, Message: req.Model, OperationID: req.OperationID, Network: network})
	files, digest, size, err := scanVLLMSnapshot(receiveCtx, stage, plan.Model.ID)
	if err != nil || digest != plan.ManifestSHA || size != plan.Model.Bytes || !equalVLLMModelFiles(files, plan.Model.Files) {
		return nil, errors.New("received vLLM snapshot differs from export plan")
	}
	if isQwen38ProfileModel(plan.Model.ID) {
		if err = validateQwen38LocalProfile(plan.Model, stage); err != nil {
			return nil, err
		}
	}
	allocated, err := vllmStageAllocatedBytes(stage, storage.Device, filesystem, allocation)
	ceiling, addErr := addVLLMStorageBytes(uint64(plan.Model.Bytes), vllmDownloadCacheCeilingBytes)
	if err != nil || addErr != nil || allocated > ceiling {
		return nil, errors.Join(err, addErr, errors.New("vLLM receive stage exceeds its reviewed payload ceiling"))
	}
	available, device, err := filesystem(stage)
	if err != nil || device != storage.Device || available < vllmPostOperationReserveBytes {
		return nil, errors.New("vLLM receive storage no longer retains the reviewed reserve")
	}
	result, err = e.promoteVLLMDownloadedModel(receiveCtx, st, stage, plan.Model)
	if err != nil {
		return nil, err
	}
	destination, _ = vllmModelPath(st, plan.Model.ID)
	if err = os.Remove(filepath.Join(destination, vllmReceiveStateFile)); err != nil {
		return nil, errors.New("promoted vLLM model retained its receive checkpoint")
	}
	if err = vllmReceiveSyncDirectory(destination); err != nil {
		return nil, err
	}
	e.emitPullProgress(ProgressEvent{Engine: "vllm", Op: "distribute", Stage: "complete", Percent: 100, Message: req.Model + " (license: " + plan.Model.License + "; notices retained with model)", OperationID: req.OperationID, Network: network})
	e.pokeLoaded()
	return result, nil
}

// Distribution cancellation must not leave a disconnected control handler
// queued behind another vLLM lifecycle operation. The engine mutex cannot be
// selected on, so poll TryLock only until this request's context ends.
func lockVLLMDistributionOperation(ctx context.Context, st *engineState) error {
	ticker := time.NewTicker(vllmDistributionLockPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if st.opMu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func completedVLLMReceiveBytes(plan vllmDistributionPlan, state vllmReceiveState) int64 {
	var total int64
	for index := 0; index < state.FileIndex && index < len(plan.Model.Files); index++ {
		total += plan.Model.Files[index].Size
	}
	return total + state.Offset
}

func equalVLLMModelFiles(left, right []vllmModelFile) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (e *Executor) cancelVLLMDistribution(ctx context.Context, req vllmDistributionRequest) (json.RawMessage, error) {
	if err := validateVLLMDistributionRequest(req); err != nil || !req.Cancel {
		return nil, errors.New("invalid vLLM distribution cancellation")
	}
	st, err := e.state("vllm")
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	active := st.distributionOperationID == req.OperationID && st.distributionCancel != nil
	if active && (st.distributionSourceNode != req.SourceNode || st.distributionModel != req.Model) {
		st.mu.Unlock()
		return nil, errors.New("active vLLM distribution cancellation ownership changed")
	}
	done := st.distributionDone
	if active {
		st.distributionCancel()
	}
	st.mu.Unlock()
	if active && done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := lockVLLMDistributionOperation(ctx, st); err != nil {
		return nil, err
	}
	defer st.opMu.Unlock()
	stage, err := vllmReceiveStage(st, req.OperationID)
	if err != nil {
		return nil, err
	}
	state, err := readVLLMReceiveState(stage)
	if os.IsNotExist(err) {
		return json.Marshal(map[string]any{"cancelled": false, "absent": true, "operationId": req.OperationID})
	}
	if err != nil || state.OperationID != req.OperationID || state.SourceNode != req.SourceNode || state.Model != req.Model {
		return nil, errors.New("vLLM receive cancellation ownership changed")
	}
	if err = validateVLLMReceiveTree(stage); err != nil {
		return nil, err
	}
	if err = os.RemoveAll(stage); err != nil {
		return nil, err
	}
	if err = vllmReceiveSyncDirectory(st.modelDir); err != nil {
		return nil, errors.New("vLLM distribution cancellation parent sync failed")
	}
	return json.Marshal(map[string]any{"cancelled": true, "operationId": req.OperationID})
}

func validateVLLMReceiveTree(stage string) error {
	root, err := os.Lstat(stage)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return errors.New("vLLM receive stage is not an owned directory")
	}
	return filepath.WalkDir(stage, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("vLLM receive stage contains a redirected path")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("vLLM receive stage contains a nonregular file")
		}
		return nil
	})
}
