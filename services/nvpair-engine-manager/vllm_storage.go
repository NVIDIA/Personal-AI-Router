// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	vllmDownloadCacheCeilingBytes uint64 = 512 << 20
	vllmPostOperationReserveBytes uint64 = 32 << 30
)

type vllmFilesystemReader func(string) (uint64, string, error)
type vllmAllocationReader func(string) (uint64, uint64, error)

func (e *Executor) vllmStorageFilesystem() vllmFilesystemReader {
	if e != nil && e.vllmStorageFS != nil {
		return e.vllmStorageFS
	}
	return platformVLLMFilesystem
}

func (e *Executor) vllmStorageAllocation() vllmAllocationReader {
	if e != nil && e.vllmStorageAlloc != nil {
		return e.vllmStorageAlloc
	}
	return platformVLLMAllocatedBytes
}

type vllmStorageObservation struct {
	Root             string
	Device           string
	AvailableBytes   uint64
	UnresolvedStages int
}

type vllmStorageReview struct {
	Root                    string `json:"root"`
	Device                  string `json:"device"`
	SnapshotBytes           uint64 `json:"snapshotBytes"`
	CacheCeilingBytes       uint64 `json:"cacheCeilingBytes"`
	HeadroomBytes           uint64 `json:"headroomBytes"`
	StagedBytes             uint64 `json:"stagedBytes"`
	RequiredAvailableBytes  uint64 `json:"requiredAvailableBytes"`
	AvailableBytes          uint64 `json:"availableBytes"`
	ProjectedRemainingBytes uint64 `json:"projectedRemainingBytes"`
	UnresolvedStages        int    `json:"unresolvedStages"`
	Ready                   bool   `json:"ready"`
	Reason                  string `json:"reason"`
}

type vllmSnapshotPlan struct {
	Files int
	Bytes uint64
}

func vllmHFSiblingBytes(sibling vllmHFSibling) (uint64, error) {
	size := sibling.Size
	if sibling.LFS != nil && sibling.LFS.Size > 0 {
		if size > 0 && size != sibling.LFS.Size {
			return 0, fmt.Errorf("vLLM model source size metadata disagrees")
		}
		size = sibling.LFS.Size
	}
	if size <= 0 {
		return 0, fmt.Errorf("vLLM model source lacks a selected file size")
	}
	return uint64(size), nil
}

func planVLLMSnapshot(siblings []vllmHFSibling) (vllmSnapshotPlan, error) {
	var plan vllmSnapshotPlan
	seen := make(map[string]bool)
	for _, sibling := range siblings {
		if !allowedVLLMAsset(sibling.Path) {
			continue
		}
		if sibling.Path == "" || sibling.Path == ".." || strings.HasPrefix(sibling.Path, "../") || path.IsAbs(sibling.Path) || path.Clean(sibling.Path) != sibling.Path || strings.ContainsAny(sibling.Path, "\x00\r\n") || seen[sibling.Path] {
			return plan, fmt.Errorf("vLLM model source has an invalid or duplicate selected path")
		}
		seen[sibling.Path] = true
		size, err := vllmHFSiblingBytes(sibling)
		if err != nil {
			return plan, err
		}
		if size > uint64(maxVLLMModelBytes)-plan.Bytes {
			return plan, fmt.Errorf("vLLM model snapshot exceeds the bounded model library limit")
		}
		plan.Bytes += size
		plan.Files++
		if plan.Files > vllmModelReceiptMaxFiles {
			return plan, fmt.Errorf("vLLM model snapshot exceeds the bounded file-count limit")
		}
	}
	if plan.Files == 0 || plan.Bytes == 0 {
		return plan, fmt.Errorf("vLLM model source has no selected snapshot files")
	}
	return plan, nil
}

func addVLLMStorageBytes(values ...uint64) (uint64, error) {
	var total uint64
	for _, value := range values {
		if value > math.MaxUint64-total {
			return 0, fmt.Errorf("vLLM storage requirement overflow")
		}
		total += value
	}
	return total, nil
}

func existingVLLMStorageAncestor(value string) (string, error) {
	for {
		info, err := os.Lstat(value)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("vLLM storage root requires a real directory ancestor")
			}
			return value, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(value)
		if parent == value {
			return "", fmt.Errorf("vLLM storage root has no existing directory ancestor")
		}
		value = parent
	}
}

func observeVLLMStorage(root, allowedStage string, filesystem vllmFilesystemReader) (vllmStorageObservation, error) {
	var observation vllmStorageObservation
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return observation, fmt.Errorf("vLLM storage root must be an absolute canonical path")
	}
	if err := validateVLLMOwnedPath(root, root); err != nil {
		return observation, err
	}
	ancestor, err := existingVLLMStorageAncestor(root)
	if err != nil {
		return observation, err
	}
	available, device, err := filesystem(ancestor)
	if err != nil {
		return observation, fmt.Errorf("inspect vLLM account-available storage: %w", err)
	}
	if device == "" {
		return observation, errors.New("inspect vLLM account-available storage: device identity is unavailable")
	}
	observation = vllmStorageObservation{Root: root, Device: device, AvailableBytes: available}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return observation, nil
	}
	if err != nil {
		return vllmStorageObservation{}, err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".download-") && !strings.HasPrefix(entry.Name(), ".import-") && !strings.HasPrefix(entry.Name(), ".receive-") {
			continue
		}
		stage := filepath.Join(root, entry.Name())
		info, statErr := os.Lstat(stage)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || validateVLLMOwnedPath(root, stage) != nil {
			return vllmStorageObservation{}, fmt.Errorf("unresolved vLLM stage is not an owned directory")
		}
		_, stageDevice, deviceErr := filesystem(stage)
		if deviceErr != nil || stageDevice != device {
			return vllmStorageObservation{}, fmt.Errorf("unresolved vLLM stage changed storage device")
		}
		if entry.Name() != allowedStage {
			observation.UnresolvedStages++
		}
	}
	return observation, nil
}

// reviewVLLMDownloadStorage counts a retained partial stage toward the final
// payload ceiling. Requiring the entire snapshot again would make a legitimate
// resume impossible once a large cancellation had consumed free space.
func reviewVLLMDownloadStorage(root, allowedStage string, snapshotBytes uint64, filesystem vllmFilesystemReader, allocation vllmAllocationReader) (vllmStorageReview, error) {
	observation, err := observeVLLMStorage(root, allowedStage, filesystem)
	if err != nil {
		return vllmStorageReview{}, err
	}
	review := vllmStorageReview{
		Root: root, Device: observation.Device, SnapshotBytes: snapshotBytes,
		CacheCeilingBytes: vllmDownloadCacheCeilingBytes, HeadroomBytes: vllmPostOperationReserveBytes,
		AvailableBytes: observation.AvailableBytes, UnresolvedStages: observation.UnresolvedStages,
	}
	if snapshotBytes == 0 || snapshotBytes > uint64(maxVLLMModelBytes) {
		return review, fmt.Errorf("vLLM storage review requires bounded nonzero snapshot bytes")
	}
	if observation.UnresolvedStages != 0 {
		review.Reason = fmt.Sprintf("retained vLLM library has %d different unresolved stage(s); resume or reconcile them before another acquisition", observation.UnresolvedStages)
		return review, nil
	}
	stage := filepath.Join(root, allowedStage)
	if info, statErr := os.Lstat(stage); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return review, fmt.Errorf("vLLM resume stage is not an owned directory")
		}
		review.StagedBytes, err = vllmStageAllocatedBytes(stage, observation.Device, filesystem, allocation)
		if err != nil {
			return review, err
		}
	} else if !os.IsNotExist(statErr) {
		return review, statErr
	}
	operationCeiling, err := addVLLMStorageBytes(snapshotBytes, vllmDownloadCacheCeilingBytes)
	if err != nil {
		return review, err
	}
	if review.StagedBytes > operationCeiling {
		return review, fmt.Errorf("vLLM resume stage exceeds the payload-plus-cache ceiling")
	}
	additional := operationCeiling - review.StagedBytes
	review.RequiredAvailableBytes, err = addVLLMStorageBytes(additional, vllmPostOperationReserveBytes)
	if err != nil {
		return review, err
	}
	if observation.AvailableBytes >= additional {
		review.ProjectedRemainingBytes = observation.AvailableBytes - additional
	}
	if observation.AvailableBytes < review.RequiredAvailableBytes {
		review.Reason = fmt.Sprintf("vLLM storage requires %d bytes but the account has %d available; add %d bytes", review.RequiredAvailableBytes, observation.AvailableBytes, review.RequiredAvailableBytes-observation.AvailableBytes)
		return review, nil
	}
	review.Ready = true
	review.Reason = "storage review passed"
	return review, nil
}

func requireVLLMStorageReady(review vllmStorageReview) error {
	if !review.Ready {
		return fmt.Errorf("vLLM storage review held: %s", review.Reason)
	}
	return nil
}

func vllmStageAllocatedBytes(stage, device string, filesystem vllmFilesystemReader, allocation vllmAllocationReader) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(stage, func(value string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("vLLM stage contains a redirected path")
		}
		_, currentDevice, err := filesystem(value)
		if err != nil || currentDevice != device {
			return fmt.Errorf("vLLM stage changed storage device")
		}
		if !entry.IsDir() {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("vLLM stage contains a nonregular file")
			}
		}
		bytes, links, err := allocation(value)
		if err != nil || (!entry.IsDir() && links != 1) {
			return fmt.Errorf("vLLM stage allocation or link ownership is invalid")
		}
		if bytes > math.MaxUint64-total {
			return fmt.Errorf("vLLM stage allocated byte count overflow")
		}
		total += bytes
		return nil
	})
	return total, err
}
